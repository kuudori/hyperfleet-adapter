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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	if err = c.ensureReadDesire(ctx, readID, gvk.Version); err != nil {
		return nil, fmt.Errorf(
			"desireclient: failed to create paired read desire for %s/%s: %w", gvk.Kind, name, err)
	}

	applyID, err := buildIdentity(tc, desire.TypeApply, gvk, namespace, name)
	if err != nil {
		return nil, err
	}

	result, err := c.upsertApplyDesire(ctx, applyID, obj, kubeContent)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// upsertApplyDesire creates or updates the apply desire, deciding the
// operation via the manifest's hyperfleet.io/generation annotation — the same
// signal k8sclient/maestroclient compare on apply. This is unrelated to the
// store's own CAS Version, used below purely as the optimistic-concurrency
// token for UpdateApplyDesireSpec.
func (c *Client) upsertApplyDesire(
	ctx context.Context,
	id desire.Identity,
	obj *unstructured.Unstructured,
	kubeContent []byte,
) (*transportclient.ApplyResult, error) {
	existing, err := c.store.GetApplyDesire(ctx, id)
	if err != nil && !errors.Is(err, desire.ErrNotFound) {
		return nil, fmt.Errorf("desireclient: failed to get apply desire for %s/%s: %w", id.Namespace, id.Name, err)
	}
	exists := err == nil

	newGen := manifest.GetGenerationFromUnstructured(obj)
	var existingGen int64
	if exists {
		existingGen = generationFromKubeContent(existing.Spec.KubeContent)
	}

	decision := manifest.CompareGenerations(newGen, existingGen, exists)
	result := &transportclient.ApplyResult{Operation: decision.Operation, Reason: decision.Reason}

	switch decision.Operation {
	case manifest.OperationCreate:
		if _, err := c.store.CreateApplyDesire(ctx, desire.ApplyDesire{
			Identity: id,
			Owner:    c.owner,
			Spec:     desire.ApplySpec{KubeContent: kubeContent},
		}); err != nil {
			return nil, fmt.Errorf("desireclient: failed to create apply desire for %s/%s: %w", id.Namespace, id.Name, err)
		}
	case manifest.OperationUpdate:
		if _, err := c.store.UpdateApplyDesireSpec(
			ctx, id, desire.ApplySpec{KubeContent: kubeContent}, c.owner, existing.Version,
		); err != nil {
			return nil, fmt.Errorf("desireclient: failed to update apply desire for %s/%s: %w", id.Namespace, id.Name, err)
		}
	case manifest.OperationSkip:
		// Nothing to do.
	default:
		return nil, fmt.Errorf("desireclient: unexpected apply decision operation %q", decision.Operation)
	}

	slog.DebugContext(ctx, "applied desire resource",
		"namespace", id.Namespace,
		"name", id.Name,
		"operation", result.Operation,
		"reason", result.Reason)
	return result, nil
}

// ensureReadDesire creates the paired read desire if it doesn't already
// exist. ErrAlreadyExists is a no-op success.
func (c *Client) ensureReadDesire(ctx context.Context, id desire.Identity, targetVersion string) error {
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
