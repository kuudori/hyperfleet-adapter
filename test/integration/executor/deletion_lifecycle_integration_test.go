package executorintegrationtest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/executor"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteConfirmationIgnoresStaleReadAndCleansBoth(t *testing.T) {
	for _, staleRead := range []string{"present", "NotFound"} {
		t.Run(staleRead, func(t *testing.T) {
			ctx := t.Context()
			store := memory.New()
			if staleRead == "present" {
				desiretest.PutSyncedReadDesire(
					t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter", []byte(mirroredStatusObject))
			} else {
				desiretest.PutConfirmedAbsentReadDesire(
					t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter")
			}
			desiretest.PutConfirmedDeleteDesire(t, ctx, store, desireDiscoveryIdentity.Delete(), "hyperfleet-adapter")

			config := desireDiscoveryConfig()
			apiClient := hyperfleetapi.NewMockClient()
			adapterExecutor, err := executor.NewBuilder().
				WithConfig(config).
				WithAPIClient(apiClient).
				WithTransportRegistry(transportclient.Registry{
					desireDiscoveryTransport: desireclient.NewClient(store, "hyperfleet-adapter"),
				}).
				Build()
			require.NoError(t, err)

			result := adapterExecutor.Execute(ctx, map[string]any{"deleted_time": true})

			require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
			require.NotNil(t, result.ExecutionContext)
			assert.Equal(t, executor.ResourceStateConfirmedDeleted,
				result.ExecutionContext.ResourceStates["remoteConfig"])
			require.Len(t, result.ResourceResults, 1)
			assert.Equal(t, "resource deletion confirmed by transport", result.ResourceResults[0].OperationReason)

			_, err = store.GetDeleteDesire(ctx, desireDiscoveryIdentity.Delete())
			assert.ErrorIs(t, err, desire.ErrNotFound, "confirmed DeleteDesire must be removed")
			_, err = store.GetReadDesire(ctx, desireDiscoveryIdentity.Read())
			assert.ErrorIs(t, err, desire.ErrNotFound, "stale ReadDesire must be removed")
		})
	}
}

func TestAlreadyCleanedResourceAllowsDependentDeleteAndFinalizationCEL(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	applierStore := &desiretest.InstantApplierStore{Store: store}
	desiretest.PutSyncedReadDesire(
		t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter", []byte(mirroredStatusObject))
	desiretest.PutConfirmedDeleteDesire(t, ctx, store, desireDiscoveryIdentity.Delete(), "hyperfleet-adapter")
	dependentIdentity := desiretest.TestIdentity{
		ManagementCluster: desireDiscoveryIdentity.ManagementCluster,
		Resource:          desireDiscoveryIdentity.Resource,
		Namespace:         desireDiscoveryIdentity.Namespace,
		Name:              "dependent-config",
	}
	desiretest.PutSyncedReadDesire(t, ctx, store, dependentIdentity.Read(), "hyperfleet-adapter", []byte(`{
		"apiVersion":"v1","kind":"ConfigMap",
		"metadata":{"name":"dependent-config","namespace":"default","annotations":{"hyperfleet.io/generation":"1"}}
	}`))

	config := desireDiscoveryConfig()
	config.Resources = append(config.Resources, configloader.Resource{
		Name: "dependentConfig",
		Manifest: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      dependentIdentity.Name,
				"namespace": dependentIdentity.Namespace,
				"annotations": map[string]any{
					"hyperfleet.io/generation": "1",
				},
			},
		},
		Transport: &configloader.TransportConfig{
			Client: desireDiscoveryTransport,
			Desire: &configloader.DesireTransportConfig{
				TargetCluster: dependentIdentity.ManagementCluster,
				Resource:      dependentIdentity.Resource,
			},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: dependentIdentity.Namespace,
			ByName:    dependentIdentity.Name,
		},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				When: &configloader.LifecycleWhen{
					Expression: `event.?second.orValue(false) && ` +
						`resource_states.?remoteConfig.orValue("") == "confirmed_deleted"`,
				},
			},
		},
	})
	// This is the same resource-state guard used by the documented Finalized condition.
	config.Post.PostActions[0].When.Expression =
		`event.?second.orValue(false) && ` +
			`resource_states.?remoteConfig.orValue("") == "confirmed_deleted" && ` +
			`resource_states.?dependentConfig.orValue("") == "confirmed_deleted"`

	apiClient := hyperfleetapi.NewMockClient()
	adapterExecutor, err := executor.NewBuilder().
		WithConfig(config).
		WithAPIClient(apiClient).
		WithTransportRegistry(transportclient.Registry{
			desireDiscoveryTransport: desireclient.NewClient(applierStore, "hyperfleet-adapter"),
		}).
		Build()
	require.NoError(t, err)

	firstResult := adapterExecutor.Execute(ctx, map[string]any{"deleted_time": true})
	require.Equal(t, executor.StatusSuccess, firstResult.Status, "errors=%v", firstResult.Errors)
	require.Equal(t, executor.ResourceStateConfirmedDeleted,
		firstResult.ExecutionContext.ResourceStates["remoteConfig"])
	_, err = store.GetDeleteDesire(ctx, desireDiscoveryIdentity.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "the first event must clean the confirmed target")
	_, err = store.GetReadDesire(ctx, desireDiscoveryIdentity.Read())
	assert.ErrorIs(t, err, desire.ErrNotFound, "the first event must remove the target's read mirror")

	result := adapterExecutor.Execute(ctx, map[string]any{"deleted_time": true, "second": true})

	require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.NotNil(t, result.ExecutionContext)
	assert.Equal(t, executor.ResourceStateConfirmedDeleted,
		result.ExecutionContext.ResourceStates["remoteConfig"])
	require.Len(t, result.ResourceResults, 2)
	assert.Equal(t, "resource already deleted or never existed", result.ResourceResults[0].OperationReason)
	assert.Equal(t, "resource deletion confirmed by transport", result.ResourceResults[1].OperationReason)
	assert.Equal(t, executor.ResourceStateConfirmedDeleted,
		result.ExecutionContext.ResourceStates["dependentConfig"])
	require.Len(t, result.PostActionResults, 2)
	assert.True(t, result.PostActionResults[0].APICallMade,
		"the Finalized-style CEL guard must observe confirmed absence after cleanup")

	_, err = store.GetDeleteDesire(ctx, desireDiscoveryIdentity.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "the already-cleaned target must not receive a new delete desire")
	_, err = store.GetApplyDesire(ctx, desireDiscoveryIdentity.Apply())
	assert.ErrorIs(t, err, desire.ErrNotFound, "the already-cleaned target must not receive a new apply desire")
	_, err = store.GetDeleteDesire(ctx, dependentIdentity.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "the dependent's instant confirmation must also be cleaned")
	_, err = store.GetReadDesire(ctx, dependentIdentity.Read())
	assert.ErrorIs(t, err, desire.ErrNotFound, "cleanup must remove the dependent's ReadDesire")
}

