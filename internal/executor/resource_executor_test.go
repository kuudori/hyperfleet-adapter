package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/metrics"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// testDeletedTime is a non-null deleted_time value used in lifecycle delete tests to trigger when-expressions.
const testDeletedTime = "2026-01-01T00:00:00Z"

const (
	testResourceAName = "resourceA"
	testResourceBName = "resourceB"
	testResourceAID   = "resource-a"
	testResourceBID   = "resource-b"
	testResourceBPath = "default/resource-b"
)

func TestResourceExecutor_ResolveTransport(t *testing.T) {
	remoteClient := k8sclient.NewMockK8sClient()
	kubernetesClient := k8sclient.NewMockK8sClient()
	namedKubernetesClient := k8sclient.NewMockK8sClient()
	re := newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{Transports: map[string]configloader.TransportDefinition{
			"remote-primary":     {Type: configloader.TransportTypeRemote},
			"kubernetes-primary": {Type: configloader.TransportTypeKubernetes},
		}},
		TransportRegistry: transportclient.Registry{
			"remote-primary":                       remoteClient,
			"kubernetes-primary":                   namedKubernetesClient,
			configloader.TransportClientKubernetes: kubernetesClient,
		},
	})
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["clusterName"] = "cluster-1"

	tests := []struct {
		resource       *configloader.Resource
		wantClient     transportclient.TransportClient
		wantTarget     transportclient.TransportContext
		name           string
		wantErrContain string
	}{
		{
			name:       "uses Kubernetes by default",
			resource:   &configloader.Resource{},
			wantClient: kubernetesClient,
		},
		{
			name: "uses named Kubernetes transport without context",
			resource: &configloader.Resource{Transport: &configloader.TransportConfig{
				Client: "kubernetes-primary",
			}},
			wantClient: namedKubernetesClient,
		},
		{
			name: "builds Desire context for named remote transport",
			resource: &configloader.Resource{Transport: &configloader.TransportConfig{
				Client: "remote-primary",
				Desire: &configloader.DesireTransportConfig{
					TargetCluster: "{{ .clusterName }}",
					Resource:      "nodepools",
				},
			}},
			wantClient: remoteClient,
			wantTarget: &desireclient.TransportContext{
				ManagementCluster: "cluster-1",
				Resource:          "nodepools",
			},
		},
		{
			name: "resolves mixed-case transport reference",
			resource: &configloader.Resource{Transport: &configloader.TransportConfig{
				Client: "Remote-Primary",
				Desire: &configloader.DesireTransportConfig{
					TargetCluster: "{{ .clusterName }}",
					Resource:      "nodepools",
				},
			}},
			wantClient: remoteClient,
			wantTarget: &desireclient.TransportContext{
				ManagementCluster: "cluster-1",
				Resource:          "nodepools",
			},
		},
		{
			name: "rejects unknown transport",
			resource: &configloader.Resource{Transport: &configloader.TransportConfig{
				Client: "missing-transport",
			}},
			wantErrContain: `transport client "missing-transport" not configured`,
		},
		{
			name: "rejects remote transport without Desire configuration",
			resource: &configloader.Resource{Transport: &configloader.TransportConfig{
				Client: "remote-primary",
			}},
			wantErrContain: `desire transport config is required for "remote-primary"`,
		},
		{
			name: "rejects invalid Desire target cluster template",
			resource: &configloader.Resource{Transport: &configloader.TransportConfig{
				Client: "remote-primary",
				Desire: &configloader.DesireTransportConfig{
					TargetCluster: "{{ .missing }}",
					Resource:      "nodepools",
				},
			}},
			wantErrContain: "render desire target cluster",
		},
		{
			name: "rejects Desire transport without resource type",
			resource: &configloader.Resource{Transport: &configloader.TransportConfig{
				Client: "remote-primary",
				Desire: &configloader.DesireTransportConfig{
					TargetCluster: "cluster-1",
				},
			}},
			wantErrContain: `desire resource is required for "remote-primary"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, target, err := re.resolveTransport(*tt.resource, execCtx)

			if tt.wantErrContain != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrContain)
				return
			}
			require.NoError(t, err)
			assert.Same(t, tt.wantClient, client)
			assert.Equal(t, tt.wantTarget, target)
		})
	}
}

func TestResourceExecutor_ResolveTransport_RejectsCustomMaestroName(t *testing.T) {
	re := newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{Transports: map[string]configloader.TransportDefinition{
			configloader.TransportClientMaestro: {Type: configloader.TransportTypeRemote},
		}},
	})

	client, target, err := re.resolveTransport(configloader.Resource{
		Transport: &configloader.TransportConfig{
			Client: configloader.TransportClientMaestro,
			Desire: &configloader.DesireTransportConfig{
				TargetCluster: "cluster-1",
				Resource:      "nodepools",
			},
		},
	}, NewExecutionContext(context.Background(), nil, nil))

	require.Error(t, err)
	assert.ErrorContains(t, err, `transport name "maestro" is reserved for the built-in maestro transport`)
	assert.Nil(t, client)
	assert.Nil(t, target)
}

func TestResourceExecutor_ExecuteAll_UnknownTransport(t *testing.T) {
	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(k8sclient.NewMockK8sClient()),
	})
	resource := configloader.Resource{
		Name: "test-resource",
		Transport: &configloader.TransportConfig{
			Client: "missing-transport",
		},
	}

	_, err := re.ExecuteAll(
		context.Background(),
		[]configloader.Resource{resource},
		NewExecutionContext(context.Background(), nil, nil),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `transport client "missing-transport" not configured`)
}

func newNamedRemoteResourceExecutor(remote, fallback transportclient.TransportClient) *ResourceExecutor {
	return newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{Transports: map[string]configloader.TransportDefinition{
			"remote-primary": {Type: configloader.TransportTypeRemote},
		}},
		TransportRegistry: transportclient.Registry{
			"remote-primary":                       remote,
			configloader.TransportClientKubernetes: fallback,
		},
	})
}

func namedRemoteResource(discovery *configloader.DiscoveryConfig) configloader.Resource {
	return configloader.Resource{
		Name: "test-resource",
		Transport: &configloader.TransportConfig{
			Client: "remote-primary",
			Desire: &configloader.DesireTransportConfig{
				TargetCluster: "cluster-1",
				Resource:      "configmaps",
			},
		},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-config",
				"namespace": "default",
			},
		},
		Discovery: discovery,
	}
}

func TestResourceExecutor_NamedRemoteTransportRoutesLifecycleOperations(t *testing.T) {
	fallback := k8sclient.NewMockK8sClient()
	fallback.ApplyResourceError = errors.New("default transport must not be used")
	fallback.GetResourceError = errors.New("default transport must not be used")
	fallback.DiscoverError = errors.New("default transport must not be used")
	fallback.DeleteResourceError = errors.New("default transport must not be used")

	t.Run("pre-discovery, apply, and by-name discovery", func(t *testing.T) {
		remote := k8sclient.NewMockK8sClient()
		resource := namedRemoteResource(&configloader.DiscoveryConfig{Namespace: "default", ByName: "test-config"})
		resource.Lifecycle = &configloader.ResourceLifecycle{
			Create: &configloader.LifecycleCreate{When: &configloader.LifecycleWhen{Expression: "true"}},
		}
		execCtx := NewExecutionContext(t.Context(), nil, nil)

		results, err := newNamedRemoteResourceExecutor(remote, fallback).ExecuteAll(
			t.Context(), []configloader.Resource{resource}, execCtx)

		require.NoError(t, err)
		require.Equal(t, manifest.OperationCreate, results[0].Operation)
		assert.Equal(t, ResourceStatePresent, execCtx.ResourceStates[resource.Name])
	})

	t.Run("selector discovery", func(t *testing.T) {
		remote := k8sclient.NewMockK8sClient()
		remote.DiscoverResult = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{{
			Object: map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap"},
		}}}
		resource := namedRemoteResource(&configloader.DiscoveryConfig{
			Namespace:   "default",
			BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{"app": "test"}},
		})

		_, err := newNamedRemoteResourceExecutor(remote, fallback).ExecuteAll(
			context.Background(), []configloader.Resource{resource}, NewExecutionContext(context.Background(), nil, nil))

		require.NoError(t, err)
	})

	t.Run("non-Desire selector no-match confirms absence", func(t *testing.T) {
		remote := k8sclient.NewMockK8sClient()
		remote.DiscoverResult = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{{
			Object: map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap"},
		}}}
		resource := namedRemoteResource(&configloader.DiscoveryConfig{
			Namespace:   "default",
			BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{"app": "missing"}},
		})
		execCtx := NewExecutionContext(t.Context(), nil, nil)
		re := newNamedRemoteResourceExecutor(remote, fallback)
		client, target, err := re.resolveTransport(resource, execCtx)
		require.NoError(t, err)
		_, err = re.discoverResource(t.Context(), resource, execCtx, client, target)
		require.NoError(t, err)
		require.Equal(t, ResourceStatePresent, execCtx.ResourceStates[resource.Name])
		remote.DiscoverResult = &unstructured.UnstructuredList{}

		_, err = re.discoverResource(t.Context(), resource, execCtx, client, target)

		require.True(t, apierrors.IsNotFound(err))
		assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[resource.Name])
	})

	t.Run("delete and post-delete discovery", func(t *testing.T) {
		remote := k8sclient.NewMockK8sClient()
		remote.Resources["default/test-config"] = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "test-config", "namespace": "default"},
		}}
		resource := namedRemoteResource(&configloader.DiscoveryConfig{Namespace: "default", ByName: "test-config"})
		resource.Lifecycle = &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{When: &configloader.LifecycleWhen{Expression: "true"}},
		}
		execCtx := NewExecutionContext(t.Context(), nil, nil)

		results, err := newNamedRemoteResourceExecutor(remote, fallback).ExecuteAll(
			t.Context(), []configloader.Resource{resource}, execCtx)

		require.NoError(t, err)
		require.Equal(t, manifest.OperationDelete, results[0].Operation)
		assert.Empty(t, remote.Resources)
		assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[resource.Name])
	})
}

func TestResourceExecutor_DesireTransport_DeletedPrerequisiteBeforeDependent(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
	dependentClient := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	dependentClient.Resources["default/test-cm"] = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "test-cm", "namespace": "default"},
	}}
	re := newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{Transports: map[string]configloader.TransportDefinition{
			desireTransportName: {Type: configloader.TransportTypeRemote},
		}},
		TransportRegistry: transportclient.Registry{
			desireTransportName:                    desireclient.NewClient(store, desireOwner),
			configloader.TransportClientKubernetes: dependentClient,
		},
	})
	prerequisite := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	prerequisite.Name = testResourceAName
	dependent := newResourceWithLifecycle(
		fmt.Sprintf("!resources.?%s.hasValue()", testResourceAName), "Background")
	dependent.Name = testResourceBName

	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err := re.ExecuteAll(ctx, []configloader.Resource{dependent, prerequisite}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	assert.True(t, dependentClient.DeleteCalled)
	assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[prerequisite.Name])
	assert.NotContains(t, execCtx.GetCELVariables()[configloader.FieldResources], prerequisite.Name)
}

type failingReadCleanupStore struct {
	desire.SpecStore
}

func (s *failingReadCleanupStore) DeleteReadDesire(
	_ context.Context, _ desire.Identity, _ string, _ int64,
) error {
	return errors.New("cleanup write failed")
}

func TestResourceExecutor_DesireCleanupFailureBlocksDependentDelete(t *testing.T) {
	tests := []struct {
		name           string
		deleteGate     string
		readNotFound   bool
		noDeleteDesire bool
	}{
		{
			name:       "stale object with resource state gate",
			deleteGate: `resource_states.?resourceA.orValue("") == "confirmed_deleted"`,
		},
		{
			name:         "NotFound mirror with legacy absence gate",
			readNotFound: true,
			deleteGate:   `!resources.?resourceA.hasValue()`,
		},
		{
			name:           "no-work cleanup failure with state gate",
			readNotFound:   true,
			noDeleteDesire: true,
			deleteGate:     `resource_states.?resourceA.orValue("") == "confirmed_deleted"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			store := memory.New()
			if tt.readNotFound {
				desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
			} else {
				desiretest.PutSyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner, configMapContent())
			}
			if !tt.noDeleteDesire {
				desiretest.PutConfirmedDeleteDesire(t, ctx, store, testDesireID.Delete(), desireOwner)
			}
			dependentClient := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
			dependentClient.Resources["default/test-cm"] = &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "test-cm", "namespace": "default"},
			}}
			re := newResourceExecutor(&ExecutorConfig{
				Config: &configloader.Config{Transports: map[string]configloader.TransportDefinition{
					desireTransportName: {Type: configloader.TransportTypeRemote},
				}},
				TransportRegistry: transportclient.Registry{
					desireTransportName: desireclient.NewClient(
						&failingReadCleanupStore{SpecStore: store}, desireOwner),
					configloader.TransportClientKubernetes: dependentClient,
				},
			})
			prerequisite := newDesireResourceWithLifecycle("deleted_time != null", "Background")
			prerequisite.Name = testResourceAName
			dependent := newResourceWithLifecycle(tt.deleteGate, "Background")
			dependent.Name = testResourceBName
			execCtx := NewExecutionContext(ctx, nil, nil)
			execCtx.Params["deleted_time"] = testDeletedTime

			results, err := re.ExecuteAll(ctx, []configloader.Resource{prerequisite, dependent}, execCtx)

			require.ErrorContains(t, err, "cleanup write failed")
			require.Len(t, results, 2)
			assert.Equal(t, StatusFailed, results[0].Status)
			assert.False(t, dependentClient.DeleteCalled, "failed cleanup must not release a dependent")
			assert.Equal(t, ResourceStateUnsynced, execCtx.ResourceStates[prerequisite.Name])
		})
	}
}

