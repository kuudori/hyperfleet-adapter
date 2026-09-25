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
// read desires in the target partition and filters their mirrored objects by
// GVK, namespace, and discovery criteria. desire.Identity carries no labels, so
// selector matching must scan client-side, the same shape as
// maestroclient.DiscoverResources scanning ManifestWorks.
//
// Selector results come from read mirrors only. An unsynced mirror in scope has
// nothing to match yet, so an empty selector result is reported as
// ErrNotSyncedYet while one remains. Apply and delete desires are not
// consulted: the partition is shared, so scanning them would couple this
// selector to every other target of the same kind.
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
	hasUnsyncedRead := false
	for _, rd := range reads {
		if rd.Identity.Group != gvk.Group || rd.Identity.Resource != tc.Resource {
			continue
		}
		if namespace := discovery.GetNamespace(); namespace != "" && rd.Identity.Namespace != namespace {
			continue
		}

		obj, err := c.decodeReadDesire(gvk, rd.Identity.Namespace, rd.Identity.Name, rd)
		switch {
		case errors.Is(err, ErrNotSyncedYet):
			// Not yet synced — nothing to match discovery criteria against,
			// so an empty result cannot confirm absence while one remains in scope.
			hasUnsyncedRead = true
			continue
		case apierrors.IsNotFound(err):
			// Confirmed absent — same as a live List not showing a deleted resource.
			continue
		case err != nil:
			// Undecodable KubeContent (whether from a synced True condition or
			// a retained mirror on a KubeAPIError/PreCheckFailed False
			// condition) is a store invariant violation for that one record,
			// not legitimate transience — but it must not fail discovery for
			// every other resource in the partition.
			slog.ErrorContext(ctx, "desireclient: discovery skipping undecodable read desire",
				"namespace", rd.Identity.Namespace, "name", rd.Identity.Name, "error", err)
			continue
		}

		if manifest.MatchesDiscoveryCriteria(obj, discovery) {
			list.Items = append(list.Items, *obj)
		}
	}

	if len(list.Items) == 0 && hasUnsyncedRead && discovery.GetLabelSelector() != "" {
		return nil, ErrNotSyncedYet
	}
	return list, nil
}
