package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/criteria"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/logctx"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	apierrors "github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/errors"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	hfl "github.com/openshift-hyperfleet/hyperfleet-logger"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// newMockAPIClient creates a new mock API client for convenience
func newMockAPIClient() *hyperfleetapi.MockClient {
	return hyperfleetapi.NewMockClient()
}

func mockErrorResponse(statusCode int) (*hyperfleetapi.Response, error) {
	status := fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))
	body := []byte(fmt.Sprintf(`{"error":"%d"}`, statusCode))
	resp := &hyperfleetapi.Response{
		StatusCode: statusCode,
		Status:     status,
		Body:       body,
		Attempts:   1,
	}
	apiErr := apierrors.NewAPIError("MOCK", "/mock", statusCode, status, body, 1, 0,
		fmt.Errorf("HTTP %d", statusCode))
	return resp, apiErr
}

// mock404Response returns a 404 response with code HYPERFLEET-NTF-002,
// representing a real resource that was not found.
// The MockClient only returns (nil, error) when an error is set, so using
// mockErrorResponse for 404 tests causes ExecuteAPICall to wrap the error
// in a new APIError with StatusCode=0, which breaks detection. Setting
// only the response lets ValidateAPIResponse create the correct APIError.
func mock404Response() *hyperfleetapi.Response {
	body := `{
		"type": "https://api.hyperfleet.io/errors/resource-not-found",
		"title": "Resource Not Found",
		"status": 404,
		"detail": "Cluster with id='abc123' not found",
		"code": "HYPERFLEET-NTF-002",
		"trace_id": "019ed716-f3cf-7b8e-b400-0796be4722c3"
	}`
	return &hyperfleetapi.Response{
		StatusCode: 404,
		Status:     "404 Not Found",
		Body:       []byte(body),
		Attempts:   1,
	}
}

// mockBrokenEndpoint404Response returns a 404 with code HYPERFLEET-NTF-000,
// representing a misconfigured/broken precondition URL.
func mockBrokenEndpoint404Response() *hyperfleetapi.Response {
	body := `{
		"type": "https://api.hyperfleet.io/errors/endpoint-not-found",
		"title": "Endpoint Not Found",
		"status": 404,
		"detail": "The requested endpoint does not exist",
		"code": "HYPERFLEET-NTF-000",
		"trace_id": ""
	}`
	return &hyperfleetapi.Response{
		StatusCode: 404,
		Status:     "404 Not Found",
		Body:       []byte(body),
		Attempts:   1,
	}
}

func build404TestExecutor(t *testing.T, config *configloader.Config, mockClient *hyperfleetapi.MockClient) *Executor {
	t.Helper()
	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(mockClient).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)
	return exec
}