// A dependent listed before its prerequisite evaluates its gate against
// pre-discovery. While the prerequisite's read mirror still shows the object,
// the dependent waits; the prerequisite's own delete path confirms from the
// delete desire and cleans up, so the next event releases the dependent. This
// is the one-pass ordering model: later resources cascade within an event,
// earlier ones on the next.
func TestResourceExecutor_DesireTransport_StaleReadDelaysEarlierDependentOneEvent(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	desiretest.PutSyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner,
		[]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test-cm","namespace":"default"}}`))
	desiretest.PutConfirmedDeleteDesire(t, ctx, store, testDesireID.Delete(), desireOwner)
	dependentClient := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	dependentClient.Resources["default/test-cm"] = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "test-cm", "namespace": "default"},
	}}
	re := newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{Transports: map[string]configloader.TransportDefinition{
			desireTransportName: {Type: configloader.TransportTypeRemote},
		}},
		TransportRegistry: transportclient.Registry{
			desireTransportName:                    desireclient.NewClient(store, desireOwner),
			configloader.TransportClientKubernetes: dependentClient,
		},
	})
	prerequisite := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	prerequisite.Name = testResourceAName
	dependent := newResourceWithLifecycle(
		`resource_states.?resourceA.orValue("") == "confirmed_deleted"`, "Background")
	dependent.Name = testResourceBName
	resources := []configloader.Resource{dependent, prerequisite}

	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err := re.ExecuteAll(ctx, resources, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.False(t, dependentClient.DeleteCalled, "the stale mirror still shows the prerequisite")
	assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[prerequisite.Name])
	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	assert.ErrorIs(t, err, desire.ErrNotFound)
	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound)

	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, resources, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.True(t, dependentClient.DeleteCalled, "no desires remain, so the prerequisite is confirmed absent")
	assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[prerequisite.Name])
}

func TestResourceExecutor_DesireTransport_CleanedPrerequisiteAfterDependent(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	dependentClient := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	dependentClient.Resources["default/test-cm"] = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "test-cm", "namespace": "default"},
	}}
	re := newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{Transports: map[string]configloader.TransportDefinition{
			desireTransportName: {Type: configloader.TransportTypeRemote},
		}},
		TransportRegistry: transportclient.Registry{
			desireTransportName:                    desireclient.NewClient(store, desireOwner),
			configloader.TransportClientKubernetes: dependentClient,
		},
	})
	prerequisite := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	prerequisite.Name = testResourceAName
	dependent := newResourceWithLifecycle(
		`resource_states.?resourceA.orValue("") == "confirmed_deleted"`, "Background")
	dependent.Name = testResourceBName
	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(ctx, []configloader.Resource{dependent, prerequisite}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.True(t, dependentClient.DeleteCalled,
		"already-cleaned prerequisite must be visible to dependent CEL regardless of resource order")
	assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[prerequisite.Name])
	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound)
}

func TestResourceExecutor_DesireTransport_EmptyStoreDeleteIsNoWork(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[resource.Name])
	assert.NotContains(t,
		execCtx.GetCELVariables()[configloader.FieldResources].(map[string]any), resource.Name)
	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "no-work deletion must not create a new DeleteDesire")
	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	assert.ErrorIs(t, err, desire.ErrNotFound)
}

// A target with no desire records reads as confirmed absent, the same as a
// Kubernetes NotFound: confirmed_deleted does not claim the object once existed.
func TestResourceExecutor_DesireTransport_EmptyStoreReadsAsConfirmedAbsent(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	resource.Name = "testResource"
	resource.Lifecycle.Create = &configloader.LifecycleCreate{
		When: &configloader.LifecycleWhen{
			Expression: `resource_states.?testResource.orValue("") == "confirmed_deleted"`,
		},
	}
	execCtx := NewExecutionContext(ctx, nil, nil)

	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	require.NoError(t, err, "the create gate sees the never-created target as absent")
	assert.Equal(t, ResourceStateUnsynced, execCtx.ResourceStates[resource.Name],
		"after the apply, the new read desire has not synced yet")
}

func TestResourceExecutor_DesireTransport_NormalEventKeepsAbsentRead(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	resource.Lifecycle.Create = &configloader.LifecycleCreate{
		When: &configloader.LifecycleWhen{Expression: "false"},
	}
	execCtx := NewExecutionContext(ctx, nil, nil)

	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSkipped, results[0].Status)
	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	require.NoError(t, err, "a normal event must not stop observing an absent resource")
}

func TestResourceExecutor_UnsyncedPrerequisiteDoesNotSatisfyCreateGate(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	desiretest.PutUnsyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
	desireClient := desireclient.NewClient(store, desireOwner)
	dependentClient := k8sclient.NewMockK8sClient()
	re := newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{Transports: map[string]configloader.TransportDefinition{
			desireTransportName: {Type: configloader.TransportTypeRemote},
		}},
		TransportRegistry: transportclient.Registry{
			desireTransportName:                    desireClient,
			configloader.TransportClientKubernetes: dependentClient,
		},
	})

	prerequisite := newDesireResourceWithLifecycle("", "Background")
	prerequisite.Name = "prerequisite"
	dependent := newResourceWithLifecycleCreate(`resource_states.?prerequisite.orValue("") == "present"`)
	dependent.Name = "dependent"

	results, err := re.ExecuteAll(ctx, []configloader.Resource{prerequisite, dependent},
		NewExecutionContext(ctx, nil, nil))

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusSkipped, results[1].Status)
	assert.Equal(t, manifest.OperationSkip, results[1].Operation)
	assert.Empty(t, dependentClient.Resources, "unsynced prerequisites are not discovered resources")
}

func TestResourceExecutor_UnsyncedDeleteErrorIsPropagated(t *testing.T) {
	ctx := t.Context()
	inner := memory.New()
	_, err := inner.CreateApplyDesire(ctx, desire.ApplyDesire{
		Identity: testDesireID.Apply(),
		Owner:    desireOwner,
		Spec:     desire.ApplySpec{KubeContent: configMapContent()},
	})
	require.NoError(t, err)
	desiretest.PutUnsyncedReadDesire(t, ctx, inner, testDesireID.Read(), desireOwner)
	wantErr := errors.New("delete desire write failed")
	store := &failingCreateDeleteDesireStore{SpecStore: inner, err: wantErr}
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

	require.ErrorIs(t, err, wantErr)
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)
	_, err = inner.GetApplyDesire(ctx, testDesireID.Apply())
	assert.NoError(t, err, "failed DeleteDesire creation must leave the ApplyDesire intact")
}

type failingCreateDeleteDesireStore struct {
	desire.SpecStore
	err error
}

func (s *failingCreateDeleteDesireStore) CreateDeleteDesire(
	context.Context, desire.DeleteDesire,
) (desire.DeleteDesire, error) {
	return desire.DeleteDesire{}, s.err
}

// TestResourceExecutor_ExecuteAll_DiscoveryFailure verifies that when discovery fails after a successful apply,
// the error is logged and notified: ExecuteAll returns an error, result is failed,
// and execCtx.Adapter.ExecutionError is set.
func TestResourceExecutor_ExecuteAll_DiscoveryFailure(t *testing.T) {
	discoveryErr := errors.New("discovery failed: resource not found")
	// This resource has no lifecycle.delete, so preDiscoverAll is skipped.
	// The only GetResource call is post-apply discovery, which returns the transient error.
	mock := k8sclient.NewMockK8sClient()
	mock.GetResourceError = discoveryErr
	// Apply succeeds so we reach post-apply discovery.
	mock.ApplyResourceResult = &transportclient.ApplyResult{
		Operation: manifest.OperationCreate,
		Reason:    "mock",
	}

	config := &ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	}
	re := newResourceExecutor(config)

	resource := configloader.Resource{
		Name:      "test-resource",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "default",
			},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: "default",
			ByName:    "test-cm",
		},
	}
	resources := []configloader.Resource{resource}
	execCtx := NewExecutionContext(context.Background(), map[string]interface{}{}, nil)

	results, err := re.ExecuteAll(context.Background(), resources, execCtx)

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status, "result status should be failed")
	require.NotNil(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "discovery failed", "result error should describe discovery failure")
	require.NotNil(t, execCtx.Adapter.ExecutionError, "ExecutionError should be set for notification")
	assert.Equal(t, string(PhaseResources), execCtx.Adapter.ExecutionError.Phase)
	assert.Equal(t, resource.Name, execCtx.Adapter.ExecutionError.Step)
	assert.Contains(t, execCtx.Adapter.ExecutionError.Message, "discovery failed")
}

func TestResourceExecutor_ExecuteAll_StoresNestedDiscoveriesByName(t *testing.T) {
	mock := k8sclient.NewMockK8sClient()
	mock.ApplyResourceResult = &transportclient.ApplyResult{
		Operation: manifest.OperationCreate,
		Reason:    "mock",
	}
	mock.GetResourceResult = &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "work.open-cluster-management.io/v1",
			"kind":       "ManifestWork",
			"metadata": map[string]interface{}{
				"name":      "cluster-1-adapter2",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"workload": map[string]interface{}{
					"manifests": []interface{}{
						map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]interface{}{
								"name":      "cluster-1-adapter2-configmap",
								"namespace": "default",
							},
							"data": map[string]interface{}{
								"cluster_id": "cluster-1",
							},
						},
					},
				},
			},
			"status": map[string]interface{}{
				"resourceStatus": map[string]interface{}{
					"manifests": []interface{}{
						map[string]interface{}{
							"resourceMeta": map[string]interface{}{
								"name":      "cluster-1-adapter2-configmap",
								"namespace": "default",
								"resource":  "configmaps",
								"group":     "",
							},
							"statusFeedback": map[string]interface{}{
								"values": []interface{}{
									map[string]interface{}{
										"name": "data",
										"fieldValue": map[string]interface{}{
											"type":    "JsonRaw",
											"jsonRaw": "{\"cluster_id\":\"cluster-1\"}",
										},
									},
								},
							},
							"conditions": []interface{}{
								map[string]interface{}{
									"type":   "Applied",
									"status": "True",
								},
							},
						},
					},
				},
			},
		},
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := configloader.Resource{
		Name: "resource0",
		Transport: &configloader.TransportConfig{
			Client: "kubernetes",
		},
		Manifest: map[string]interface{}{
			"apiVersion": "work.open-cluster-management.io/v1",
			"kind":       "ManifestWork",
			"metadata": map[string]interface{}{
				"name":      "cluster-1-adapter2",
				"namespace": "default",
			},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: "default",
			ByName:    "cluster-1-adapter2",
		},
		NestedDiscoveries: []configloader.NestedDiscovery{
			{
				Name: "configmap0",
				Discovery: &configloader.DiscoveryConfig{
					Namespace: "default",
					ByName:    "cluster-1-adapter2-configmap",
				},
			},
		},
	}

	execCtx := NewExecutionContext(context.Background(), map[string]interface{}{}, nil)
	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	parent, ok := execCtx.Resources["resource0"].(*unstructured.Unstructured)
	require.True(t, ok, "resource0 should store the discovered parent resource")
	assert.Equal(t, "ManifestWork", parent.GetKind())
	assert.Equal(t, "cluster-1-adapter2", parent.GetName())

	nested, ok := execCtx.Resources["configmap0"].(*unstructured.Unstructured)
	require.True(t, ok, "configmap0 should be stored as top-level nested discovery")
	assert.Equal(t, "ConfigMap", nested.GetKind())
	assert.Equal(t, "cluster-1-adapter2-configmap", nested.GetName())

	// Verify statusFeedback and conditions were enriched from parent's status.resourceStatus
	_, hasSF := nested.Object["statusFeedback"]
	assert.True(t, hasSF, "configmap0 should have statusFeedback merged from parent")
	_, hasConds := nested.Object["conditions"]
	assert.True(t, hasConds, "configmap0 should have conditions merged from parent")

	sf := nested.Object["statusFeedback"].(map[string]interface{})
	values := sf["values"].([]interface{})
	assert.Len(t, values, 1)
	v0 := values[0].(map[string]interface{})
	assert.Equal(t, "data", v0["name"])
}

func runNestedDiscoveryExecuteAll(
	t *testing.T,
	parent *unstructured.Unstructured,
	nestedByName string,
) ([]ResourceResult, *ExecutionContext, error) {
	t.Helper()
	mock := k8sclient.NewMockK8sClient()
	mock.ApplyResourceResult = &transportclient.ApplyResult{
		Operation: manifest.OperationCreate,
		Reason:    "mock",
	}
	mock.GetResourceResult = parent

	name := parent.GetName()
	ns := parent.GetNamespace()
	resource := configloader.Resource{
		Name: "resource0",
		Transport: &configloader.TransportConfig{
			Client: "kubernetes",
		},
		Manifest: map[string]interface{}{
			"apiVersion": parent.GetAPIVersion(),
			"kind":       parent.GetKind(),
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: ns,
			ByName:    name,
		},
		NestedDiscoveries: []configloader.NestedDiscovery{
			{
				Name: "nested0",
				Discovery: &configloader.DiscoveryConfig{
					Namespace: ns,
					ByName:    nestedByName,
				},
			},
		},
	}
	execCtx := NewExecutionContext(context.Background(), map[string]interface{}{}, nil)
	results, err := newResourceExecutor(&ExecutorConfig{TransportRegistry: testTransportRegistry(mock)}).
		ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)
	return results, execCtx, err
}

