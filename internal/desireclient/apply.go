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
		return nil, fmt.Errorf("desireclient: manifest bytes cannot be empty")
	}

	tc, err := resolveTransportContext(target)
	if err != nil {
		return nil, err
	}

	obj, err := parseToUnstructured(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("desireclient: failed to parse manifest: %w", err)
	}
	if err = manifest.ValidateGenerationFromUnstructured(obj); err != nil {
		return nil, fmt.Errorf("desireclient: invalid manifest generation: %w", err)
	}

	// The store's ApplySpec.KubeContent must be valid JSON, but manifestBytes
	// may have been YAML (parseToUnstructured accepts both). Re-marshal the
	// parsed object rather than storing the original bytes verbatim.
	kubeContent, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("desireclient: failed to marshal manifest to JSON: %w", err)
	}

	gvk := obj.GroupVersionKind()
	namespace, name := obj.GetNamespace(), obj.GetName()

	readID, err := buildIdentity(tc, desire.TypeRead, gvk, namespace, name)
	if err != nil {
		return nil, err
	}

	applyID, err := buildIdentity(tc, desire.TypeApply, gvk, namespace, name)
	if err != nil {
		return nil, err
	}

	existing, err := c.store.GetApplyDesire(ctx, applyID)
	if err != nil && !errors.Is(err, desire.ErrNotFound) {
		return nil, fmt.Errorf("desireclient: failed to get apply desire for %s/%s: %w", applyID.Namespace, applyID.Name, err)
	}
	exists := err == nil

	newGen := manifest.GetGenerationFromUnstructured(obj)
	var existingGen int64
	if exists {
		if err = desire.CheckOwner(ctx, applyID, existing.Owner, c.owner); err != nil {
			return nil, fmt.Errorf("desireclient: apply desire owner: %w", err)
		}
		existingGen = generationFromKubeContent(existing.Spec.KubeContent)
	}

	read, err := c.store.GetReadDesire(ctx, readID)
	if err != nil && !errors.Is(err, desire.ErrNotFound) {
		return nil, fmt.Errorf("desireclient: failed to get paired read desire for %s/%s: %w",
			readID.Namespace, readID.Name, err)
	}
	var paired *desire.ReadDesire
	if err == nil {
		if err = desire.CheckOwner(ctx, readID, read.Owner, c.owner); err != nil {
			return nil, fmt.Errorf("desireclient: read desire owner: %w", err)
		}
		paired = &read
	}
	if exists && newGen < existingGen {
		stored, parseErr := parseToUnstructured(existing.Spec.KubeContent)
		if parseErr != nil {
			return nil, fmt.Errorf("desireclient: failed to parse stored apply desire for %s/%s: %w", namespace, name, parseErr)
		}
		if err = c.ensureReadDesireWithExisting(ctx, readID, stored.GroupVersionKind().Version, false, paired); err != nil {
			return nil, fmt.Errorf("desireclient: failed to create paired read desire for %s/%s: %w", gvk.Kind, name, err)
		}
		return staleGenerationResult(ctx, applyID, newGen, existingGen, 0), nil
	}
	if paired != nil {
		mirrored, readErr := c.decodeReadDesire(gvk, namespace, name, read)
		switch {
		case errors.Is(readErr, ErrNotSyncedYet), apierrors.IsNotFound(readErr):
			// No content can establish a generation floor yet.
		case readErr != nil:
			return nil, fmt.Errorf("desireclient: failed to decode paired read desire for %s/%s: %w", namespace, name, readErr)
		default:
			mirrorGen := manifest.GetGenerationFromUnstructured(mirrored)
			if _, annotated := mirrored.GetAnnotations()[constants.AnnotationGeneration]; annotated {
				mirrorGVK := mirrored.GroupVersionKind()
				if mirrorGVK.Group != gvk.Group || mirrorGVK.Kind != gvk.Kind ||
					mirrored.GetNamespace() != namespace || mirrored.GetName() != name {
					return nil, fmt.Errorf("desireclient: annotated read mirror target does not match %s/%s", namespace, name)
				}
				if newGen < mirrorGen {
					return staleGenerationResult(ctx, applyID, newGen, existingGen, mirrorGen), nil
				}
			}
		}
	}

	decision := manifest.CompareGenerations(newGen, existingGen, exists)
	result := &transportclient.ApplyResult{Operation: decision.Operation, Reason: decision.Reason}

	// Pairing self-heals on every path, including skip: an externally
	// deleted read desire is re-created on the next event. But the mirror
	// only recreates to a new TargetVersion when this call is actually
	// writing new apply content — on skip, ApplyDesire.Spec.KubeContent
	// stays at the old API version, so the mirror must too. They move
	// together or not at all.
	recreate := decision.Operation != manifest.OperationSkip
	if err = c.ensureReadDesireWithExisting(ctx, readID, gvk.Version, recreate, paired); err != nil {
		return nil, fmt.Errorf(
			"desireclient: failed to create paired read desire for %s/%s: %w", gvk.Kind, name, err)
	}

	switch decision.Operation {
	case manifest.OperationCreate:
		if _, err = c.store.CreateApplyDesire(ctx, desire.ApplyDesire{
			Identity: applyID,
			Owner:    c.owner,
			Spec:     desire.ApplySpec{KubeContent: kubeContent},
		}); err != nil {
			c.logApplyError(ctx, applyID, err)
			return nil, fmt.Errorf("desireclient: failed to create apply desire for %s/%s: %w", namespace, name, err)
		}
	case manifest.OperationUpdate:
		if _, err = c.store.UpdateApplyDesireSpec(
			ctx, applyID, desire.ApplySpec{KubeContent: kubeContent}, c.owner, existing.Version,
		); err != nil {
			c.logApplyError(ctx, applyID, err)
			return nil, fmt.Errorf(
				"desireclient: failed to update apply desire for %s/%s: %w", applyID.Namespace, applyID.Name, err)
		}
	case manifest.OperationSkip:
		// Nothing to do.
	default:
		return nil, fmt.Errorf("desireclient: unexpected apply decision operation %q", decision.Operation)
	}

	slog.DebugContext(ctx, "ApplyResource completed",
		"namespace", applyID.Namespace, "name", applyID.Name, "operation", result.Operation, "reason", result.Reason)
	return result, nil
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
		slog.WarnContext(ctx, "ApplyResource: version conflict, retrying on next event", attrs...)
	default:
		slog.ErrorContext(ctx, "ApplyResource: operation failed with unexpected error", attrs...)
	}
}

