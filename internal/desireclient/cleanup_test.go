package desireclient

import (
	"context"
	"errors"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCleanupAfterDeletion_ConfirmedDelete_RemovesBothDespiteStaleRead(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)

	deleteID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeDelete,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}

	desiretest.PutConfirmedDeleteDesire(t, ctx, store, testID.Delete(), testOwner)
	desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)

	_, err = store.GetDeleteDesire(ctx, deleteID)
	assert.True(t, errors.Is(err, desire.ErrNotFound), "delete desire must be removed")

	_, err = store.GetReadDesire(ctx, readID)
	assert.ErrorIs(t, err, desire.ErrNotFound, "confirmed cleanup must remove the stale read mirror")
	require.NoError(t, c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext()))
}

func TestCleanupAfterDeletion_PendingDelete_SkipsCleanup(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	deleteID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeDelete,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}

	desiretest.PutDeleteDesire(t, ctx, store, testID.Delete(), testOwner,
		metav1.ConditionFalse, desire.ReasonWaitingForDeletion)

	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: readID, Owner: testOwner, TargetVersion: "v1",
	})
	require.NoError(t, err)

	err = c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err, "pending delete desire must return an error")
	assert.Contains(t, err.Error(), "deletion not yet confirmed")
	assert.True(t, errors.Is(err, ErrDeletionPending), "must wrap ErrDeletionPending")

	_, err = store.GetDeleteDesire(ctx, deleteID)
	assert.NoError(t, err, "delete desire must still exist")

	_, err = store.GetReadDesire(ctx, readID)
	assert.NoError(t, err, "read desire must still exist")
}

func TestCleanupAfterDeletion_NoDeleteDesire_RemovesConfirmedAbsentRead(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)

	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testID.Read(), testOwner)

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)

	_, err = store.GetReadDesire(ctx, readID)
	assert.True(t, errors.Is(err, desire.ErrNotFound), "read desire must be removed")
}

func TestCleanupAfterDeletion_NoDeleteDesire_UnconfirmedReadRemainsPending(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)

	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: testID.Read(), Owner: testOwner, TargetVersion: "v1",
	})
	require.NoError(t, err)

	err = c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.ErrorIs(t, err, ErrDeletionPending)
	_, err = store.GetReadDesire(ctx, testID.Read())
	require.NoError(t, err, "an unsynced read desire cannot be removed as confirmed absence")
}

func TestCleanupAfterDeletion_NoDesires_NoError(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)
}

func TestCleanupAfterDeletion_ApplyDesireExists_NoDeleteDesire_ReturnsError(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	applyID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeApply,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{
		Identity: applyID, Owner: testOwner,
		Spec: desire.ApplySpec{KubeContent: configMapManifest(1)},
	})
	require.NoError(t, err)

	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	_, err = store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: readID, Owner: testOwner, TargetVersion: "v1",
	})
	require.NoError(t, err)

	err = c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apply desire still exists")
	assert.True(t, errors.Is(err, ErrDeletionPending), "must wrap ErrDeletionPending")

	_, err = store.GetApplyDesire(ctx, applyID)
	assert.NoError(t, err, "apply desire must still exist")

	_, err = store.GetReadDesire(ctx, readID)
	assert.NoError(t, err, "read desire must still exist")
}

func TestCleanupAfterDeletion_DeleteDesireOnly_NoReadDesire(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	deleteID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeDelete,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}

	desiretest.PutConfirmedDeleteDesire(t, ctx, store, testID.Delete(), testOwner)

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)

	_, err = store.GetDeleteDesire(ctx, deleteID)
	require.ErrorIs(t, err, desire.ErrNotFound, "confirmed delete desire must be removed without a read mirror")
	_, err = store.GetReadDesire(ctx, testID.Read())
	assert.ErrorIs(t, err, desire.ErrNotFound, "cleanup must not recreate a missing read desire")
}

func TestCleanupAfterDeletion_RequiresTransportContext(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(newMemoryStore())

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, nil)
	require.Error(t, err)
}

func TestCleanupAfterDeletion_GetDeleteDesireError(t *testing.T) {
	ctx := context.Background()
	store := &failingGetDeleteDesireStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get delete desire")
}

func TestCleanupAfterDeletion_DeleteDeleteDesireError(t *testing.T) {
	ctx := context.Background()
	inner := newMemoryStore()

	desiretest.PutConfirmedDeleteDesire(t, ctx, inner, testID.Delete(), testOwner)
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, inner, testID.Read(), testOwner)

	store := &failingDeleteDeleteDesireStore{SpecStore: inner}
	c := newTestClient(store)

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to delete delete desire")
}

func TestCleanupAfterDeletion_DeleteReadDesireError(t *testing.T) {
	ctx := context.Background()
	inner := newMemoryStore()

	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	_, err := inner.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: readID, Owner: testOwner, TargetVersion: "v1",
	})
	require.NoError(t, err)
	desiretest.MarkReadDesireNotFound(t, ctx, inner, testID.Read())

	store := &failingDeleteReadDesireStore{SpecStore: inner}
	c := newTestClient(store)

	err = c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to delete read desire")
}

func TestCleanupAfterDeletion_GetApplyDesireError_NoDeleteDesire_ReturnsStoreError(t *testing.T) {
	ctx := context.Background()
	store := &failingGetApplyDesireStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get apply desire")
	assert.False(t, errors.Is(err, ErrDeletionPending), "genuine store error must not be wrapped as ErrDeletionPending")
}

// --- Test store wrappers ---

type failingGetDeleteDesireStore struct {
	desire.SpecStore
}

func (f *failingGetDeleteDesireStore) GetDeleteDesire(
	_ context.Context, _ desire.Identity,
) (desire.DeleteDesire, error) {
	return desire.DeleteDesire{}, errors.New("boom: store unavailable")
}

type failingGetApplyDesireStore struct {
	desire.SpecStore
}

func (f *failingGetApplyDesireStore) GetApplyDesire(
	_ context.Context, _ desire.Identity,
) (desire.ApplyDesire, error) {
	return desire.ApplyDesire{}, errors.New("boom: store unavailable")
}

type failingDeleteDeleteDesireStore struct {
	desire.SpecStore
}

func (f *failingDeleteDeleteDesireStore) DeleteDeleteDesire(
	_ context.Context, _ desire.Identity, _ string, _ int64,
) error {
	return errors.New("boom: version conflict")
}

type failingDeleteReadDesireStore struct {
	desire.SpecStore
}

func (f *failingDeleteReadDesireStore) DeleteReadDesire(
	_ context.Context, _ desire.Identity, _ string, _ int64,
) error {
	return errors.New("boom: version conflict")
}