func TestResourceExecutor_ExecuteAll_NestedDiscoveryConfigError(t *testing.T) {
	parent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "parent",
				"namespace": "default",
			},
		},
	}

	results, execCtx, err := runNestedDiscoveryExecuteAll(t, parent, "{{ .missing }}")

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	_, found := execCtx.Resources["nested0"]
	assert.False(t, found, "failed nested discovery must not be stored")
}

func TestResourceExecutor_ExecuteAll_NestedDiscoveryManifestError(t *testing.T) {
	parent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "work.open-cluster-management.io/v1",
			"kind":       "ManifestWork",
			"metadata": map[string]interface{}{
				"name":      "mw",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"workload": map[string]interface{}{
					"manifests": "not-a-slice",
				},
			},
		},
	}

	results, execCtx, err := runNestedDiscoveryExecuteAll(t, parent, "child")

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	_, found := execCtx.Resources["nested0"]
	assert.False(t, found, "failed nested discovery must not be stored")
}

func TestResourceExecutor_ExecuteAll_NestedDiscoveryNoMatchSucceeds(t *testing.T) {
	parent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "work.open-cluster-management.io/v1",
			"kind":       "ManifestWork",
			"metadata": map[string]interface{}{
				"name":      "mw",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"workload": map[string]interface{}{
					"manifests": []interface{}{},
				},
			},
		},
	}

	results, execCtx, err := runNestedDiscoveryExecuteAll(t, parent, "missing-child")

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	_, found := execCtx.Resources["nested0"]
	assert.False(t, found, "unmatched nested discovery must not be stored")
}

func TestRenderToBytes_StringManifest(t *testing.T) {
	re := newResourceExecutor(&ExecutorConfig{})

	tests := []struct {
		name         string
		manifest     string
		params       map[string]interface{}
		wantContains []string
		wantErr      bool
	}{
		{
			name: "simple string manifest with template values",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  name: "{{ .name }}"
  namespace: "{{ .namespace }}"
data:
  key: value`,
			params: map[string]interface{}{
				"name":      "my-config",
				"namespace": "default",
			},
			wantContains: []string{`"name":"my-config"`, `"namespace":"default"`},
		},
		{
			name: "structural if template",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  name: "test"
{{ if .addLabels }}
  labels:
    app: "myapp"
{{ end }}
data:
  key: value`,
			params: map[string]interface{}{
				"addLabels": true,
			},
			wantContains: []string{`"labels"`, `"app":"myapp"`},
		},
		{
			name: "structural if template - false branch",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  name: "test"
{{ if .addLabels }}
  labels:
    app: "myapp"
{{ end }}
data:
  key: value`,
			params: map[string]interface{}{
				"addLabels": false,
			},
			wantContains: []string{`"name":"test"`, `"key":"value"`},
		},
		{
			name: "range template for list generation",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  name: "test"
data:
{{ range $k, $v := .items }}
  {{ $k }}: "{{ $v }}"
{{ end }}`,
			params: map[string]interface{}{
				"items": map[string]interface{}{
					"key1": "val1",
					"key2": "val2",
				},
			},
			wantContains: []string{`"key1":"val1"`, `"key2":"val2"`},
		},
		{
			name: "if-else template for conditional properties",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  name: "test"
  labels:
{{ if .isGood }}
    status: "good"
{{ else }}
    status: "bad"
{{ end }}`,
			params: map[string]interface{}{
				"isGood": true,
			},
			wantContains: []string{`"status":"good"`},
		},
		{
			name:     "invalid template syntax",
			manifest: `apiVersion: v1{{ if }}`,
			params:   map[string]interface{}{},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource := configloader.Resource{
				Name:     "test",
				Manifest: tt.manifest,
			}
			execCtx := NewExecutionContext(context.Background(), nil, nil)
			execCtx.Params = tt.params

			data, err := re.renderToBytes(resource, execCtx)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			for _, want := range tt.wantContains {
				assert.Contains(t, string(data), want)
			}
		})
	}
}

func TestRenderToBytes_StringManifestWithSubnetList(t *testing.T) {
	// Test the customer's original use case: generating a list of subnets
	re := newResourceExecutor(&ExecutorConfig{})

	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: "subnet-config"
data:
  subnets: |
{{ range .subnetIds }}
    - id: {{ . }}
{{ end }}`

	params := map[string]interface{}{
		"subnetIds": []interface{}{"sub1", "sub2", "sub3"},
	}

	resource := configloader.Resource{
		Name:     "subnets",
		Manifest: manifest,
	}
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params = params

	data, err := re.renderToBytes(resource, execCtx)
	require.NoError(t, err)
	assert.Contains(t, string(data), "sub1")
	assert.Contains(t, string(data), "sub2")
	assert.Contains(t, string(data), "sub3")
}

func TestRenderToBytes_StringManifestEdgeCases(t *testing.T) {
	re := newResourceExecutor(&ExecutorConfig{})

	t.Run("plain YAML string without templates", func(t *testing.T) {
		// Backward compatibility: plain YAML ref files (no templates) still work
		manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: "static-config"
data:
  key: value`
		resource := configloader.Resource{
			Name:     "test",
			Manifest: manifest,
		}
		execCtx := NewExecutionContext(context.Background(), nil, nil)
		execCtx.Params = map[string]interface{}{}

		data, err := re.renderToBytes(resource, execCtx)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"name":"static-config"`)
		assert.Contains(t, string(data), `"key":"value"`)
	})

	t.Run("empty string manifest", func(t *testing.T) {
		resource := configloader.Resource{
			Name:     "test",
			Manifest: "",
		}
		execCtx := NewExecutionContext(context.Background(), nil, nil)
		execCtx.Params = map[string]interface{}{}

		_, err := re.renderToBytes(resource, execCtx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty manifest")
	})

	t.Run("template rendering produces invalid YAML", func(t *testing.T) {
		manifest := `{{ .content }}`
		resource := configloader.Resource{
			Name:     "test",
			Manifest: manifest,
		}
		execCtx := NewExecutionContext(context.Background(), nil, nil)
		execCtx.Params = map[string]interface{}{
			"content": "not: valid: yaml: [broken",
		}

		_, err := re.renderToBytes(resource, execCtx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse rendered manifest as YAML")
	})

	t.Run("missing template variable errors", func(t *testing.T) {
		manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: "{{ .missingVar }}"`
		resource := configloader.Resource{
			Name:     "test",
			Manifest: manifest,
		}
		execCtx := NewExecutionContext(context.Background(), nil, nil)
		execCtx.Params = map[string]interface{}{} // missingVar not provided

		_, err := re.renderToBytes(resource, execCtx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missingVar")
	})

	t.Run("nil manifest", func(t *testing.T) {
		resource := configloader.Resource{
			Name:     "test",
			Manifest: nil,
		}
		execCtx := NewExecutionContext(context.Background(), nil, nil)

		_, err := re.renderToBytes(resource, execCtx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no manifest specified")
	})

	t.Run("map manifest still works (backward compatibility)", func(t *testing.T) {
		resource := configloader.Resource{
			Name: "test",
			Manifest: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "{{ .name }}",
					"namespace": "default",
				},
			},
		}
		execCtx := NewExecutionContext(context.Background(), nil, nil)
		execCtx.Params = map[string]interface{}{
			"name": "rendered-name",
		}

		data, err := re.renderToBytes(resource, execCtx)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"name":"rendered-name"`)
		assert.Contains(t, string(data), `"namespace":"default"`)
	})
}

func TestResourceExecutor_ExecuteAll_StringManifest(t *testing.T) {
	// End-to-end test: string manifest through the full executor flow
	mock := k8sclient.NewMockK8sClient()
	// Don't set ApplyResourceResult — use default behavior which parses and stores the resource
	mock.GetResourceResult = &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-config",
				"namespace": "default",
			},
		},
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	// Use a string manifest with structural Go templates
	manifestStr := `apiVersion: v1
kind: ConfigMap
metadata:
  name: "{{ .configName }}"
  namespace: "{{ .namespace }}"
{{ if .addLabels }}
  labels:
    managed-by: "adapter"
{{ end }}
data:
  cluster: "{{ .clusterId }}"`

	resource := configloader.Resource{
		Name:     "testConfig",
		Manifest: manifestStr,
		Discovery: &configloader.DiscoveryConfig{
			Namespace: "default",
			ByName:    "test-config",
		},
	}

	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params = map[string]interface{}{
		"configName": "test-config",
		"namespace":  "default",
		"addLabels":  true,
		"clusterId":  "cluster-1",
	}

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, "ConfigMap", results[0].Kind)
	assert.Equal(t, "test-config", results[0].ResourceName)

	// Verify the mock stored the rendered resource correctly
	stored, ok := mock.Resources["default/test-config"]
	require.True(t, ok, "Resource should be stored in mock")
	assert.Equal(t, "ConfigMap", stored.GetKind())
	assert.Equal(t, "test-config", stored.GetName())

	// Verify labels were rendered (addLabels=true)
	labels := stored.GetLabels()
	assert.Equal(t, "adapter", labels["managed-by"])

	// Verify data was rendered
	data, found, _ := unstructured.NestedString(stored.Object, "data", "cluster")
	assert.True(t, found)
	assert.Equal(t, "cluster-1", data)
}

func TestResolveGVK_StringManifest(t *testing.T) {
	re := &ResourceExecutor{}

	tests := []struct {
		name        string
		manifest    interface{}
		wantGroup   string
		wantVersion string
		wantKind    string
		wantEmpty   bool
	}{
		{
			name: "map manifest",
			manifest: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			},
			wantVersion: "v1",
			wantKind:    "ConfigMap",
		},
		{
			name:        "string manifest with Go templates",
			manifest:    "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: \"{{ .clusterId }}\"\n",
			wantVersion: "v1",
			wantKind:    "ConfigMap",
		},
		{
			name: "string manifest with structural Go template directives",
			manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
				"  name: \"test-{{ .clusterId }}\"\n  labels:\n    app: test\n" +
				"{{ if .testRunId }}\n    run-id: \"{{ .testRunId }}\"\n{{ end }}\n" +
				"data:\n  key: value\n",
			wantVersion: "v1",
			wantKind:    "ConfigMap",
		},
		{
			name:        "string manifest with apps/v1",
			manifest:    "apiVersion: apps/v1\nkind: Deployment\n",
			wantGroup:   "apps",
			wantVersion: "v1",
			wantKind:    "Deployment",
		},
		{
			name:      "nil manifest",
			manifest:  nil,
			wantEmpty: true,
		},
		{
			name:      "invalid string YAML",
			manifest:  "not: valid: yaml: {{{}",
			wantEmpty: true,
		},
		{
			name:      "string manifest missing kind",
			manifest:  "apiVersion: v1\nmetadata:\n  name: test\n",
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource := configloader.Resource{
				Manifest: tt.manifest,
			}
			gvk := re.resolveGVK(resource)

			if tt.wantEmpty {
				assert.True(t, gvk.Empty(), "expected empty GVK")
			} else {
				assert.Equal(t, tt.wantGroup, gvk.Group)
				assert.Equal(t, tt.wantVersion, gvk.Version)
				assert.Equal(t, tt.wantKind, gvk.Kind)
			}
		})
	}
}

// newResourceWithLifecycle is a helper that builds a Resource with lifecycle.delete config.
func newResourceWithLifecycle(expression, propagationPolicy string) configloader.Resource {
	r := configloader.Resource{
		Name:      "test-resource",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "default",
			},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: "default",
			ByName:    "test-cm",
		},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				PropagationPolicy: propagationPolicy,
			},
		},
	}
	if expression != "" {
		r.Lifecycle.Delete.When = &configloader.LifecycleWhen{Expression: expression}
	}
	return r
}

// newResourceWithLifecycleCreate is a helper that builds a Resource with lifecycle.create config.
func newResourceWithLifecycleCreate(expression string) configloader.Resource {
	r := configloader.Resource{
		Name:      "test-resource",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "default",
			},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: "default",
			ByName:    "test-cm",
		},
		Lifecycle: &configloader.ResourceLifecycle{
			Create: &configloader.LifecycleCreate{
				When: &configloader.LifecycleWhen{Expression: expression},
			},
		},
	}
	return r
}

// sequencedGetResourceMock returns GetResource results/errors in order (one entry per call),
// repeating the last entry once exhausted. Used when pre-discovery and post-apply discovery
// need different GetResource outcomes (e.g. NotFound, then the newly-applied resource).
type sequencedGetResourceMock struct {
	*k8sclient.MockK8sClient
	results     []*unstructured.Unstructured
	errs        []error
	callCount   int
	ApplyCalled bool
}

func (m *sequencedGetResourceMock) GetResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	target transportclient.TransportContext,
) (*unstructured.Unstructured, error) {
	idx := m.callCount
	if idx >= len(m.results) {
		idx = len(m.results) - 1
	}
	m.callCount++
	return m.results[idx], m.errs[idx]
}