// ensureReadDesire creates the paired read desire if it doesn't already
// exist. ErrAlreadyExists is a no-op success.
func (c *Client) ensureReadDesire(ctx context.Context, id desire.Identity, targetVersion string, recreate bool) error {
	read, err := c.store.GetReadDesire(ctx, id)
	if err != nil && !errors.Is(err, desire.ErrNotFound) {
		return fmt.Errorf("desireclient: failed to get read desire for %s/%s: %w", id.Namespace, id.Name, err)
	}
	if err == nil {
		return c.ensureReadDesireWithExisting(ctx, id, targetVersion, recreate, &read)
	}
	return c.ensureReadDesireWithExisting(ctx, id, targetVersion, recreate, nil)
}

func (c *Client) ensureReadDesireWithExisting(
	ctx context.Context, id desire.Identity, targetVersion string, recreate bool, existing *desire.ReadDesire,
) error {
	// At most one retry: a second ErrAlreadyExists in a row means another
	// writer is racing on every attempt, which is left to the next event.
	for attempt := 0; ; attempt++ {
		if existing != nil {
			if err := desire.CheckOwner(ctx, id, existing.Owner, c.owner); err != nil {
				return err
			}
			if !recreate || existing.TargetVersion == targetVersion {
				return nil
			}
			if err := c.store.DeleteReadDesire(ctx, id, c.owner, existing.Version); err != nil {
				return fmt.Errorf("desireclient: failed to delete existing read desire for %s/%s: %w", id.Namespace, id.Name, err)
			}
		}

		_, err := c.store.CreateReadDesire(ctx, desire.ReadDesire{
			Identity:      id,
			Owner:         c.owner,
			TargetVersion: targetVersion,
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, desire.ErrAlreadyExists) || attempt > 0 {
			return fmt.Errorf("desireclient: failed to create read desire for %s/%s: %w", id.Namespace, id.Name, err)
		}

		des, err := c.store.GetReadDesire(ctx, id)
		if err != nil {
			return fmt.Errorf("desireclient: failed to get existing read desire for %s/%s: %w", id.Namespace, id.Name, err)
		}
		existing = &des
	}
}
