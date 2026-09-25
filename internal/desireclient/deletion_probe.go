package desireclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ProbeDeletion implements transportclient.DeletionLifecycle. It reports the
// target's delete desire status without changing any desire record. The read
// mirror is deliberately not consulted: it can lag a confirmed delete, and
// whether an undeleted target still needs a delete is a discovery question.
func (c *Client) ProbeDeletion(ctx context.Context, gvk schema.GroupVersionKind,
	namespace, name string, target transportclient.TransportContext,
) (transportclient.DeletionState, error) {
	tc, err := resolveTransportContext(target)
	if err != nil {
		return transportclient.DeletionNone, err
	}
	deleteID, err := buildIdentity(tc, desire.TypeDelete, gvk, namespace, name)
	if err != nil {
		return transportclient.DeletionNone, err
	}
	dd, err := c.store.GetDeleteDesire(ctx, deleteID)
	switch {
	case errors.Is(err, desire.ErrNotFound):
		return transportclient.DeletionNone, nil
	case err != nil:
		return transportclient.DeletionNone, fmt.Errorf(
			"desireclient: probe delete desire for %s/%s: %w", namespace, name, err)
	case desire.IsDeleted(dd.Status):
		return transportclient.DeletionConfirmed, nil
	default:
		return transportclient.DeletionPending, nil
	}
}
