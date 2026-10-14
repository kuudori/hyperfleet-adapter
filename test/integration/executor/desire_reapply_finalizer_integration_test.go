package executorintegrationtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/executor"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	holdDeleteFinalizer = "test.hyperfleet.io/hold-delete"
	initialGeneration   = "1"
	initialValue        = "original"
)

var finalizerTestConfigMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

var finalizerTestIdentity = desiretest.TestIdentity{
	ManagementCluster: "cluster-1",
	Resource:          "configmaps",
	Namespace:         "default",
}

// TestDesireDeleteThenReapply_WaitsForFinalizer uses envtest for Kubernetes
// deletion semantics. Store status updates and apply writes model applier work
// explicitly; no applier process is started.
func TestDesireDeleteThenReapply_WaitsForFinalizer(t *testing.T) {
	env := SetupK8sTestEnv(t)
	ctx := t.Context()
	id := finalizerTestIdentity.WithName(fmt.Sprintf("reapply-finalizer-%d", time.Now().UnixNano()))
	store := memory.New()
	adapter := newDeletionChainExecutor(t, finalizerLifecycleConfig(id), hyperfleetapi.NewMockClient(), store)
	t.Cleanup(func() { cleanupFinalizerTestResource(t, env, id) })

	// Initial apply
	result := adapter.Execute(ctx, deletionGateParams(false))
	require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.ResourceResults, 1)

	// Apply the k8s resource
	applyDesire, err := store.GetApplyDesire(ctx, id.Apply())
	require.NoError(t, err)
	_, err = env.Client.ApplyResource(ctx, applyDesire.Spec.KubeContent, nil, nil)
	require.NoError(t, err)

	live := getConfigMap(t, ctx, env, id)
	originalUID := live.GetUID()

	// Set finalizer
	live.SetFinalizers([]string{holdDeleteFinalizer})
	live, err = env.Client.UpdateResource(ctx, live)
	require.NoError(t, err)

	// Update the read desire
	originalContent, err := json.Marshal(live.Object)
	require.NoError(t, err)
	desiretest.MarkReadDesireSynced(t, ctx, store, id.Read(), originalContent)
	read, err := store.GetReadDesire(ctx, id.Read())
	require.NoError(t, err)
	readVersion := read.Version

	// Execute deletion
	result = adapter.Execute(ctx, deletionGateParams(true))
	require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.ResourceResults, 1)

	// Try to delete the k8s resource
	require.NoError(t, env.Client.DeleteResource(ctx, finalizerTestConfigMapGVK, id.Namespace, id.Name, nil, nil))
	live = getConfigMap(t, ctx, env, id)
	require.NotNil(t, live.GetDeletionTimestamp())

	// Execute Apply
	pending := adapter.Execute(ctx, deletionGateParams(false))
	require.Equal(t, executor.StatusSuccess, pending.Status, "errors=%v", pending.Errors)
	require.Len(t, pending.ResourceResults, 1)
	assert.Equal(t, "delete pending, retrying on next event", pending.ResourceResults[0].OperationReason)
	assert.Nil(t, pending.ResourceResults[0].Error)
	assert.Equal(t, executor.ResourceStateUnsynced, pending.ExecutionContext.ResourceStates["remoteConfig"])
	mirrored, ok := pending.ExecutionContext.Resources["remoteConfig"].(*unstructured.Unstructured)
	require.True(t, ok)
	require.NotNil(t, mirrored)
	assert.Equal(t, id.Name, mirrored.GetName())
	assert.Nil(t, pending.ExecutionContext.Adapter.ExecutionError)

	// Make sure the apply desire has not replaced the delete desire
	_, err = store.GetDeleteDesire(ctx, id.Delete())
	require.NoError(t, err)
	_, err = store.GetApplyDesire(ctx, id.Apply())
	assert.ErrorIs(t, err, desire.ErrNotFound)

	// Assert that the same configmap and read desire still exist
	live = getConfigMap(t, ctx, env, id)
	assert.Equal(t, originalUID, live.GetUID())
	assertConfigMapValue(t, live, initialValue)
	read, err = store.GetReadDesire(ctx, id.Read())
	require.NoError(t, err)
	assert.Equal(t, readVersion, read.Version, "the ReadDesire must not be recreated while delete is pending")

	// Kubernetes confirms absence after the finalizer clears, but the store still
	// rejects apply until the applier confirms the DeleteDesire.
	live.SetFinalizers(nil)
	_, err = env.Client.UpdateResource(ctx, live)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, getErr := env.Client.GetResource(ctx, finalizerTestConfigMapGVK, id.Namespace, id.Name, nil)
		return apierrors.IsNotFound(getErr)
	}, 10*time.Second, 50*time.Millisecond)
	desiretest.MarkReadDesireNotFound(t, ctx, store, id.Read())

	pending = adapter.Execute(ctx, deletionGateParams(false))
	require.Equal(t, executor.StatusSuccess, pending.Status, "errors=%v", pending.Errors)
	require.Len(t, pending.ResourceResults, 1)
	assert.Equal(t, "delete pending, retrying on next event", pending.ResourceResults[0].OperationReason)
	_, err = store.GetDeleteDesire(ctx, id.Delete())
	require.NoError(t, err)
	_, err = store.GetApplyDesire(ctx, id.Apply())
	assert.ErrorIs(t, err, desire.ErrNotFound)

	// A confirmed DeleteDesire is atomically replaced by the new ApplyDesire.
	desiretest.MarkDeleteDesireConfirmed(t, ctx, store, id.Delete())
	result = adapter.Execute(ctx, deletionGateParams(false))
	require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.ResourceResults, 1)
	_, err = store.GetDeleteDesire(ctx, id.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound)
	applyDesire, err = store.GetApplyDesire(ctx, id.Apply())
	require.NoError(t, err)

	// Apply the configmap
	_, err = env.Client.ApplyResource(ctx, applyDesire.Spec.KubeContent, nil, nil)
	require.NoError(t, err)
	live = getConfigMap(t, ctx, env, id)
	assert.NotEqual(t, originalUID, live.GetUID())
	assert.Nil(t, live.GetDeletionTimestamp())
	assertConfigMapValue(t, live, initialValue)

	currentContent, err := json.Marshal(live.Object)
	require.NoError(t, err)
	desiretest.MarkReadDesireSynced(t, ctx, store, id.Read(), currentContent)

	// Verify the read desire was never recreated and the configmap is present in context
	result = adapter.Execute(ctx, deletionGateParams(false))
	require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.ResourceResults, 1)
	assert.Equal(t, executor.ResourceStatePresent, result.ExecutionContext.ResourceStates["remoteConfig"])
	read, err = store.GetReadDesire(ctx, id.Read())
	require.NoError(t, err)
	assert.Equal(t, readVersion, read.Version, "the ReadDesire must not be recreated after reapply")
	require.Len(t, read.Status.Conditions, 1)
	assert.Equal(t, desire.ReasonSynced, read.Status.Conditions[0].Reason)
}

