package desireclient

import (
	"context"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDiscoverResources_EmptyPartitionReturnsEmptyList(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(newMemoryStore())

	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.NoError(t, err)
	assert.Empty(t, list.Items)
}

func TestDiscoverResources_ReturnsSyncedResourceByName(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))

	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{ByName: testName}, testTransportContext())
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, testName, list.Items[0].GetName())
}

func TestDiscoverResources_ByNameExcludesNonMatchingName(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))

	discovery := &manifest.DiscoveryConfig{ByName: "other-name"}
	list, err := c.DiscoverResources(ctx, testGVK(), discovery, testTransportContext())
	require.NoError(t, err)
	assert.Empty(t, list.Items, "discovery criteria must filter out non-matching names")
}

func TestDiscoverResources_LabelSelectorMatchesSubset(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	labeledManifest := []byte(`{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": {"name": "labeled", "namespace": "default", "labels": {"app": "myapp"}}
	}`)
	unlabeledManifest := []byte(`{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": {"name": "unlabeled", "namespace": "default"}
	}`)
	desiretest.PutSyncedReadDesire(t, ctx, store,
		testID.WithNamespace("labeled").WithName("labeled").Read(), testOwner, labeledManifest)
	desiretest.PutSyncedReadDesire(t, ctx, store,
		testID.WithNamespace("unlabeled").WithName("unlabeled").Read(), testOwner, unlabeledManifest)

	list, err := c.DiscoverResources(ctx, testGVK(),
		&manifest.DiscoveryConfig{LabelSelector: "app=myapp"}, testTransportContext())
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "labeled", list.Items[0].GetName())
}

func TestDiscoverResources_SkipsNotYetSyncedResource(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	id := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{Identity: id, Owner: testOwner, TargetVersion: "v1"})
	require.NoError(t, err)

	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.NoError(t, err)
	assert.Empty(t, list.Items, "a read desire with no Successful condition yet must not appear in discovery")
}

func TestDiscoverResources_SurfacesRetainedMirrorOnFailedRead(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutKubeAPIErrorReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))

	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.NoError(t, err)
	require.Len(t, list.Items, 1,
		"Successful=False/KubeAPIError retains the last mirrored content - it is still 'present', just possibly stale")
	assert.Equal(t, testName, list.Items[0].GetName())
}

func TestDiscoverResources_SkipsFailedReadWithNoRetainedMirror(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutKubeAPIErrorReadDesire(t, ctx, store, testID.Read(), testOwner, nil)

	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.NoError(t, err)
	assert.Empty(t, list.Items, "no content has ever been mirrored, so there is nothing to surface")
}

func TestDiscoverResources_SkipsNotFoundFalseDesire(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutNotFoundReadDesire(t, ctx, store, testID.Read(), testOwner)

	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.NoError(t, err)
	assert.Empty(t, list.Items, "Successful=False/NotFound desire must not appear in discovery")
}

func TestDiscoverResources_SelectorIgnoresApplyAndDeleteDesires(t *testing.T) {
	// Selector results come from read mirrors only. The partition is shared by
	// every target of the same kind, so another target's apply or unconfirmed
	// delete must not make this selector's result uncertain.
	tests := []struct {
		seed      func(t *testing.T, ctx context.Context, store *memory.Store)
		name      string
		namespace string
	}{
		{
			name: "unrelated pending delete in the partition",
			seed: func(t *testing.T, ctx context.Context, store *memory.Store) {
				desiretest.PutDeleteDesire(t, ctx, store,
					testID.WithNamespace("other-tenant").WithName("other-config").Delete(), testOwner,
					metav1.ConditionFalse, desire.ReasonWaitingForDeletion)
			},
		},
		{
			name:      "pending apply behind an old NotFound read",
			namespace: testNamespace,
			seed: func(t *testing.T, ctx context.Context, store *memory.Store) {
				_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{
					Identity: testID.Apply(), Owner: testOwner,
					Spec: desire.ApplySpec{KubeContent: []byte(`{
						"apiVersion":"v1","kind":"ConfigMap",
						"metadata":{"name":"my-config","namespace":"default","labels":{"app":"pending"}}
					}`)},
				})
				require.NoError(t, err)
				desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testID.Read(), testOwner)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			store := newMemoryStore()
			tt.seed(t, ctx, store)

			list, err := newTestClient(store).DiscoverResources(ctx, testGVK(),
				&manifest.DiscoveryConfig{Namespace: tt.namespace, LabelSelector: "app=pending"}, testTransportContext())

			require.NoError(t, err)
			assert.Empty(t, list.Items)
		})
	}
}

