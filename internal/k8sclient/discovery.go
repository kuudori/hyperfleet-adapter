package k8sclient

import (
	"context"
	"log/slog"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DiscoveryConfig is an alias to manifest.DiscoveryConfig for convenience.
// This allows k8sclient users to use k8sclient.DiscoveryConfig without importing manifest package.
type DiscoveryConfig = manifest.DiscoveryConfig

// BuildLabelSelector is an alias to manifest.BuildLabelSelector for convenience.
// Converts a map of labels to a selector string.
// Example: {"env": "prod", "app": "myapp"} -> "app=myapp,env=prod"
var BuildLabelSelector = manifest.BuildLabelSelector

// DiscoverResources discovers Kubernetes resources based on the Discovery configuration.
//
// If Discovery.IsSingleResource() is true, it fetches a single resource by name.
// Otherwise, it lists resources matching the label selector.
//
// Example:
//
//	discovery := &k8sclient.DiscoveryConfig{
//	    Namespace:     "default",
//	    LabelSelector: "app=myapp",
//	}
//	list, err := client.DiscoverResources(ctx, gvk, discovery, nil)
func (c *Client) DiscoverResources(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	discovery manifest.Discovery,
	_ transportclient.TransportContext,
) (*unstructured.UnstructuredList, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)
	if discovery == nil {
		return list, nil
	}

	if discovery.IsSingleResource() {
		// Single resource by name
		slog.InfoContext(ctx, "discovering single resource",
			"kind", gvk.Kind, "name", discovery.GetName(), "namespace", discovery.GetNamespace())

		obj, err := c.GetResource(ctx, gvk, discovery.GetNamespace(), discovery.GetName(), nil)
		if err != nil {
			return list, err
		}

		// Wrap single resource in a list for consistent return type
		list.Items = []unstructured.Unstructured{*obj}
		return list, nil
	}

	// List resources by selector
	return c.ListResources(ctx, gvk, discovery.GetNamespace(), discovery.GetLabelSelector())
}
