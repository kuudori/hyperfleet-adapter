package desireclient

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// TransportContext carries per-request routing information for the desire
// transport backend. Pass this as the TransportContext (any) in ApplyResource,
// GetResource, DiscoverResources, and DeleteResource.
type TransportContext struct {
	// ManagementCluster is the target managed cluster identifier — the desire
	// store's partition key. Required for all operations.
	ManagementCluster string
	// Resource is the plural Kubernetes resource type (e.g. "networkpolicies"),
	// declared in the task config alongside the manifest. The desire store's
	// Identity is keyed by this plural form directly (see
	// pkg/desire.Identity.Resource in hyperfleet-applier); the adapter never
	// derives it from the manifest's Kind — no RESTMapper is used or needed.
	// Required for all operations.
	Resource string
}

// ErrNotSyncedYet indicates the read desire's mirror has not yet been
// populated by the applier — a transient, non-terminal outcome distinct from
// a confirmed-absent resource (reported via apierrors.NewNotFound). Per the
// eventual-consistency contract (docs/adapter-authoring-guide.md), callers
// should treat this as "not converged yet" rather than a failure. Use
// errors.Is to check for it.
var ErrNotSyncedYet = errors.New("desireclient: resource not synced yet")

// resolveTransportContext type-asserts the generic TransportContext and
// validates both fields are set.
func resolveTransportContext(target transportclient.TransportContext) (*TransportContext, error) {
	tc, ok := target.(*TransportContext)
	if !ok || tc == nil {
		return nil, fmt.Errorf("desireclient: TransportContext with ManagementCluster and Resource is required")
	}
	if tc.ManagementCluster == "" {
		return nil, fmt.Errorf("desireclient: TransportContext.ManagementCluster is required")
	}
	if tc.Resource == "" {
		return nil, fmt.Errorf("desireclient: TransportContext.Resource is required")
	}
	return tc, nil
}

// buildIdentity is the single shared helper for assembling a desire.Identity
// from a resolved TransportContext, GVK, namespace, and name. All four
// TransportClient methods route through this rather than duplicating identity
// construction.
func buildIdentity(
	tc *TransportContext, dtype desire.DesireType, gvk schema.GroupVersionKind, namespace, name string,
) (desire.Identity, error) {
	id := desire.Identity{
		ManagementCluster: tc.ManagementCluster,
		Type:              dtype,
		Group:             gvk.Group,
		Resource:          tc.Resource,
		Namespace:         namespace,
		Name:              name,
	}
	if err := id.Validate(); err != nil {
		return desire.Identity{}, fmt.Errorf("desireclient: invalid identity: %w", err)
	}
	return id, nil
}

// generationFromKubeContent extracts the hyperfleet.io/generation annotation
// from a previously-stored ApplyDesire's KubeContent. Returns 0 if the content
// can't be parsed or carries no annotation.
func generationFromKubeContent(kubeContent []byte) int64 {
	obj, err := parseToUnstructured(kubeContent)
	if err != nil {
		return 0
	}
	return manifest.GetGenerationFromUnstructured(obj)
}

// parseToUnstructured parses JSON or YAML bytes into an unstructured resource,
// mirroring the pattern in internal/k8sclient/apply.go and
// internal/maestroclient/client.go.
func parseToUnstructured(data []byte) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(data, &obj.Object); err == nil && obj.Object != nil {
		return obj, nil
	}
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("failed to convert YAML to JSON: %w", err)
	}
	if err := json.Unmarshal(jsonData, &obj.Object); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}
	return obj, nil
}