// TestNewExecutor tests the NewExecutor function
func TestNewExecutor(t *testing.T) {
	registry := testTransportRegistry(k8sclient.NewMockK8sClient())
	apiCall := &configloader.APICall{Method: "GET", URL: "/clusters/x"}
	apiCallParam := configloader.Parameter{Name: "clusterStatus", Source: configloader.APICallSource(apiCall)}
	apiCallPrecondition := configloader.Precondition{
		ActionBase: configloader.ActionBase{Name: "clusterCheck", APICall: apiCall},
	}
	apiCallPost := &configloader.PostConfig{PostActions: []configloader.PostAction{
		{ActionBase: configloader.ActionBase{Name: "logDone", Log: &configloader.LogAction{Message: "done"}}},
		{ActionBase: configloader.ActionBase{Name: "reportStatus", APICall: apiCall}},
	}}

	tests := []struct {
		config      *ExecutorConfig
		name        string
		errContains []string
		expectError bool
	}{
		{
			name:        "nil config",
			config:      nil,
			expectError: true,
		},
		{
			name: "missing adapter config",
			config: &ExecutorConfig{
				APIClient: newMockAPIClient(),
			},
			expectError: true,
		},
		{
			name: "missing API client without api_call",
			config: &ExecutorConfig{
				Config:            &configloader.Config{},
				TransportRegistry: registry,
			},
			expectError: false,
		},
		{
			name: "missing API client with api_call param",
			config: &ExecutorConfig{
				Config: &configloader.Config{Params: []configloader.Parameter{
					{Name: "clusterId", Source: configloader.StringSource("event.id")},
					apiCallParam,
				}},
				TransportRegistry: registry,
			},
			expectError: true,
			errContains: []string{"params[1].source.api_call (clusterStatus)"},
		},
		{
			name: "missing API client with api_call precondition",
			config: &ExecutorConfig{
				Config:            &configloader.Config{Preconditions: []configloader.Precondition{apiCallPrecondition}},
				TransportRegistry: registry,
			},
			expectError: true,
			errContains: []string{"preconditions[0].api_call (clusterCheck)"},
		},
		{
			name: "missing API client with api_call post action",
			config: &ExecutorConfig{
				Config:            &configloader.Config{Post: apiCallPost},
				TransportRegistry: registry,
			},
			expectError: true,
			errContains: []string{"post.post_actions[1].api_call (reportStatus)"},
		},
		{
			name: "missing API client lists every api_call in pipeline order",
			config: &ExecutorConfig{
				Config: &configloader.Config{
					Params:        []configloader.Parameter{apiCallParam},
					Preconditions: []configloader.Precondition{apiCallPrecondition},
					Post:          apiCallPost,
				},
				TransportRegistry: registry,
			},
			expectError: true,
			errContains: []string{"task config makes API calls but no API client was supplied: " +
				"params[0].source.api_call (clusterStatus), " +
				"preconditions[0].api_call (clusterCheck), " +
				"post.post_actions[1].api_call (reportStatus)"},
		},
		{
			name: "missing API client with optional api_call param and gated post action",
			config: &ExecutorConfig{
				Config: &configloader.Config{
					Params: []configloader.Parameter{{
						Name:    "clusterStatus",
						Source:  configloader.APICallSource(apiCall),
						Default: map[string]interface{}{},
					}},
					Post: &configloader.PostConfig{PostActions: []configloader.PostAction{{
						ActionBase: configloader.ActionBase{Name: "reportStatus", APICall: apiCall},
						When:       &configloader.PostActionWhen{Expression: "false"},
					}}},
				},
				TransportRegistry: registry,
			},
			expectError: true,
			errContains: []string{
				"params[0].source.api_call (clusterStatus)",
				"post.post_actions[0].api_call (reportStatus)",
			},
		},
		{
			name: "API client with api_call",
			config: &ExecutorConfig{
				Config: &configloader.Config{
					Params:        []configloader.Parameter{apiCallParam},
					Preconditions: []configloader.Precondition{apiCallPrecondition},
					Post:          apiCallPost,
				},
				APIClient:         newMockAPIClient(),
				TransportRegistry: registry,
			},
			expectError: false,
		},
		{
			name: "missing transport client",
			config: &ExecutorConfig{
				Config:    &configloader.Config{},
				APIClient: newMockAPIClient(),
			},
			expectError: true,
		},
		{
			name: "valid config",
			config: &ExecutorConfig{
				Config:            &configloader.Config{},
				APIClient:         newMockAPIClient(),
				TransportRegistry: testTransportRegistry(k8sclient.NewMockK8sClient()),
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewExecutor(tt.config)
			if tt.expectError {
				require.Error(t, err)
				for _, want := range tt.errContains {
					assert.Contains(t, err.Error(), want)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestExecutorBuilder(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{
			Name:    "test-adapter",
			Version: "1.0.0",
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()

	require.NoError(t, err)
	require.NotNil(t, exec)
}

func TestExecutorBuilder_WithoutAPIClient(t *testing.T) {
	k8sClient := k8sclient.NewMockK8sClient()
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Params: []configloader.Parameter{
			{Name: "clusterId", Source: configloader.StringSource("event.id")},
		},
		Resources: []configloader.Resource{{
			Name: "clusterConfig",
			Manifest: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "cm-{{ .clusterId }}", "namespace": "default"},
			},
		}},
		Post: &configloader.PostConfig{
			Payloads: []configloader.Payload{{
				Name:  "statusPayload",
				Build: map[string]interface{}{"cluster": "{{ .clusterId }}"},
			}},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithTransportClient(k8sClient).
		Build()
	require.NoError(t, err)

	result := exec.Execute(t.Context(), map[string]interface{}{"id": "cluster-1"})

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	assert.Equal(t, "cluster-1", result.Params["clusterId"])
	require.Len(t, result.ResourceResults, 1)
	assert.Contains(t, k8sClient.Resources, "default/cm-cluster-1")
}

func TestExecutionContext(t *testing.T) {
	ctx := context.Background()
	eventData := map[string]interface{}{
		"id": "test-cluster",
	}

	execCtx := NewExecutionContext(ctx, eventData, nil)

	assert.Equal(t, "test-cluster", execCtx.EventData["id"])
	assert.Empty(t, execCtx.Params)
	assert.Empty(t, execCtx.Resources)
	assert.Equal(t, string(StatusSuccess), execCtx.Adapter.ExecutionStatus)
}

func TestExecutionContext_GetCELVariables_ResourceStates(t *testing.T) {
	execCtx := NewExecutionContext(t.Context(), nil, nil)
	execCtx.Resources["remoteConfig"] = &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"availableReplicas": int64(3)},
	}}
	execCtx.Resources["deletedConfig"] = nil
	execCtx.ResourceStates["remoteConfig"] = ResourceStatePresent
	execCtx.ResourceStates["pendingConfig"] = ResourceStateUnsynced
	execCtx.ResourceStates["deletedConfig"] = ResourceStateConfirmedDeleted

	variables := execCtx.GetCELVariables()

	resources, ok := variables[configloader.FieldResources].(map[string]interface{})
	require.True(t, ok)
	remoteConfig := resources["remoteConfig"].(map[string]interface{})
	status := remoteConfig["status"].(map[string]interface{})
	assert.Equal(t, int64(3), status["availableReplicas"])
	assert.NotContains(t, resources, "deletedConfig")
	assert.Equal(t, map[string]interface{}{}, resources["pendingConfig"],
		"an unsynced resource uses a placeholder rather than a discovered object")

	states, ok := variables[configloader.FieldResourceStates].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, map[string]interface{}{
		"remoteConfig":  "present",
		"pendingConfig": "unsynced",
		"deletedConfig": "confirmed_deleted",
	}, states)
}

func TestExecutionContext_SetError(t *testing.T) {
	ctx := context.Background()
	execCtx := NewExecutionContext(ctx, map[string]interface{}{}, nil)
	execCtx.SetError("TestReason", "Test message")

	assert.Equal(t, string(StatusFailed), execCtx.Adapter.ExecutionStatus)
	assert.Equal(t, "TestReason", execCtx.Adapter.ErrorReason)
	assert.Equal(t, "Test message", execCtx.Adapter.ErrorMessage)
}

func TestExecutionContext_EvaluationTracking(t *testing.T) {
	ctx := context.Background()
	execCtx := NewExecutionContext(ctx, map[string]interface{}{}, nil)

	// Verify evaluations are empty initially
	assert.Empty(t, execCtx.Evaluations, "expected empty evaluations initially")

	// Add a CEL evaluation
	execCtx.AddCELEvaluation(PhasePreconditions, "check-status", "status == 'active'", true)

	require.Len(t, execCtx.Evaluations, 1, "evaluation")

	eval := execCtx.Evaluations[0]
	assert.Equal(t, PhasePreconditions, eval.Phase)
	assert.Equal(t, "check-status", eval.Name)
	assert.Equal(t, EvaluationTypeCEL, eval.EvaluationType)
	assert.Equal(t, "status == 'active'", eval.Expression)
	assert.True(t, eval.Matched)

	// Add a conditions evaluation with field results (using criteria.EvaluationResult)
	fieldResults := map[string]criteria.EvaluationResult{
		"status.phase": {
			Field:         "status.phase",
			Operator:      criteria.OperatorEquals,
			ExpectedValue: "Running",
			FieldValue:    "Running",
			Matched:       true,
		},
		"replicas": {
			Field:         "replicas",
			Operator:      criteria.OperatorGreaterThan,
			ExpectedValue: 0,
			FieldValue:    3,
			Matched:       true,
		},
	}
	execCtx.AddConditionsEvaluation(PhasePreconditions, "check-replicas", true, fieldResults)

	require.Len(t, execCtx.Evaluations, 2, "evaluations")

	condEval := execCtx.Evaluations[1]
	assert.Equal(t, EvaluationTypeConditions, condEval.EvaluationType)
	assert.Len(t, condEval.FieldResults, 2)

	// Verify lookup by field name works
	assert.Contains(t, condEval.FieldResults, "status.phase")
	assert.Equal(t, "Running", condEval.FieldResults["status.phase"].FieldValue)

	assert.Contains(t, condEval.FieldResults, "replicas")
	assert.Equal(t, 3, condEval.FieldResults["replicas"].FieldValue)
}

func TestExecutionContext_GetEvaluationsByPhase(t *testing.T) {
	ctx := context.Background()
	execCtx := NewExecutionContext(ctx, map[string]interface{}{}, nil)

	// Add evaluations in different phases
	execCtx.AddCELEvaluation(PhasePreconditions, "precond-1", "true", true)
	execCtx.AddCELEvaluation(PhasePreconditions, "precond-2", "false", false)
	execCtx.AddCELEvaluation(PhasePostActions, "post-1", "true", true)

	// Get preconditions evaluations
	precondEvals := execCtx.GetEvaluationsByPhase(PhasePreconditions)
	require.Len(t, precondEvals, 2, "precondition evaluations")

	// Get post actions evaluations
	postEvals := execCtx.GetEvaluationsByPhase(PhasePostActions)
	require.Len(t, postEvals, 1, "post action evaluation")

	// Get resources evaluations (none)
	resourceEvals := execCtx.GetEvaluationsByPhase(PhaseResources)
	require.Len(t, resourceEvals, 0, "resource evaluations")
}

func TestExecutionContext_GetFailedEvaluations(t *testing.T) {
	ctx := context.Background()
	execCtx := NewExecutionContext(ctx, map[string]interface{}{}, nil)

	// Add mixed evaluations
	execCtx.AddCELEvaluation(PhasePreconditions, "passed-1", "true", true)
	execCtx.AddCELEvaluation(PhasePreconditions, "failed-1", "false", false)
	execCtx.AddCELEvaluation(PhasePreconditions, "passed-2", "true", true)
	execCtx.AddCELEvaluation(PhasePostActions, "failed-2", "false", false)

	failedEvals := execCtx.GetFailedEvaluations()
	require.Len(t, failedEvals, 2, "failed evaluations")

	// Verify the failed ones are correct
	names := make(map[string]bool)
	for _, eval := range failedEvals {
		names[eval.Name] = true
	}
	assert.True(t, names["failed-1"], "failed-1")
	assert.True(t, names["failed-2"], "failed-2")
}

func TestExecutorError(t *testing.T) {
	err := NewExecutorError(PhasePreconditions, "test-step", "test message", nil)

	expected := "[preconditions] test-step: test message"
	if err.Error() != expected {
		t.Errorf("expected '%s', got '%s'", expected, err.Error())
	}

	// With wrapped error
	wrappedErr := NewExecutorError(PhaseResources, "create", "failed to create", context.Canceled)
	assert.Equal(t, context.Canceled, wrappedErr.Unwrap())
}

func TestExecute_ParamExtraction(t *testing.T) {
	// Set up environment variable for test
	t.Setenv("TEST_VAR", "test-value")

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{
			Name:    "test-adapter",
			Version: "1.0.0",
		},
		Params: []configloader.Parameter{
			{
				Name:     "testParam",
				Source:   configloader.StringSource("env.TEST_VAR"),
				Required: true,
			},
			{
				Name:     "eventParam",
				Source:   configloader.StringSource("event.id"),
				Required: true,
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	if err != nil {
		t.Fatalf("unexpected error creating executor: %v", err)
	}

	// Create event data
	eventData := map[string]interface{}{
		"id": "cluster-456",
	}

	// Execute with event ID in context
	ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-event-123")
	result := exec.Execute(ctx, eventData)

	// Check result

	// Check extracted params
	if result.Params["testParam"] != "test-value" {
		t.Errorf("expected testParam to be 'test-value', got '%v'", result.Params["testParam"])
	}

	if result.Params["eventParam"] != "cluster-456" {
		t.Errorf("expected eventParam to be 'cluster-456', got '%v'", result.Params["eventParam"])
	}
}

// TestExecute_ParamsAPICallSource verifies the full executor pipeline when params use
// api_call and expression sources
func TestExecute_ParamsAPICallSource(t *testing.T) {
	clusterBody := `{
		"name": "unit-cluster",
		"generation": 3,
		"spec": {"region": "us-east-1"},
		"status": {
			"conditions": [{"type": "Reconciled", "status": "True"}]
		}
	}`

	mockClient := newMockAPIClient()
	mockClient.GetResponse = &hyperfleetapi.Response{StatusCode: 200, Body: []byte(clusterBody)}
	mockClient.PutResponse = &hyperfleetapi.Response{StatusCode: 200, Body: []byte(`{}`)}

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Params: []configloader.Parameter{
			{Name: "clusterId", Source: configloader.StringSource("event.id"), Required: true},
			{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
				Method: "GET",
				URL:    "/clusters/{{ .clusterId }}",
			})},
			{Name: "clusterName", Source: configloader.StringSource("clusterData.name")},
			{Name: "clusterRegion", Source: configloader.StringSource("clusterData.spec.region")},
			{Name: "isReconciled", Source: configloader.ExpressionSource(
				`clusterData.status.conditions.filter(c, c.type == "Reconciled").size() > 0
				  ? clusterData.status.conditions.filter(c, c.type == "Reconciled")[0].status == "True"
				  : false`,
			)},
		},
		// Precondition evaluates only params
		Preconditions: []configloader.Precondition{
			{
				ActionBase: configloader.ActionBase{Name: "clusterReady"},
				Expression: "isReconciled",
			},
		},
		Resources: []configloader.Resource{},
		Post: &configloader.PostConfig{
			Payloads: []configloader.Payload{
				{
					Name: "statusPayload",
					Build: map[string]interface{}{
						"clusterName":   map[string]interface{}{"expression": "clusterName"},
						"clusterRegion": map[string]interface{}{"expression": "clusterRegion"},
						"isReconciled":  map[string]interface{}{"expression": "isReconciled"},
					},
				},
			},
			PostActions: []configloader.PostAction{
				{
					ActionBase: configloader.ActionBase{
						Name: "reportStatus",
						APICall: &configloader.APICall{
							Method: "PUT",
							URL:    "/clusters/{{ .clusterId }}/statuses",
							Body:   "{{ .statusPayload }}",
						},
					},
				},
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(mockClient).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	eventData := map[string]interface{}{"id": "cluster-unit"}
	result := exec.Execute(context.Background(), eventData)

	require.Equal(t, StatusSuccess, result.Status, "expected success; errors=%v", result.Errors)
	assert.False(t, result.ResourcesSkipped, "precondition should match — resources not skipped")

	// Derived params must be resolved
	assert.Equal(t, "unit-cluster", result.Params["clusterName"])
	assert.Equal(t, "us-east-1", result.Params["clusterRegion"])
	assert.Equal(t, true, result.Params["isReconciled"])

	// Precondition must have matched
	require.Len(t, result.PreconditionResults, 1)
	assert.True(t, result.PreconditionResults[0].Matched)

	// Two API calls: one GET (params), one PUT (post-action)
	require.Len(t, mockClient.Requests, 2)
	assert.Equal(t, "GET", mockClient.Requests[0].Method)
	assert.Equal(t, "PUT", mockClient.Requests[1].Method)
}

func TestParamExtractor(t *testing.T) {
	t.Setenv("TEST_ENV", "env-value")

	eventData := map[string]interface{}{
		"id": "test-cluster",
		"nested": map[string]interface{}{
			"value": "nested-value",
		},
	}

	tests := []struct {
		expectValue interface{}
		name        string
		expectKey   string
		params      []configloader.Parameter
		expectError bool
	}{
		{
			name: "extract from env",
			params: []configloader.Parameter{
				{Name: "envVar", Source: configloader.StringSource("env.TEST_ENV")},
			},
			expectKey:   "envVar",
			expectValue: "env-value",
		},
		{
			name: "extract from event",
			params: []configloader.Parameter{
				{Name: "clusterId", Source: configloader.StringSource("event.id")},
			},
			expectKey:   "clusterId",
			expectValue: "test-cluster",
		},
		{
			name: "extract nested from event",
			params: []configloader.Parameter{
				{Name: "nestedVal", Source: configloader.StringSource("event.nested.value")},
			},
			expectKey:   "nestedVal",
			expectValue: "nested-value",
		},
		{
			name: "use default for missing optional",
			params: []configloader.Parameter{
				{Name: "optional", Source: configloader.StringSource("env.MISSING"), Default: "default-val"},
			},
			expectKey:   "optional",
			expectValue: "default-val",
		},
		{
			name: "fail on missing required",
			params: []configloader.Parameter{
				{Name: "required", Source: configloader.StringSource("env.MISSING"), Required: true},
			},
			expectError: true,
		},
		{
			name: "extract from config",
			params: []configloader.Parameter{
				{Name: "adapterName", Source: configloader.StringSource("config.adapter.name")},
			},
			expectKey:   "adapterName",
			expectValue: "test",
		},
		{
			name: "extract nested from config",
			params: []configloader.Parameter{
				{Name: "adapterVersion", Source: configloader.StringSource("config.adapter.version")},
			},
			expectKey:   "adapterVersion",
			expectValue: "1.0.0",
		},
		{
			name: "derive from adapter variable",
			params: []configloader.Parameter{
				{Name: "derivedAdapterName", Source: configloader.StringSource("adapter.name")},
			},
			expectKey:   "derivedAdapterName",
			expectValue: "test",
		},
		{
			name: "use default for missing optional config field",
			params: []configloader.Parameter{
				{Name: "optional", Source: configloader.StringSource("config.nonexistent"), Default: "fallback"},
			},
			expectKey:   "optional",
			expectValue: "fallback",
		},
		{
			name: "fail on missing required config field",
			params: []configloader.Parameter{
				{Name: "required", Source: configloader.StringSource("config.nonexistent"), Required: true},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create config with test params
			config := &configloader.Config{
				Adapter: configloader.AdapterInfo{
					Name:    "test",
					Version: "1.0.0",
				},
				Params: tt.params,
			}

			// Create a new context for each test. The config supplies adapter.name.
			execCtx := NewExecutionContext(t.Context(), eventData, config)

			// Extract params using pure function
			configMap, err := configToMap(config)
			require.NoError(t, err)
			err = extractConfigParams(context.Background(), config, execCtx, configMap, nil)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)

			if tt.expectKey != "" {
				if execCtx.Params[tt.expectKey] != tt.expectValue {
					t.Errorf("expected %s=%v, got %v", tt.expectKey, tt.expectValue, execCtx.Params[tt.expectKey])
				}
			}
		})
	}
}

// runParamExtraction is a test helper that wires up the full param extraction
// pipeline
func runParamExtraction(
	t *testing.T, config *configloader.Config, mockClient hyperfleetapi.Client, eventData map[string]interface{},
) (*ExecutionContext, error) {
	t.Helper()
	execCtx := NewExecutionContext(context.Background(), eventData, nil)
	return execCtx, extractParams(execCtx, config, mockClient)
}

// An env param and env.* read the same snapshot, even if the process
// environment changes after NewExecutionContext returns.
func TestParamExtractor_EnvSourceMatchesEnvVariable(t *testing.T) {
	t.Setenv("SNAPSHOT_VAR", "before")
	execCtx := NewExecutionContext(context.Background(), map[string]interface{}{}, nil)
	t.Setenv("SNAPSHOT_VAR", "after")

	config := &configloader.Config{
		Params: []configloader.Parameter{{Name: "fromEnv", Source: configloader.StringSource("env.SNAPSHOT_VAR")}},
	}
	require.NoError(t, extractParams(execCtx, config, nil))

	env, ok := execCtx.templateVariables()["env"].(map[string]any)
	require.True(t, ok, "env must be a map")
	assert.Equal(t, "before", execCtx.Params["fromEnv"])
	assert.Equal(t, "before", env["SNAPSHOT_VAR"])
}

// TestParamExtractor_APICallSource tests params with source: api_call
func TestParamExtractor_APICallSource(t *testing.T) {
	eventData := map[string]interface{}{"id": "cluster-123"}

	responseBody := `{"name":"my-cluster","generation":5,"status":{"phase":"Active"}}`

	t.Run("single api_call stores full response map", func(t *testing.T) {
		mockClient := newMockAPIClient()
		mockClient.GetResponse = &hyperfleetapi.Response{StatusCode: 200, Body: []byte(responseBody)}

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterId", Source: configloader.StringSource("event.id")},
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/{{ .clusterId }}",
				})},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)

		val, ok := execCtx.Params["clusterData"]
		require.True(t, ok, "clusterData should be in params")
		m, ok := val.(map[string]interface{})
		require.True(t, ok, "clusterData should be a map")
		assert.Equal(t, "my-cluster", m["name"])
	})

	t.Run("dot-notation derivation extracts field from api_call param", func(t *testing.T) {
		mockClient := newMockAPIClient()
		mockClient.GetResponse = &hyperfleetapi.Response{StatusCode: 200, Body: []byte(responseBody)}

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterId", Source: configloader.StringSource("event.id")},
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/{{ .clusterId }}",
				})},
				{Name: "clusterName", Source: configloader.StringSource("clusterData.name"), Type: "string"},
				{Name: "generationId", Source: configloader.StringSource("clusterData.generation"), Type: "int"},
				{Name: "clusterPhase", Source: configloader.StringSource("clusterData.status.phase")},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)

		assert.Equal(t, "my-cluster", execCtx.Params["clusterName"])
		assert.Equal(t, int64(5), execCtx.Params["generationId"])
		assert.Equal(t, "Active", execCtx.Params["clusterPhase"])
	})

	t.Run("chained api_calls — second URL uses first param", func(t *testing.T) {
		mockClient := newMockAPIClient()
		mockClient.GetResponse = &hyperfleetapi.Response{
			StatusCode: 200,
			Body:       []byte(`{"defaultNodePool":"np-primary","replicas":3}`),
		}

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterId", Source: configloader.StringSource("event.id")},
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/{{ .clusterId }}",
				})},
				{Name: "defaultNodePool", Source: configloader.StringSource("clusterData.defaultNodePool")},
				{Name: "nodePoolData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/{{ .clusterId }}/nodepools/{{ .defaultNodePool }}",
				})},
				{Name: "nodeCount", Source: configloader.StringSource("nodePoolData.replicas"), Type: "int"},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)

		assert.Equal(t, "np-primary", execCtx.Params["defaultNodePool"])
		assert.Equal(t, int64(3), execCtx.Params["nodeCount"])
		require.Len(t, mockClient.Requests, 2)
		assert.Contains(t, mockClient.Requests[1].URL, "np-primary")
	})

	t.Run("CEL expression source over resolved params", func(t *testing.T) {
		mockClient := newMockAPIClient()
		mockClient.GetResponse = &hyperfleetapi.Response{StatusCode: 200, Body: []byte(responseBody)}

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterId", Source: configloader.StringSource("event.id")},
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/{{ .clusterId }}",
				})},
				{Name: "isActive", Source: configloader.ExpressionSource(`clusterData.status.phase == "Active"`)},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)

		assert.Equal(t, true, execCtx.Params["isActive"])
	})

	t.Run("required api_call failure returns error", func(t *testing.T) {
		mockClient := newMockAPIClient()
		mockClient.GetError = fmt.Errorf("connection refused")

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/x",
				}), Required: true},
			},
		}
		_, err := runParamExtraction(t, config, mockClient, eventData)
		require.Error(t, err)
	})

	t.Run("non-required api_call failure falls back to default", func(t *testing.T) {
		mockClient := newMockAPIClient()
		mockClient.GetError = fmt.Errorf("connection refused")

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/x",
				}), Default: map[string]interface{}{"name": "fallback"}},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)
		assert.Equal(t, map[string]interface{}{"name": "fallback"}, execCtx.Params["clusterData"])
	})

	t.Run("required api_call without API client returns error", func(t *testing.T) {
		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/x",
				}), Required: true},
			},
		}
		_, err := runParamExtraction(t, config, nil, eventData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no API client")
	})

	t.Run("optional api_call without API client falls back to default", func(t *testing.T) {
		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/x",
				}), Default: map[string]interface{}{"name": "fallback"}},
			},
		}
		execCtx, err := runParamExtraction(t, config, nil, eventData)
		require.NoError(t, err)
		assert.Equal(t, map[string]interface{}{"name": "fallback"}, execCtx.Params["clusterData"])
	})

	t.Run("api_call 200 with non-JSON body returns error", func(t *testing.T) {
		mockClient := newMockAPIClient()
		mockClient.GetResponse = &hyperfleetapi.Response{
			StatusCode: 200,
			Body:       []byte("not json at all"),
		}

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterData", Source: configloader.APICallSource(&configloader.APICall{
					Method: "GET", URL: "/clusters/x",
				}), Required: true},
			},
		}
		_, err := runParamExtraction(t, config, mockClient, eventData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse API response as JSON")
	})

	t.Run("dot-notation on non-map param returns error", func(t *testing.T) {
		mockClient := newMockAPIClient()

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "clusterId", Source: configloader.StringSource("event.id")},
				{Name: "clusterIdField", Source: configloader.StringSource("clusterId.nested"), Required: true},
			},
		}
		_, err := runParamExtraction(t, config, mockClient, eventData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not a map")
	})
}

