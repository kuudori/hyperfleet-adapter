package desiretest

import (
	"context"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PutUnsyncedReadDesire creates a read desire with no status condition,
// simulating a desire the applier has not observed yet.
func PutUnsyncedReadDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string,
) {
	t.Helper()
	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: id, Owner: owner, TargetVersion: "v1",
	})
	require.NoError(t, err)
}

// PutReadDesire creates a read desire with the given identity, owner, and status.
func PutReadDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string, status desire.ReadStatus,
) {
	t.Helper()
	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: id, Owner: owner, TargetVersion: "v1",
	})
	require.NoError(t, err)
	_, err = store.UpdateReadDesireStatus(ctx, id, status)
	require.NoError(t, err)
}

// PutNotFoundReadDesire creates a read desire marked as not found.
func PutNotFoundReadDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string,
) {
	t.Helper()
	PutReadDesire(t, ctx, store, id, owner, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionFalse, Reason: desire.ReasonNotFound,
		}}},
	})
}

// PutConfirmedAbsentReadDesire creates a read desire marked as not found.
func PutConfirmedAbsentReadDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string,
) {
	t.Helper()
	PutNotFoundReadDesire(t, ctx, store, id, owner)
}

// PutSyncedReadDesire creates a read desire with synced content.
func PutSyncedReadDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string, content []byte,
) {
	t.Helper()
	PutReadDesire(t, ctx, store, id, owner, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionTrue, Reason: desire.ReasonSynced,
		}}},
		KubeContent: content,
	})
}

// PutInvalidReadDesire creates a read desire with Successful=True but Reason=NotFound.
func PutInvalidReadDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string,
) {
	t.Helper()
	PutReadDesire(t, ctx, store, id, owner, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionTrue, Reason: desire.ReasonNotFound,
		}}},
	})
}

// PutKubeAPIErrorReadDesire creates a read desire with a transient kube API error.
func PutKubeAPIErrorReadDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string, content []byte,
) {
	t.Helper()
	PutReadDesire(t, ctx, store, id, owner, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionFalse, Reason: desire.ReasonKubeAPIError,
		}}},
		KubeContent: content,
	})
}

// PutDeleteDesire creates a delete desire with the given condition status and reason.
func PutDeleteDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string,
	condStatus metav1.ConditionStatus, reason string,
) {
	t.Helper()
	dd, err := store.CreateDeleteDesire(ctx, desire.DeleteDesire{Identity: id, Owner: owner})
	require.NoError(t, err)
	_, err = store.UpdateDeleteDesireStatus(ctx, id, desire.Status{
		Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: condStatus, Reason: reason,
		}},
	}, dd.Version)
	require.NoError(t, err)
}

// PutConfirmedDeleteDesire creates a delete desire marked as successfully deleted.
func PutConfirmedDeleteDesire(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, owner string,
) {
	t.Helper()
	PutDeleteDesire(t, ctx, store, id, owner, metav1.ConditionTrue, desire.ReasonDeleted)
}

// MarkReadDesireNotFound updates an existing read desire to NotFound status.
func MarkReadDesireNotFound(
	t testing.TB, ctx context.Context, store *memory.Store, id desire.Identity,
) {
	t.Helper()
	_, err := store.UpdateReadDesireStatus(ctx, id, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionFalse, Reason: desire.ReasonNotFound,
		}}},
	})
	require.NoError(t, err)
}

// MarkReadDesireSynced updates an existing read desire to Synced status with content.
func MarkReadDesireSynced(
	t testing.TB, ctx context.Context, store *memory.Store,
	id desire.Identity, content []byte,
) {
	t.Helper()
	_, err := store.UpdateReadDesireStatus(ctx, id, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionTrue, Reason: desire.ReasonSynced,
		}}},
		KubeContent: content,
	})
	require.NoError(t, err)
}

// MarkDeleteDesireConfirmed updates an existing delete desire to confirmed-deleted status.
func MarkDeleteDesireConfirmed(
	t testing.TB, ctx context.Context, store *memory.Store, id desire.Identity,
) {
	t.Helper()
	dd, err := store.GetDeleteDesire(ctx, id)
	require.NoError(t, err)
	_, err = store.UpdateDeleteDesireStatus(ctx, id, desire.Status{
		Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionTrue, Reason: desire.ReasonDeleted,
		}},
	}, dd.Version)
	require.NoError(t, err)
}

// SuccessfulCondition builds the single summary condition every desire carries.
func SuccessfulCondition(status metav1.ConditionStatus, reason string) *metav1.Condition {
	return &metav1.Condition{Type: desire.TypeSuccessful, Status: status, Reason: reason}
}

// TestIdentity is a builder for desire.Identity values in tests.
// The With* methods return copies, so the original is never mutated.
type TestIdentity struct {
	ManagementCluster string
	Resource          string
	Namespace         string
	Name              string
}

func (ti TestIdentity) build(t desire.DesireType) desire.Identity {
	return desire.Identity{
		ManagementCluster: ti.ManagementCluster, Type: t,
		Resource: ti.Resource, Namespace: ti.Namespace, Name: ti.Name,
	}
}

func (ti TestIdentity) Read() desire.Identity   { return ti.build(desire.TypeRead) }
func (ti TestIdentity) Delete() desire.Identity { return ti.build(desire.TypeDelete) }
func (ti TestIdentity) Apply() desire.Identity  { return ti.build(desire.TypeApply) }

func (ti TestIdentity) WithName(name string) TestIdentity { ti.Name = name; return ti }
func (ti TestIdentity) WithNamespace(namespace string) TestIdentity {
	ti.Namespace = namespace
	return ti
}

// InstantApplierStore wraps memory.Store. When a DeleteDesire is created it
// immediately marks it Deleted and the paired ReadDesire as NotFound,
// simulating an applier that confirms before post-delete discovery runs.
type InstantApplierStore struct {
	*memory.Store
}

func (s *InstantApplierStore) CreateDeleteDesire(
	ctx context.Context, dd desire.DeleteDesire,
) (desire.DeleteDesire, error) {
	created, err := s.Store.CreateDeleteDesire(ctx, dd)
	if err != nil {
		return created, err
	}
	if _, err = s.UpdateDeleteDesireStatus(ctx, dd.Identity, desire.Status{
		Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionTrue, Reason: desire.ReasonDeleted,
		}},
	}, created.Version); err != nil {
		return created, err
	}

	readID := dd.Identity
	readID.Type = desire.TypeRead
	if _, err = s.UpdateReadDesireStatus(ctx, readID, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionFalse, Reason: desire.ReasonNotFound,
		}}},
	}); err != nil {
		return created, err
	}
	return created, nil
}

// PendingDeleteApplierStore wraps memory.Store. When a DeleteDesire is
// created it marks the paired ReadDesire as NotFound (applier saw the
// resource gone) but does NOT confirm the delete desire, simulating
// an applier that is slow to ack the deletion.
type PendingDeleteApplierStore struct {
	*memory.Store
}

func (s *PendingDeleteApplierStore) CreateDeleteDesire(
	ctx context.Context, dd desire.DeleteDesire,
) (desire.DeleteDesire, error) {
	created, err := s.Store.CreateDeleteDesire(ctx, dd)
	if err != nil {
		return created, err
	}

	readID := dd.Identity
	readID.Type = desire.TypeRead
	if _, err = s.UpdateReadDesireStatus(ctx, readID, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionFalse, Reason: desire.ReasonNotFound,
		}}},
	}); err != nil {
		return created, err
	}
	return created, nil
}
