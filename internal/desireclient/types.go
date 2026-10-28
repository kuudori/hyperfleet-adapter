package desireclient

import (
	"context"
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
	// declared on the named remote transport for the manifest's GVK. The desire store's
	// Identity is keyed by this plural form directly (see
	// pkg/desire.Identity.Resource in hyperfleet-applier). The adapter selects
	// it from the route's GVK map instead of guessing from Kind or using a RESTMapper.
	// Required for all operations.
	Resource string
}

// resolveTransportContext type-asserts the generic TransportContext and
// validates both fields are set.
func resolveTransportContext(target transportclient.TransportContext) (*TransportContext, error) {
	tc, ok := target.(*TransportContext)
	if !ok || tc == nil {
		return nil, fmt.Errorf("remote transport: TransportContext with ManagementCluster and Resource is required")
	}
	if tc.ManagementCluster == "" {
		return nil, fmt.Errorf("remote transport: TransportContext.ManagementCluster is required")
	}
	if tc.Resource == "" {
		return nil, fmt.Errorf("remote transport: TransportContext.Resource is required")
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
		return desire.Identity{}, fmt.Errorf("remote transport: invalid identity: %w", err)
	}
	return id, nil
}

// hasActiveApplyDesire reports whether an apply desire currently exists for
// the target, treating desire.ErrNotFound as "no active apply".
func (c *Client) hasActiveApplyDesire(
	ctx context.Context, tc *TransportContext, gvk schema.GroupVersionKind, namespace, name string,
) (bool, error) {
	applyID, err := buildIdentity(tc, desire.TypeApply, gvk, namespace, name)
	if err != nil {
		return false, err
	}
	_, err = c.store.GetApplyDesire(ctx, applyID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, desire.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf(
			"remote transport: failed to get the apply request for %s/%s: %w", namespace, name, storeErr(err))
	}
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
// mirroring the pattern in internal/k8sclient/apply.go.
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