// deletionChainResource is one resource of the delete-ordering example in the
// architecture repo's adapter-lifecycle-delete-design.md: the namespace waits
// for the configmap and the job, the configmap waits for the job, and the
// dependents are listed first.
type deletionChainResource struct {
	alias     string
	name      string
	dependsOn []string
}

var deletionChain = []deletionChainResource{
	{alias: "clusterNamespace", name: "cluster-namespace", dependsOn: []string{"clusterConfigMap", "clusterJob"}},
	{alias: "clusterConfigMap", name: "cluster-config", dependsOn: []string{"clusterJob"}},
	{alias: "clusterJob", name: "cluster-job"},
}

// TestDeletionChainFinalizesWithoutReapplying runs a whole deletion across
// events, with the applier acting between them. Once a resource's desires are
// cleaned up, it must read as absent to every gate in any list order;
// otherwise a dependent's gate turns false and falls through to apply, which
// re-creates a deleted resource.
func TestDeletionChainFinalizesWithoutReapplying(t *testing.T) {
	dependenciesFirst := []deletionChainResource{deletionChain[2], deletionChain[1], deletionChain[0]}
	tests := []struct {
		name      string
		order     []deletionChainResource
		useStates bool
		mirrorLag int
	}{
		{name: "documented order", order: deletionChain},
		{name: "documented order with resource_states gates", order: deletionChain, useStates: true},
		{name: "documented order with lagging mirrors", order: deletionChain, mirrorLag: 1},
		{name: "documented order with mirrors that never observe the delete", order: deletionChain, mirrorLag: math.MaxInt},
		{name: "dependencies first", order: dependenciesFirst},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			store := memory.New()
			applier := newSimulatedApplier(store, tt.mirrorLag)
			for _, r := range tt.order {
				applier.seedExisting(t, ctx, r.name)
			}
			applier.pass(t, ctx)
			apiClient := hyperfleetapi.NewMockClient()
			adapterExecutor := newDeletionChainExecutor(t, deletionChainConfig(tt.order, tt.useStates), apiClient, store)

			finalizedAt := 0
			for event := 1; event <= 10; event++ {
				result := adapterExecutor.Execute(ctx, map[string]any{"deleted_time": true})
				require.Equal(t, executor.StatusSuccess, result.Status, "event %d: errors=%v", event, result.Errors)
				finalized := postPayloadValue(t, apiClient, "finalized")
				if finalizedAt > 0 {
					assert.Equal(t, true, finalized, "event %d: Finalized must stay true once reached", event)
				} else if finalized == true {
					finalizedAt = event
				}
				applier.pass(t, ctx)
			}

			assert.Empty(t, applier.reapplied, "a deleted resource was applied again")
			require.NotZero(t, finalizedAt, "the deletion never finalized")
			assert.Empty(t, applier.cluster, "every resource must be deleted")
			assertNoDesires(t, ctx, store)
		})
	}
}

