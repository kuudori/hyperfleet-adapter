package desireclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/constants"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ApplyResource implements transportclient.TransportClient. It upserts an
// apply desire for the rendered manifest and auto-creates its paired read
// desire so the applied resource becomes visible to discovery. A read-desire
// pairing failure is returned as an error: an apply desire without its read
// desire is permanently invisible to discovery.
func (c *Client) ApplyResource(
	ctx context.Context,
	manifestBytes []byte,
	opts *transportclient.ApplyOptions,
	target transportclient.TransportContext,
) (*transportclient.ApplyResult, error) {
	if len(manifestBytes) == 0 {
		return nil, fmt.Errorf("remote transport: manifest bytes cannot be empty")
	}

	tc, err := resolveTransportContext(target)
	if err != nil {
		return nil, err
	}

	obj, err := parseToUnstructured(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("remote transport: failed to parse manifest: %w", err)
	}
	if err = manifest.ValidateGenerationFromUnstructured(obj); err != nil {
		return nil, fmt.Errorf("remote transport: invalid manifest generation: %w", err)
	}

	// The store's ApplySpec.KubeContent must be valid JSON, but manifestBytes
	// may have been YAML (parseToUnstructured accepts both). Re-marshal the
	// parsed object rather than storing the original bytes verbatim.
	kubeContent, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("remote transport: failed to marshal manifest to JSON: %w", err)
	}

	// At most one re-run: the store reported that another writer created or
	// changed the apply request since it was read. A second conflict in a row
	// is left to the next event.
	result, conflict, err := c.applyOnce(ctx, tc, obj, kubeContent)
	if !conflict {
		return result, err
	}
	result, _, err = c.applyOnce(ctx, tc, obj, kubeContent)
	return result, err
}

// applyState is what applyOnce reads from the store before it writes.
type applyState struct {
	paired      *desire.ReadDesire
	existing    *desire.ApplyDesire
	gvk         schema.GroupVersionKind
	applyID     desire.Identity
	readID      desire.Identity
	newGen      int64
	existingGen int64
}

// applyOnce reads the stored state and writes the apply request once. It
// reports a conflict that a fresh read can resolve.
func (c *Client) applyOnce(
	ctx context.Context, tc *TransportContext, obj *unstructured.Unstructured, kubeContent []byte,
) (*transportclient.ApplyResult, bool, error) {
	st, err := c.loadApplyState(ctx, tc, obj)
	if err != nil {
		return nil, false, err
	}
	if stale, isStale, staleErr := c.staleResult(ctx, tc, st); isStale || staleErr != nil {
		return stale, mirrorConflict(staleErr), staleErr
	}

	decision := manifest.CompareGenerations(st.newGen, st.existingGen, st.existing != nil)
	result := &transportclient.ApplyResult{Operation: decision.Operation, Reason: decision.Reason}

	// Pairing self-heals on every path, including skip: an externally
	// deleted read desire is re-created on the next event. But the mirror
	// only recreates to a new TargetVersion when this call is actually
	// writing new apply content — on skip, ApplyDesire.Spec.KubeContent
	// stays at the old API version, so the mirror must too. They move
	// together or not at all.
	recreate := decision.Operation != manifest.OperationSkip
	if err = c.ensureReadDesireWithExisting(
		ctx, st.readID, st.gvk.Version, tc.ResourceID, recreate, st.paired,
	); err != nil {
		return nil, mirrorConflict(err), fmt.Errorf(
			"remote transport: failed to create the paired mirror for %s/%s: %w", st.gvk.Kind, st.applyID.Name, err)
	}

	if conflict, err := c.writeApply(ctx, tc, st, decision.Operation, kubeContent); err != nil {
		return nil, conflict, err
	}

	slog.DebugContext(ctx, "ApplyResource completed",
		"namespace", st.applyID.Namespace, "name", st.applyID.Name,
		"operation", result.Operation, "reason", result.Reason)
	return result, false, nil
}