func (m *sequencedGetResourceMock) ApplyResource(
	ctx context.Context,
	data []byte,
	opts *transportclient.ApplyOptions,
	target transportclient.TransportContext,
) (*transportclient.ApplyResult, error) {
	m.ApplyCalled = true
	return m.MockK8sClient.ApplyResource(ctx, data, opts, target)
}

func TestResourceExecutor_LifecycleCreate_WhenTrue_ResourceNotFound_Applied(t *testing.T) {
	// Resource doesn't exist yet, create.when evaluates true → applied normally.
	notFoundErr := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "test-cm")
	discovered := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
	}}
	mock := &sequencedGetResourceMock{
		MockK8sClient: k8sclient.NewMockK8sClient(),
		results:       []*unstructured.Unstructured{nil, discovered}, // pre-discovery: absent; post-apply: found
		errs:          []error{notFoundErr, nil},
	}
	mock.ApplyResourceResult = &transportclient.ApplyResult{
		Operation: manifest.OperationCreate,
		Reason:    "mock create",
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycleCreate("shouldCreate")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["shouldCreate"] = true

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationCreate, results[0].Operation)
	assert.False(t, execCtx.Adapter.ResourcesSkipped, "resource was applied, not skipped")
}

func TestResourceExecutor_LifecycleCreate_WhenFalse_ResourceNotFound_Skipped(t *testing.T) {
	// Resource doesn't exist yet, create.when evaluates false → skipped, apply never called.
	mock := &trackingApplyMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	mock.GetResourceError = apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "test-cm")

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycleCreate("shouldCreate")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["shouldCreate"] = false

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSkipped, results[0].Status)
	assert.Equal(t, manifest.OperationSkip, results[0].Operation)
	assert.False(t, mock.ApplyCalled, "ApplyResource should not be called when create.when is false")

	// Verifies the fix: skipping a resource must be reflected in adapter metadata so
	// post-action `when` gates can observe it.
	assert.True(t, execCtx.Adapter.ResourcesSkipped, "adapter.resourcesSkipped must be set on skip")
	assert.NotEmpty(t, execCtx.Adapter.SkipReason)
}

func TestResourceExecutor_LifecycleCreate_WhenCELError_ExecutionFails(t *testing.T) {
	// Resource doesn't exist yet, create.when fails to evaluate → execution fails,
	// resource is neither applied nor silently skipped.
	mock := &trackingApplyMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	mock.GetResourceError = apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "test-cm")

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	// Invalid CEL syntax — evaluateLifecycleWhen will error.
	resource := newResourceWithLifecycleCreate("shouldCreate &&")
	execCtx := NewExecutionContext(context.Background(), nil, nil)

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to evaluate")
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.False(t, mock.ApplyCalled, "ApplyResource should not be called when create.when errors")
	assert.NotNil(t, execCtx.Adapter.ExecutionError)
}

func TestResourceExecutor_LifecycleCreate_ResourceAlreadyExists_IgnoresWhen(t *testing.T) {
	// Resource already exists (pre-discovered) → create.when is ignored, normal apply (update flow).
	discovered := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "default",
			},
		},
	}
	mock := k8sclient.NewMockK8sClient()
	mock.GetResourceResult = discovered
	mock.ApplyResourceResult = &transportclient.ApplyResult{
		Operation: manifest.OperationUpdate,
		Reason:    "mock update",
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycleCreate("shouldCreate")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["shouldCreate"] = false

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationUpdate, results[0].Operation,
		"existing resource must be applied, ignoring create.when=false")
	assert.False(t, execCtx.Adapter.ResourcesSkipped)
}

func TestResourceExecutor_LifecycleCreate_Absent_NormalApply(t *testing.T) {
	// No lifecycle.create configured at all → resource applied normally (regression guard).
	// No lifecycle configured means preDiscoverAll is skipped, so the only GetResource call
	// is post-apply discovery — it must succeed for the apply flow to complete.
	mock := k8sclient.NewMockK8sClient()
	mock.GetResourceResult = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
	}}
	mock.ApplyResourceResult = &transportclient.ApplyResult{
		Operation: manifest.OperationCreate,
		Reason:    "mock create",
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := configloader.Resource{
		Name:      "test-resource",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "default",
			},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: "default",
			ByName:    "test-cm",
		},
	}
	execCtx := NewExecutionContext(context.Background(), nil, nil)

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationCreate, results[0].Operation)
	assert.False(t, execCtx.Adapter.ResourcesSkipped)
}

// trackingApplyMockClient is a thin wrapper around MockK8sClient to capture whether
// ApplyResource was called.
type trackingApplyMockClient struct {
	*k8sclient.MockK8sClient
	ApplyCalled bool
}

func (m *trackingApplyMockClient) ApplyResource(
	ctx context.Context,
	data []byte,
	opts *transportclient.ApplyOptions,
	target transportclient.TransportContext,
) (*transportclient.ApplyResult, error) {
	m.ApplyCalled = true
	return m.MockK8sClient.ApplyResource(ctx, data, opts, target)
}

// trackingMockClient is a thin wrapper around MockK8sClient to also capture DeleteResource calls.
type trackingMockClient struct {
	*k8sclient.MockK8sClient
	DeleteCalledWithPolicy string
	DeleteCalled           bool
}

func (m *trackingMockClient) DeleteResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	opts *transportclient.DeleteOptions,
	target transportclient.TransportContext,
) error {
	m.DeleteCalled = true
	if opts != nil {
		m.DeleteCalledWithPolicy = opts.PropagationPolicy
	}
	return m.MockK8sClient.DeleteResource(ctx, gvk, namespace, name, opts, target)
}

// firstCallResultMock returns (firstResult, firstErr) on the first GetResource call and
// (nil, laterErr) on all subsequent calls. It also tracks whether DeleteResource was invoked.
// Use this when preDiscoverAll and a later discovery step need different GetResource outcomes.
type firstCallResultMock struct {
	*k8sclient.MockK8sClient
	firstResult  *unstructured.Unstructured
	firstErr     error
	laterErr     error
	callCount    int
	DeleteCalled bool
}

func (m *firstCallResultMock) GetResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	target transportclient.TransportContext,
) (*unstructured.Unstructured, error) {
	m.callCount++
	if m.callCount == 1 {
		return m.firstResult, m.firstErr
	}
	return nil, m.laterErr
}

func (m *firstCallResultMock) DeleteResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	opts *transportclient.DeleteOptions,
	target transportclient.TransportContext,
) error {
	m.DeleteCalled = true
	return m.MockK8sClient.DeleteResource(ctx, gvk, namespace, name, opts, target)
}

func TestResourceExecutor_LifecycleDelete_WhenTrue_ResourceFound_InstantDelete(t *testing.T) {
	// when.expression is true + resource exists, post-delete rediscovery returns NotFound
	// (no finalizers — resource is instantly gone after delete API call).
	// Expected: nil stored → dependent resources can cascade in the same reconciliation.
	discovered := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "default",
			},
		},
	}

	mock := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	// Store via Resources map so DeleteResource clears it and post-delete GetResource returns NotFound.
	mock.Resources["default/test-cm"] = discovered

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	assert.True(t, mock.DeleteCalled, "DeleteResource should have been called")
	assert.Equal(t, "Background", mock.DeleteCalledWithPolicy)

	// nil stored: resource is confirmed gone → dependent resources can cascade in this reconciliation.
	storedVal, exists := execCtx.Resources[resource.Name]
	assert.True(t, exists, "nil sentinel should be in execCtx.Resources")
	assert.Nil(t, storedVal, "nil stored when post-delete rediscovery returns NotFound")
}

func TestResourceExecutor_LifecycleDelete_WhenTrue_ResourceFound_WithFinalizers(t *testing.T) {
	// when.expression is true + resource exists, post-delete rediscovery still returns the resource
	// (finalizers are running, or Maestro deletion is async).
	// Expected: discovered object stored (non-nil) → dependent resources wait for next reconciliation.
	discovered := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "default",
			},
		},
	}

	mock := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	// GetResourceResult persists through DeleteResource → simulates finalizers / async deletion.
	mock.GetResourceResult = discovered

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	assert.True(t, mock.DeleteCalled, "DeleteResource should have been called")

	// non-nil stored: resource still present (finalizers) → dependents wait for next reconciliation.
	storedVal, exists := execCtx.Resources[resource.Name]
	assert.True(t, exists, "resource should be in execCtx.Resources")
	assert.NotNil(t, storedVal, "non-nil stored when post-delete rediscovery still finds the resource")
}

func TestResourceExecutor_LifecycleDelete_WhenTrue_ResourceNotFound(t *testing.T) {
	// when.expression is true + resource not found → no-op, nil stored in execCtx
	gr := schema.GroupResource{Group: "", Resource: "configmaps"}
	notFoundErr := apierrors.NewNotFound(gr, "test-cm")

	mock := k8sclient.NewMockK8sClient()
	mock.GetResourceError = notFoundErr

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	assert.Contains(t, results[0].OperationReason, "already deleted")

	// nil stored so dependent resources' ordering conditions evaluate to true
	storedVal, exists := execCtx.Resources[resource.Name]
	assert.True(t, exists, "nil sentinel should be in execCtx.Resources")
	assert.Nil(t, storedVal, "stored value should be nil when resource not found")
}

func TestResourceExecutor_LifecycleDelete_WhenFalse_NormalApply(t *testing.T) {
	// when.expression is false → normal apply path, no delete called
	mock := k8sclient.NewMockK8sClient()
	mock.ApplyResourceResult = &transportclient.ApplyResult{
		Operation: manifest.OperationCreate,
		Reason:    "mock create",
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	// deleted_time is null → expression "deleted_time != null" is false
	resource := newResourceWithLifecycle("deleted_time != null", "Background")

	// Set up GetResource result for post-apply discovery (resource uses ByName discovery → GetResource)
	mock.GetResourceResult = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
	}}

	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = nil // deleted_time is null/missing

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationCreate, results[0].Operation, "should use normal apply path")
}

func TestResourceExecutor_LifecycleDelete_NoLifecycle_NormalApply(t *testing.T) {
	// No lifecycle config → normal apply path (backward compatible)
	mock := k8sclient.NewMockK8sClient()
	mock.ApplyResourceResult = &transportclient.ApplyResult{
		Operation: manifest.OperationCreate,
		Reason:    "mock create",
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := configloader.Resource{
		Name:      "test-resource",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "default",
			},
		},
		// No Lifecycle
	}
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, manifest.OperationCreate, results[0].Operation, "no lifecycle → normal apply")
}

func TestResourceExecutor_LifecycleDelete_NoExpression_DefaultsFalse(t *testing.T) {
	// lifecycle.delete with no when.expression → defaults to false, resource is applied normally
	mock := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("", "Foreground")
	execCtx := NewExecutionContext(context.Background(), nil, nil)

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.False(t, mock.DeleteCalled, "DeleteResource should not be called when expression is absent")
}

func TestResourceExecutor_LifecycleDelete_OrderingViaResources_InstantDelete(t *testing.T) {
	// Two resources with ordered deletion (no finalizers).
	// clusterJob is deleted; post-delete rediscovery returns NotFound (instant delete).
	// clusterConfigMap sees resources.clusterJob == null in the SAME reconciliation → also deletes.
	jobResource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata":   map[string]interface{}{"name": "my-job", "namespace": "default"},
		},
	}
	configMapResource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "my-cm", "namespace": "default"},
		},
	}

	mock := k8sclient.NewMockK8sClient()
	// Resources map: DeleteResource clears entries → post-delete GetResource returns NotFound.
	mock.Resources["default/my-job"] = jobResource
	mock.Resources["default/my-cm"] = configMapResource

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	clusterJob := configloader.Resource{
		Name:      "clusterJob",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata":   map[string]interface{}{"name": "my-job", "namespace": "default"},
		},
		Discovery: &configloader.DiscoveryConfig{Namespace: "default", ByName: "my-job"},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				PropagationPolicy: "Background",
				When:              &configloader.LifecycleWhen{Expression: "deleted_time != null"},
			},
		},
	}

	clusterConfigMap := configloader.Resource{
		Name:      "clusterConfigMap",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "my-cm", "namespace": "default"},
		},
		Discovery: &configloader.DiscoveryConfig{Namespace: "default", ByName: "my-cm"},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				PropagationPolicy: "Background",
				When: &configloader.LifecycleWhen{
					Expression: "deleted_time != null && !resources.?clusterJob.hasValue()",
				},
			},
		},
	}

	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{clusterJob, clusterConfigMap}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)

	// clusterJob: deleted; post-delete NotFound → nil stored.
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	assert.Equal(t, "clusterJob", results[0].Name)
	assert.Nil(t, execCtx.Resources["clusterJob"], "clusterJob nil after instant delete")

	// clusterConfigMap: cascades in the SAME reconciliation because clusterJob is already nil.
	assert.Equal(t, "clusterConfigMap", results[1].Name)
	assert.Equal(t, manifest.OperationDelete, results[1].Operation,
		"clusterConfigMap should cascade in same reconciliation when clusterJob instantly deleted")
}

