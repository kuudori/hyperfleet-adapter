package desireclient

import (
	"context"
	"errors"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetResource_NoDesiresIsNotFound(t *testing.T) {
	ctx := t.Context()
	c := newTestClient(newMemoryStore())

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.True(t, apierrors.IsNotFound(err),
		"no desire records and nothing in flight: never created or already cleaned up, got %v", err)
	assert.False(t, errors.Is(err, ErrNotSyncedYet), "nothing is waiting to sync")
}

func TestGetResource_MissingReadWithConfirmedDeleteIsNotFound(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)
	// Cleanup removes the read desire before the delete desire, so an
	// interrupted cleanup leaves only the confirmed delete desire behind.
	desiretest.PutConfirmedDeleteDesire(t, ctx, store, testID.Delete(), testOwner)

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.True(t, apierrors.IsNotFound(err), "a confirmed delete is not work in flight, got %v", err)
}

func TestGetResource_ReadDesireExistsNoResourceObservedIsNotSyncedYet(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: testID.Read(), Owner: testOwner, TargetVersion: "v1",
	})
	require.NoError(t, err)

	_, err = c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotSyncedYet), "no Successful condition yet must read as not-synced-yet")
}

func TestGetResource_SyncedReturnsMirroredObject(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))

	obj, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, testName, obj.GetName())
	assert.Equal(t, testNamespace, obj.GetNamespace())
}

func TestGetResource_ConfirmedNotFound(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testID.Read(), testOwner)

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "confirmed-gone must be a real NotFound, not ErrNotSyncedYet")
	assert.False(t, errors.Is(err, ErrNotSyncedYet))
}

func TestGetResource_NotFoundWithActiveApplyIsUnsynced(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testID.Read(), testOwner)
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{
		Identity: testID.Apply(), Owner: testOwner,
		Spec: desire.ApplySpec{KubeContent: configMapManifest(1)},
	})
	require.NoError(t, err)

	_, err = c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.ErrorIs(t, err, ErrNotSyncedYet)
	assert.False(t, apierrors.IsNotFound(err))
}

func TestGetResource_MissingReadWithActiveApplyIsUnsynced(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{
		Identity: testID.Apply(), Owner: testOwner,
		Spec: desire.ApplySpec{KubeContent: configMapManifest(1)},
	})
	require.NoError(t, err)

	_, err = c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.ErrorIs(t, err, ErrNotSyncedYet)
	assert.False(t, apierrors.IsNotFound(err))
}

func TestGetResource_MissingReadWithPendingDeleteIsUnsynced(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)
	desiretest.PutDeleteDesire(t, ctx, store, testID.Delete(), testOwner,
		metav1.ConditionFalse, desire.ReasonWaitingForDeletion)

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.ErrorIs(t, err, ErrNotSyncedYet)
	assert.False(t, apierrors.IsNotFound(err))
}

func TestGetResource_PendingDeleteDoesNotTrustReadNotFound(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testID.Read(), testOwner)
	desiretest.PutDeleteDesire(t, ctx, store, testID.Delete(), testOwner,
		metav1.ConditionFalse, desire.ReasonWaitingForDeletion)

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.ErrorIs(t, err, ErrNotSyncedYet)
	assert.False(t, apierrors.IsNotFound(err), "a pending delete is not confirmed by a stale read mirror")
}

func TestGetResource_ConfirmedDeleteStillReturnsStaleMirror(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)
	desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))
	desiretest.PutConfirmedDeleteDesire(t, ctx, store, testID.Delete(), testOwner)

	object, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, testName, object.GetName(), "ordinary discovery returns the mirror")
	state, err := c.ProbeDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, transportclient.DeletionConfirmed, state)
}

func TestGetResource_ConfirmedDeleteDoesNotChangeAbsentMirror(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testID.Read(), testOwner)
	desiretest.PutConfirmedDeleteDesire(t, ctx, store, testID.Delete(), testOwner)

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.True(t, apierrors.IsNotFound(err), "ordinary discovery reports the absent mirror")
	state, err := c.ProbeDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, transportclient.DeletionConfirmed, state)
}

func TestGetResource_InvalidReadDesire(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutInvalidReadDesire(t, ctx, store, testID.Read(), testOwner)

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	// ConditionTrue/ReasonNotFound is a shape the applier never reports; with no content it reads as not-synced-yet.
	assert.True(t, errors.Is(err, ErrNotSyncedYet),
		"empty content falls through to the empty-content rule: not-synced-yet")
	assert.False(t, apierrors.IsNotFound(err))
}