// loadApplyState reads the stored apply request and its paired mirror and
// checks that this adapter owns both.
func (c *Client) loadApplyState(
	ctx context.Context, tc *TransportContext, obj *unstructured.Unstructured,
) (*applyState, error) {
	st := &applyState{
		gvk:    obj.GroupVersionKind(),
		newGen: manifest.GetGenerationFromUnstructured(obj),
	}
	namespace, name := obj.GetNamespace(), obj.GetName()
	var err error
	if st.readID, err = buildIdentity(tc, desire.TypeRead, st.gvk, namespace, name); err != nil {
		return nil, err
	}
	if st.applyID, err = buildIdentity(tc, desire.TypeApply, st.gvk, namespace, name); err != nil {
		return nil, err
	}

	existing, err := c.store.GetApplyDesire(ctx, st.applyID)
	if err != nil && !errors.Is(err, desire.ErrNotFound) {
		return nil, fmt.Errorf("remote transport: failed to get the apply request for %s/%s: %w",
			st.applyID.Namespace, st.applyID.Name, storeErr(err))
	}
	if err == nil {
		st.existing = &existing
		if err = desire.CheckOwner(ctx, st.applyID, st.existing.Owner, c.owner); err != nil {
			return nil, fmt.Errorf("remote transport: apply request owner: %w", storeErr(err))
		}
		st.existingGen = generationFromKubeContent(st.existing.Spec.KubeContent)
	}

	read, err := c.store.GetReadDesire(ctx, st.readID)
	if err != nil && !errors.Is(err, desire.ErrNotFound) {
		return nil, fmt.Errorf("remote transport: failed to get the paired mirror for %s/%s: %w",
			st.readID.Namespace, st.readID.Name, storeErr(err))
	}
	if err == nil {
		if err = desire.CheckOwner(ctx, st.readID, read.Owner, c.owner); err != nil {
			return nil, fmt.Errorf("remote transport: mirror owner: %w", storeErr(err))
		}
		st.paired = &read
	}
	return st, nil
}

// staleResult returns a skip result and true when the incoming generation is
// below the stored apply request or the annotated mirror.
func (c *Client) staleResult(
	ctx context.Context, tc *TransportContext, st *applyState,
) (*transportclient.ApplyResult, bool, error) {
	if st.existing != nil && st.newGen < st.existingGen {
		stored, parseErr := parseToUnstructured(st.existing.Spec.KubeContent)
		if parseErr != nil {
			return nil, false, fmt.Errorf("remote transport: failed to parse the stored apply request for %s/%s: %w",
				st.applyID.Namespace, st.applyID.Name, parseErr)
		}
		if err := c.ensureReadDesireWithExisting(
			ctx, st.readID, stored.GroupVersionKind().Version, tc.ResourceID, false, st.paired,
		); err != nil {
			return nil, false, fmt.Errorf("remote transport: failed to create the paired mirror for %s/%s: %w",
				st.gvk.Kind, st.applyID.Name, err)
		}
		return staleGenerationResult(ctx, st.applyID, st.newGen, st.existingGen, 0), true, nil
	}
	if st.paired == nil {
		return nil, false, nil
	}
	mirrorGen, err := c.mirrorGeneration(st)
	if err != nil || st.newGen >= mirrorGen {
		return nil, false, err
	}
	return staleGenerationResult(ctx, st.applyID, st.newGen, st.existingGen, mirrorGen), true, nil
}

// mirrorGeneration returns the generation annotated on the paired mirror's
// content, or 0 when the mirror has no annotated content yet.
func (c *Client) mirrorGeneration(st *applyState) (int64, error) {
	mirrored, readErr := c.decodeReadDesire(st.gvk, st.applyID.Namespace, st.applyID.Name, *st.paired)
	switch {
	case errors.Is(readErr, transportclient.ErrNotSyncedYet), apierrors.IsNotFound(readErr):
		// No content can establish a generation floor yet.
		return 0, nil
	case readErr != nil:
		return 0, fmt.Errorf(
			"remote transport: failed to decode the paired mirror for %s/%s: %w", st.applyID.Namespace, st.applyID.Name, readErr)
	}
	if _, annotated := mirrored.GetAnnotations()[constants.AnnotationGeneration]; !annotated {
		return 0, nil
	}
	mirrorGVK := mirrored.GroupVersionKind()
	if mirrorGVK.Group != st.gvk.Group || mirrorGVK.Kind != st.gvk.Kind ||
		mirrored.GetNamespace() != st.applyID.Namespace || mirrored.GetName() != st.applyID.Name {
		return 0, fmt.Errorf(
			"remote transport: annotated read mirror target does not match %s/%s", st.applyID.Namespace, st.applyID.Name)
	}
	return manifest.GetGenerationFromUnstructured(mirrored), nil
}

