package desireclient

import (
	"context"
	"errors"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readIdentity() desire.Identity {
	return desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
}

func TestGetResource_ReadDesireNotFoundIsNotSyncedYet(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(newMemoryStore())

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotSyncedYet))
	assert.False(t, apierrors.IsNotFound(err), "absent mirror must not collapse into NotFound")
}

func TestGetResource_ReadDesireExistsNoResourceObservedIsNotSyncedYet(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: readIdentity(), Owner: testOwner, TargetVersion: "v1",
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

	putSyncedReadDesire(t, ctx, store, testNamespace, testName, configMapManifest(1))

	obj, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, testName, obj.GetName())
	assert.Equal(t, testNamespace, obj.GetNamespace())
}

func TestGetResource_ConfirmedNotFound(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	putConfirmedAbsentReadDesire(t, ctx, store, testNamespace, testName)

	_, err := c.GetResource(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "confirmed-gone must be a real NotFound, not ErrNotSyncedYet")
	assert.False(t, errors.Is(err, ErrNotSyncedYet))
}

func TestGetResource_InvalidReadDesire(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	putInvalidReadDesire(t, ctx, store, testNamespace, testName)

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

	putKubeAPIErrorReadDesire(t, ctx, store, testNamespace, testName, configMapManifest(1))

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

	putKubeAPIErrorReadDesire(t, ctx, store, testNamespace, testName, nil)

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
			condition:   successfulCondition(metav1.ConditionTrue, desire.ReasonSynced),
			content:     configMapManifest(1),
			wantContent: true,
		},
		{
			// ConditionTrue/ReasonNotFound is a shape the applier never reports; with no content it reads as not-synced-yet.
			name:          "successful true with empty content is not synced yet",
			condition:     successfulCondition(metav1.ConditionTrue, desire.ReasonNotFound),
			wantNotSynced: true,
		},
		{
			name:         "false with notfound reason is confirmed absent",
			condition:    successfulCondition(metav1.ConditionFalse, desire.ReasonNotFound),
			wantNotFound: true,
		},
		{
			name:        "false with other reason decodes the retained mirror when present",
			condition:   successfulCondition(metav1.ConditionFalse, desire.ReasonKubeAPIError),
			content:     configMapManifest(1),
			wantContent: true,
		},
		{
			name:          "false with other reason and no retained mirror is not synced yet",
			condition:     successfulCondition(metav1.ConditionFalse, desire.ReasonKubeAPIError),
			wantNotSynced: true,
		},
	}

	c := newTestClient(newMemoryStore())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rd := desire.ReadDesire{Identity: readIdentity()}
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