func TestResourceExecutor_LifecycleDelete_OrderingViaResources_WithFinalizers(t *testing.T) {
	// Two resources with ordered deletion (clusterJob has finalizers / async deletion).
	// clusterJob is deleted; post-delete rediscovery still returns the resource (finalizers running).
	// clusterConfigMap sees resources.clusterJob != null → waits for next reconciliation.
	jobResource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata":   map[string]interface{}{"name": "my-job", "namespace": "default"},
		},
	}

	mock := k8sclient.NewMockK8sClient()
	// GetResourceResult persists through DeleteResource → simulates finalizers / async deletion.
	mock.GetResourceResult = jobResource
	mock.ApplyResourceResult = &transportclient.ApplyResult{Operation: manifest.OperationCreate, Reason: "mock"}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	clusterJob := configloader.Resource{
		Name:      "clusterJob",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata":   map[string]interface{}{"name": "my-job", "namespace": "default"},
		},
		Discovery: &configloader.DiscoveryConfig{Namespace: "default", ByName: "my-job"},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				PropagationPolicy: "Background",
				When:              &configloader.LifecycleWhen{Expression: "deleted_time != null"},
			},
		},
	}

	clusterConfigMap := configloader.Resource{
		Name:      "clusterConfigMap",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "my-cm", "namespace": "default"},
		},
		Discovery: &configloader.DiscoveryConfig{Namespace: "default", ByName: "my-cm"},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				PropagationPolicy: "Background",
				When: &configloader.LifecycleWhen{
					Expression: "deleted_time != null && !resources.?clusterJob.hasValue()",
				},
			},
		},
	}

	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{clusterJob, clusterConfigMap}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)

	// clusterJob: delete sent; post-delete still present (finalizers) → non-nil stored.
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	assert.NotNil(t, execCtx.Resources["clusterJob"], "clusterJob non-nil while finalizers running")

	// clusterConfigMap: condition false (clusterJob still present) → waits for next reconciliation.
	assert.Equal(t, "clusterConfigMap", results[1].Name)
	assert.NotEqual(t, manifest.OperationDelete, results[1].Operation,
		"clusterConfigMap must wait while clusterJob finalizers are running")
}

func TestResourceExecutor_LifecycleDelete_OrderingSecondReconciliation(t *testing.T) {
	// Second reconciliation: clusterJob is now gone (NotFound)
	// clusterConfigMap sees resources.clusterJob == null → deletes
	// Job returns NotFound; ConfigMap returns a found resource
	configMapResource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "my-cm", "namespace": "default"},
		},
	}

	// Use mock with configmap stored but job not present (job is gone → GetResource returns NotFound)
	mock2 := k8sclient.NewMockK8sClient()
	mock2.Resources["default/my-cm"] = configMapResource

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock2),
	})

	clusterJob := configloader.Resource{
		Name:      "clusterJob",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata":   map[string]interface{}{"name": "my-job", "namespace": "default"},
		},
		Discovery: &configloader.DiscoveryConfig{Namespace: "default", ByName: "my-job"},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				When: &configloader.LifecycleWhen{Expression: "deleted_time != null"},
			},
		},
	}
	clusterConfigMap := configloader.Resource{
		Name:      "clusterConfigMap",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "my-cm", "namespace": "default"},
		},
		Discovery: &configloader.DiscoveryConfig{Namespace: "default", ByName: "my-cm"},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				When: &configloader.LifecycleWhen{Expression: "deleted_time != null && !resources.?clusterJob.hasValue()"},
			},
		},
	}

	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{clusterJob, clusterConfigMap}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)

	// clusterJob: not found → nil stored → already deleted
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	assert.Contains(t, results[0].OperationReason, "already deleted")

	// nil stored in context for clusterJob
	assert.Nil(t, execCtx.Resources["clusterJob"])

	// clusterConfigMap: clusterJob is nil in context → when-expression is true → delete
	assert.Equal(t, manifest.OperationDelete, results[1].Operation)
	assert.Equal(t, StatusSuccess, results[1].Status)
}

func TestResourceExecutor_LifecycleDelete_DeleteError(t *testing.T) {
	// Delete call fails → result is failed, ExecutionError is set
	discovered := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
		},
	}
	deleteErr := errors.New("RBAC denied")

	mock := k8sclient.NewMockK8sClient()
	mock.GetResourceResult = discovered
	mock.DeleteResourceError = deleteErr

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	require.NotNil(t, execCtx.Adapter.ExecutionError)
	assert.Equal(t, string(PhaseResources), execCtx.Adapter.ExecutionError.Phase)
	require.NotNil(t, execCtx.Adapter.ResourceErrors, "ResourceErrors map should be populated")
	assert.Contains(t, execCtx.Adapter.ResourceErrors, resource.Name, "resource error should be keyed by resource name")
}

// keepOnDeleteMockClient wraps MockK8sClient but intentionally does NOT remove resources on
// DeleteResource — it only records the TransportContext that was passed. This simulates the
// async Maestro deletion model: the resource stays discoverable after the delete call until
// Maestro finishes cleaning up sub-resources.
type keepOnDeleteMockClient struct {
	*k8sclient.MockK8sClient
	DeleteCalledWithTarget transportclient.TransportContext
}

func (m *keepOnDeleteMockClient) DeleteResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	opts *transportclient.DeleteOptions,
	target transportclient.TransportContext,
) error {
	m.DeleteCalledWithTarget = target
	// Intentionally skip removal: resource remains discoverable (Maestro async behavior).
	return nil
}

// ---- HIGH: CEL expression error handling ----

func TestResourceExecutor_LifecycleDelete_InvalidCELExpression(t *testing.T) {
	// when.expression contains a syntax error that makes the CEL compiler reject it.
	// Expected: evaluator returns error → executor fails with StatusFailed and ExecutionError set.
	// DeleteResource must not be called.
	mock := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	// "deleted_time != null &&" is a dangling logical-AND — invalid CEL syntax.
	resource := newResourceWithLifecycle("deleted_time != null &&", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.Error(t, err, "CEL syntax error must surface as an executor error")
	assert.Contains(t, err.Error(), "failed to evaluate")
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.False(t, mock.DeleteCalled, "DeleteResource must not be called when CEL has a syntax error")
}

func TestResourceExecutor_LifecycleDelete_CELUndeclaredVariable(t *testing.T) {
	// when.expression references a variable that was never captured in the precondition phase.
	// CEL treats undeclared variables as null (DynType), so "not_captured_var != null" evaluates
	// to false without error — the executor falls through to the normal apply path (OperationCreate).
	// This is distinct from a syntax error: the expression is syntactically valid but semantically
	// evaluates to false because the variable resolves to null.
	mock := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	// "not_captured_var" is intentionally absent from execCtx.Params.
	resource := newResourceWithLifecycle("not_captured_var != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	// Note: "not_captured_var" is deliberately not set — simulates a missing precondition capture.

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err, "CEL with undeclared variable evaluates to false (null != null = false), not an error")
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationCreate, results[0].Operation)
	assert.False(t, mock.DeleteCalled, "DeleteResource must not be called when lifecycle.delete.when evaluates to false")
}

// ---- MEDIUM: PropagationPolicy passthrough ----

func TestResourceExecutor_LifecycleDelete_PropagationPolicy(t *testing.T) {
	// Verifies that the configured PropagationPolicy is passed through to DeleteResource,
	// and that an empty policy defaults to "Background".
	tests := []struct {
		name       string
		policy     string
		wantPolicy string
	}{
		{"Foreground passed through", "Foreground", "Foreground"},
		{"Orphan passed through", "Orphan", "Orphan"},
		{"empty policy defaults to Background", "", "Background"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discovered := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
				},
			}
			mock := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
			// Store the resource so pre-delete discovery finds it and delete is attempted.
			mock.Resources["default/test-cm"] = discovered

			re := newResourceExecutor(&ExecutorConfig{
				TransportRegistry: testTransportRegistry(mock),
			})

			resource := newResourceWithLifecycle("deleted_time != null", tt.policy)
			execCtx := NewExecutionContext(context.Background(), nil, nil)
			execCtx.Params["deleted_time"] = testDeletedTime

			results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, manifest.OperationDelete, results[0].Operation)
			assert.True(t, mock.DeleteCalled)
			assert.Equal(t, tt.wantPolicy, mock.DeleteCalledWithPolicy,
				"propagation policy %q should be passed to DeleteResource as %q", tt.policy, tt.wantPolicy)
		})
	}
}

// ---- MEDIUM: Pre-delete discovery transient error ----

func TestResourceExecutor_LifecycleDelete_PreDeleteDiscoveryError(t *testing.T) {
	// Pre-delete discovery (inside executeResourceDelete) returns a non-NotFound error.
	// preDiscoverAll (call 1) sees the resource as existing so the when-expression evaluates
	// to true and the delete path is entered. The discovery inside executeResourceDelete
	// (call 2) then fails with a transient error (e.g. network timeout, RBAC denial).
	// Expected: result is failed, ExecutionError is populated, DeleteResource is not called.
	transientErr := errors.New("connection timeout: context deadline exceeded")
	existingCM := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
		},
	}
	mock := &firstCallResultMock{
		MockK8sClient: k8sclient.NewMockK8sClient(),
		firstResult:   existingCM,
		firstErr:      nil,
		laterErr:      transientErr,
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.Error(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	require.NotNil(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "connection timeout")
	require.NotNil(t, execCtx.Adapter.ExecutionError, "ExecutionError must be set for upstream notification")
	assert.Equal(t, string(PhaseResources), execCtx.Adapter.ExecutionError.Phase)
	require.NotNil(t, execCtx.Adapter.ResourceErrors, "ResourceErrors map should be populated")
	assert.Contains(t, execCtx.Adapter.ResourceErrors, resource.Name, "resource error should be keyed by resource name")
	assert.False(t, mock.DeleteCalled, "DeleteResource must not be called when pre-delete discovery fails")
}

// ---- MEDIUM: lifecycle block present, but Delete is nil ----

func TestResourceExecutor_LifecycleDelete_DeleteConfigNil(t *testing.T) {
	// lifecycle block is present but has no delete sub-key (Lifecycle.Delete == nil).
	// The condition `lifecycle != nil && lifecycle.Delete != nil` is false — falls through to apply.
	// Expected: normal apply, no delete attempted.
	mock := k8sclient.NewMockK8sClient()

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := configloader.Resource{
		Name:      "test-resource",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
		},
		Discovery: &configloader.DiscoveryConfig{Namespace: "default", ByName: "test-cm"},
		Lifecycle: &configloader.ResourceLifecycle{
			// Delete is nil — lifecycle block exists but no delete config
		},
	}
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationCreate, results[0].Operation,
		"lifecycle block with no delete config must fall through to normal apply")
}

// ---- MEDIUM: Maestro async deletion through executor ----

func TestResourceExecutor_LifecycleDelete_Maestro_AsyncDeletion(t *testing.T) {
	// Maestro transport: when.expression is true → delete is sent with a non-nil TransportContext.
	// Maestro deletion is async: the resource stays discoverable (deletionTimestamp set, not removed).
	// Post-delete rediscovery still finds the resource → stored as non-nil → dependents wait.
	discovered := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "work.open-cluster-management.io/v1",
			"kind":       "ManifestWork",
			"metadata":   map[string]interface{}{"name": "cluster-1-work", "namespace": "cluster-1"},
		},
	}

	// keepOnDeleteMockClient does NOT remove from Resources on delete — simulates Maestro async cleanup.
	mock := &keepOnDeleteMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	mock.Resources["cluster-1/cluster-1-work"] = discovered

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: transportclient.Registry{configloader.TransportClientMaestro: mock},
	})

	resource := configloader.Resource{
		Name: "clusterWork",
		Transport: &configloader.TransportConfig{
			Client:  "maestro",
			Maestro: &configloader.MaestroTransportConfig{TargetCluster: "cluster-1"},
		},
		Manifest: map[string]interface{}{
			"apiVersion": "work.open-cluster-management.io/v1",
			"kind":       "ManifestWork",
			"metadata":   map[string]interface{}{"name": "cluster-1-work", "namespace": "cluster-1"},
		},
		Discovery: &configloader.DiscoveryConfig{Namespace: "cluster-1", ByName: "cluster-1-work"},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				When: &configloader.LifecycleWhen{Expression: "deleted_time != null"},
			},
		},
	}

	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)

	// Maestro transport must pass a non-nil TransportContext to DeleteResource.
	assert.NotNil(t, mock.DeleteCalledWithTarget,
		"Maestro transport must supply a non-nil TransportContext to DeleteResource")

	// Resource stays non-nil in context: async cleanup → dependents wait for next reconciliation.
	storedVal, exists := execCtx.Resources["clusterWork"]
	assert.True(t, exists, "resource key must be present in execCtx after Maestro delete")
	assert.NotNil(t, storedVal,
		"non-nil stored: Maestro deletion is async — dependents must wait for next reconciliation")
}

// multiDeleteMock tracks which resource names were passed to DeleteResource and always
// returns a configured error, while GetResource returns a distinct object per name.
type multiDeleteMock struct {
	*k8sclient.MockK8sClient
	objects     map[string]*unstructured.Unstructured
	deleteErr   error
	deleteCalls []string
}

func (m *multiDeleteMock) GetResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	target transportclient.TransportContext,
) (*unstructured.Unstructured, error) {
	if obj, ok := m.objects[name]; ok {
		return obj, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
}

func (m *multiDeleteMock) DeleteResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	opts *transportclient.DeleteOptions,
	target transportclient.TransportContext,
) error {
	m.deleteCalls = append(m.deleteCalls, name)
	return m.deleteErr
}

