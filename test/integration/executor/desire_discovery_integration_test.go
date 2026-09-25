package executorintegrationtest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/executor"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/constants"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const desireDiscoveryTransport = "desire-primary"

const mirroredStatusObject = `{
	"apiVersion":"v1","kind":"ConfigMap",
	"metadata":{"name":"remote-config","namespace":"default"},
	"status":{"availableReplicas":3,"conditions":[{"type":"Available","status":"True"}]}
}`

var desireDiscoveryIdentity = desiretest.TestIdentity{
	ManagementCluster: "cluster-1",
	Resource:          "configmaps",
	Namespace:         "default",
	Name:              "remote-config",
}

func TestDesireDiscoveryFullObjectAndCELState(t *testing.T) {
	tests := []struct {
		name             string
		seed             func(t testing.TB, ctx context.Context, store *memory.Store)
		event            map[string]interface{}
		wantState        string
		wantAvailability float64
		wantGate         bool
		wantOptional     bool
	}{
		{
			name: "present status-bearing object",
			seed: func(t testing.TB, ctx context.Context, store *memory.Store) {
				desiretest.PutSyncedReadDesire(
					t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter", []byte(mirroredStatusObject))
			},
			wantState:        "present",
			wantAvailability: 3,
			wantGate:         true,
			wantOptional:     true,
		},
		{
			name: "present status below gate threshold",
			seed: func(t testing.TB, ctx context.Context, store *memory.Store) {
				content := strings.Replace(mirroredStatusObject, `"availableReplicas":3`, `"availableReplicas":1`, 1)
				desiretest.PutSyncedReadDesire(t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter", []byte(content))
			},
			wantState:        "present",
			wantAvailability: 1,
			wantOptional:     true,
		},
		{
			name: "unsynced mirror",
			seed: func(t testing.TB, ctx context.Context, store *memory.Store) {
				desiretest.PutUnsyncedReadDesire(t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter")
			},
			wantState:        "unsynced",
			wantAvailability: -1,
			wantOptional:     true,
		},
		{
			name: "stale NotFound mirror while apply is active",
			seed: func(t testing.TB, ctx context.Context, store *memory.Store) {
				desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter")
			},
			wantState:        "unsynced",
			wantAvailability: -1,
			wantOptional:     true,
		},
		{
			name: "present empty object",
			seed: func(t testing.TB, ctx context.Context, store *memory.Store) {
				desiretest.PutSyncedReadDesire(t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter", []byte(`{}`))
			},
			wantState:        "present",
			wantAvailability: -1,
			wantOptional:     true,
		},
		{
			name: "confirmed deletion",
			seed: func(t testing.TB, ctx context.Context, store *memory.Store) {
				desiretest.PutConfirmedAbsentReadDesire(t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter")
			},
			event:            map[string]interface{}{"deleted_time": true},
			wantState:        "confirmed_deleted",
			wantAvailability: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			store := memory.New()
			tt.seed(t, ctx, store)
			apiClient := hyperfleetapi.NewMockClient()
			adapterExecutor, err := executor.NewBuilder().
				WithConfig(desireDiscoveryConfig()).
				WithAPIClient(apiClient).
				WithTransportRegistry(transportclient.Registry{
					desireDiscoveryTransport: desireclient.NewClient(store, "hyperfleet-adapter"),
				}).
				Build()
			require.NoError(t, err)

			result := adapterExecutor.Execute(ctx, tt.event)

			require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
			require.NotNil(t, result.ExecutionContext)
			assert.Equal(t, tt.wantState, string(result.ExecutionContext.ResourceStates["remoteConfig"]))
			assert.Equal(t, tt.wantOptional, resourceOptionalPresent(t, result.ExecutionContext))
			require.Len(t, result.PostActionResults, 2)
			assert.Equal(t, tt.wantGate, result.PostActionResults[0].APICallMade)
			assert.Equal(t, !tt.wantGate, result.PostActionResults[0].Skipped)
			if tt.wantGate {
				assert.Equal(t, executor.StatusSuccess, result.PostActionResults[0].Status)
			} else {
				assert.Equal(t, executor.StatusSkipped, result.PostActionResults[0].Status)
			}
			require.Len(t, apiClient.Requests, 1+boolToInt(tt.wantGate))
			assert.Equal(t, tt.wantState, postPayloadValue(t, apiClient, "state"))
			assert.Equal(t, tt.wantAvailability, postPayloadValue(t, apiClient, "availableReplicas"))
			assert.Equal(t, tt.wantOptional, postPayloadValue(t, apiClient, "optionalPresent"))
			assert.Equal(t, tt.wantState == "present", postPayloadValue(t, apiClient, "livePresent"))
			if tt.wantState == "confirmed_deleted" {
				assert.NotContains(t, result.ExecutionContext.GetCELVariables()[configloader.FieldResources], "remoteConfig")
			}
		})
	}
}

func TestDesireDiscoveryMatchesLocalCELShape(t *testing.T) {
	ctx := t.Context()
	localObject := &unstructured.Unstructured{}
	require.NoError(t, json.Unmarshal([]byte(mirroredStatusObject), &localObject.Object))
	// Kubernetes unstructured objects retain JSON integer fields as int64.
	require.NoError(t, unstructured.SetNestedField(localObject.Object, int64(3), "status", "availableReplicas"))
	mirroredObject, err := json.Marshal(localObject.Object)
	require.NoError(t, err)
	store := memory.New()
	desiretest.PutSyncedReadDesire(
		t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter", mirroredObject)

	localClient := k8sclient.NewMockK8sClient()
	localClient.GetResourceResult = localObject
	localClient.ApplyResourceResult = &transportclient.ApplyResult{}

	remoteConfig := desireDiscoveryConfig()
	localConfig := desireDiscoveryConfig()
	localConfig.Resources[0].Transport = nil
	localConfig.Transports = nil
	stateGate := `resource_states.remoteConfig == "present" && resources.remoteConfig.status.availableReplicas >= 3`
	localConfig.Post.PostActions[0].When.Expression = stateGate
	remoteConfig.Post.PostActions[0].When.Expression = stateGate

	var localShape map[string]any
	for _, tt := range []struct {
		name       string
		config     *configloader.Config
		client     transportclient.TransportClient
		clientName string
	}{
		{
			name: "local Kubernetes", config: localConfig,
			client: localClient, clientName: configloader.TransportClientKubernetes,
		},
		{
			name: "desire transport", config: remoteConfig,
			client: desireclient.NewClient(store, "hyperfleet-adapter"), clientName: desireDiscoveryTransport,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			apiClient := hyperfleetapi.NewMockClient()
			adapterExecutor, err := executor.NewBuilder().
				WithConfig(tt.config).
				WithAPIClient(apiClient).
				WithTransportRegistry(transportclient.Registry{tt.clientName: tt.client}).
				Build()
			require.NoError(t, err)

			result := adapterExecutor.Execute(ctx, nil)
			require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
			require.NotNil(t, result.ExecutionContext)
			resources, ok := result.ExecutionContext.GetCELVariables()[configloader.FieldResources].(map[string]any)
			require.True(t, ok)
			shape, ok := resources["remoteConfig"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, float64(3), postPayloadValue(t, apiClient, "availableReplicas"))
			assert.Equal(t, "present", postPayloadValue(t, apiClient, "state"))
			require.Len(t, result.PostActionResults, 2)
			assert.True(t, result.PostActionResults[0].APICallMade)
			if localShape == nil {
				localShape = shape
			} else {
				assert.Equal(t, localShape, shape)
			}
		})
	}
}

func TestDesireGenerationMirrorLagVisibleToCEL(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	withGeneration := func(generation string) []byte {
		return []byte(strings.Replace(mirroredStatusObject,
			`"namespace":"default"`, `"namespace":"default","annotations":{"hyperfleet.io/generation":"`+generation+`"}`, 1))
	}
	desiretest.PutSyncedReadDesire(
		t, ctx, store, desireDiscoveryIdentity.Read(), "hyperfleet-adapter", withGeneration("1"))
	config := desireDiscoveryConfig()
	manifest := config.Resources[0].Manifest.(map[string]interface{})
	metadata := manifest["metadata"].(map[string]interface{})
	metadata["annotations"] = map[string]interface{}{constants.AnnotationGeneration: "2"}
	config.Post.PostActions[0].When.Expression =
		`resources.remoteConfig.metadata.annotations["hyperfleet.io/generation"] == "2"`
	client := desireclient.NewClient(store, "hyperfleet-adapter")
	adapterExecutor, err := executor.NewBuilder().WithConfig(config).
		WithAPIClient(hyperfleetapi.NewMockClient()).
		WithTransportRegistry(transportclient.Registry{desireDiscoveryTransport: client}).Build()
	require.NoError(t, err)

	check := func(want string) {
		t.Helper()
		result := adapterExecutor.Execute(ctx, nil)
		require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
		resources := result.ExecutionContext.GetCELVariables()[configloader.FieldResources].(map[string]any)
		remote := resources["remoteConfig"].(map[string]any)
		metadata := remote["metadata"].(map[string]any)
		annotations := metadata["annotations"].(map[string]any)
		require.Equal(t, want, annotations[constants.AnnotationGeneration])
		require.Equal(t, want == "2", result.PostActionResults[0].APICallMade)
	}
	check("1")
	applyID := desire.Identity{
		ManagementCluster: desireDiscoveryIdentity.ManagementCluster, Type: desire.TypeApply,
		Resource: desireDiscoveryIdentity.Resource, Namespace: desireDiscoveryIdentity.Namespace,
		Name: desireDiscoveryIdentity.Name,
	}
	first, err := store.GetApplyDesire(ctx, applyID)
	require.NoError(t, err)
	check("1")
	second, err := store.GetApplyDesire(ctx, applyID)
	require.NoError(t, err)
	require.Equal(t, first.Version, second.Version)

	_, err = store.UpdateReadDesireStatus(ctx, desireDiscoveryIdentity.Read(), desire.ReadStatus{
		Status: desire.Status{Conditions: []metav1.Condition{{Type: desire.TypeSuccessful,
			Status: metav1.ConditionTrue, Reason: desire.ReasonSynced}}}, KubeContent: withGeneration("2"),
	})
	require.NoError(t, err)
	check("2")
}

func desireDiscoveryConfig() *configloader.Config {
	return &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "desire-discovery-test"},
		Transports: map[string]configloader.TransportDefinition{
			desireDiscoveryTransport: {Type: configloader.TransportTypeRemote},
		},
		Resources: []configloader.Resource{{
			Name: "remoteConfig",
			Manifest: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":        desireDiscoveryIdentity.Name,
					"namespace":   desireDiscoveryIdentity.Namespace,
					"annotations": map[string]interface{}{constants.AnnotationGeneration: "1"},
				},
			},
			Transport: &configloader.TransportConfig{
				Client: desireDiscoveryTransport,
				Desire: &configloader.DesireTransportConfig{
					TargetCluster: desireDiscoveryIdentity.ManagementCluster,
					Resource:      desireDiscoveryIdentity.Resource,
				},
			},
			Discovery: &configloader.DiscoveryConfig{
				Namespace: desireDiscoveryIdentity.Namespace,
				ByName:    desireDiscoveryIdentity.Name,
			},
			Lifecycle: &configloader.ResourceLifecycle{Delete: &configloader.LifecycleDelete{
				When: &configloader.LifecycleWhen{Expression: "has(event.deleted_time) && event.deleted_time == true"},
			}},
		}},
		Post: &configloader.PostConfig{
			Payloads: []configloader.Payload{{
				Name: "statusPayload",
				Build: map[string]interface{}{
					"state": map[string]interface{}{
						"expression": `resource_states.?remoteConfig.orValue("local")`,
					},
					"optionalPresent": map[string]interface{}{
						"expression": `resources.?remoteConfig.hasValue()`,
					},
					"livePresent": map[string]interface{}{
						"expression": `resource_states.?remoteConfig.orValue("") == "present"`,
					},
					"availableReplicas": map[string]interface{}{
						"expression": `resources.?remoteConfig.hasValue() && has(resources.remoteConfig.status) && ` +
							`has(resources.remoteConfig.status.availableReplicas) ? ` +
							`resources.remoteConfig.status.availableReplicas : -1`,
					},
				},
			}},
			PostActions: []configloader.PostAction{{
				ActionBase: configloader.ActionBase{
					Name:    "reportAvailable",
					APICall: &configloader.APICall{Method: "PUT", URL: "/available", Body: `{"available":true}`},
				},
				When: &configloader.PostActionWhen{
					Expression: `resources.?remoteConfig.hasValue() && has(resources.remoteConfig.status) && ` +
						`has(resources.remoteConfig.status.availableReplicas) && ` +
						`resources.remoteConfig.status.availableReplicas >= 3`,
				},
			}, {
				ActionBase: configloader.ActionBase{
					Name: "reportStatus",
					APICall: &configloader.APICall{
						Method: "PUT",
						URL:    "/status",
						Body:   "{{ .statusPayload }}",
					},
				},
			}},
		},
	}
}

func resourceOptionalPresent(t *testing.T, execCtx *executor.ExecutionContext) bool {
	t.Helper()
	resources, ok := execCtx.GetCELVariables()[configloader.FieldResources].(map[string]any)
	require.True(t, ok)
	_, present := resources["remoteConfig"]
	return present
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func postPayloadValue(t *testing.T, client *hyperfleetapi.MockClient, field string) any {
	t.Helper()
	request := client.GetLastRequest()
	require.NotNil(t, request)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(request.Body, &payload))
	return payload[field]
}