// TestDeletionChainWithNoDesiresFinalizesWithoutWrites covers a deletion that
// starts before the adapter ever created anything: with no desire records the
// resources are already absent, so nothing is written, not even a delete desire.
func TestDeletionChainWithNoDesiresFinalizesWithoutWrites(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	apiClient := hyperfleetapi.NewMockClient()
	adapterExecutor := newDeletionChainExecutor(t, deletionChainConfig(deletionChain, false), apiClient, store)

	result := adapterExecutor.Execute(ctx, map[string]any{"deleted_time": true})

	require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
	assert.Equal(t, true, postPayloadValue(t, apiClient, "finalized"))
	assertNoDesires(t, ctx, store)
}

func deletionChainConfig(order []deletionChainResource, useStates bool) *configloader.Config {
	absent := func(alias string) string {
		if useStates {
			return fmt.Sprintf(`resource_states.?%s.orValue("") == "confirmed_deleted"`, alias)
		}
		return fmt.Sprintf("!resources.?%s.hasValue()", alias)
	}
	const isDeleting = "has(event.deleted_time) && event.deleted_time == true"
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "deletion-chain-test"},
		Transports: map[string]configloader.TransportDefinition{
			desireDiscoveryTransport: {Type: configloader.TransportTypeRemote},
		},
	}
	finalized := []string{isDeleting, `adapter.?executionStatus.orValue("") == "success"`}
	for _, r := range order {
		gate := []string{isDeleting}
		for _, dependency := range r.dependsOn {
			gate = append(gate, absent(dependency))
		}
		config.Resources = append(config.Resources, configloader.Resource{
			Name:     r.alias,
			Manifest: deletionChainManifest(r.name),
			Transport: &configloader.TransportConfig{
				Client: desireDiscoveryTransport,
				Desire: &configloader.DesireTransportConfig{
					TargetCluster: desireDiscoveryIdentity.ManagementCluster,
					Resource:      desireDiscoveryIdentity.Resource,
				},
			},
			Discovery: &configloader.DiscoveryConfig{Namespace: desireDiscoveryIdentity.Namespace, ByName: r.name},
			Lifecycle: &configloader.ResourceLifecycle{Delete: &configloader.LifecycleDelete{
				When: &configloader.LifecycleWhen{Expression: strings.Join(gate, " && ")},
			}},
		})
		finalized = append(finalized, absent(r.alias))
	}
	config.Post = &configloader.PostConfig{
		Payloads: []configloader.Payload{{
			Name:  "statusPayload",
			Build: map[string]any{"finalized": map[string]any{"expression": strings.Join(finalized, " && ")}},
		}},
		PostActions: []configloader.PostAction{{
			ActionBase: configloader.ActionBase{
				Name:    "reportStatus",
				APICall: &configloader.APICall{Method: "PUT", URL: "/status", Body: "{{ .statusPayload }}"},
			},
		}},
	}
	return config
}