func TestResourceExecutor_ExecuteAll_ContinuesAfterDeleteFailure(t *testing.T) {
	// JIRA HYPERFLEET-849: "continue with the rest of resources deletion" even when one fails.
	// If resource A's delete fails, resource B must still be attempted.
	makeObj := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
			},
		}
	}

	mock := &multiDeleteMock{
		MockK8sClient: k8sclient.NewMockK8sClient(),
		objects: map[string]*unstructured.Unstructured{
			testResourceAID: makeObj(testResourceAID),
			testResourceBID: makeObj(testResourceBID),
		},
		deleteErr: errors.New("RBAC denied"),
	}
	// ApplyResource is never reached in the delete path, but set a result to be safe.
	mock.ApplyResourceResult = &transportclient.ApplyResult{Operation: manifest.OperationCreate}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resourceA := newResourceWithLifecycle("deleted_time != null", "Background")
	resourceA.Name = testResourceAID
	resourceA.Discovery = &configloader.DiscoveryConfig{ByName: testResourceAID, Namespace: "default"}

	resourceB := newResourceWithLifecycle("deleted_time != null", "Background")
	resourceB.Name = testResourceBID
	resourceB.Discovery = &configloader.DiscoveryConfig{ByName: testResourceBID, Namespace: "default"}

	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resourceA, resourceB}, execCtx)

	// Both resources must have been attempted despite the first failure.
	require.Len(t, mock.deleteCalls, 2, "both resources must be attempted even after first delete fails")
	assert.Equal(t, testResourceAID, mock.deleteCalls[0])
	assert.Equal(t, testResourceBID, mock.deleteCalls[1])

	// ExecuteAll returns the first delete error.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RBAC denied")

	// Both results are present and marked failed.
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusFailed, results[1].Status)
}

func TestResourceExecutor_ExecuteAll_ContinuesAfterCELEvalError(t *testing.T) {
	// JIRA HYPERFLEET-849: a CEL evaluation error on resourceA's lifecycle.delete.when must not
	// skip resourceB. Before the fix, result.Operation was "" (zero value), so the delete-continue
	// branch was never taken and ExecuteAll returned early.
	// Default mock behavior stores applied resources, so post-apply discovery succeeds for resourceB.
	mock := k8sclient.NewMockK8sClient()

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	// resourceA has an invalid CEL expression — evaluateLifecycleDeleteWhen will error.
	resourceA := newResourceWithLifecycle("deleted_time != null &&", "Background")
	resourceA.Name = testResourceAID

	// resourceB has a valid expression that evaluates to false → normal apply.
	resourceB := newResourceWithLifecycle("false", "Background")
	resourceB.Name = testResourceBID

	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resourceA, resourceB}, execCtx)

	// Both resources must appear in results.
	require.Len(t, results, 2, "resourceB must be processed even after resourceA's CEL error")
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation, "CEL error on delete path must set OperationDelete")
	// resourceB: lifecycle.delete.when=false → falls through to apply → success.
	assert.Equal(t, StatusSuccess, results[1].Status)

	// ExecuteAll returns the accumulated CEL error.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to evaluate")
}

func TestResourceExecutor_GetCELVariables_DeletedResourceAbsent(t *testing.T) {
	// Nil (deleted) resources must be absent from the CEL resources map so that
	// "!resources.?clusterJob.hasValue()" correctly evaluates to true when a
	// resource is confirmed deleted. Optional.none().hasValue() == false, which
	// is the intended semantics for lifecycle ordering expressions.
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Resources["clusterJob"] = nil // explicitly stored nil (deleted resource)
	execCtx.Resources["clusterConfigMap"] = &unstructured.Unstructured{
		Object: map[string]interface{}{"kind": "ConfigMap"},
	}

	vars := execCtx.GetCELVariables()
	resources, ok := vars["resources"].(map[string]interface{})
	require.True(t, ok)

	// nil-sentinel resource must NOT be in the CEL resources map;
	// accessing via resources.?clusterJob returns Optional.none() → hasValue() = false
	_, exists := resources["clusterJob"]
	assert.False(t, exists, "nil-sentinel (deleted) resource must be absent from CEL resources map")

	// non-nil resource is present normally
	_, exists = resources["clusterConfigMap"]
	assert.True(t, exists, "non-nil resource should be in CEL resources map")
}

// selectorTrackingMockClient overrides DiscoverResources to return the pre-delete list on the
// first call and an empty list on subsequent calls, simulating instant K8s deletion via
// label-selector discovery.
type selectorTrackingMockClient struct {
	*k8sclient.MockK8sClient
	DeleteCalledWithPolicy string
	discoverCalls          int
	DeleteCalled           bool
}

func (m *selectorTrackingMockClient) DiscoverResources(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	discovery manifest.Discovery,
	target transportclient.TransportContext,
) (*unstructured.UnstructuredList, error) {
	m.discoverCalls++
	// Calls 1 (preDiscoverAll) and 2 (executeResourceDelete pre-delete): resource exists.
	// Call 3+ (post-delete rediscovery): resource gone (instant K8s delete, no finalizers).
	if m.discoverCalls <= 2 {
		return m.MockK8sClient.DiscoverResources(ctx, gvk, discovery, target)
	}
	return &unstructured.UnstructuredList{}, nil
}

func (m *selectorTrackingMockClient) DeleteResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	opts *transportclient.DeleteOptions,
	target transportclient.TransportContext,
) error {
	m.DeleteCalled = true
	if opts != nil {
		m.DeleteCalledWithPolicy = opts.PropagationPolicy
	}
	return m.MockK8sClient.DeleteResource(ctx, gvk, namespace, name, opts, target)
}

// TestResourceExecutor_LifecycleDelete_BySelectors verifies that lifecycle delete works with
// label-selector discovery (JIRA HYPERFLEET-849 AC: "Label selector discovery works for
// multi-generation resources"). DiscoverResources is used instead of GetResource, and the
// resource returned by GetLatestGenerationFromList is the one that gets deleted.
func TestResourceExecutor_LifecycleDelete_BySelectors(t *testing.T) {
	discovered := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "my-cm",
				"namespace": "default",
			},
		},
	}

	inner := k8sclient.NewMockK8sClient()
	inner.DiscoverResult = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*discovered}}

	mock := &selectorTrackingMockClient{MockK8sClient: inner}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := configloader.Resource{
		Name:      "test-resource",
		Transport: &configloader.TransportConfig{Client: "kubernetes"},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "my-cm", "namespace": "default"},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: "default",
			BySelectors: &configloader.SelectorConfig{
				LabelSelector: map[string]string{"app": "test"},
			},
		},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				When: &configloader.LifecycleWhen{Expression: "deleted_time != null"},
			},
		},
	}
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, manifest.OperationDelete, results[0].Operation)
	assert.True(t, mock.DeleteCalled, "DeleteResource should have been called")
	assert.GreaterOrEqual(t, mock.discoverCalls, 2, "DiscoverResources called for pre- and post-delete")

	// Resource confirmed gone (post-delete discovery returned empty) → nil stored.
	storedVal, exists := execCtx.Resources[resource.Name]
	assert.True(t, exists, "nil sentinel should be in execCtx.Resources")
	assert.Nil(t, storedVal, "nil stored when post-delete discovery finds no resources")
}

// ---- DesireCleaner integration ----

func TestResourceExecutor_LifecycleDelete_Step2_CleanupCalled(t *testing.T) {
	inner := k8sclient.NewMockK8sClient()
	mock := &cleanupTrackingDeleteMockClient{
		MockK8sClient: inner,
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.False(t, mock.DeleteCalled, "DeleteResource must not be called when resource was already gone")
	assert.True(t, mock.CleanupCalled, "CleanupAfterDeletion must be called when resource is not found")
	assert.Equal(t, "default", mock.CleanupNamespace)
	assert.Equal(t, "test-cm", mock.CleanupName)
}

func TestResourceExecutor_LifecycleDelete_Step6_CleanupCalled(t *testing.T) {
	discovered := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
		},
	}

	inner := k8sclient.NewMockK8sClient()
	// Resource exists initially, DeleteResource removes it from Resources map,
	// so post-delete GetResource returns NotFound.
	inner.Resources["default/test-cm"] = discovered
	mock := &cleanupTrackingDeleteMockClient{
		MockK8sClient: inner,
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.True(t, mock.DeleteCalled, "DeleteResource must be called")
	assert.True(t, mock.CleanupCalled, "CleanupAfterDeletion must be called when post-delete discovery confirms gone")
	assert.Equal(t, "default", mock.CleanupNamespace)
	assert.Equal(t, "test-cm", mock.CleanupName)
}

// cleanupTrackingDeleteMockClient delegates to MockK8sClient (which removes resources on delete)
// and implements DesireCleaner to track cleanup calls.
type cleanupTrackingDeleteMockClient struct {
	*k8sclient.MockK8sClient
	CleanupError     error
	CleanupErrorFor  string
	CleanupNamespace string
	CleanupName      string
	DeleteCalled     bool
	CleanupCalled    bool
}

func (m *cleanupTrackingDeleteMockClient) DeleteResource(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	opts *transportclient.DeleteOptions,
	target transportclient.TransportContext,
) error {
	m.DeleteCalled = true
	return m.MockK8sClient.DeleteResource(ctx, gvk, namespace, name, opts, target)
}

func (m *cleanupTrackingDeleteMockClient) CleanupAfterDeletion(
	_ context.Context,
	_ schema.GroupVersionKind,
	namespace, name string,
	_ transportclient.TransportContext,
) error {
	m.CleanupCalled = true
	m.CleanupNamespace = namespace
	m.CleanupName = name
	if m.CleanupErrorFor == "" || m.CleanupErrorFor == name {
		return m.CleanupError
	}
	return nil
}

func TestResourceExecutor_LifecycleDelete_CleanupError_StatusFailed(t *testing.T) {
	inner := k8sclient.NewMockK8sClient()
	mock := &cleanupTrackingDeleteMockClient{
		MockK8sClient: inner,
		CleanupError:  errors.New("cleanup: deletion not yet confirmed"),
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.ErrorContains(t, err, "desire cleanup failed")
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.True(t, mock.CleanupCalled)
}

func TestResourceExecutor_CleanupFailureDoesNotBlockOtherDeletes(t *testing.T) {
	inner := k8sclient.NewMockK8sClient()
	inner.Resources[testResourceBPath] = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": testResourceBID, "namespace": "default"},
	}}
	mock := &cleanupTrackingDeleteMockClient{
		MockK8sClient:   inner,
		CleanupError:    errors.New("cleanup store unavailable"),
		CleanupErrorFor: testResourceAID,
	}
	re := newResourceExecutor(&ExecutorConfig{TransportRegistry: testTransportRegistry(mock)})

	resourceA := newResourceWithLifecycle("deleted_time != null", "Background")
	resourceA.Name = testResourceAID
	resourceA.Manifest.(map[string]interface{})["metadata"].(map[string]interface{})["name"] = testResourceAID
	resourceA.Discovery = &configloader.DiscoveryConfig{Namespace: "default", ByName: testResourceAID}
	resourceB := newResourceWithLifecycle("deleted_time != null", "Background")
	resourceB.Name = testResourceBID
	resourceB.Manifest.(map[string]interface{})["metadata"].(map[string]interface{})["name"] = testResourceBID
	resourceB.Discovery = &configloader.DiscoveryConfig{Namespace: "default", ByName: testResourceBID}
	execCtx := NewExecutionContext(t.Context(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(t.Context(), []configloader.Resource{resourceA, resourceB}, execCtx)

	require.ErrorContains(t, err, "cleanup store unavailable")
	require.Len(t, results, 2)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, StatusSuccess, results[1].Status)
	assert.NotContains(t, inner.Resources, testResourceBPath,
		"an unrelated deletion should proceed despite the cleanup failure")
}