// TestParamExtractor_FileSource tests params with source: file
func TestParamExtractor_FileSource(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	eventData := map[string]interface{}{"id": "cluster-123"}
	mockClient := newMockAPIClient()

	t.Run("successful file read", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokenPath := filepath.Join(tmpDir, "token")
		require.NoError(t, os.WriteFile(tokenPath, []byte("my-secret-token"), 0o600))

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "token", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: tokenPath,
				}), Required: true},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)
		assert.Equal(t, "my-secret-token", execCtx.Params["token"])
	})

	t.Run("file read with trim=true (default) strips whitespace", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokenPath := filepath.Join(tmpDir, "token")
		require.NoError(t, os.WriteFile(tokenPath, []byte("  my-token\n"), 0o600))

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "token", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: tokenPath,
				})},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)
		assert.Equal(t, "my-token", execCtx.Params["token"])
	})

	t.Run("file read with trim=false preserves whitespace", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokenPath := filepath.Join(tmpDir, "token")
		require.NoError(t, os.WriteFile(tokenPath, []byte("  my-token\n"), 0o600))

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "token", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: tokenPath,
					Trim: boolPtr(false),
				})},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)
		assert.Equal(t, "  my-token\n", execCtx.Params["token"])
	})

	t.Run("file not found returns error", func(t *testing.T) {
		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "token", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: "/nonexistent/path/token",
				}), Required: true},
			},
		}
		_, err := runParamExtraction(t, config, mockClient, eventData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/nonexistent/path/token")
		assert.Contains(t, err.Error(), "token")
	})

	t.Run("required empty file returns error", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokenPath := filepath.Join(tmpDir, "empty")
		require.NoError(t, os.WriteFile(tokenPath, []byte(""), 0o600))

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "token", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: tokenPath,
				}), Required: true},
			},
		}
		_, err := runParamExtraction(t, config, mockClient, eventData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("optional empty file returns empty string", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokenPath := filepath.Join(tmpDir, "empty")
		require.NoError(t, os.WriteFile(tokenPath, []byte(""), 0o600))

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "token", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: tokenPath,
				}), Required: false},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)
		// Empty string is stored (value is not nil, no default to override)
		val, ok := execCtx.Params["token"]
		assert.True(t, ok, "empty optional file value should be stored")
		assert.Equal(t, "", val)
	})

	t.Run("file exceeds max size returns error", func(t *testing.T) {
		tmpDir := t.TempDir()
		bigPath := filepath.Join(tmpDir, "big")
		// Write 1MB + 1 byte
		data := make([]byte, 1<<20+1)
		require.NoError(t, os.WriteFile(bigPath, data, 0o600))

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "bigFile", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: bigPath,
				}), Required: true},
			},
		}
		_, err := runParamExtraction(t, config, mockClient, eventData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds maximum size")
	})

	t.Run("non-required file failure falls back to default", func(t *testing.T) {
		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "token", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: "/nonexistent/path/token",
				}), Required: false, Default: "fallback-token"},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)
		assert.Equal(t, "fallback-token", execCtx.Params["token"])
	})

	t.Run("file source with type conversion", func(t *testing.T) {
		tmpDir := t.TempDir()
		numPath := filepath.Join(tmpDir, "count")
		require.NoError(t, os.WriteFile(numPath, []byte("42\n"), 0o600))

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "count", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: numPath,
				}), Type: "int", Required: true},
			},
		}
		execCtx, err := runParamExtraction(t, config, mockClient, eventData)
		require.NoError(t, err)
		assert.Equal(t, int64(42), execCtx.Params["count"])
	})

	t.Run("required whitespace-only file with trim=false returns error", func(t *testing.T) {
		tmpDir := t.TempDir()
		wsPath := filepath.Join(tmpDir, "whitespace")
		require.NoError(t, os.WriteFile(wsPath, []byte("  \n\t"), 0o600))

		config := &configloader.Config{
			Params: []configloader.Parameter{
				{Name: "token", Source: configloader.FileSource(&configloader.FileSourceConfig{
					Path: wsPath,
					Trim: boolPtr(false),
				}), Required: true},
			},
		}
		_, err := runParamExtraction(t, config, mockClient, eventData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})
}