func deletionChainManifest(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": desireDiscoveryIdentity.Namespace,
			"annotations": map[string]any{
				"hyperfleet.io/generation": "1",
			},
		},
	}
}

func newDeletionChainExecutor(
	t *testing.T, config *configloader.Config, apiClient *hyperfleetapi.MockClient, store *memory.Store,
) *executor.Executor {
	t.Helper()
	adapterExecutor, err := executor.NewBuilder().
		WithConfig(config).
		WithAPIClient(apiClient).
		WithTransportRegistry(transportclient.Registry{
			desireDiscoveryTransport: desireclient.NewClient(store, "hyperfleet-adapter"),
		}).
		Build()
	require.NoError(t, err)
	return adapterExecutor
}

// simulatedApplier reconciles the desire store against an in-memory cluster
// between events: it applies apply desires, deletes and confirms delete
// desires, then mirrors every read desire. A deleted object's mirror keeps its
// last content for mirrorLag passes.
type simulatedApplier struct {
	store       *memory.Store
	cluster     map[string][]byte
	deletedPass map[string]int
	reapplied   []string
	mirrorLag   int
	passes      int
}

func newSimulatedApplier(store *memory.Store, mirrorLag int) *simulatedApplier {
	return &simulatedApplier{
		store: store, mirrorLag: mirrorLag, cluster: map[string][]byte{}, deletedPass: map[string]int{},
	}
}

// seedExisting records an apply desire and its read desire, as a previous
// apply event would have; the next pass creates the object.
func (a *simulatedApplier) seedExisting(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	content, err := json.Marshal(deletionChainManifest(name))
	require.NoError(t, err)
	target := desireDiscoveryIdentity.WithName(name)
	_, err = a.store.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: target.Read(), Owner: "hyperfleet-adapter", TargetVersion: "v1",
	})
	require.NoError(t, err)
	_, err = a.store.CreateApplyDesire(ctx, desire.ApplyDesire{
		Identity: target.Apply(), Owner: "hyperfleet-adapter", Spec: desire.ApplySpec{KubeContent: content},
	})
	require.NoError(t, err)
}

func (a *simulatedApplier) pass(t *testing.T, ctx context.Context) {
	t.Helper()
	a.passes++
	partition := desireDiscoveryIdentity.ManagementCluster
	applies, err := a.store.ListApplyDesires(ctx, partition)
	require.NoError(t, err)
	for _, apply := range applies {
		name := apply.Identity.Name
		if _, exists := a.cluster[name]; !exists {
			if _, deleted := a.deletedPass[name]; deleted {
				a.reapplied = append(a.reapplied, name)
			}
		}
		a.cluster[name] = apply.Spec.KubeContent
	}
	deletes, err := a.store.ListDeleteDesires(ctx, partition)
	require.NoError(t, err)
	for _, deletion := range deletes {
		if desire.IsDeleted(deletion.Status) {
			continue
		}
		delete(a.cluster, deletion.Identity.Name)
		a.deletedPass[deletion.Identity.Name] = a.passes
		desiretest.MarkDeleteDesireConfirmed(t, ctx, a.store, deletion.Identity)
	}
	reads, err := a.store.ListReadDesires(ctx, partition)
	require.NoError(t, err)
	for _, read := range reads {
		name := read.Identity.Name
		if deletedPass, deleted := a.deletedPass[name]; deleted && a.passes-deletedPass < a.mirrorLag {
			continue
		}
		if content, exists := a.cluster[name]; exists {
			desiretest.MarkReadDesireSynced(t, ctx, a.store, read.Identity, content)
		} else {
			desiretest.MarkReadDesireNotFound(t, ctx, a.store, read.Identity)
		}
	}
}

func assertNoDesires(t *testing.T, ctx context.Context, store *memory.Store) {
	t.Helper()
	partition := desireDiscoveryIdentity.ManagementCluster
	applies, err := store.ListApplyDesires(ctx, partition)
	require.NoError(t, err)
	assert.Empty(t, applies, "apply desires must be removed")
	deletes, err := store.ListDeleteDesires(ctx, partition)
	require.NoError(t, err)
	assert.Empty(t, deletes, "delete desires must be removed")
	reads, err := store.ListReadDesires(ctx, partition)
	require.NoError(t, err)
	assert.Empty(t, reads, "read desires must be removed")
}