// writeApply creates or updates the apply request as the decision says. It
// reports a conflict that a fresh read can resolve.
func (c *Client) writeApply(
	ctx context.Context, tc *TransportContext, st *applyState, op manifest.Operation, kubeContent []byte,
) (bool, error) {
	switch op {
	case manifest.OperationCreate:
		_, err := c.store.CreateApplyDesire(ctx, desire.ApplyDesire{
			Identity: st.applyID,
			Owner:    c.owner,
			OriginID: tc.ResourceID,
			Spec:     desire.ApplySpec{KubeContent: kubeContent},
		})
		if err == nil {
			return false, nil
		}
		c.logApplyError(ctx, st.applyID, err)
		conflict := errors.Is(err, desire.ErrAlreadyExists)
		if errors.Is(err, desire.ErrDeletePending) {
			err = transportclient.ErrDeletionPending
		} else {
			err = storeErr(err)
		}
		return conflict, fmt.Errorf(
			"remote transport: failed to create the apply request for %s/%s: %w", st.applyID.Namespace, st.applyID.Name, err)
	case manifest.OperationUpdate:
		_, err := c.store.UpdateApplyDesireSpec(
			ctx, st.applyID, desire.ApplySpec{KubeContent: kubeContent}, c.owner, st.existing.Version)
		if err == nil {
			return false, nil
		}
		c.logApplyError(ctx, st.applyID, err)
		return errors.Is(err, desire.ErrVersionConflict), fmt.Errorf(
			"remote transport: failed to update the apply request for %s/%s: %w",
			st.applyID.Namespace, st.applyID.Name, storeErr(err))
	case manifest.OperationSkip:
		return false, nil
	case manifest.OperationRecreate, manifest.OperationDelete:
		return false, fmt.Errorf("remote transport: unexpected apply decision operation %q", op)
	default:
		return false, fmt.Errorf("remote transport: unexpected apply decision operation %q", op)
	}
}

func staleGenerationResult(
	ctx context.Context, id desire.Identity, incoming, stored, mirrored int64,
) *transportclient.ApplyResult {
	reason := fmt.Sprintf("stale generation %d below stored %d or mirrored %d", incoming, stored, mirrored)
	if mirrored > stored && stored > 0 {
		slog.WarnContext(ctx, "read mirror ahead of stored apply desire",
			"namespace", id.Namespace, "name", id.Name,
			"incoming_generation", incoming, "stored_generation", stored, "mirrored_generation", mirrored)
	}
	return &transportclient.ApplyResult{Operation: manifest.OperationSkip, Reason: reason}
}

func (c *Client) logApplyError(ctx context.Context, id desire.Identity, err error) {
	attrs := []any{"namespace", id.Namespace, "name", id.Name, "error", err}
	switch {
	case errors.Is(err, desire.ErrOwnerConflict):
		slog.ErrorContext(ctx,
			"ApplyResource: existing apply desire owned by another adapter, retrying on next event", attrs...)
	case errors.Is(err, desire.ErrDeletePending):
		slog.WarnContext(ctx, "ApplyResource: delete pending, retrying on next event", attrs...)
	case errors.Is(err, desire.ErrVersionConflict):
		slog.WarnContext(ctx, "ApplyResource: version conflict, re-reading once, then retrying on next event", attrs...)
	case errors.Is(err, desire.ErrAlreadyExists):
		slog.WarnContext(ctx,
			"ApplyResource: apply desire created concurrently, re-reading once, then retrying on next event", attrs...)
	default:
		slog.ErrorContext(ctx, "ApplyResource: operation failed with unexpected error", attrs...)
	}
}

// ensureReadDesire reads the paired mirror and creates or replaces it as needed.
// If a fresh read can resolve a write conflict, it retries once and returns any second conflict.
func (c *Client) ensureReadDesire(
	ctx context.Context, id desire.Identity, targetVersion, originID string, recreate bool,
) error {
	for attempt := 0; ; attempt++ {
		read, err := c.store.GetReadDesire(ctx, id)
		if err != nil && !errors.Is(err, desire.ErrNotFound) {
			return fmt.Errorf("remote transport: failed to get the mirror for %s/%s: %w", id.Namespace, id.Name, storeErr(err))
		}
		var existing *desire.ReadDesire
		if err == nil {
			existing = &read
		}
		err = c.ensureReadDesireWithExisting(ctx, id, targetVersion, originID, recreate, existing)
		if attempt == 1 || !mirrorConflict(err) {
			return err
		}
	}
}

func (c *Client) ensureReadDesireWithExisting(
	ctx context.Context, id desire.Identity, targetVersion, originID string, recreate bool,
	existing *desire.ReadDesire,
) error {
	if existing != nil {
		if err := desire.CheckOwner(ctx, id, existing.Owner, c.owner); err != nil {
			return storeErr(err)
		}
		if !recreate || existing.TargetVersion == targetVersion {
			return nil
		}
		if err := c.store.DeleteReadDesire(ctx, id, c.owner, existing.Version); err != nil {
			return fmt.Errorf("remote transport: failed to delete the existing mirror for %s/%s: %w",
				id.Namespace, id.Name, storeErr(err))
		}
	}

	_, err := c.store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity:      id,
		Owner:         c.owner,
		OriginID:      originID,
		TargetVersion: targetVersion,
	})
	if err == nil {
		return nil
	}
	return fmt.Errorf("remote transport: failed to create the mirror for %s/%s: %w", id.Namespace, id.Name, storeErr(err))
}

func mirrorConflict(err error) bool {
	return reReadable(err) || errors.Is(err, desire.ErrAlreadyExists)
}