// TestSequentialExecution_Preconditions tests that preconditions stop on first failure
func TestSequentialExecution_Preconditions(t *testing.T) {
	tests := []struct {
		name             string
		expectedLastName string
		preconditions    []configloader.Precondition
		expectedResults  int // number of results before stopping
		expectError      bool
		expectNotMet     bool
	}{
		{
			name: "all pass - all executed",
			preconditions: []configloader.Precondition{
				{ActionBase: configloader.ActionBase{Name: "precond1"}, Expression: "true"},
				{ActionBase: configloader.ActionBase{Name: "precond2"}, Expression: "true"},
				{ActionBase: configloader.ActionBase{Name: "precond3"}, Expression: "true"},
			},
			expectedResults:  3,
			expectError:      false,
			expectNotMet:     false,
			expectedLastName: "precond3",
		},
		{
			name: "first fails - stops immediately",
			preconditions: []configloader.Precondition{
				{ActionBase: configloader.ActionBase{Name: "precond1"}, Expression: "false"},
				{ActionBase: configloader.ActionBase{Name: "precond2"}, Expression: "true"},
				{ActionBase: configloader.ActionBase{Name: "precond3"}, Expression: "true"},
			},
			expectedResults:  1,
			expectError:      false,
			expectNotMet:     true,
			expectedLastName: "precond1",
		},
		{
			name: "second fails - first executes, stops at second",
			preconditions: []configloader.Precondition{
				{ActionBase: configloader.ActionBase{Name: "precond1"}, Expression: "true"},
				{ActionBase: configloader.ActionBase{Name: "precond2"}, Expression: "false"},
				{ActionBase: configloader.ActionBase{Name: "precond3"}, Expression: "true"},
			},
			expectedResults:  2,
			expectError:      false,
			expectNotMet:     true,
			expectedLastName: "precond2",
		},
		{
			name: "third fails - first two execute, stops at third",
			preconditions: []configloader.Precondition{
				{ActionBase: configloader.ActionBase{Name: "precond1"}, Expression: "true"},
				{ActionBase: configloader.ActionBase{Name: "precond2"}, Expression: "true"},
				{ActionBase: configloader.ActionBase{Name: "precond3"}, Expression: "false"},
			},
			expectedResults:  3,
			expectError:      false,
			expectNotMet:     true,
			expectedLastName: "precond3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &configloader.Config{
				Adapter: configloader.AdapterInfo{
					Name:    "test-adapter",
					Version: "1.0.0",
				},
				Preconditions: tt.preconditions,
			}

			exec, err := NewBuilder().
				WithConfig(config).
				WithAPIClient(newMockAPIClient()).
				WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
				Build()
			if err != nil {
				t.Fatalf("unexpected error creating executor: %v", err)
			}

			ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-event-seq")
			result := exec.Execute(ctx, map[string]interface{}{})

			// Verify number of precondition results
			assert.Equal(t, tt.expectedResults, len(result.PreconditionResults),
				"unexpected precondition result count")

			// Verify last executed precondition name
			if len(result.PreconditionResults) > 0 {
				lastResult := result.PreconditionResults[len(result.PreconditionResults)-1]
				if lastResult.Name != tt.expectedLastName {
					t.Errorf("expected last precondition to be '%s', got '%s'",
						tt.expectedLastName, lastResult.Name)
				}
			}

			// Verify error/not met status
			if tt.expectNotMet {
				// Precondition not met is a successful execution, just with resources skipped
				assert.Equal(t, StatusSuccess, result.Status, "expected status Success (precondition not met is valid outcome)")
				assert.True(t, result.ResourcesSkipped, "ResourcesSkipped")
				assert.NotEmpty(t, result.SkipReason, "expected SkipReason to be set")
			}

			if !tt.expectNotMet && !tt.expectError {
				assert.Equal(t, StatusSuccess, result.Status, "expected status Success")
			}
		})
	}
}

func TestPreconditionExecutor_WithoutAPIClient(t *testing.T) {
	preconditions := []configloader.Precondition{{ActionBase: configloader.ActionBase{
		Name:    "clusterCheck",
		APICall: &configloader.APICall{Method: "GET", URL: "/clusters/x"},
	}}}
	execCtx := NewExecutionContext(t.Context(), map[string]interface{}{}, nil)

	outcome := newPreconditionExecutor(&ExecutorConfig{}).ExecuteAll(t.Context(), preconditions, execCtx)

	require.Error(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "no API client")
	assert.False(t, outcome.AllMatched)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, StatusFailed, outcome.Results[0].Status)
}

// TestPrecondition_CustomCELFunctions tests that custom CEL functions
// (like now()) are available in precondition expressions
func TestPrecondition_CustomCELFunctions(t *testing.T) {
	tests := []struct {
		name        string
		expression  string
		shouldMatch bool
	}{
		{
			name:        "now() returns valid timestamp",
			expression:  `timestamp(now()).getFullYear() >= 2024`,
			shouldMatch: true,
		},
		{
			name:        "now() can be used in time comparisons",
			expression:  `(timestamp(now()) - timestamp("2020-01-01T00:00:00Z")).getSeconds() > 0`,
			shouldMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &configloader.Config{
				Adapter: configloader.AdapterInfo{
					Name:    "test-adapter",
					Version: "1.0.0",
				},
				Preconditions: []configloader.Precondition{
					{ActionBase: configloader.ActionBase{Name: "test-custom-function"}, Expression: tt.expression},
				},
			}

			exec, err := NewBuilder().
				WithConfig(config).
				WithAPIClient(newMockAPIClient()).
				WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
				Build()
			require.NoError(t, err, "failed to create executor")

			ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-custom-cel")
			result := exec.Execute(ctx, map[string]interface{}{})

			// Verify precondition executed
			require.Len(t, result.PreconditionResults, 1, "expected one precondition result")
			precondResult := result.PreconditionResults[0]

			// Verify the expression evaluated correctly
			assert.Equal(t, tt.shouldMatch, precondResult.Matched, "unexpected match result")
			assert.Equal(t, StatusSuccess, precondResult.Status, "expected precondition status to be success")

			if tt.shouldMatch {
				// If precondition matched, resources should have been attempted
				assert.Equal(t, StatusSuccess, result.Status, "expected overall status success")
				assert.False(t, result.ResourcesSkipped, "resources should not be skipped")
			}
		})
	}
}

// TestSequentialExecution_Resources tests that resources stop on first failure
func TestSequentialExecution_Resources(t *testing.T) {
	// Note: This test uses dry-run mode and focuses on the sequential logic
	// without requiring a real K8s cluster. Resource sequential execution is better
	// tested in integration tests with real K8s API.

	tests := []struct {
		name            string
		resources       []configloader.Resource
		expectedResults int
		expectFailure   bool
	}{
		{
			name: "single resource with valid manifest",
			resources: []configloader.Resource{
				{
					Name: "resource1",
					Manifest: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name": "test-cm",
						},
					},
				},
			},
			expectedResults: 1,
			expectFailure:   false,
		},
		{
			name: "first resource has no manifest - stops immediately",
			resources: []configloader.Resource{
				{Name: "resource1"}, // No manifest at all
				{
					Name: "resource2",
					Manifest: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name": "test-cm2",
						},
					},
				},
			},
			expectedResults: 1, // Stops at first failure
			expectFailure:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &configloader.Config{
				Adapter: configloader.AdapterInfo{
					Name:    "test-adapter",
					Version: "1.0.0",
				},
				Resources: tt.resources,
			}

			exec, err := NewBuilder().
				WithConfig(config).
				WithAPIClient(newMockAPIClient()).
				WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
				Build()
			if err != nil {
				t.Fatalf("unexpected error creating executor: %v", err)
			}

			ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-event-resources")
			result := exec.Execute(ctx, map[string]interface{}{})

			// Verify sequential stop-on-failure: number of results should match expected
			assert.Equal(t, tt.expectedResults, len(result.ResourceResults),
				"sequential execution should stop at failure")

			// Verify failure status
			if tt.expectFailure {
				if result.Status == StatusSuccess {
					t.Error("expected execution to fail but got success")
				}
			}
		})
	}
}

