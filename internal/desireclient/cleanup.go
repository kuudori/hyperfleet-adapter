package desireclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CleanupAfterDeletion implements transportclient.DesireCleaner. It removes
// the delete desire (only when the applier confirms deletion) then the read
// desire. Returns an error if the delete desire exists but is not yet confirmed,
// or if no delete desire exists but an apply desire is still present (the
// applier may not have applied it yet), causing the executor to retry on the
// next reconciliation.
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
	switch {
	case errors.Is(err, desire.ErrNotFound):
		applyID, buildErr := buildIdentity(tc, desire.TypeApply, gvk, namespace, name)
		if buildErr != nil {
			return buildErr
		}
		_, applyErr := c.store.GetApplyDesire(ctx, applyID)
		switch {
		case applyErr == nil:
			return fmt.Errorf(
				"desireclient: cleanup: apply desire still exists for %s/%s,"+
					" resource may not have been created yet: %w",
				namespace, name, ErrDeletionPending)
		case !errors.Is(applyErr, desire.ErrNotFound):
			return fmt.Errorf("desireclient: cleanup: failed to get apply desire for %s/%s: %w",
				namespace, name, applyErr)
		}
	case err != nil:
		return fmt.Errorf("desireclient: cleanup: failed to get delete desire for %s/%s: %w",
			namespace, name, err)
	case !desire.IsDeleted(dd.Status):
		return fmt.Errorf("desireclient: cleanup: deletion not yet confirmed for %s/%s: %w",
			namespace, name, ErrDeletionPending)
	default:
		if delErr := c.store.DeleteDeleteDesire(ctx, deleteID, c.owner, dd.Version); delErr != nil {
			return fmt.Errorf("desireclient: cleanup: failed to delete delete desire for %s/%s: %w",
				namespace, name, delErr)
		}
		slog.DebugContext(ctx, "desireclient: cleanup: removed confirmed delete desire",
			"namespace", namespace, "name", name)
	}

	readID, err := buildIdentity(tc, desire.TypeRead, gvk, namespace, name)
	if err != nil {
		return err
	}

	rd, err := c.store.GetReadDesire(ctx, readID)
	switch {
	case errors.Is(err, desire.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("desireclient: cleanup: failed to get read desire for %s/%s: %w",
			namespace, name, err)
	default:
		if delErr := c.store.DeleteReadDesire(ctx, readID, c.owner, rd.Version); delErr != nil {
			return fmt.Errorf("desireclient: cleanup: failed to delete read desire for %s/%s: %w",
				namespace, name, delErr)
		}
		slog.DebugContext(ctx, "desireclient: cleanup: removed read desire",
			"namespace", namespace, "name", name)
	}

	return nil
}
