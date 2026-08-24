package desireclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DiscoverResources implements transportclient.TransportClient. It lists
// every read desire in the target partition and filters by GVK and the
// discovery criteria. desire.Identity carries no labels, so label-selector
// discovery must scan client-side, the same shape as
// maestroclient.DiscoverResources scanning ManifestWorks.
func (c *Client) DiscoverResources(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	discovery manifest.Discovery,
	target transportclient.TransportContext,
) (*unstructured.UnstructuredList, error) {
	tc, err := resolveTransportContext(target)
	if err != nil {
		return nil, err
	}

	reads, err := c.store.ListReadDesires(ctx, tc.ManagementCluster)
	if err != nil {
		return nil, fmt.Errorf("desireclient: failed to list read desires for partition %q: %w", tc.ManagementCluster, err)
	}

	list := &unstructured.UnstructuredList{}
	for _, rd := range reads {
		if rd.Identity.Group != gvk.Group || rd.Identity.Resource != tc.Resource {
			continue
		}

		// Route through the same three-way interpretation GetResource uses, so a
		// single item's outcome here can never drift from what a Get on that same
		// identity would report.
		obj, err := c.decodeReadDesire(gvk, rd.Identity.Namespace, rd.Identity.Name, rd)
		switch {
		case errors.Is(err, ErrNotSyncedYet):
			// Not yet synced — nothing to match discovery criteria against,
			// same as a live List not yet showing a slow-to-create resource.
			continue
		case apierrors.IsNotFound(err):
			// Confirmed absent — same as a live List not showing a deleted resource.
			continue
		case err != nil:
			// A True condition with undecodable content is a store invariant
			// violation for that one record, not legitimate transience — but it
			// must not fail discovery for every other resource in the partition.
			slog.ErrorContext(ctx, "desireclient: discovery skipping invalid read desire",
				"namespace", rd.Identity.Namespace,
				"name", rd.Identity.Name,
				"error", err)
			continue
		}

		if manifest.MatchesDiscoveryCriteria(obj, discovery) {
			list.Items = append(list.Items, *obj)
		}
	}

	return list, nil
}