// TestSequentialExecution_PostActions tests that post actions stop on first failure
func TestSequentialExecution_PostActions(t *testing.T) {
	tests := []struct {
		mockError       error
		mockResponse    *hyperfleetapi.Response
		name            string
		postActions     []configloader.PostAction
		expectedResults int
		expectError     bool
	}{
		{
			name: "all log actions succeed",
			postActions: []configloader.PostAction{
				{ActionBase: configloader.ActionBase{Name: "log1", Log: &configloader.LogAction{Message: "msg1"}}},
				{ActionBase: configloader.ActionBase{Name: "log2", Log: &configloader.LogAction{Message: "msg2"}}},
				{ActionBase: configloader.ActionBase{Name: "log3", Log: &configloader.LogAction{Message: "msg3"}}},
			},
			expectedResults: 3,
			expectError:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			postConfig := &configloader.PostConfig{
				PostActions: tt.postActions,
			}

			config := &configloader.Config{
				Adapter: configloader.AdapterInfo{
					Name:    "test-adapter",
					Version: "1.0.0",
				},
				Post: postConfig,
			}

			mockClient := newMockAPIClient()
			mockClient.GetResponse = tt.mockResponse
			mockClient.GetError = tt.mockError
			mockClient.PostResponse = tt.mockResponse
			mockClient.PostError = tt.mockError

			exec, err := NewBuilder().
				WithConfig(config).
				WithAPIClient(mockClient).
				WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
				Build()
			if err != nil {
				t.Fatalf("unexpected error creating executor: %v", err)
			}

			ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-event-post")
			result := exec.Execute(ctx, map[string]interface{}{})

			// Verify number of post action results
			assert.Equal(t, tt.expectedResults, len(result.PostActionResults),
				"unexpected post action result count")

			// Verify error expectation
			if tt.expectError {
				assert.NotEmpty(t, result.Errors, "expected errors, got none")
				assert.NotNil(t, result.Errors[PhasePostActions], "expected post_actions error, got %#v", result.Errors)
			} else {
				assert.Empty(t, result.Errors, "expected no errors, got %#v", result.Errors)
			}
		})
	}
}

// TestSequentialExecution_SkipReasonCapture tests that SkipReason captures which precondition wasn't met
func TestSequentialExecution_SkipReasonCapture(t *testing.T) {
	tests := []struct {
		name           string
		expectedStatus ExecutionStatus
		preconditions  []configloader.Precondition
		expectSkipped  bool
	}{
		{
			name: "first precondition not met",
			preconditions: []configloader.Precondition{
				{ActionBase: configloader.ActionBase{Name: "check1"}, Expression: "false"},
				{ActionBase: configloader.ActionBase{Name: "check2"}, Expression: "true"},
				{ActionBase: configloader.ActionBase{Name: "check3"}, Expression: "true"},
			},
			expectedStatus: StatusSuccess, // Successful execution, just resources skipped
			expectSkipped:  true,
		},
		{
			name: "second precondition not met",
			preconditions: []configloader.Precondition{
				{ActionBase: configloader.ActionBase{Name: "check1"}, Expression: "true"},
				{ActionBase: configloader.ActionBase{Name: "check2"}, Expression: "false"},
				{ActionBase: configloader.ActionBase{Name: "check3"}, Expression: "true"},
			},
			expectedStatus: StatusSuccess, // Successful execution, just resources skipped
			expectSkipped:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &configloader.Config{
				Adapter: configloader.AdapterInfo{
					Name:    "test-adapter",
					Version: "1.0.0",
				},
				Preconditions: tt.preconditions,
			}

			exec, err := NewBuilder().
				WithConfig(config).
				WithAPIClient(newMockAPIClient()).
				WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
				Build()
			if err != nil {
				t.Fatalf("unexpected error creating executor: %v", err)
			}

			ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-event-skip")
			result := exec.Execute(ctx, map[string]interface{}{})

			// Verify execution status is success (adapter executed successfully)
			if result.Status != tt.expectedStatus {
				t.Errorf("expected status %s, got %s", tt.expectedStatus, result.Status)
			}

			// Verify resources were skipped
			if tt.expectSkipped {
				assert.True(t, result.ResourcesSkipped, "ResourcesSkipped")
				assert.NotEmpty(t, result.SkipReason, "expected SkipReason to be set")
				// Verify execution context captures skip information
				if result.ExecutionContext != nil {
					assert.True(t, result.ExecutionContext.Adapter.ResourcesSkipped, "adapter.ResourcesSkipped")
				}
			}
		})
	}
}

func TestExecute_PendingDeleteIsNotAnError(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	_, err := store.CreateDeleteDesire(ctx, desire.DeleteDesire{
		Identity: testDesireID.Delete(),
		Owner:    desireOwner,
	})
	require.NoError(t, err)

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "v0.1.0"},
		Transports: map[string]configloader.TransportDefinition{
			desireTransportName: testRemoteDefinition(),
		},
		Resources: []configloader.Resource{
			newDesireResourceWithLifecycle("", "Background"),
		},
	}
	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(transportclient.Registry{
			desireTransportName: testRemoteEntry(desireTransportName, desireclient.NewClient(store, desireOwner)),
		}).
		Build()
	require.NoError(t, err)

	result := exec.Execute(ctx, map[string]interface{}{"id": "pending-delete"})
	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	assert.Empty(t, result.Errors)
	require.Len(t, result.ResourceResults, 1)
	assert.Equal(t, applyDeletePendingReason, result.ResourceResults[0].OperationReason)
	assert.Nil(t, result.ResourceResults[0].Error)
}

// TestPreconditionAPIFailure_ExecutionStatusRemainsFailed verifies that when a precondition
// API call fails, adapter.executionStatus stays "failed" and is not overwritten to "success".
// This is a regression test for a bug where SetSkipped() was called after SetError(),
// resetting executionStatus and causing Health CEL expressions to evaluate incorrectly.
func TestPreconditionAPIFailure_ExecutionStatusRemainsFailed(t *testing.T) {
	// Configure mock to return an error on GET (simulating precondition API failure)
	mockClient := newMockAPIClient()
	mockClient.GetError = fmt.Errorf("connection refused")
	mockClient.GetResponse = nil

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{
			Name:    "test-adapter",
			Version: "1.0.0",
		},
		Clients: configloader.ClientsConfig{
			HyperfleetAPI: configloader.HyperfleetAPIConfig{
				BaseURL: "http://mock-api:8000",
				Version: "v1",
			},
		},
		Params: []configloader.Parameter{
			{Name: "clusterId", Source: configloader.StringSource("event.id"), Required: true},
		},
		Preconditions: []configloader.Precondition{
			{
				ActionBase: configloader.ActionBase{
					Name: "clusterStatus",
					APICall: &configloader.APICall{
						Method:  "GET",
						URL:     "/clusters/{{ .clusterId }}",
						Timeout: "2s",
					},
				},
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(mockClient).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-precond-fail")
	result := exec.Execute(ctx, map[string]interface{}{"id": "cluster-123"})

	// Verify overall result status is failed
	assert.Equal(t, StatusFailed, result.Status, "expected overall status to be failed")
	assert.True(t, result.ResourcesSkipped, "resources should be skipped on precondition failure")

	// Critical assertion: verify adapter.executionStatus is "failed", not "success"
	require.NotNil(t, result.ExecutionContext, "execution context should be present")
	assert.Equal(t, string(StatusFailed), result.ExecutionContext.Adapter.ExecutionStatus,
		"adapter.executionStatus must remain 'failed' after precondition API failure")

	// Verify error information is preserved
	assert.Equal(t, "PreconditionFailed", result.ExecutionContext.Adapter.ErrorReason,
		"adapter.errorReason should be 'PreconditionFailed'")
	assert.NotEmpty(t, result.ExecutionContext.Adapter.ErrorMessage,
		"adapter.errorMessage should contain the error details")

	// Verify skip metadata is also set (for CEL expressions that check resourcesSkipped)
	assert.True(t, result.ExecutionContext.Adapter.ResourcesSkipped,
		"adapter.resourcesSkipped should be true")
	assert.NotEmpty(t, result.ExecutionContext.Adapter.SkipReason,
		"adapter.skipReason should be set")
}

// TestPreconditionCapture_NamedMapVariable verifies Option 1: the full API response is
// exposed as a named map variable in the capture CEL context under the precondition name,
// enabling safe optional-field access via dig() and has().
func TestPreconditionCapture_NamedMapVariable(t *testing.T) {
	responseWithField := `{"name":"cluster-1","deleted_time":"2026-04-14T10:00:00Z"}`
	responseWithoutField := `{"name":"cluster-1"}`

	tests := []struct {
		name         string
		responseBody string
		wantValue    interface{}
		captureExpr  string
		wantCaptured bool
	}{
		{
			name:         "dig() returns value when field present",
			responseBody: responseWithField,
			captureExpr:  `dig(fetchCluster, "deleted_time") != null`,
			wantValue:    true,
			wantCaptured: true,
		},
		{
			name:         "dig() returns false when field absent - no error",
			responseBody: responseWithoutField,
			captureExpr:  `dig(fetchCluster, "deleted_time") != null`,
			wantValue:    false,
			wantCaptured: true,
		},
		{
			name:         "has() returns true when field present",
			responseBody: responseWithField,
			captureExpr:  `has(fetchCluster.deleted_time)`,
			wantValue:    true,
			wantCaptured: true,
		},
		{
			name:         "has() returns false when field absent - no error",
			responseBody: responseWithoutField,
			captureExpr:  `has(fetchCluster.deleted_time)`,
			wantValue:    false,
			wantCaptured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := newMockAPIClient()
			mockClient.GetResponse = &hyperfleetapi.Response{
				StatusCode: 200,
				Status:     "200 OK",
				Body:       []byte(tt.responseBody),
			}

			config := &configloader.Config{
				Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
				Clients: configloader.ClientsConfig{
					HyperfleetAPI: configloader.HyperfleetAPIConfig{
						BaseURL: "http://mock-api:8000",
						Version: "v1",
					},
				},
				Preconditions: []configloader.Precondition{
					{
						ActionBase: configloader.ActionBase{
							Name: "fetchCluster",
							APICall: &configloader.APICall{
								Method:  "GET",
								URL:     "/clusters/test",
								Timeout: "2s",
							},
						},
						Capture: []configloader.CaptureField{
							{
								Name:               "is_deleting",
								FieldExpressionDef: configloader.FieldExpressionDef{Expression: tt.captureExpr},
							},
						},
					},
				},
			}

			exec, err := NewBuilder().
				WithConfig(config).
				WithAPIClient(mockClient).
				WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
				Build()
			require.NoError(t, err)

			ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-named-map")
			result := exec.Execute(ctx, map[string]interface{}{})

			require.Equal(t, StatusSuccess, result.Status)
			require.Len(t, result.PreconditionResults, 1)
			captured := result.PreconditionResults[0].CapturedFields
			if tt.wantCaptured {
				assert.Equal(t, tt.wantValue, captured["is_deleting"],
					"captured is_deleting should be %v", tt.wantValue)
			}
		})
	}
}

// TestPreconditionCapture_FieldDefault verifies Option 2: when a field: capture is absent
// from the API response, the configured Default is used and no WARN is logged.
// Expression captures are unaffected by Default.
func TestPreconditionCapture_FieldDefault(t *testing.T) {
	responseWithField := `{"name":"cluster-1","status_code":"active"}`
	responseWithoutField := `{"name":"cluster-1"}`

	tests := []struct {
		name         string
		responseBody string
		wantValue    interface{}
		capture      configloader.CaptureField
		wantCaptured bool
	}{
		{
			name:         "field present - default not used",
			responseBody: responseWithField,
			capture: configloader.CaptureField{
				Name:               "statusCode",
				Default:            "unknown",
				FieldExpressionDef: configloader.FieldExpressionDef{Field: "status_code"},
			},
			wantValue:    "active",
			wantCaptured: true,
		},
		{
			name:         "field absent with default - uses default, no WARN",
			responseBody: responseWithoutField,
			capture: configloader.CaptureField{
				Name:               "statusCode",
				Default:            "unknown",
				FieldExpressionDef: configloader.FieldExpressionDef{Field: "status_code"},
			},
			wantValue:    "unknown",
			wantCaptured: true,
		},
		{
			name:         "field absent without default - value is nil",
			responseBody: responseWithoutField,
			capture: configloader.CaptureField{
				Name:               "statusCode",
				FieldExpressionDef: configloader.FieldExpressionDef{Field: "status_code"},
			},
			wantValue:    nil,
			wantCaptured: true,
		},
		{
			name:         "bool default false when field absent",
			responseBody: responseWithoutField,
			capture: configloader.CaptureField{
				Name:               "is_deleting",
				Default:            false,
				FieldExpressionDef: configloader.FieldExpressionDef{Field: "deleted_time"},
			},
			wantValue:    false,
			wantCaptured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := newMockAPIClient()
			mockClient.GetResponse = &hyperfleetapi.Response{
				StatusCode: 200,
				Status:     "200 OK",
				Body:       []byte(tt.responseBody),
			}

			config := &configloader.Config{
				Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
				Clients: configloader.ClientsConfig{
					HyperfleetAPI: configloader.HyperfleetAPIConfig{
						BaseURL: "http://mock-api:8000",
						Version: "v1",
					},
				},
				Preconditions: []configloader.Precondition{
					{
						ActionBase: configloader.ActionBase{
							Name: "fetchCluster",
							APICall: &configloader.APICall{
								Method:  "GET",
								URL:     "/clusters/test",
								Timeout: "2s",
							},
						},
						Capture: []configloader.CaptureField{tt.capture},
					},
				},
			}

			exec, err := NewBuilder().
				WithConfig(config).
				WithAPIClient(mockClient).
				WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
				Build()
			require.NoError(t, err)

			ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-field-default")
			result := exec.Execute(ctx, map[string]interface{}{})

			require.Equal(t, StatusSuccess, result.Status)
			require.Len(t, result.PreconditionResults, 1)
			captured := result.PreconditionResults[0].CapturedFields
			assert.Equal(t, tt.wantValue, captured[tt.capture.Name],
				"captured %s should be %v", tt.capture.Name, tt.wantValue)
		})
	}
}

