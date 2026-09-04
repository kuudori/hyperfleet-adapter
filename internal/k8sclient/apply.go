package k8sclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/logctx"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	hfl "github.com/openshift-hyperfleet/hyperfleet-logger"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Type aliases for backward compatibility.
type (
	ApplyOptions = transportclient.ApplyOptions
	ApplyResult  = transportclient.ApplyResult
)

// ApplyResource implements transportclient.TransportClient.
// It accepts rendered JSON/YAML bytes, parses them into an unstructured K8s resource,
// discovers the existing resource by name, and applies with generation comparison.
func (c *Client) ApplyResource(
	ctx context.Context,
	manifestBytes []byte,
	opts *transportclient.ApplyOptions,
	_ transportclient.TransportContext,
) (*transportclient.ApplyResult, error) {
	if len(manifestBytes) == 0 {
		return nil, fmt.Errorf("manifest bytes cannot be empty")
	}

	// Parse bytes into unstructured
	obj, err := parseToUnstructured(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	// Discover existing resource by name
	gvk := obj.GroupVersionKind()
	existing, err := c.GetResource(ctx, gvk, obj.GetNamespace(), obj.GetName(), nil)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get existing resource %s/%s: %w", gvk.Kind, obj.GetName(), err)
	}

	// Apply with generation comparison
	return c.ApplyManifest(ctx, obj, existing, opts)
}

// ApplyManifest creates or updates a Kubernetes resource based on generation comparison.
// This is the K8s-specific method that operates on parsed unstructured resources.
//
// If the resource doesn't exist, it creates it.
// If it exists and the generation differs, it updates (or recreates if RecreateOnChange=true).
// If it exists and the generation matches, it skips the update (idempotent).
//
// The manifest must have the hyperfleet.io/generation annotation set.
func (c *Client) ApplyManifest(
	ctx context.Context,
	newManifest *unstructured.Unstructured,
	existing *unstructured.Unstructured,
	opts *ApplyOptions,
) (*ApplyResult, error) {
	if newManifest == nil {
		return nil, fmt.Errorf("new manifest cannot be nil")
	}

	if opts == nil {
		opts = &ApplyOptions{}
	}

	// Get generation from new manifest
	newGen := manifest.GetGenerationFromUnstructured(newManifest)

	// Get existing generation (0 if not found)
	var existingGen int64
	if existing != nil {
		existingGen = manifest.GetGenerationFromUnstructured(existing)
	}

	// Compare generations to determine operation
	decision := manifest.CompareGenerations(newGen, existingGen, existing != nil)

	result := &ApplyResult{
		Operation: decision.Operation,
		Reason:    decision.Reason,
	}

	// Handle recreateOnChange override
	if decision.Operation == manifest.OperationUpdate && opts.RecreateOnChange {
		result.Operation = manifest.OperationRecreate
		result.Reason = fmt.Sprintf("%s, recreateOnChange=true", decision.Reason)
	}

	gvk := newManifest.GroupVersionKind()
	name := newManifest.GetName()
	ctx = hfl.Set(ctx, logctx.K8sKindKey, gvk.Kind)
	ctx = hfl.Set(ctx, logctx.K8sNameKey, name)
	ctx = hfl.Set(ctx, logctx.K8sNamespaceKey, newManifest.GetNamespace())

	slog.DebugContext(ctx, "apply manifest", "operation", result.Operation, "reason", result.Reason)

	// Execute the operation
	var applyErr error
	switch result.Operation {
	case manifest.OperationCreate:
		_, applyErr = c.CreateResource(ctx, newManifest)
		if applyErr != nil && apierrors.IsAlreadyExists(applyErr) {
			// Resource was created by a concurrent process between our Get and Create.
			// Treat as a successful no-op rather than an error.
			slog.DebugContext(ctx, "resource already exists (concurrent create), treating as skip")
			result.Operation = manifest.OperationSkip
			result.Reason = "already exists (concurrent create)"
			applyErr = nil
		}

	case manifest.OperationUpdate:
		// Preserve resourceVersion and UID from existing for update
		newManifest.SetResourceVersion(existing.GetResourceVersion())
		newManifest.SetUID(existing.GetUID())
		_, applyErr = c.UpdateResource(ctx, newManifest)

	case manifest.OperationRecreate:
		_, applyErr = c.recreateResource(ctx, existing, newManifest)

	case manifest.OperationSkip:
		// Nothing to do

	case manifest.OperationDelete:
		// Not handled in ApplyManifest — deletion is performed via DeleteResource directly.
	}

	if applyErr != nil {
		return nil, fmt.Errorf("failed to %s resource %s/%s: %w",
			result.Operation, gvk.Kind, name, applyErr)
	}

	return result, nil
}

// recreateResource deletes and recreates a Kubernetes resource.
// It waits for the resource to be fully deleted before creating the new one
// to avoid race conditions with Kubernetes asynchronous deletion.
func (c *Client) recreateResource(
	ctx context.Context,
	existing *unstructured.Unstructured,
	newManifest *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	gvk := existing.GroupVersionKind()
	namespace := existing.GetNamespace()
	name := existing.GetName()
	ctx = hfl.Set(ctx, logctx.K8sKindKey, gvk.Kind)
	ctx = hfl.Set(ctx, logctx.K8sNameKey, name)
	ctx = hfl.Set(ctx, logctx.K8sNamespaceKey, namespace)

	// Delete the existing resource
	slog.DebugContext(ctx, "deleting resource for recreation")
	if err := c.deleteResource(ctx, gvk, namespace, name); err != nil {
		return nil, fmt.Errorf("failed to delete resource for recreation: %w", err)
	}

	// Wait for the resource to be fully deleted
	slog.DebugContext(ctx, "waiting for resource deletion to complete")
	if err := c.waitForDeletion(ctx, gvk, namespace, name); err != nil {
		return nil, fmt.Errorf("failed waiting for resource deletion: %w", err)
	}

	// Create the new resource
	slog.DebugContext(ctx, "creating new resource after deletion confirmed")
	return c.CreateResource(ctx, newManifest)
}

// waitForDeletion polls until the resource is confirmed deleted or context times out.
// Returns nil when the resource is confirmed gone (NotFound), or an error otherwise.
func (c *Client) waitForDeletion(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
) error {
	const pollInterval = 100 * time.Millisecond

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "context canceled/timed out while waiting for deletion")
			return fmt.Errorf("context canceled while waiting for resource deletion: %w", ctx.Err())
		case <-ticker.C:
			_, err := c.GetResource(ctx, gvk, namespace, name, nil)
			if err != nil {
				// NotFound means the resource is deleted - this is success
				if apierrors.IsNotFound(err) {
					slog.DebugContext(ctx, "resource deletion confirmed")
					return nil
				}
				// Any other error is unexpected
				slog.ErrorContext(ctx, "error checking deletion status", "error", err)
				return fmt.Errorf("error checking deletion status: %w", err)
			}
			// Resource still exists, continue polling
			slog.DebugContext(ctx, "resource still exists, waiting for deletion")
		}
	}
}

// parseToUnstructured parses JSON or YAML bytes into an unstructured Kubernetes resource.
func parseToUnstructured(data []byte) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}

	// Try JSON first
	if err := json.Unmarshal(data, &obj.Object); err == nil && obj.Object != nil {
		return obj, nil
	}

	// Fall back to YAML → JSON → unstructured
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("failed to convert YAML to JSON: %w", err)
	}

	if err := json.Unmarshal(jsonData, &obj.Object); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	return obj, nil
}
