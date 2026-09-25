package desireclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
)

// GetResource implements transportclient.TransportClient. It returns the
// object mirrored by the read desire. Absence is conclusive only when no apply
// or unconfirmed delete is in flight for the target: a NotFound mirror, or no
// read desire at all, is then reported as apierrors.NewNotFound, and otherwise
// as ErrNotSyncedYet. A target with no desire records is therefore not found,
// whether it was never created or its desires were already cleaned up. A
// confirmed delete never hides a mirrored object here; the executor confirms
// deletion through transportclient.DeletionLifecycle.
func (c *Client) GetResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	target transportclient.TransportContext,
) (*unstructured.Unstructured, error) {
	tc, err := resolveTransportContext(target)
	if err != nil {
		return nil, err
	}

	readID, err := buildIdentity(tc, desire.TypeRead, gvk, namespace, name)
	if err != nil {
		return nil, err
	}

	rd, err := c.store.GetReadDesire(ctx, readID)
	switch {
	case errors.Is(err, desire.ErrNotFound):
		// No mirror to decode; in-flight work below decides whether it is absent.
	case err != nil:
		return nil, fmt.Errorf("desireclient: failed to get read desire for %s/%s: %w", namespace, name, err)
	default:
		object, readErr := c.decodeReadDesire(gvk, namespace, name, rd)
		if !apierrors.IsNotFound(readErr) {
			return object, readErr
		}
	}
	// A missing or NotFound mirror cannot confirm absence while a delete or
	// apply is still in flight: a NotFound mirror can predate either write.
	deleteID, err := buildIdentity(tc, desire.TypeDelete, gvk, namespace, name)
	if err != nil {
		return nil, err
	}
	dd, err := c.store.GetDeleteDesire(ctx, deleteID)
	switch {
	case err == nil && !desire.IsDeleted(dd.Status):
		return nil, ErrNotSyncedYet
	case err != nil && !errors.Is(err, desire.ErrNotFound):
		return nil, fmt.Errorf("desireclient: failed to get delete desire for %s/%s: %w",
			namespace, name, err)
	}

	applyID, err := buildIdentity(tc, desire.TypeApply, gvk, namespace, name)
	if err != nil {
		return nil, err
	}
	_, err = c.store.GetApplyDesire(ctx, applyID)
	switch {
	case err == nil:
		return nil, ErrNotSyncedYet
	case !errors.Is(err, desire.ErrNotFound):
		return nil, fmt.Errorf("desireclient: failed to get apply desire for %s/%s: %w",
			namespace, name, err)
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: tc.Resource}, name)
}

// decodeReadDesire translates a ReadDesire's status conditions into the
// three-way outcome the eventual-consistency contract defines
// (docs/adapter-authoring-guide.md, "The eventual-consistency contract for
// remote reads"): no condition yet is not-synced-yet, Reason=NotFound is a
// confirmed absence, and everything else - including a transient failure
// (KubeAPIError/PreCheckFailed) - decodes whatever KubeContent the applier
// last mirrored. readdesire's status.go (applier) retains the prior mirror
// across a transient failure rather than clearing it, so that content is
// still "present" per the contract, just possibly stale; staleness is the
// caller's concern via the generation annotation, not this layer's.
func (c *Client) decodeReadDesire(
	gvk schema.GroupVersionKind, namespace, name string, rd desire.ReadDesire,
) (*unstructured.Unstructured, error) {
	cond := apimeta.FindStatusCondition(rd.Status.Conditions, desire.TypeSuccessful)

	switch {
	case cond == nil:
		// read desire exists but the applier hasn't observed it yet.
		return nil, ErrNotSyncedYet

	case cond.Status == metav1.ConditionFalse && cond.Reason == desire.ReasonNotFound:
		// confirmed absent
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: rd.Identity.Resource}, name)

	case len(rd.Status.KubeContent) == 0:
		// empty content, but absence is not confirmed
		return nil, ErrNotSyncedYet

	default:
		// return the stale mirror even if the last condition was a transient failure (KubeAPIError/PreCheckFailed)
		return decodeKubeContent(rd.Status.KubeContent, namespace, name)
	}
}

// decodeKubeContent decodes the mirrored content from a read desire
func decodeKubeContent(
	kubeContent []byte, namespace, name string,
) (*unstructured.Unstructured, error) {
	var object map[string]any
	if err := json.Unmarshal(kubeContent, &object); err != nil {
		return nil, fmt.Errorf("desireclient: failed to decode mirrored content for %s/%s: %w", namespace, name, err)
	}
	// return the possibly stale mirror, even in the case of a KubeAPIError or PreCheckFailed
	return &unstructured.Unstructured{Object: object}, nil
}
