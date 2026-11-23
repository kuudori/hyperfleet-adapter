package desireclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DiscoverResources implements transportclient.TransportClient. It lists
// read desires in the target partition and filters their mirrored objects by
// GVK, namespace, and discovery criteria. desire.Identity carries no labels, so
// selector matching must scan client-side.
//
// Selector results come from read mirrors only. An unsynced mirror in scope has
// nothing to match yet, so an empty selector result is reported as
// transportclient.ErrNotSyncedYet while one remains. Apply and delete desires are not
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

	namespace := discovery.GetNamespace()
	inScope := func(id desire.Identity) bool {
		return id.Group == gvk.Group && id.Resource == tc.Resource && (namespace == "" || id.Namespace == namespace) &&
			(!discovery.IsSingleResource() || id.Name == discovery.GetName())
	}
	reads, err := c.listReadDesires(ctx, tc.ManagementCluster, inScope)
	if err != nil {
		return nil, fmt.Errorf(
			"remote transport: failed to list mirrors for partition %q: %w", tc.ManagementCluster, storeErr(err))
	}

	list := &unstructured.UnstructuredList{}
	hasUnsyncedRead := false
	for _, rd := range reads {
		if !inScope(rd.Identity) {
			continue
		}

		obj, err := c.decodeReadDesire(gvk, rd.Identity.Namespace, rd.Identity.Name, rd)
		switch {
		case errors.Is(err, transportclient.ErrNotSyncedYet):
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
		return nil, transportclient.ErrNotSyncedYet
	}
	return list, nil
}

// readDesireMatcher is a store that can skip records while it lists, such as
// desirestore.Store, whose list reads each record with its own request.
type readDesireMatcher interface {
	ListReadDesiresMatching(
		ctx context.Context, managementCluster string, keep func(desire.Identity) bool,
	) ([]desire.ReadDesire, error)
}

func (c *Client) listReadDesires(
	ctx context.Context, managementCluster string, keep func(desire.Identity) bool,
) ([]desire.ReadDesire, error) {
	if matcher, ok := c.store.(readDesireMatcher); ok {
		return matcher.ListReadDesiresMatching(ctx, managementCluster, keep)
	}
	return c.store.ListReadDesires(ctx, managementCluster)
}