func TestGetResource_K8sAPIErrorWithRetainedMirrorReturnsStaleContent(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutKubeAPIErrorReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))

	obj, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err,
		"readdesire.kubeAPIError retains the last mirrored content across a transient failure - "+
			"it is still 'present' per the eventual-consistency contract, just possibly stale")
	assert.Equal(t, testName, obj.GetName())
}

func TestGetResource_K8sAPIErrorWithNoMirrorYetIsNotSyncedYet(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutKubeAPIErrorReadDesire(t, ctx, store, testID.Read(), testOwner, nil)

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotSyncedYet),
		"a transient failure with no content ever mirrored decodes as empty, "+
			"same as decodeKubeContent's own empty-content rule")
}

func TestDecodeKubeContent(t *testing.T) {
	tests := []struct {
		name         string
		content      []byte
		wantNotFound bool
		wantErr      bool
	}{
		{name: "nil content is a decode error", content: nil, wantErr: true},
		{name: "valid content decodes", content: configMapManifest(1)},
		{name: "invalid json is a decode error", content: []byte("not-json"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj, err := decodeKubeContent(tt.content, testNamespace, testName)
			switch {
			case tt.wantNotFound:
				require.Error(t, err)
				assert.True(t, apierrors.IsNotFound(err))
			case tt.wantErr:
				require.Error(t, err)
				assert.False(t, apierrors.IsNotFound(err), "a decode failure is not the same outcome as confirmed-absence")
			default:
				require.NoError(t, err)
				assert.Equal(t, testName, obj.GetName())
				assert.Equal(t, testNamespace, obj.GetNamespace())
			}
		})
	}
}

func TestDecodeKubeContent_EmptyObjectIsPresent(t *testing.T) {
	obj, err := decodeKubeContent([]byte(`{}`), testNamespace, testName)

	require.NoError(t, err)
	require.NotNil(t, obj)
	assert.Empty(t, obj.Object)
}

func TestDecodeReadDesire(t *testing.T) {
	tests := []struct {
		name          string
		condition     *metav1.Condition
		content       []byte
		wantNotSynced bool
		wantNotFound  bool
		wantContent   bool
	}{
		{
			name:          "no condition yet is not synced",
			condition:     nil,
			wantNotSynced: true,
		},
		{
			name:        "successful true decodes content",
			condition:   desiretest.SuccessfulCondition(metav1.ConditionTrue, desire.ReasonSynced),
			content:     configMapManifest(1),
			wantContent: true,
		},
		{
			// ConditionTrue/ReasonNotFound is a shape the applier never reports; with no content it reads as not-synced-yet.
			name:          "successful true with empty content is not synced yet",
			condition:     desiretest.SuccessfulCondition(metav1.ConditionTrue, desire.ReasonNotFound),
			wantNotSynced: true,
		},
		{
			name:         "false with notfound reason is confirmed absent",
			condition:    desiretest.SuccessfulCondition(metav1.ConditionFalse, desire.ReasonNotFound),
			wantNotFound: true,
		},
		{
			name:        "false with other reason decodes the retained mirror when present",
			condition:   desiretest.SuccessfulCondition(metav1.ConditionFalse, desire.ReasonKubeAPIError),
			content:     configMapManifest(1),
			wantContent: true,
		},
		{
			name:          "false with other reason and no retained mirror is not synced yet",
			condition:     desiretest.SuccessfulCondition(metav1.ConditionFalse, desire.ReasonKubeAPIError),
			wantNotSynced: true,
		},
	}

	c := newTestClient(newMemoryStore())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rd := desire.ReadDesire{Identity: testID.Read()}
			if tt.condition != nil {
				rd.Status = desire.ReadStatus{
					Status:      desire.Status{Conditions: []metav1.Condition{*tt.condition}},
					KubeContent: tt.content,
				}
			}

			obj, err := c.decodeReadDesire(testGVK(), testNamespace, testName, rd)
			switch {
			case tt.wantNotSynced:
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrNotSyncedYet))
			case tt.wantNotFound:
				require.Error(t, err)
				assert.True(t, apierrors.IsNotFound(err))
			case tt.wantContent:
				require.NoError(t, err)
				assert.Equal(t, testName, obj.GetName())
			}
		})
	}
}
