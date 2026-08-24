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
// three-way outcome the eventual-consistency contract defines. Successful=True
// covers both a synced mirror and a confirmed-absent resource (the applier's
// notFound() writes Successful=True with empty KubeContent) — decodeKubeContent
// tells those apart. Successful=False means the read itself didn't succeed
// (KubeAPIError/PreCheckFailed); content is never relayed in that case, even
// if a stale mirror is sitting there from a prior sync, since the caller has
// no way to know it's stale.
func (c *Client) decodeReadDesire(
	gvk schema.GroupVersionKind, namespace, name string, rd desire.ReadDesire,
) (*unstructured.Unstructured, error) {
	cond := apimeta.FindStatusCondition(rd.Status.Conditions, desire.TypeSuccessful)

	switch {
	case cond == nil:
		// Read desire exists but the applier hasn't observed it yet.
		return nil, ErrNotSyncedYet

	case cond.Status == metav1.ConditionTrue:
		return decodeKubeContent(rd.Status.KubeContent, gvk, rd.Identity.Resource, namespace, name)

	case cond.Reason == desire.ReasonNotFound:
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: rd.Identity.Resource}, name)
	default:
		return nil, ErrNotSyncedYet
	}
}

// decodeKubeContent decodes a synced read desire's mirrored content, or
// reports confirmed-absence when content is empty. Only ever called under
// Successful=True, where the applier's status-writers guarantee emptiness
// means Reason=NotFound and non-emptiness means Reason=Synced — so content
// length alone is an unambiguous stand-in for Reason here.
func decodeKubeContent(
	kubeContent []byte, gvk schema.GroupVersionKind, resource, namespace, name string,
) (*unstructured.Unstructured, error) {
	if len(kubeContent) == 0 {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: resource}, name)
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(kubeContent, obj); err != nil {
		return nil, fmt.Errorf("desireclient: failed to decode mirrored content for %s/%s: %w", namespace, name, err)
	}
	return obj, nil
}
