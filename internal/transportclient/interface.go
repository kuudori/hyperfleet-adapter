package transportclient

import (
	"context"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TransportClient defines the interface for applying Kubernetes resources.
// This interface abstracts the underlying implementation, allowing resources
// to be applied via different backends:
//   - Direct Kubernetes API (k8sclient)
//   - Maestro/OCM ManifestWork (maestroclient)
//
// All implementations must support generation-aware apply operations:
//   - Create if resource doesn't exist
//   - Update if generation changed
//   - Skip if generation matches (idempotent)
type TransportClient interface {
	// ApplyResource applies a rendered manifest (JSON/YAML bytes).
	// Each backend parses the bytes into its expected type:
	//   - k8sclient: parses as K8s resource (unstructured), applies to K8s API
	//   - maestroclient: parses as ManifestWork, applies via Maestro gRPC
	//
	// The backend handles discovery of existing resources internally for generation comparison.
	//
	// Parameters:
	//   - ctx: Context for the operation
	//   - manifest: Rendered JSON/YAML bytes of the resource to apply
	//   - opts: Apply options (e.g., RecreateOnChange). Nil uses defaults.
	//   - target: Per-request routing context (nil for k8sclient)
	ApplyResource(
		ctx context.Context,
		manifest []byte,
		opts *ApplyOptions,
		target TransportContext,
	) (*ApplyResult, error)

	// GetResource retrieves a single Kubernetes resource by GVK, namespace, and name.
	// The target parameter provides per-request routing context (nil for k8sclient).
	// Returns the resource or an error if not found.
	GetResource(
		ctx context.Context,
		gvk schema.GroupVersionKind,
		namespace, name string,
		target TransportContext,
	) (*unstructured.Unstructured, error)

	// DiscoverResources discovers Kubernetes resources based on the Discovery configuration.
	// The target parameter provides per-request routing context (nil for k8sclient).
	// If Discovery.IsSingleResource() is true, it fetches a single resource by name.
	// Otherwise, it lists resources matching the label selector.
	DiscoverResources(
		ctx context.Context,
		gvk schema.GroupVersionKind,
		discovery manifest.Discovery,
		target TransportContext,
	) (*unstructured.UnstructuredList, error)

	// DeleteResource deletes a resource by GVK, namespace, and name.
	// For K8s transport: uses the propagationPolicy from opts.
	// For Maestro transport: calls ManifestWork delete; propagationPolicy is ignored.
	// Returns nil if the resource is not found (idempotent).
	DeleteResource(
		ctx context.Context,
		gvk schema.GroupVersionKind,
		namespace, name string,
		opts *DeleteOptions,
		target TransportContext,
	) error
}

// DesireCleaner is an optional interface that transport clients may
// implement to remove transport-layer bookkeeping after a resource has been
// confirmed deleted. The executor calls this when pre-delete discovery
// confirms the resource is already gone, and when post-delete re-discovery
// confirms it was removed. Transports that also implement DeletionLifecycle
// are confirmed through ProbeDeletion instead of re-discovery.
type DesireCleaner interface {
	CleanupAfterDeletion(
		ctx context.Context,
		gvk schema.GroupVersionKind,
		namespace, name string,
		target TransportContext,
	) error
}

// DeletionState is the progress of a transport's delete request for one
// by-name target. It says nothing about the object itself: discovery reports
// that, and an asynchronous transport's mirror may lag a confirmed delete.
type DeletionState uint8

const (
	// DeletionNone means no delete request exists for the target.
	DeletionNone DeletionState = iota
	// DeletionPending means a delete request exists but is not confirmed yet.
	DeletionPending
	// DeletionConfirmed means the transport confirmed the target is gone.
	DeletionConfirmed
)

// DeletionLifecycle is an optional capability for transports that delete
// asynchronously. The executor confirms deletion with the read-only
// ProbeDeletion instead of re-discovering the object, then calls
// CleanupAfterDeletion to remove the transport's bookkeeping.
type DeletionLifecycle interface {
	DesireCleaner
	ProbeDeletion(ctx context.Context, gvk schema.GroupVersionKind,
		namespace, name string, target TransportContext) (DeletionState, error)
}
