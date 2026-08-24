package desireclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DeleteResource implements transportclient.TransportClient. It ensures a
// paired read desire exists, then posts a delete desire — CreateDeleteDesire
// atomically removes any sibling apply desire for the same target (see
// desire.SpecStore), so nothing re-applies. The read desire is created first
// so a failure there leaves the apply desire (if any) untouched — a safe,
// retryable state — rather than risking a delete desire with no read desire
// to observe its disappearance through, which a CreateDeleteDesire-then-read
// ordering could leave behind if the read-desire write failed afterward.
//
// opts is unused: propagation policy has no equivalent in the desire model
// (the applier owns deletion semantics against the target cluster), the same
// way maestroclient ignores it for ManifestWork deletes.
func (c *Client) DeleteResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	opts *transportclient.DeleteOptions,
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
	readID, err := buildIdentity(tc, desire.TypeRead, gvk, namespace, name)
	if err != nil {
		return err
	}

	if err = c.ensureReadDesire(ctx, readID, gvk.Version); err != nil {
		return fmt.Errorf(
			"desireclient: failed to create paired read desire for %s/%s: %w", gvk.Kind, name, err)
	}

	if _, err = c.store.CreateDeleteDesire(ctx, desire.DeleteDesire{
		Identity: deleteID,
		Owner:    c.owner,
	}); err != nil && !errors.Is(err, desire.ErrAlreadyExists) {
		return fmt.Errorf("desireclient: failed to create delete desire for %s/%s: %w", namespace, name, err)
	}

	return nil
}
