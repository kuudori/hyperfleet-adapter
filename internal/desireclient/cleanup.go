package desireclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CleanupAfterDeletion implements transportclient.DesireCleaner. A confirmed
// delete desire is sufficient evidence to remove both desires; the read mirror
// may lag behind that confirmation. Without a delete desire, cleanup removes a
// read desire only when it independently confirms absence.
func (c *Client) CleanupAfterDeletion(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	target transportclient.TransportContext,
) error {
	tc, err := resolveTransportContext(target)
	if err != nil {
		return err
	}

	deleteID, err := buildIdentity(tc, desire.TypeDelete, gvk, namespace, name)
	if err != nil {
		return err
	}

	dd, err := c.store.GetDeleteDesire(ctx, deleteID)
	if errors.Is(err, desire.ErrNotFound) {
		return c.cleanupReadWithoutDelete(ctx, tc, gvk, namespace, name)
	}
	if err != nil {
		return fmt.Errorf("remote transport: cleanup: failed to get the delete request for %s/%s: %w",
			namespace, name, storeErr(err))
	}
	if !desire.IsDeleted(dd.Status) {
		return fmt.Errorf("remote transport: cleanup: deletion not yet confirmed for %s/%s: %w",
			namespace, name, ErrDeletionPending)
	}

	readID, err := buildIdentity(tc, desire.TypeRead, gvk, namespace, name)
	if err != nil {
		return err
	}
	rd, err := c.store.GetReadDesire(ctx, readID)
	if err != nil && !errors.Is(err, desire.ErrNotFound) {
		return fmt.Errorf("remote transport: cleanup: failed to get the mirror for %s/%s: %w",
			namespace, name, storeErr(err))
	}
	if err == nil {
		// Remove the read mirror first. If deleting the confirmed delete desire
		// then fails, its status remains available for the next reconciliation.
		if delErr := c.store.DeleteReadDesire(ctx, readID, c.owner, rd.Version); delErr != nil {
			return fmt.Errorf("remote transport: cleanup: failed to delete the mirror for %s/%s: %w",
				namespace, name, storeErr(delErr))
		}
		slog.DebugContext(ctx, "desireclient: cleanup: removed read desire",
			"namespace", namespace, "name", name)
	}

	if delErr := c.store.DeleteDeleteDesire(ctx, deleteID, c.owner, dd.Version); delErr != nil {
		return fmt.Errorf("remote transport: cleanup: failed to remove the delete request for %s/%s: %w",
			namespace, name, storeErr(delErr))
	}
	slog.DebugContext(ctx, "desireclient: cleanup: removed confirmed delete desire",
		"namespace", namespace, "name", name)
	return nil
}

// cleanupReadWithoutDelete handles a target with no delete desire. Nothing
// asked for this deletion, so it only removes a read desire that mirrors
// NotFound; an active apply or a mirror that still shows (or has not yet
// observed) the object leaves everything in place and reports
// ErrDeletionPending.
func (c *Client) cleanupReadWithoutDelete(
	ctx context.Context,
	tc *TransportContext,
	gvk schema.GroupVersionKind,
	namespace, name string,
) error {
	active, err := c.hasActiveApplyDesire(ctx, tc, gvk, namespace, name)
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf(
			"remote transport: cleanup: apply request still exists for %s/%s, resource may not have been created yet: %w",
			namespace, name, ErrDeletionPending)
	}

	readID, err := buildIdentity(tc, desire.TypeRead, gvk, namespace, name)
	if err != nil {
		return err
	}
	rd, err := c.store.GetReadDesire(ctx, readID)
	if errors.Is(err, desire.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remote transport: cleanup: failed to get the mirror for %s/%s: %w",
			namespace, name, storeErr(err))
	}

	_, mirrorErr := c.decodeReadDesire(gvk, namespace, name, rd)
	switch {
	case apierrors.IsNotFound(mirrorErr):
	case errors.Is(mirrorErr, ErrNotSyncedYet):
		return fmt.Errorf("remote transport: cleanup: absence is not yet confirmed for %s/%s: %w",
			namespace, name, ErrDeletionPending)
	case mirrorErr != nil:
		return fmt.Errorf("remote transport: cleanup: failed to inspect the mirror for %s/%s: %w",
			namespace, name, mirrorErr)
	default:
		return fmt.Errorf("remote transport: cleanup: resource is still present for %s/%s: %w",
			namespace, name, ErrDeletionPending)
	}

	if delErr := c.store.DeleteReadDesire(ctx, readID, c.owner, rd.Version); delErr != nil {
		return fmt.Errorf("remote transport: cleanup: failed to delete the mirror for %s/%s: %w",
			namespace, name, storeErr(delErr))
	}
	slog.DebugContext(ctx, "desireclient: cleanup: removed read desire",
		"namespace", namespace, "name", name)
	return nil
}