func finalizerLifecycleConfig(id desiretest.TestIdentity) *configloader.Config {
	return &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "desire-reapply-finalizer-test"},
		Transports: map[string]configloader.TransportDefinition{
			desireDiscoveryTransport: desireDiscoveryDefinition(),
		},
		Resources: []configloader.Resource{finalizerLifecycleResource(id)},
	}
}

func finalizerLifecycleResource(id desiretest.TestIdentity) configloader.Resource {
	return configloader.Resource{
		Name:      "remoteConfig",
		Manifest:  configMapManifest(id),
		Transport: configloader.NamedTransport(desireDiscoveryTransport),
		Discovery: &configloader.DiscoveryConfig{Namespace: id.Namespace, ByName: id.Name},
		Lifecycle: &configloader.ResourceLifecycle{Delete: &configloader.LifecycleDelete{
			When: &configloader.LifecycleWhen{Expression: "event.hold"},
		}},
	}
}

func configMapManifest(id desiretest.TestIdentity) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      id.Name,
			"namespace": id.Namespace,
			"annotations": map[string]any{
				"hyperfleet.io/generation": initialGeneration,
			},
		},
		"data": map[string]any{"key": initialValue},
	}
}

func deletionGateParams(delete bool) map[string]any {
	return map[string]any{"hold": delete}
}

func getConfigMap(
	t *testing.T, ctx context.Context, env *K8sTestEnv, id desiretest.TestIdentity,
) *unstructured.Unstructured {
	t.Helper()

	live, err := env.Client.GetResource(ctx, finalizerTestConfigMapGVK, id.Namespace, id.Name, nil)
	require.NoError(t, err)
	return live
}

func assertConfigMapValue(t *testing.T, object *unstructured.Unstructured, want string) {
	t.Helper()

	value, found, err := unstructured.NestedString(object.Object, "data", "key")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, want, value)
}

func cleanupFinalizerTestResource(t *testing.T, env *K8sTestEnv, id desiretest.TestIdentity) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	live, err := env.Client.GetResource(ctx, finalizerTestConfigMapGVK, id.Namespace, id.Name, nil)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Logf("get %s for cleanup: %v", id.Name, err)
		return
	}

	live.SetFinalizers(nil)
	if _, err = env.Client.UpdateResource(ctx, live); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("clear finalizers from %s: %v", id.Name, err)
		return
	}
	err = env.Client.DeleteResource(ctx, finalizerTestConfigMapGVK, id.Namespace, id.Name, nil, nil)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("delete %s: %v", id.Name, err)
	}
}