func TestDiscoverResources_SelectorWithUnsyncedReadInScopeIsNotSyncedYet(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	desiretest.PutUnsyncedReadDesire(t, ctx, store, testID.WithName("syncing-config").Read(), testOwner)
	c := newTestClient(store)

	_, err := c.DiscoverResources(ctx, testGVK(),
		&manifest.DiscoveryConfig{Namespace: testNamespace, LabelSelector: "app=missing"}, testTransportContext())
	require.ErrorIs(t, err, ErrNotSyncedYet, "an unsynced mirror in scope might still match")

	list, err := c.DiscoverResources(ctx, testGVK(),
		&manifest.DiscoveryConfig{Namespace: "other", LabelSelector: "app=missing"}, testTransportContext())
	require.NoError(t, err, "an unsynced mirror outside the selector's namespace cannot match")
	assert.Empty(t, list.Items)
}

func TestDiscoverResources_ConfirmedDeleteStillReturnsStaleMatchingRead(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	c := newTestClient(store)
	content := []byte(`{
		"apiVersion":"v1","kind":"ConfigMap",
		"metadata":{"name":"my-config","namespace":"default","labels":{"app":"deleted"}}
	}`)
	desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, content)
	desiretest.PutConfirmedDeleteDesire(t, ctx, store, testID.Delete(), testOwner)

	list, err := c.DiscoverResources(ctx, testGVK(),
		&manifest.DiscoveryConfig{Namespace: testNamespace, LabelSelector: "app=deleted"}, testTransportContext())
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "ordinary selector discovery returns the mirror")
}

func TestDiscoverResources_SkipsUndecodableContentButKeepsOthers(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutSyncedReadDesire(t, ctx, store, testID.WithName("bad").Read(), testOwner, []byte("not-json"))
	desiretest.PutSyncedReadDesire(t, ctx, store, testID.WithName("good").Read(), testOwner, configMapManifest(1))

	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.NoError(t, err, "a single bad record must not fail discovery for the whole partition")
	require.Len(t, list.Items, 1)
	assert.Equal(t, testName, list.Items[0].GetName())
}

func TestDiscoverResources_FiltersOutOtherResourceType(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))

	otherContext := &TransportContext{ManagementCluster: testManagementCluster, Resource: "secrets"}
	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, otherContext)
	require.NoError(t, err)
	assert.Empty(t, list.Items, "a read desire for a different plural resource must not appear")
}

func TestDiscoverResources_FiltersOutOtherGroup(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	id := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Group: "apps", Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	_, err := store.CreateReadDesire(ctx, desire.ReadDesire{Identity: id, Owner: testOwner, TargetVersion: "v1"})
	require.NoError(t, err)
	_, err = store.UpdateReadDesireStatus(ctx, id, desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{
			Type: desire.TypeSuccessful, Status: metav1.ConditionTrue, Reason: desire.ReasonSynced,
		}}},
		KubeContent: configMapManifest(1),
	})
	require.NoError(t, err)

	// testGVK() has an empty Group ("core"), so a read desire recorded under
	// group "apps" must not match even though Resource matches.
	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.NoError(t, err)
	assert.Empty(t, list.Items, "a read desire in a different API group must not appear")
}

func TestDiscoverResources_ScopedToPartition(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(1))

	otherPartition := &TransportContext{ManagementCluster: "other-cluster", Resource: testResource}
	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, otherPartition)
	require.NoError(t, err)
	assert.Empty(t, list.Items, "discovery must not leak resources across management-cluster partitions")
}

func TestDiscoverResources_MultipleMatches(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	first := []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "first", "namespace": "default"}}`)
	second := []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "second", "namespace": "default"}}`)
	desiretest.PutSyncedReadDesire(t, ctx, store, testID.WithName("first").Read(), testOwner, first)
	desiretest.PutSyncedReadDesire(t, ctx, store, testID.WithName("second").Read(), testOwner, second)

	list, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.NoError(t, err)
	require.Len(t, list.Items, 2)
	names := []string{list.Items[0].GetName(), list.Items[1].GetName()}
	assert.ElementsMatch(t, []string{"first", "second"}, names)
}

func TestDiscoverResources_ListFailure_ReturnsError(t *testing.T) {
	ctx := context.Background()
	store := &failingListReadDesiresStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	_, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, testTransportContext())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to list read desires")
}

func TestDiscoverResources_RequiresTransportContext(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(newMemoryStore())

	_, err := c.DiscoverResources(ctx, testGVK(), &manifest.DiscoveryConfig{}, nil)
	require.Error(t, err)
}