func TestPreconditionCapture_ExtractErrorFailsPhase(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.GetResponse = &hyperfleetapi.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Body:       []byte(`{"name":"cluster-1"}`),
	}

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Clients: configloader.ClientsConfig{
			HyperfleetAPI: configloader.HyperfleetAPIConfig{
				BaseURL: "http://mock-api:8000",
				Version: "v1",
			},
		},
		Preconditions: []configloader.Precondition{
			{
				ActionBase: configloader.ActionBase{
					Name: "fetchCluster",
					APICall: &configloader.APICall{
						Method:  "GET",
						URL:     "/clusters/test",
						Timeout: "2s",
					},
				},
				Capture: []configloader.CaptureField{
					{
						Name: "bad",
						FieldExpressionDef: configloader.FieldExpressionDef{
							Field:      "name",
							Expression: "true",
						},
					},
				},
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(mockClient).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	result := exec.Execute(context.Background(), map[string]interface{}{})

	require.Equal(t, StatusFailed, result.Status)
	require.Error(t, result.Errors[PhasePreconditions])
	assert.Contains(t, result.Errors[PhasePreconditions].Error(), "mutually exclusive")
	require.NotNil(t, result.ExecutionContext)
	require.NotNil(t, result.ExecutionContext.Adapter.ExecutionError)
	assert.Equal(t, "PreconditionFailed", result.ExecutionContext.Adapter.ExecutionError.Phase)
}

// TestPrecondition404_GracefulStop verifies that when a precondition API call returns 404
// (resource no longer exists), the executor stops gracefully: status remains "success",
// resources are skipped, and no error state is set.
func new404PreconditionConfig() *configloader.Config {
	return &configloader.Config{
		Adapter: configloader.AdapterInfo{
			Name:    "test-adapter",
			Version: "1.0.0",
		},
		Clients: configloader.ClientsConfig{
			HyperfleetAPI: configloader.HyperfleetAPIConfig{
				BaseURL: "http://mock-api:8000",
				Version: "v1",
			},
		},
		Params: []configloader.Parameter{
			{Name: "clusterId", Source: configloader.StringSource("event.id"), Required: true},
		},
		Preconditions: []configloader.Precondition{
			{
				ActionBase: configloader.ActionBase{
					Name: "clusterStatus",
					APICall: &configloader.APICall{
						Method:  "GET",
						URL:     "/clusters/{{ .clusterId }}",
						Timeout: "2s",
					},
				},
			},
		},
	}
}

func new404PostActionConfig() *configloader.Config {
	cfg := new404PreconditionConfig()
	cfg.Post = &configloader.PostConfig{
		PostActions: []configloader.PostAction{
			{
				ActionBase: configloader.ActionBase{
					Name: "reportStatus",
					APICall: &configloader.APICall{
						Method:  "PUT",
						URL:     "/clusters/{{ .clusterId }}/statuses",
						Body:    `{"status":"done"}`,
						Timeout: "2s",
					},
				},
			},
		},
	}
	return cfg
}

func TestPrecondition404_GracefulStop(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.GetResponse = mock404Response()

	config := new404PreconditionConfig()
	exec := build404TestExecutor(t, config, mockClient)

	ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-precond-404")
	result := exec.Execute(ctx, map[string]interface{}{"id": "cluster-gone"})

	assert.Equal(t, StatusSuccess, result.Status,
		"404 on resource should not mark execution as failed")

	assert.True(t, result.ResourcesSkipped, "resources should be skipped")
	assert.Equal(t, ResourceNotFoundReason, result.SkipReason,
		"skip reason should be ResourceNotFoundReason")

	assert.Empty(t, result.Errors, "no errors should be recorded for a 404")
	assert.Equal(t, "", result.ExecutionContext.Adapter.ErrorReason,
		"adapter.errorReason should be empty for a 404")
	assert.Equal(t, "", result.ExecutionContext.Adapter.ErrorMessage,
		"adapter.errorMessage should be empty for a 404")
	assert.Nil(t, result.ExecutionContext.Adapter.ExecutionError,
		"adapter.executionError should be nil for a 404")
}

// TestPostAction404_GracefulHandling verifies that when a post-action API call returns 404
// (resource no longer exists), the executor does not mark the execution as failed.
func TestPostAction404_GracefulHandling(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.GetResponse = &hyperfleetapi.Response{
		StatusCode: 200,
		Body:       []byte(`{"id":"cluster-456","status":{"conditions":[{"type":"Reconciled","status":"False"}]}}`),
	}
	mockClient.PutResponse = mock404Response()

	config := new404PostActionConfig()
	exec := build404TestExecutor(t, config, mockClient)

	ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-postaction-404")
	result := exec.Execute(ctx, map[string]interface{}{"id": "cluster-gone"})

	// Status should be success; 404 in post-actions is gracefully handled
	assert.Equal(t, StatusSuccess, result.Status,
		"404 in post-actions should result in success, not failure")

	// No post-action errors should be recorded
	assert.Nil(t, result.Errors[PhasePostActions],
		"no post_actions error should be recorded for a 404")

	// Verify adapter metadata is consistent
	require.NotNil(t, result.ExecutionContext)
	assert.Equal(t, string(StatusSuccess), result.ExecutionContext.Adapter.ExecutionStatus,
		"adapter.executionStatus should be success for a post-action 404")
	assert.True(t, result.ExecutionContext.Adapter.ResourcesSkipped,
		"adapter.resourcesSkipped should be true")
	assert.Equal(t, ResourceNotFoundReason, result.SkipReason,
		"skip reason should be ResourceNotFound")
	assert.Nil(t, result.ExecutionContext.Adapter.ExecutionError,
		"adapter.executionError should be nil for a post-action 404")
}

// TestPrecondition404_SkipsPostActions verifies that when a precondition returns 404,
// post-actions are never attempted (no PUT/POST requests after the 404 GET).
func TestPrecondition404_SkipsPostActions(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.GetResponse = mock404Response()

	config := new404PostActionConfig()
	exec := build404TestExecutor(t, config, mockClient)

	ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-precond-404-skips-post")
	_ = exec.Execute(ctx, map[string]interface{}{"id": "cluster-gone"})

	// Only the precondition GET should have been made; no PUT for post-actions
	var getCalls, putCalls int
	for _, req := range mockClient.Requests {
		switch req.Method {
		case "GET":
			getCalls++
		case "PUT":
			putCalls++
		}
	}
	assert.Equal(t, 1, getCalls,
		"expected exactly 1 GET call (precondition)")
	assert.Equal(t, 0, putCalls,
		"expected 0 PUT calls (post-actions should not run after precondition 404)")
}

// TestPreconditionFail_PostAction404 verifies that when preconditions fail with a non-404 error
// and post-actions also return 404, the execution status remains failed (not masked by ResourceNotFound)
// and the original error context is preserved.
func TestPreconditionFail_PostAction404(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.GetResponse, mockClient.GetError = mockErrorResponse(500)
	mockClient.PutResponse = mock404Response()

	config := new404PostActionConfig()
	exec := build404TestExecutor(t, config, mockClient)

	ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-precond-fail-post-404")
	result := exec.Execute(ctx, map[string]interface{}{"id": "cluster-gone"})

	// Status stays failed (precondition error is the primary failure)
	assert.Equal(t, StatusFailed, result.Status,
		"status should remain failed from precondition error")

	// SkipReason gets overwritten to ResourceNotFound (resource no longer exists, overrides PreconditionFailed)
	assert.Equal(t, ResourceNotFoundReason, result.SkipReason,
		"skip reason should be ResourceNotFound when post-action 404 overwrites it")
	assert.True(t, result.ResourcesSkipped, "resources should be skipped")

	// The precondition error should still be recorded
	assert.NotNil(t, result.Errors[PhasePreconditions],
		"precondition error should still be recorded")

	// No post-action error recorded (404 is handled gracefully)
	assert.Nil(t, result.Errors[PhasePostActions],
		"post-action 404 should not add an error")
}

// TestPreconditionFail_PostActionFail_KeepsFirstExecutionError verifies that when
// a precondition fails and a later post-action also fails, adapter.executionError
// (and therefore CEL) keeps the original precondition failure.
func TestPreconditionFail_PostActionFail_KeepsFirstExecutionError(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.GetResponse, mockClient.GetError = mockErrorResponse(500)
	mockClient.PutResponse, mockClient.PutError = mockErrorResponse(500)

	config := new404PostActionConfig()
	exec := build404TestExecutor(t, config, mockClient)

	result := exec.Execute(context.Background(), map[string]interface{}{"id": "cluster-1"})

	require.Equal(t, StatusFailed, result.Status)
	require.Error(t, result.Errors[PhasePreconditions])
	require.Error(t, result.Errors[PhasePostActions])

	execErr := result.ExecutionContext.Adapter.ExecutionError
	require.NotNil(t, execErr)
	assert.Equal(t, string(PhasePreconditions), execErr.Phase)
	assert.Equal(t, "clusterStatus", execErr.Step)

	vars := result.ExecutionContext.GetCELVariables()
	adapter, ok := vars["adapter"].(map[string]interface{})
	require.True(t, ok)
	celExecErr, ok := adapter["executionError"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, string(PhasePreconditions), celExecErr["phase"])
	assert.Equal(t, "clusterStatus", celExecErr["step"])
	assert.Equal(t, execErr.Message, celExecErr["message"])
}

// TestPreconditionBrokenURL404_ReportsError verifies that when a
// precondition API call returns 404 due to a misconfigured URL (error
// code HYPERFLEET-NTF-000), the adapter treats it as an error rather than a graceful stop.
func TestPreconditionBrokenURL404_ReportsError(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.GetResponse = mockBrokenEndpoint404Response()

	config := new404PreconditionConfig()
	exec := build404TestExecutor(t, config, mockClient)

	ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-broken-url-404")
	result := exec.Execute(ctx, map[string]interface{}{"id": "cls-123"})

	assert.Equal(t, StatusFailed, result.Status,
		"broken URL 404 should mark execution as failed")

	assert.True(t, result.ResourcesSkipped,
		"resources should be skipped on precondition failure")

	assert.NotEmpty(t, result.Errors,
		"errors should be recorded for a broken URL 404")
}

// TestPostActionBrokenURL404_ReportsError verifies that when a post-action
// API call returns 404 due to a misconfigured URL (error code
// HYPERFLEET-NTF-000), the adapter treats it as an error instead of a graceful stop.
func TestPostActionBrokenURL404_ReportsError(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.GetResponse = &hyperfleetapi.Response{
		StatusCode: 200,
		Body: []byte(
			`{"id":"cluster-456","status":{"conditions":` +
				`[{"type":"Reconciled","status":"False"}]}}`,
		),
	}
	mockClient.PutResponse = mockBrokenEndpoint404Response()

	config := new404PostActionConfig()
	exec := build404TestExecutor(t, config, mockClient)

	ctx := hfl.Set(context.Background(), logctx.EventIDKey, "test-post-broken-url-404")
	result := exec.Execute(ctx, map[string]interface{}{"id": "cls-123"})

	assert.Equal(t, StatusFailed, result.Status,
		"broken URL 404 in post-actions should mark execution as failed")

	assert.NotNil(t, result.Errors[PhasePostActions],
		"post-action error should be recorded for a broken URL 404")
}

// TestGetCELVariables_AllNamespaces verifies that GetCELVariables includes all required namespaces
func TestGetCELVariables_AllNamespaces(t *testing.T) {
	t.Setenv("CEL_TEST_VAR", "cel-test-value")

	eventData := map[string]interface{}{
		"id":   "cluster-abc",
		"kind": "ManagedCluster",
	}
	config := &configloader.Config{Adapter: configloader.AdapterInfo{Name: "cel-adapter", Version: "2.0.0"}}
	execCtx := NewExecutionContext(t.Context(), eventData, config)
	execCtx.Params["myParam"] = "param-value"
	configMap, err := configToMap(config.Redacted())
	require.NoError(t, err)
	execCtx.configVariable = configMap

	vars := execCtx.GetCELVariables()

	require.Contains(t, vars, "event", "GetCELVariables must include 'event'")
	eventMap, ok := vars["event"].(map[string]interface{})
	require.True(t, ok, "event must be a map")
	assert.Equal(t, "cluster-abc", eventMap["id"])
	assert.Equal(t, "ManagedCluster", eventMap["kind"])

	require.Contains(t, vars, "env", "GetCELVariables must include 'env'")
	envMap, ok := vars["env"].(map[string]interface{})
	require.True(t, ok, "env must be a map")
	assert.Equal(t, "cel-test-value", envMap["CEL_TEST_VAR"])

	// params are available as top-level names
	assert.Equal(t, "param-value", vars["myParam"])

	// config is the redacted config map
	assert.Equal(t, configMap, vars["config"])

	// adapter contains the adapter name and version and the execution metadata
	adapterMap, ok := vars["adapter"].(map[string]any)
	require.True(t, ok, "adapter must be a map")
	assert.Equal(t, "cel-adapter", adapterMap["name"])
	assert.Equal(t, "2.0.0", adapterMap["version"])
	assert.Equal(t, string(StatusSuccess), adapterMap["executionStatus"])

	// existing namespaces must still be present
	assert.Contains(t, vars, "resources")
	assert.Contains(t, vars, "resource_states")
}

// TestCELExpression_EnvVariable verifies env.* is accessible in a precondition CEL expression
func TestCELExpression_EnvVariable(t *testing.T) {
	t.Setenv("EXPECTED_REGION", "us-east-1")

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Preconditions: []configloader.Precondition{
			{
				ActionBase: configloader.ActionBase{Name: "checkRegion"},
				Expression: `env.EXPECTED_REGION == "us-east-1"`,
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	result := exec.Execute(context.Background(), map[string]interface{}{"id": "cluster-env-test"})

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.PreconditionResults, 1)
	assert.True(t, result.PreconditionResults[0].Matched, "env.EXPECTED_REGION CEL expression should match")
}

// TestCELExpression_EventVariable verifies event.* is accessible in a precondition CEL expression
func TestCELExpression_EventVariable(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Preconditions: []configloader.Precondition{
			{
				ActionBase: configloader.ActionBase{Name: "checkEventKind"},
				Expression: `event.kind == "ManagedCluster"`,
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	eventData := map[string]interface{}{
		"id":   "cluster-event-test",
		"kind": "ManagedCluster",
	}
	result := exec.Execute(context.Background(), eventData)

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.PreconditionResults, 1)
	assert.True(t, result.PreconditionResults[0].Matched, "event.kind CEL expression should match")
}

// TestCELExpression_EnvInPostActionWhen verifies env.* works in a post-action when gate
func TestCELExpression_EnvInPostActionWhen(t *testing.T) {
	t.Setenv("SKIP_POST_ACTION", "true")

	mockClient := newMockAPIClient()
	mockClient.PutResponse = &hyperfleetapi.Response{StatusCode: 200, Body: []byte(`{}`)}

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Post: &configloader.PostConfig{
			PostActions: []configloader.PostAction{
				{
					ActionBase: configloader.ActionBase{
						Name: "conditionalAction",
						APICall: &configloader.APICall{
							Method: "PUT",
							URL:    "/clusters/test/status",
							Body:   `{}`,
						},
					},
					When: &configloader.PostActionWhen{Expression: `env.SKIP_POST_ACTION == "false"`},
				},
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(mockClient).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	result := exec.Execute(context.Background(), map[string]interface{}{"id": "cluster-env-when"})

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.PostActionResults, 1)
	assert.True(t, result.PostActionResults[0].Skipped,
		"post-action should be skipped when env.SKIP_POST_ACTION != \"false\"")
	assert.Empty(t, mockClient.Requests, "API call should not be made when action is skipped")
}

// TestCELExpression_EventInPostActionWhen verifies event.* works in a post-action when gate
func TestCELExpression_EventInPostActionWhen(t *testing.T) {
	mockClient := newMockAPIClient()
	mockClient.PutResponse = &hyperfleetapi.Response{StatusCode: 200, Body: []byte(`{}`)}

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Post: &configloader.PostConfig{
			PostActions: []configloader.PostAction{
				{
					ActionBase: configloader.ActionBase{
						Name: "gatedAction",
						APICall: &configloader.APICall{
							Method: "PUT",
							URL:    "/clusters/test/status",
							Body:   `{}`,
						},
					},
					When: &configloader.PostActionWhen{Expression: `event.kind == "ManagedCluster"`},
				},
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(mockClient).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	eventData := map[string]interface{}{
		"id":   "cluster-event-when",
		"kind": "ManagedCluster",
	}
	result := exec.Execute(context.Background(), eventData)

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.PostActionResults, 1)
	assert.False(t, result.PostActionResults[0].Skipped,
		"post-action should NOT be skipped when event.kind matches")
	assert.Len(t, mockClient.Requests, 1, "API call should be made when when-condition is true")
}

// TestCELExpression_EnvInParamExpression verifies env.* works in a param source.expression
func TestCELExpression_EnvInParamExpression(t *testing.T) {
	t.Setenv("CLUSTER_REGION", "eu-west-1")

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Params: []configloader.Parameter{
			{Name: "region", Source: configloader.ExpressionSource(`env.CLUSTER_REGION`)},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	result := exec.Execute(context.Background(), map[string]interface{}{"id": "cluster-env-param"})

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	assert.Equal(t, "eu-west-1", result.Params["region"])
}

// TestCELExpression_EventInParamExpression verifies event.* works in a param source.expression
func TestCELExpression_EventInParamExpression(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Params: []configloader.Parameter{
			{Name: "derivedID", Source: configloader.ExpressionSource(`"cluster-" + event.id`)},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	result := exec.Execute(context.Background(), map[string]interface{}{"id": "abc"})

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	assert.Equal(t, "cluster-abc", result.Params["derivedID"])
}

// TestGoTemplate_EnvInManifest verifies that {{ .env.X }} resolves in resource manifest templates.
func TestGoTemplate_EnvInManifest(t *testing.T) {
	t.Setenv("TPL_REGION", "us-east-1")

	mockClient := k8sclient.NewMockK8sClient()
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test", Version: "1.0.0"},
		Resources: []configloader.Resource{
			{
				Name: "testResource",
				Manifest: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name":      "test-cm",
						"namespace": "{{ .env.TPL_REGION }}",
					},
				},
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(mockClient)).
		Build()
	require.NoError(t, err)

	result := exec.Execute(context.Background(), map[string]interface{}{"id": "cluster-123"})

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	applied := mockClient.Resources["us-east-1/test-cm"]
	require.NotNil(t, applied, "resource should be applied with rendered namespace")
	assert.Equal(t, "us-east-1", applied.GetNamespace())
	assert.Equal(t, "test-cm", applied.GetName())
}

// TestGoTemplate_EventInManifest verifies that {{ .event.X }} resolves in resource manifest templates.
func TestGoTemplate_EventInManifest(t *testing.T) {
	mockClient := k8sclient.NewMockK8sClient()
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test", Version: "1.0.0"},
		Resources: []configloader.Resource{
			{
				Name: "testResource",
				Manifest: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name":      "{{ .event.id }}",
						"namespace": "default",
					},
				},
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(mockClient)).
		Build()
	require.NoError(t, err)

	eventData := map[string]interface{}{
		"id":   "cluster-123",
		"kind": "ManagedCluster",
	}
	result := exec.Execute(context.Background(), eventData)

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	applied := mockClient.Resources["default/cluster-123"]
	require.NotNil(t, applied, "resource should be applied with rendered name")
	assert.Equal(t, "default", applied.GetNamespace())
	assert.Equal(t, "cluster-123", applied.GetName())
}

// TestExecutionContext_AdapterVariableSharedByTemplatesAndCEL verifies that Go
// templates and CEL read the same adapter map: name, version and execution metadata.
func TestExecutionContext_AdapterVariableSharedByTemplatesAndCEL(t *testing.T) {
	config := &configloader.Config{Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.2.3"}}
	execCtx := NewExecutionContext(t.Context(), map[string]any{"id": "cluster-1"}, config)
	execCtx.SetError("X", "m")

	templateAdapter, ok := execCtx.templateVariables()[configloader.FieldAdapter].(map[string]any)
	require.True(t, ok)
	celAdapter, ok := execCtx.GetCELVariables()[configloader.FieldAdapter].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, celAdapter, templateAdapter)
	assert.Equal(t, "test-adapter", templateAdapter["name"])
	assert.Equal(t, "1.2.3", templateAdapter["version"])
	assert.Equal(t, string(StatusFailed), templateAdapter["executionStatus"])

	t.Run("nil config keeps name and version keys", func(t *testing.T) {
		adapter := NewExecutionContext(t.Context(), nil, nil).templateVariables()[configloader.FieldAdapter]
		assert.Subset(t, adapter, map[string]any{"name": "", "version": ""})
	})
}

// TestCELExpression_AdapterNameInPrecondition verifies adapter.name is readable from CEL.
func TestCELExpression_AdapterNameInPrecondition(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Preconditions: []configloader.Precondition{
			{
				ActionBase: configloader.ActionBase{Name: "checkAdapterName"},
				Expression: `adapter.name == "test-adapter"`,
			},
		},
	}

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(newMockAPIClient()).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	result := exec.Execute(t.Context(), map[string]any{"id": "cluster-adapter-name"})

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	require.Len(t, result.PreconditionResults, 1)
	assert.True(t, result.PreconditionResults[0].Matched, "adapter.name CEL expression should match")
}

// statusPayloadConfig returns a config whose single post action PUTs the
// statusPayload payload built from the given template values.
func statusPayloadConfig(build map[string]any, resources []configloader.Resource) *configloader.Config {
	return &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Params: []configloader.Parameter{
			{Name: "clusterId", Source: configloader.StringSource("event.id")},
		},
		Resources: resources,
		Post: &configloader.PostConfig{
			Payloads: []configloader.Payload{{Name: "statusPayload", Build: build}},
			PostActions: []configloader.PostAction{{
				ActionBase: configloader.ActionBase{
					Name: "reportStatus",
					APICall: &configloader.APICall{
						Method: "PUT",
						URL:    "/clusters/{{ .clusterId }}/statuses",
						Body:   "{{ .statusPayload }}",
					},
				},
			}},
		},
	}
}

// putRequestBody returns the decoded body of the only PUT request.
func putRequestBody(t *testing.T, mockClient *hyperfleetapi.MockClient) map[string]any {
	t.Helper()
	var body map[string]any
	for _, req := range mockClient.Requests {
		if req.Method == http.MethodPut {
			require.Nil(t, body, "expected a single PUT request")
			require.NoError(t, json.Unmarshal(req.Body, &body))
		}
	}
	require.NotNil(t, body, "expected a PUT request")
	return body
}

// TestGoTemplate_AdapterMetadataInPostPayload verifies that templates read the
// current execution metadata, not a copy from the param extraction phase.
func TestGoTemplate_AdapterMetadataInPostPayload(t *testing.T) {
	k8sClient := k8sclient.NewMockK8sClient()
	k8sClient.ApplyResourceError = fmt.Errorf("apply failed")
	resources := []configloader.Resource{{
		Name: "testResource",
		Manifest: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "test-cm", "namespace": "default"},
		},
	}}
	config := statusPayloadConfig(map[string]any{
		"status":  "{{ .adapter.executionStatus }}",
		"adapter": "{{ .adapter.name }}",
	}, resources)
	mockClient := newMockAPIClient()

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(mockClient).
		WithTransportRegistry(testTransportRegistry(k8sClient)).
		Build()
	require.NoError(t, err)

	result := exec.Execute(t.Context(), map[string]any{"id": "cluster-1"})

	require.Equal(t, StatusFailed, result.Status)
	require.Error(t, result.Errors[PhaseResources])
	require.NoError(t, result.Errors[PhasePostActions])
	body := putRequestBody(t, mockClient)
	assert.Equal(t, string(StatusFailed), body["status"])
	assert.Equal(t, "test-adapter", body["adapter"])
}

// TestExecutionContext_TemplateAndCELVariablesAgree verifies that templates and CEL
// read the same value for every built-in variable and every param, and that the
// runtime adds every name the config validator reserves. When you add a name to
// the configloader lists, also add it to the runtime. It also verifies that only
// CEL reads resources and resource_states.
func TestExecutionContext_TemplateAndCELVariablesAgree(t *testing.T) {
	config := &configloader.Config{Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"}}
	execCtx := NewExecutionContext(t.Context(), map[string]any{"id": "cluster-1"}, config)
	execCtx.configVariable = map[string]any{"adapter": map[string]any{"name": "test-adapter"}}
	execCtx.Params["clusterId"] = "cluster-1"

	templateVars, celVars := execCtx.templateVariables(), execCtx.GetCELVariables()

	// ObjectsAreEqual and key checks keep the environment map out of failure messages.
	for _, name := range slices.Concat(configloader.BuiltinVariables(), []string{"clusterId"}) {
		_, found := templateVars[name]
		assert.True(t, found, "%q missing from template variables", name)
		assert.True(t, assert.ObjectsAreEqual(templateVars[name], celVars[name]),
			"%q differs between template and CEL variables", name)
	}
	for _, name := range configloader.ReservedVariableNames() {
		_, found := celVars[name]
		assert.True(t, found, "reserved %q missing from CEL variables", name)
	}
	for _, name := range []string{configloader.FieldResources, configloader.FieldResourceStates} {
		_, inTemplates := templateVars[name]
		assert.False(t, inTemplates, "%q must be CEL-only", name)
	}
}

// TestExecute_BuiltinVariablesNotInParams verifies that Params does not hold
// adapter, config, env and event, and that templates can still read them.
func TestExecute_BuiltinVariablesNotInParams(t *testing.T) {
	config := statusPayloadConfig(map[string]any{
		"configAdapterName": "{{ .config.adapter.name }}",
	}, nil)
	mockClient := newMockAPIClient()

	exec, err := NewBuilder().
		WithConfig(config).
		WithAPIClient(mockClient).
		WithTransportRegistry(testTransportRegistry(k8sclient.NewMockK8sClient())).
		Build()
	require.NoError(t, err)

	result := exec.Execute(t.Context(), map[string]any{"id": "cluster-1"})

	require.Equal(t, StatusSuccess, result.Status, "errors=%v", result.Errors)
	assert.Equal(t, "cluster-1", result.Params["clusterId"])
	assert.Contains(t, result.Params, "statusPayload")
	for _, name := range configloader.BuiltinVariables() {
		// Check only the key. A failure message that prints Params shows the whole environment.
		_, found := result.Params[name]
		assert.False(t, found, "built-in %q must not be stored in params", name)
	}
	assert.Equal(t, "test-adapter", putRequestBody(t, mockClient)["configAdapterName"])
}