func TestResourceExecutor_NormalCreateSkipDoesNotRunDeletionCleanup(t *testing.T) {
	inner := k8sclient.NewMockK8sClient()
	inner.Resources[testResourceBPath] = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": testResourceBID, "namespace": "default"},
	}}
	mock := &cleanupTrackingDeleteMockClient{
		MockK8sClient: inner, CleanupError: errors.New("cleanup store unavailable"), CleanupErrorFor: testResourceAID,
	}
	re := newResourceExecutor(&ExecutorConfig{TransportRegistry: testTransportRegistry(mock)})

	resourceA := newResourceWithLifecycle("", "Background")
	resourceA.Name = testResourceAID
	resourceA.Discovery.ByName = testResourceAID
	resourceA.Lifecycle.Create = &configloader.LifecycleCreate{
		When: &configloader.LifecycleWhen{Expression: "false"},
	}
	resourceB := newResourceWithLifecycle("deleted_time != null", "Background")
	resourceB.Name = testResourceBID
	resourceB.Manifest.(map[string]interface{})["metadata"].(map[string]interface{})["name"] = testResourceBID
	resourceB.Discovery.ByName = testResourceBID
	execCtx := NewExecutionContext(t.Context(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(t.Context(), []configloader.Resource{resourceA, resourceB}, execCtx)

	require.NoError(t, err, "a resource that is not deleting must not run desire cleanup")
	require.Len(t, results, 2)
	assert.Equal(t, StatusSkipped, results[0].Status)
	assert.Equal(t, StatusSuccess, results[1].Status)
	assert.True(t, mock.DeleteCalled)
}

// cleanupKeepOnDeleteMockClient keeps the resource after delete (simulating finalizers/async)
// and implements DesireCleaner to track whether cleanup was attempted.
type cleanupKeepOnDeleteMockClient struct {
	*keepOnDeleteMockClient
	CleanupCalled bool
}

func (m *cleanupKeepOnDeleteMockClient) CleanupAfterDeletion(
	_ context.Context,
	_ schema.GroupVersionKind,
	_, _ string,
	_ transportclient.TransportContext,
) error {
	m.CleanupCalled = true
	return nil
}

func TestResourceExecutor_LifecycleDelete_StillPresent_NoCleanup(t *testing.T) {
	discovered := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
		},
	}

	inner := k8sclient.NewMockK8sClient()
	inner.Resources["default/test-cm"] = discovered
	mock := &cleanupKeepOnDeleteMockClient{
		keepOnDeleteMockClient: &keepOnDeleteMockClient{MockK8sClient: inner},
	}

	re := newResourceExecutor(&ExecutorConfig{
		TransportRegistry: testTransportRegistry(mock),
	})

	resource := newResourceWithLifecycle("deleted_time != null", "Background")
	execCtx := NewExecutionContext(context.Background(), nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := re.ExecuteAll(context.Background(), []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.False(t, mock.CleanupCalled, "CleanupAfterDeletion must not be called when resource is still present")
}

// ---- Desire transport lifecycle tests ----

const (
	desireTransportName = "desire-primary"
	desireOwner         = "hyperfleet-adapter"
)

var testDesireID = desiretest.TestIdentity{
	ManagementCluster: "cluster-1",
	Resource:          "configmaps",
	Namespace:         "default",
	Name:              "test-config",
}

func configMapContent() []byte {
	return []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "test-config",
			"namespace": "default",
			"annotations": {"hyperfleet.io/generation": "1"}
		},
		"data": {"key": "value"}
	}`)
}

func newDesireExecutor(store desire.SpecStore) *ResourceExecutor {
	c := desireclient.NewClient(store, desireOwner)
	return newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{
			Transports: map[string]configloader.TransportDefinition{
				desireTransportName: {Type: configloader.TransportTypeRemote},
			},
		},
		TransportRegistry: transportclient.Registry{
			desireTransportName: c,
		},
	})
}

func newDesireResourceWithLifecycle(expression, propagationPolicy string) configloader.Resource {
	r := configloader.Resource{
		Name: "test-resource",
		Transport: &configloader.TransportConfig{
			Client: desireTransportName,
			Desire: &configloader.DesireTransportConfig{
				TargetCluster: testDesireID.ManagementCluster,
				Resource:      testDesireID.Resource,
			},
		},
		Manifest: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      testDesireID.Name,
				"namespace": testDesireID.Namespace,
				"annotations": map[string]interface{}{
					"hyperfleet.io/generation": "1",
				},
			},
			"data": map[string]interface{}{"key": "value"},
		},
		Discovery: &configloader.DiscoveryConfig{
			Namespace: testDesireID.Namespace,
			ByName:    testDesireID.Name,
		},
		Lifecycle: &configloader.ResourceLifecycle{
			Delete: &configloader.LifecycleDelete{
				PropagationPolicy: propagationPolicy,
			},
		},
	}
	if expression != "" {
		r.Lifecycle.Delete.When = &configloader.LifecycleWhen{Expression: expression}
	}
	return r
}

// Test 1: Slow applier — three events to complete the full apply → delete → cleanup cycle.
func TestResourceExecutor_DesireTransport_SlowApplier(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")

	desiretest.PutUnsyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)

	// ---- Event 1: apply (deleted_time absent → delete.when false) ----
	execCtx := NewExecutionContext(ctx, nil, nil)
	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	require.NoError(t, err, "ApplyDesire must exist after apply")
	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	require.NoError(t, err, "ReadDesire must exist after apply")

	desiretest.MarkReadDesireSynced(t, ctx, store, testDesireID.Read(), configMapContent())

	// ---- Event 2: delete (applier hasn't confirmed yet) ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	assert.ErrorIs(t, err, desire.ErrNotFound, "ApplyDesire must be removed by DeleteResource")

	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.NoError(t, err, "DeleteDesire must exist (pending)")

	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	assert.NoError(t, err, "ReadDesire must still exist")

	desiretest.MarkDeleteDesireConfirmed(t, ctx, store, testDesireID.Delete())

	// ---- Event 3: delete (DeleteDesire confirms while ReadDesire stays stale) ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, "resource deletion confirmed by transport", results[0].OperationReason)

	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "DeleteDesire must be removed by cleanup")

	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	assert.ErrorIs(t, err, desire.ErrNotFound, "cleanup must remove the stale ReadDesire")

	// A later delete event with no remaining desires must not recreate work.
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[resource.Name])
	assert.NotContains(t, execCtx.GetCELVariables()[configloader.FieldResources], resource.Name)
	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "the no-work path must not recreate a delete desire")
}

// Test 2: Fast applier — applier confirms between DeleteResource and
// post-delete discovery within the same event.
func TestResourceExecutor_DesireTransport_FastApplier(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	store := &desiretest.InstantApplierStore{Store: inner}
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")

	desiretest.PutUnsyncedReadDesire(t, ctx, inner, testDesireID.Read(), desireOwner)

	// ---- Event 1: apply ----
	execCtx := NewExecutionContext(ctx, nil, nil)
	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	desiretest.MarkReadDesireSynced(t, ctx, inner, testDesireID.Read(), configMapContent())

	// ---- Event 2: delete (applier confirms instantly via wrapper) ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	_, err = inner.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "DeleteDesire must be removed in single cycle")

	_, err = inner.GetReadDesire(ctx, testDesireID.Read())
	assert.ErrorIs(t, err, desire.ErrNotFound, "confirmed cleanup must remove the ReadDesire")
}

// Test 3: Transient NotFound before apply lands — regression test.
//
// The applier's read informer ran before the apply pass and wrote
// Reason=NotFound on the ReadDesire. The ApplyDesire is still live.
// Cleanup must refuse because the apply desire exists, preventing
// an orphaned resource on the target cluster.
func TestResourceExecutor_DesireTransport_TransientNotFound(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")

	desiretest.PutUnsyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)

	// ---- Event 1: apply ----
	execCtx := NewExecutionContext(ctx, nil, nil)
	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	// Applier's read informer ran before the apply pass: ReadDesire
	// stays NotFound. ApplyDesire exists but hasn't been applied yet.

	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	require.NoError(t, err, "ApplyDesire must exist after apply")

	// ---- Event 2: delete.when true ----
	// Discovery reads mirror → NotFound → step 2 → tryCleanupDesires.
	// CleanupAfterDeletion sees no DeleteDesire but ApplyDesire exists → safe wait.
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, "desire deletion pending", results[0].OperationReason)
	assert.Equal(t, ResourceStateUnsynced, execCtx.ResourceStates[resource.Name])

	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	assert.ErrorIs(t, err, desire.ErrNotFound,
		"DeleteResource must atomically remove the active ApplyDesire")

	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.NoError(t, err, "DeleteResource must leave a DeleteDesire for the applier")

	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	assert.NoError(t, err, "ReadDesire must survive — no orphan")
}

// Test 4: Full lifecycle from an empty store — covers fresh-cluster apply
// and post-cleanup re-apply without seeding any read desire.
func TestResourceExecutor_DesireTransport_FullLifecycleFromEmptyStore(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")

	// ---- Event 1: apply on empty store (no read desire exists) ----
	execCtx := NewExecutionContext(ctx, nil, nil)
	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	require.NoError(t, err, "ApplyDesire must exist")
	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	require.NoError(t, err, "ReadDesire must be auto-created by ensureReadDesire")

	// Simulate applier syncing the read mirror.
	desiretest.MarkReadDesireSynced(t, ctx, store, testDesireID.Read(), configMapContent())

	// ---- Event 2: apply after sync (post-apply discovery finds the resource) ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.NotNil(t, execCtx.Resources["test-resource"], "synced resource must be stored in context")

	// ---- Event 3: delete (applier hasn't confirmed yet) ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	assert.ErrorIs(t, err, desire.ErrNotFound, "ApplyDesire must be removed")
	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.NoError(t, err, "DeleteDesire must exist (pending)")

	// Simulate applier confirming deletion.
	desiretest.MarkDeleteDesireConfirmed(t, ctx, store, testDesireID.Delete())

	// ---- Event 4: delete (DeleteDesire confirms while ReadDesire stays stale) ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, "resource deletion confirmed by transport", results[0].OperationReason)

	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "DeleteDesire must be removed by cleanup")
	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	assert.ErrorIs(t, err, desire.ErrNotFound, "confirmed cleanup must remove the ReadDesire")

	// ---- Event 5: apply again after both desires were removed ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	require.NoError(t, err, "ApplyDesire must exist after re-apply")
	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	require.NoError(t, err, "the re-apply must recreate its paired ReadDesire")
}

func TestResourceExecutor_PendingDesireDeleteDoesNotCountAsDeleted(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	desiretest.PutSyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner, configMapContent())
	_, err := store.CreateDeleteDesire(ctx, desire.DeleteDesire{
		Identity: testDesireID.Delete(), Owner: desireOwner,
	})
	require.NoError(t, err)

	registry := prometheus.NewRegistry()
	re := newDesireExecutor(store)
	re.metrics = metrics.NewRecorder("test-adapter", "v0.1.0", "test", registry)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	countedDeletions := func() float64 {
		families, gatherErr := registry.Gather()
		require.NoError(t, gatherErr)
		for _, family := range families {
			if family.GetName() != "hyperfleet_adapter_resources_deleted_total" {
				continue
			}
			var count float64
			for _, metric := range family.GetMetric() {
				count += metric.GetCounter().GetValue()
			}
			return count
		}
		return 0
	}

	for _, wantCount := range []float64{0, 1} {
		execCtx := NewExecutionContext(ctx, nil, nil)
		execCtx.Params["deleted_time"] = testDeletedTime
		results, executeErr := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
		require.NoError(t, executeErr)
		require.Len(t, results, 1)
		assert.Equal(t, StatusSuccess, results[0].Status)
		assert.Equal(t, wantCount, countedDeletions())
		if wantCount == 0 {
			assert.Equal(t, desireDeletionPendingReason, results[0].OperationReason)
			desiretest.MarkDeleteDesireConfirmed(t, ctx, store, testDesireID.Delete())
		}
	}
}

// Test 5: Step 2 ErrDeletionPending — delete desire exists but applier
// hasn't confirmed yet. The read desire shows NotFound (applier's
// informer observed the resource gone) but the delete desire is still
// pending. Cleanup must refuse with ErrDeletionPending, keeping both
// desires alive so the next reconciliation retries.
func TestResourceExecutor_DesireTransport_Step2_DeletionPending_DeleteNotConfirmed(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")

	desiretest.PutUnsyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)

	// ---- Event 1: apply ----
	execCtx := NewExecutionContext(ctx, nil, nil)
	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	desiretest.MarkReadDesireSynced(t, ctx, store, testDesireID.Read(), configMapContent())

	// ---- Event 2: delete (creates delete desire, applier hasn't confirmed) ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	require.NoError(t, err, "DeleteDesire must exist (pending)")

	// Applier's read informer saw the resource gone but the delete
	// desire hasn't been confirmed yet.
	desiretest.MarkReadDesireNotFound(t, ctx, store, testDesireID.Read())

	// ---- Event 3: delete (discovery → NotFound → Step 2 → cleanup → ErrDeletionPending) ----
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err, "pending deletion must not fail reconciliation")
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.Equal(t, "desire deletion pending", results[0].OperationReason)
	assert.Equal(t, ResourceStateUnsynced, execCtx.ResourceStates[resource.Name])
	assert.Nil(t, execCtx.Adapter.ExecutionError)
	resources := execCtx.GetCELVariables()[configloader.FieldResources].(map[string]interface{})
	assert.Equal(t, map[string]interface{}{}, resources[resource.Name],
		"pending cleanup must remain visible as an unsynced placeholder")

	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.NoError(t, err, "DeleteDesire must survive — not yet confirmed")

	_, err = store.GetReadDesire(ctx, testDesireID.Read())
	assert.NoError(t, err, "ReadDesire must survive")
}

// Test 6: Step 6 ErrDeletionPending — resource existed, was deleted,
// post-delete discovery confirms gone, but the delete desire is still
// pending. Cleanup returns ErrDeletionPending which is non-fatal at
// Step 6: the executor restores the pre-delete discovered state in
// context (dependents wait) and still reports success.
func TestResourceExecutor_DesireTransport_Step6_DeletionPending_NonFatal(t *testing.T) {
	ctx := context.Background()
	inner := memory.New()
	store := &desiretest.PendingDeleteApplierStore{Store: inner}
	re := newDesireExecutor(store)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")

	desiretest.PutUnsyncedReadDesire(t, ctx, inner, testDesireID.Read(), desireOwner)

	// ---- Event 1: apply ----
	execCtx := NewExecutionContext(ctx, nil, nil)
	results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)

	desiretest.MarkReadDesireSynced(t, ctx, inner, testDesireID.Read(), configMapContent())

	// ---- Event 2: delete ----
	// Discovery finds the resource (synced). DeleteResource creates a
	// delete desire. The pendingDeleteApplierStore wrapper marks the
	// read desire as NotFound (applier saw it gone) but does NOT confirm
	// the delete desire. Post-delete discovery → NotFound → Step 6 →
	// tryCleanupDesires → delete desire not confirmed → ErrDeletionPending.
	// Step 6 treats ErrDeletionPending as non-fatal.
	execCtx = NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err = re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)
	require.NoError(t, err, "ErrDeletionPending at Step 6 must be non-fatal")
	require.Len(t, results, 1)
	assert.Equal(t, StatusSuccess, results[0].Status)
	assert.NotNil(t, execCtx.Resources["test-resource"],
		"pre-delete discovered state must be restored so dependents wait")

	_, err = inner.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.NoError(t, err, "DeleteDesire must still exist (pending, not confirmed)")

	_, err = inner.GetApplyDesire(ctx, testDesireID.Apply())
	assert.ErrorIs(t, err, desire.ErrNotFound, "ApplyDesire must be removed by DeleteResource")
}

func TestResourceExecutor_DesireTransport_RecordsResourceStates(t *testing.T) {
	tests := []struct {
		name           string
		seed           string
		wantState      ResourceState
		wantAvailable  int64
		wantObject     bool
		checkAvailable bool
	}{
		{
			name:           "present full object",
			seed:           "synced-full",
			wantState:      ResourceStatePresent,
			wantAvailable:  3,
			wantObject:     true,
			checkAvailable: true,
		},
		{
			name:       "present empty object",
			seed:       "synced-empty",
			wantState:  ResourceStatePresent,
			wantObject: true,
		},
		{
			name:      "unsynced mirror",
			seed:      "unsynced",
			wantState: ResourceStateUnsynced,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			store := memory.New()
			switch tt.seed {
			case "synced-full":
				desiretest.PutSyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner, []byte(`{
					"apiVersion":"v1","kind":"ConfigMap",
					"metadata":{"name":"test-config","namespace":"default"},
					"status":{"availableReplicas":3,"conditions":[{"type":"Available","status":"True"}]}
				}`))
			case "synced-empty":
				desiretest.PutSyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner, []byte(`{}`))
			case "unsynced":
				desiretest.PutUnsyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
			default:
				t.Fatalf("unknown seed %q", tt.seed)
			}
			re := newDesireExecutor(store)
			resource := newDesireResourceWithLifecycle("", "Background")
			execCtx := NewExecutionContext(ctx, nil, nil)

			results, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tt.wantState, execCtx.ResourceStates[resource.Name])
			if tt.wantObject {
				object, ok := execCtx.Resources[resource.Name].(*unstructured.Unstructured)
				require.True(t, ok)
				if tt.checkAvailable {
					status, ok := object.Object["status"].(map[string]interface{})
					require.True(t, ok)
					assert.Equal(t, tt.wantAvailable, status["availableReplicas"])
				} else {
					assert.Empty(t, object.Object)
				}
			} else {
				assert.NotContains(t, execCtx.Resources, resource.Name)
			}
		})
	}

	t.Run("confirmed deletion by name", func(t *testing.T) {
		ctx := t.Context()
		store := memory.New()
		desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
		re := newDesireExecutor(store)
		resource := newDesireResourceWithLifecycle("", "Background")
		execCtx := NewExecutionContext(ctx, nil, nil)
		client, target, err := re.resolveTransport(resource, execCtx)
		require.NoError(t, err)

		discovered, err := re.discoverResource(ctx, resource, execCtx, client, target)

		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
		assert.Nil(t, discovered)
		assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[resource.Name])
	})

	t.Run("selector no-match confirms absence", func(t *testing.T) {
		ctx := t.Context()
		store := memory.New()
		desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
		re := newDesireExecutor(store)
		resource := newDesireResourceWithLifecycle("", "Background")
		resource.Discovery = &configloader.DiscoveryConfig{
			Namespace: "default",
			BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{
				"app": "missing",
			}},
		}
		execCtx := NewExecutionContext(ctx, nil, nil)
		client, target, err := re.resolveTransport(resource, execCtx)
		require.NoError(t, err)

		discovered, err := re.discoverResource(ctx, resource, execCtx, client, target)

		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
		assert.Nil(t, discovered)
		assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[resource.Name])
	})

	t.Run("selector no-match with an unsynced read remains uncertain", func(t *testing.T) {
		ctx := t.Context()
		store := memory.New()
		desiretest.PutUnsyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
		re := newDesireExecutor(store)
		resource := newDesireResourceWithLifecycle("", "Background")
		resource.Discovery = &configloader.DiscoveryConfig{
			Namespace: "default",
			BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{
				"app": "missing",
			}},
		}
		execCtx := NewExecutionContext(ctx, nil, nil)
		client, target, err := re.resolveTransport(resource, execCtx)
		require.NoError(t, err)

		discovered, err := re.discoverResource(ctx, resource, execCtx, client, target)

		require.ErrorIs(t, err, desireclient.ErrNotSyncedYet)
		assert.Nil(t, discovered)
		assert.Equal(t, ResourceStateUnsynced, execCtx.ResourceStates[resource.Name])
	})

	t.Run("decode failure remains an operational error", func(t *testing.T) {
		ctx := t.Context()
		store := memory.New()
		desiretest.PutKubeAPIErrorReadDesire(t, ctx, store, testDesireID.Read(), desireOwner, []byte("not-json"))
		re := newDesireExecutor(store)
		resource := newDesireResourceWithLifecycle("", "Background")
		execCtx := NewExecutionContext(ctx, nil, nil)

		_, err := re.ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

		require.Error(t, err)
		assert.Empty(t, execCtx.ResourceStates)
	})
}

func TestResourceExecutor_DesireTransport_UnsyncedDependencyBlocksDelete(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{
		Identity: testDesireID.Apply(),
		Owner:    desireOwner,
		Spec:     desire.ApplySpec{KubeContent: configMapContent()},
	})
	require.NoError(t, err)

	desireClient := desireclient.NewClient(store, desireOwner)
	dependentClient := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	dependentClient.Resources["default/test-cm"] = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
	}}

	re := newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{
			Transports: map[string]configloader.TransportDefinition{
				desireTransportName: {Type: configloader.TransportTypeRemote},
			},
		},
		TransportRegistry: transportclient.Registry{
			desireTransportName:                    desireClient,
			configloader.TransportClientKubernetes: dependentClient,
		},
	})

	unsynced := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	unsynced.Name = testResourceAName
	dependent := newResourceWithLifecycle(
		fmt.Sprintf(`resource_states.?%s.orValue("") == "confirmed_deleted"`, testResourceAName), "Background")
	dependent.Name = testResourceBName

	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err := re.ExecuteAll(ctx, []configloader.Resource{unsynced, dependent}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, manifest.OperationCreate, results[1].Operation)
	assert.False(t, dependentClient.DeleteCalled,
		"dependent deletion must wait while the prerequisite desire read is unsynced")
	assert.Equal(t, ResourceStateUnsynced, execCtx.ResourceStates[unsynced.Name])
}

func TestResourceExecutor_DesireTransport_PendingCleanupBlocksDeleteDependency(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{
		Identity: testDesireID.Apply(),
		Owner:    desireOwner,
		Spec:     desire.ApplySpec{KubeContent: configMapContent()},
	})
	require.NoError(t, err)
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)

	desireClient := desireclient.NewClient(store, desireOwner)
	dependentClient := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	dependentClient.Resources["default/test-cm"] = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "test-cm", "namespace": "default"},
	}}

	re := newResourceExecutor(&ExecutorConfig{
		Config: &configloader.Config{
			Transports: map[string]configloader.TransportDefinition{
				desireTransportName: {Type: configloader.TransportTypeRemote},
			},
		},
		TransportRegistry: transportclient.Registry{
			desireTransportName:                    desireClient,
			configloader.TransportClientKubernetes: dependentClient,
		},
	})

	unsynced := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	unsynced.Name = testResourceAName
	dependent := newResourceWithLifecycle(
		fmt.Sprintf(`resource_states.?%s.orValue("") == "confirmed_deleted"`, testResourceAName), "Background")
	dependent.Name = testResourceBName

	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime
	results, err := re.ExecuteAll(ctx, []configloader.Resource{unsynced, dependent}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, manifest.OperationCreate, results[1].Operation)
	assert.False(t, dependentClient.DeleteCalled,
		"dependent deletion must wait while desire cleanup is pending")
	assert.Equal(t, ResourceStateUnsynced, execCtx.ResourceStates[unsynced.Name])
}

func TestResourceExecutor_DesireSelectorDeleteRejectedWithPendingApply(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	content := []byte(`{
		"apiVersion":"v1","kind":"ConfigMap",
		"metadata":{"name":"test-config","namespace":"default","labels":{"app":"prerequisite"}}
	}`)
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{
		Identity: testDesireID.Apply(), Owner: desireOwner,
		Spec: desire.ApplySpec{KubeContent: content},
	})
	require.NoError(t, err)
	desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, testDesireID.Read(), desireOwner)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	resource.Manifest.(map[string]any)["metadata"].(map[string]any)["labels"] = map[string]any{"app": "prerequisite"}
	resource.Discovery = &configloader.DiscoveryConfig{
		Namespace: "default",
		BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{
			"app": "prerequisite",
		}},
	}
	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := newDesireExecutor(store).ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

	require.ErrorContains(t, err, "selector-based lifecycle deletion is unsupported for desire transport")
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)
	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	require.ErrorIs(t, err, desire.ErrNotFound, "unsupported selector deletion must not create a delete desire")
	_, err = store.GetApplyDesire(ctx, testDesireID.Apply())
	require.NoError(t, err, "a rejected selector deletion must leave the pending apply untouched")
}

// By-name deletion targets discovery.by_name, the identity discovery reads, even
// before the read mirror has synced.
func TestResourceExecutor_DesireByNameUnsyncedDeletesDiscoveryTarget(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	target := testDesireID.WithName("discovery-target")
	desiretest.PutUnsyncedReadDesire(t, ctx, store, target.Read(), desireOwner)
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	resource.Discovery.ByName = target.Name
	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := newDesireExecutor(store).ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, desireDeletionPendingReason, results[0].OperationReason)
	assert.Equal(t, target.Name, results[0].ResourceName)
	assert.Equal(t, ResourceStateUnsynced, execCtx.ResourceStates[resource.Name])
	_, err = store.GetDeleteDesire(ctx, target.Delete())
	assert.NoError(t, err, "the delete desire targets discovery.by_name")
	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "the manifest's own name is not a delete target")
}

func TestResourceExecutor_DesireSelectorDeleteIsRejected(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	desiretest.PutSyncedReadDesire(t, ctx, store, testDesireID.Read(), desireOwner, []byte(`{
		"apiVersion":"v1","kind":"ConfigMap",
		"metadata":{"name":"test-config","namespace":"default","labels":{"app":"prerequisite"}}
	}`))
	resource := newDesireResourceWithLifecycle("deleted_time != null", "Background")
	resource.Discovery = &configloader.DiscoveryConfig{
		Namespace: "default",
		BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{
			"app": "prerequisite",
		}},
	}
	execCtx := NewExecutionContext(ctx, nil, nil)
	execCtx.Params["deleted_time"] = testDeletedTime

	results, err := newDesireExecutor(store).ExecuteAll(ctx, []configloader.Resource{resource}, execCtx)

	require.ErrorContains(t, err, "selector-based lifecycle deletion is unsupported for desire transport")
	require.Len(t, results, 1)
	assert.Equal(t, StatusFailed, results[0].Status)
	assert.Equal(t, ResourceStatePresent, execCtx.ResourceStates[resource.Name],
		"the rejection leaves the discovered state alone")
	_, err = store.GetDeleteDesire(ctx, testDesireID.Delete())
	assert.ErrorIs(t, err, desire.ErrNotFound, "unsupported selector deletion must not create a delete desire")
}

func TestResourceExecutor_KubernetesSelectorAbsenceDeletesDependent(t *testing.T) {
	client := &trackingMockClient{MockK8sClient: k8sclient.NewMockK8sClient()}
	client.Resources["default/test-cm"] = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "test-cm", "namespace": "default"},
	}}
	re := newResourceExecutor(&ExecutorConfig{TransportRegistry: testTransportRegistry(client)})
	prerequisite := newResourceWithLifecycle("", "Background")
	prerequisite.Name = testResourceAName
	prerequisite.Discovery = &configloader.DiscoveryConfig{
		Namespace:   "default",
		BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{"app": "missing"}},
	}
	prerequisite.Lifecycle.Create = &configloader.LifecycleCreate{
		When: &configloader.LifecycleWhen{Expression: "false"},
	}
	dependent := newResourceWithLifecycle(
		`resource_states.?resourceA.orValue("") == "confirmed_deleted"`, "Background")
	dependent.Name = testResourceBName
	execCtx := NewExecutionContext(t.Context(), nil, nil)

	results, err := re.ExecuteAll(t.Context(), []configloader.Resource{prerequisite, dependent}, execCtx)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, StatusSkipped, results[0].Status)
	assert.True(t, client.DeleteCalled)
	assert.Equal(t, ResourceStateConfirmedDeleted, execCtx.ResourceStates[prerequisite.Name])
}
