package desireclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
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
		existingGen = generationFromKubeContent(existing.Spec.KubeContent)
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
	if err = c.ensureReadDesire(ctx, readID, gvk.Version, recreate); err != nil {
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
	_, err := c.store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity:      id,
		Owner:         c.owner,
		TargetVersion: targetVersion,
	})
	if err == nil {
		return nil
	}

	if !errors.Is(err, desire.ErrAlreadyExists) {
		return fmt.Errorf("desireclient: failed to create read desire for %s/%s: %w", id.Namespace, id.Name, err)
	}

	if !recreate {
		return nil
	}

	des, err := c.store.GetReadDesire(ctx, id)
	if err != nil {
		return fmt.Errorf("desireclient: failed to get existing read desire for %s/%s: %w", id.Namespace, id.Name, err)
	}

	if des.TargetVersion == targetVersion {
		return nil
	}

	err = c.store.DeleteReadDesire(ctx, id, c.owner, des.Version)
	if err != nil {
		return fmt.Errorf("desireclient: failed to delete existing read desire for %s/%s: %w", id.Namespace, id.Name, err)
	}

	if _, err := c.store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity:      id,
		Owner:         c.owner,
		TargetVersion: targetVersion,
	}); err != nil {
		return fmt.Errorf("desireclient: failed to create read desire for %s/%s: %w", id.Namespace, id.Name, err)
	}

	return nil

}
