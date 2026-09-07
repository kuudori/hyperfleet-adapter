package desireclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GetResource implements transportclient.TransportClient. It reads the
// mirrored live object from the read desire's status, distinguishing three
// outcomes per the eventual-consistency contract
// (docs/adapter-authoring-guide.md): not-synced-yet (ErrNotSyncedYet),
// confirmed-absent (apierrors.NewNotFound), and present (the mirrored
// object).
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

	id, err := buildIdentity(tc, desire.TypeRead, gvk, namespace, name)
	if err != nil {
		return nil, err
	}

	rd, err := c.store.GetReadDesire(ctx, id)
	if errors.Is(err, desire.ErrNotFound) {
		return nil, ErrNotSyncedYet
	}
	if err != nil {
		return nil, fmt.Errorf("desireclient: failed to get read desire for %s/%s: %w", namespace, name, err)
	}

	return c.decodeReadDesire(gvk, namespace, name, rd)
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
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(kubeContent, obj); err != nil {
		return nil, fmt.Errorf("desireclient: failed to decode mirrored content for %s/%s: %w", namespace, name, err)
	}
	// return the possibly stale mirror, even in the case of a KubeAPIError or PreCheckFailed
	return obj, nil
}
