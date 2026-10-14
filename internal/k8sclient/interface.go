package k8sclient

import (
	"context"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// K8sClient defines the interface for Kubernetes operations.
// This interface allows for easy mocking in unit tests without requiring
// a real Kubernetes cluster or DryRun mode.
//
// K8sClient extends TransportClient with K8s-specific operations.
// Both k8sclient.Client and desireclient.Client implement TransportClient,
// allowing the executor to use either as the transport backend.
type K8sClient interface {
	// Embed TransportClient interface
	// This provides: ApplyResource([]byte), GetResource, DiscoverResources
	transportclient.TransportClient

	// ApplyManifest creates or updates a single Kubernetes resource based on generation comparison.
	// This is a K8sClient-specific method for applying parsed unstructured resources.
	//
	// If the resource doesn't exist, it creates it.
	// If it exists and the generation differs, it updates (or recreates if RecreateOnChange=true).
	// If it exists and the generation matches, it skips the update (idempotent).
	//
	// The manifest must have the hyperfleet.io/generation annotation set.
	ApplyManifest(
		ctx context.Context,
		newManifest *unstructured.Unstructured,
		existing *unstructured.Unstructured,
		opts *ApplyOptions,
	) (*ApplyResult, error)

	// Resource CRUD operations

	// CreateResource creates a new Kubernetes resource.
	// Returns the created resource with server-generated fields populated.
	CreateResource(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)

	// UpdateResource updates an existing Kubernetes resource.
	// The resource must have resourceVersion set for optimistic concurrency.
	UpdateResource(
		ctx context.Context,
		obj *unstructured.Unstructured,
	) (*unstructured.Unstructured, error)
}

// Ensure Client implements K8sClient interface
var _ K8sClient = (*Client)(nil)

// Ensure Client implements TransportClient interface
var _ transportclient.TransportClient = (*Client)(nil)
