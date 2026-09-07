package desireclient

import (
	"context"
	"errors"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// putReadDesire creates a read desire with the given status.
func putReadDesire(
	t *testing.T, ctx context.Context, store *memory.Store, namespace, name string, status desire.ReadStatus,
) {
	t.Helper()
	id := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: namespace, Name: name,
	}
	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{Identity: id, Owner: testOwner, TargetVersion: "v1"})
	require.NoError(t, err)
	_, err = store.UpdateReadDesireStatus(ctx, id, status)
	require.NoError(t, err)
}

// putConfirmedAbsentReadDesire creates a read desire marked as not found.
func putConfirmedAbsentReadDesire(t *testing.T, ctx context.Context, store *memory.Store, namespace, name string) {
	t.Helper()
	putReadDesire(t, ctx, store, namespace, name, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionFalse, Reason: desire.ReasonNotFound,
		}}},
	})
}

// putSyncedReadDesire creates a read desire with synced content.
func putSyncedReadDesire(
	t *testing.T, ctx context.Context, store *memory.Store, namespace, name string, content []byte,
) {
	t.Helper()
	putReadDesire(t, ctx, store, namespace, name, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionTrue, Reason: desire.ReasonSynced,
		}}},
		KubeContent: content,
	})
}

// putNotFoundReadDesire creates a read desire marked as not found.
func putNotFoundReadDesire(t *testing.T, ctx context.Context, store *memory.Store, namespace, name string) {
	t.Helper()
	putReadDesire(t, ctx, store, namespace, name, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionFalse, Reason: desire.ReasonNotFound,
		}}},
	})
}

// putInvalidReadDesire creates a read desire with invalid content (successful=true but notfound reason).
func putInvalidReadDesire(t *testing.T, ctx context.Context, store *memory.Store, namespace, name string) {
	t.Helper()
	putReadDesire(t, ctx, store, namespace, name, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionTrue, Reason: desire.ReasonNotFound,
		}}},
	})
}

// putKubeAPIErrorReadDesire creates a read desire with a transient kube API error.
func putKubeAPIErrorReadDesire(
	t *testing.T, ctx context.Context, store *memory.Store, namespace, name string, content []byte,
) {
	t.Helper()
	putReadDesire(t, ctx, store, namespace, name, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionFalse, Reason: desire.ReasonKubeAPIError,
		}}},
		KubeContent: content,
	})
}

// successfulCondition builds the single summary condition every desire carries.
func successfulCondition(status metav1.ConditionStatus, reason string) *metav1.Condition {
	return &metav1.Condition{Type: desire.TypeSuccessful, Status: status, Reason: reason}
}

// failingListReadDesiresStore wraps a real SpecStore but forces
// ListReadDesires to fail, simulating a store-level outage during discovery.
type failingListReadDesiresStore struct {
	desire.SpecStore
}

func (f *failingListReadDesiresStore) ListReadDesires(
	_ context.Context, _ string,
) ([]desire.ReadDesire, error) {
	return nil, errors.New("boom: read desire store unavailable")
}

// spyDeleteReadDesireStore counts DeleteReadDesire calls so tests can assert
// whether ensureReadDesire actually attempted a recreate.
type spyDeleteReadDesireStore struct {
	desire.SpecStore
	deleteReadDesireCalls int
}

func (s *spyDeleteReadDesireStore) DeleteReadDesire(
	ctx context.Context, id desire.Identity, owner string, version int64,
) error {
	s.deleteReadDesireCalls++
	return s.SpecStore.DeleteReadDesire(ctx, id, owner, version)
}

// staleApplyVersionStore wraps a real SpecStore but returns a stale version
// on GetApplyDesire to simulate the case where an external client has
// concurrently updated the apply desire while this one is computing.
type staleApplyVersionStore struct {
	desire.SpecStore
}

func (s *staleApplyVersionStore) GetApplyDesire(ctx context.Context, id desire.Identity) (desire.ApplyDesire, error) {
	ad, err := s.SpecStore.GetApplyDesire(ctx, id)
	if err == nil {
		ad.Version++
	}
	return ad, err
}
