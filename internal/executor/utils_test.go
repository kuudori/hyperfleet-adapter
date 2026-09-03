package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/criteria"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	apierrors "github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestValidateAPIResponse_NilError_SuccessResponse(t *testing.T) {
	resp := &hyperfleetapi.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Body:       []byte(`{"status":"ok"}`),
		Attempts:   1,
		Duration:   100 * time.Millisecond,
	}

	err := ValidateAPIResponse(resp, nil, "GET", "http://example.com/api")

	assert.NoError(t, err)
}

func TestValidateAPIResponse_NilError_NilResponse(t *testing.T) {
	err := ValidateAPIResponse(nil, nil, "GET", "http://example.com/api")

	require.Error(t, err)

	// Should be wrapped as APIError
	apiErr, ok := apierrors.IsAPIError(err)
	require.True(t, ok, "Expected error to be APIError")
	assert.Equal(t, "GET", apiErr.Method)
	assert.Equal(t, "http://example.com/api", apiErr.URL)
	assert.Equal(t, 0, apiErr.StatusCode)
	assert.Contains(t, apiErr.Error(), "nil")
}

func TestValidateAPIResponse_WithError_AlreadyAPIError(t *testing.T) {
	// If error is already an APIError, it should be returned as-is
	originalErr := apierrors.NewAPIError(
		"POST",
		"http://example.com/api/create",
		503,
		"503 Service Unavailable",
		[]byte("service down"),
		3,
		5*time.Second,
		errors.New("connection refused"),
	)

	err := ValidateAPIResponse(nil, originalErr, "GET", "http://other.com")

	require.Error(t, err)

	// Should be the same error, not re-wrapped
	apiErr, ok := apierrors.IsAPIError(err)
	require.True(t, ok)
	assert.Equal(t, "POST", apiErr.Method) // Original method preserved
	assert.Equal(t, "http://example.com/api/create", apiErr.URL)
	assert.Equal(t, 503, apiErr.StatusCode)
}

func TestValidateAPIResponse_WithError_NonAPIError(t *testing.T) {
	// Non-APIError should be wrapped
	originalErr := errors.New("network timeout")

	err := ValidateAPIResponse(nil, originalErr, "PUT", "http://example.com/api/update")

	require.Error(t, err)

	apiErr, ok := apierrors.IsAPIError(err)
	require.True(t, ok, "Expected error to be wrapped as APIError")
	assert.Equal(t, "PUT", apiErr.Method)
	assert.Equal(t, "http://example.com/api/update", apiErr.URL)
	assert.Equal(t, 0, apiErr.StatusCode) // No status code for network errors
	assert.True(t, errors.Is(err, originalErr), "Original error should be unwrappable")
}

func TestValidateAPIResponse_NonSuccessStatusCodes(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		body        []byte
		statusCode  int
		expectError bool
	}{
		{
			name:        "400 Bad Request",
			statusCode:  400,
			status:      "400 Bad Request",
			body:        []byte(`{"error":"invalid input"}`),
			expectError: true,
		},
		{
			name:        "401 Unauthorized",
			statusCode:  401,
			status:      "401 Unauthorized",
			body:        []byte(`{"error":"invalid token"}`),
			expectError: true,
		},
		{
			name:        "403 Forbidden",
			statusCode:  403,
			status:      "403 Forbidden",
			body:        nil,
			expectError: true,
		},
		{
			name:        "404 Not Found",
			statusCode:  404,
			status:      "404 Not Found",
			body:        []byte(`{"message":"resource not found"}`),
			expectError: true,
		},
		{
			name:        "429 Too Many Requests",
			statusCode:  429,
			status:      "429 Too Many Requests",
			body:        []byte(`{"retry_after":60}`),
			expectError: true,
		},
		{
			name:        "500 Internal Server Error",
			statusCode:  500,
			status:      "500 Internal Server Error",
			body:        []byte(`{"error":"internal error"}`),
			expectError: true,
		},
		{
			name:        "502 Bad Gateway",
			statusCode:  502,
			status:      "502 Bad Gateway",
			body:        nil,
			expectError: true,
		},
		{
			name:        "503 Service Unavailable",
			statusCode:  503,
			status:      "503 Service Unavailable",
			body:        []byte("service temporarily unavailable"),
			expectError: true,
		},
		{
			name:        "504 Gateway Timeout",
			statusCode:  504,
			status:      "504 Gateway Timeout",
			body:        nil,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &hyperfleetapi.Response{
				StatusCode: tt.statusCode,
				Status:     tt.status,
				Body:       tt.body,
				Attempts:   1,
				Duration:   50 * time.Millisecond,
			}

			err := ValidateAPIResponse(resp, nil, "GET", "http://example.com/api")

			if tt.expectError {
				require.Error(t, err)

				apiErr, ok := apierrors.IsAPIError(err)
				require.True(t, ok, "Expected error to be APIError")

				assert.Equal(t, tt.statusCode, apiErr.StatusCode)
				assert.Equal(t, tt.status, apiErr.Status)
				assert.Equal(t, "GET", apiErr.Method)
				assert.Equal(t, "http://example.com/api", apiErr.URL)

				if len(tt.body) > 0 {
					assert.NotContains(t, apiErr.Error(), string(tt.body))
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateAPIResponse_SuccessStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		statusCode int
	}{
		{
			name:       "200 OK",
			statusCode: 200,
			status:     "200 OK",
		},
		{
			name:       "201 Created",
			statusCode: 201,
			status:     "201 Created",
		},
		{
			name:       "202 Accepted",
			statusCode: 202,
			status:     "202 Accepted",
		},
		{
			name:       "204 No Content",
			statusCode: 204,
			status:     "204 No Content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &hyperfleetapi.Response{
				StatusCode: tt.statusCode,
				Status:     tt.status,
				Body:       nil,
				Attempts:   1,
				Duration:   30 * time.Millisecond,
			}

			err := ValidateAPIResponse(resp, nil, "POST", "http://example.com/api/create")

			assert.NoError(t, err)
		})
	}
}

func TestValidateAPIResponse_PreservesAttempts(t *testing.T) {
	resp := &hyperfleetapi.Response{
		StatusCode: 500,
		Status:     "500 Internal Server Error",
		Body:       []byte("error"),
		Attempts:   5,
		Duration:   10 * time.Second,
	}

	err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

	require.Error(t, err)
	apiErr, ok := apierrors.IsAPIError(err)
	require.True(t, ok)

	assert.Equal(t, 5, apiErr.Attempts)
	assert.Equal(t, 10*time.Second, apiErr.Duration)
}

func TestValidateAPIResponse_AllHTTPMethods(t *testing.T) {
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			resp := &hyperfleetapi.Response{
				StatusCode: 404,
				Status:     "404 Not Found",
			}

			err := ValidateAPIResponse(resp, nil, method, "http://example.com")

			require.Error(t, err)
			apiErr, ok := apierrors.IsAPIError(err)
			require.True(t, ok)
			assert.Equal(t, method, apiErr.Method)
		})
	}
}

func TestValidateAPIResponse_URLPreserved(t *testing.T) {
	urls := []string{
		"http://localhost:8080/api/v1/clusters",
		"https://api.example.com/resources/123",
		"http://service.namespace.svc.cluster.local:9090/health",
		"http://10.0.0.1:3000/path/to/resource?query=value",
	}

	for _, url := range urls {
		t.Run(url, func(t *testing.T) {
			resp := &hyperfleetapi.Response{
				StatusCode: 500,
				Status:     "500 Internal Server Error",
			}

			err := ValidateAPIResponse(resp, nil, "GET", url)

			require.Error(t, err)
			apiErr, ok := apierrors.IsAPIError(err)
			require.True(t, ok)
			assert.Equal(t, url, apiErr.URL)
			assert.Contains(t, apiErr.Error(), url)
		})
	}
}

func TestValidateAPIResponse_WrappedErrorChain(t *testing.T) {
	// Test that error wrapping works correctly for error inspection
	innerErr := fmt.Errorf("connection reset by peer")
	wrappedErr := fmt.Errorf("dial failed: %w", innerErr)

	err := ValidateAPIResponse(nil, wrappedErr, "GET", "http://example.com")

	require.Error(t, err)

	// Should be an APIError
	apiErr, ok := apierrors.IsAPIError(err)
	require.True(t, ok)

	// The underlying error chain should be preserved
	assert.Contains(t, apiErr.Error(), "connection reset")
}

func TestValidateAPIResponse_ErrorMessageContainsContext(t *testing.T) {
	resp := &hyperfleetapi.Response{
		StatusCode: 503,
		Status:     "503 Service Unavailable",
		Body:       []byte(`{"message":"database connection failed","retry_after":30}`),
		Attempts:   3,
		Duration:   9 * time.Second,
	}

	err := ValidateAPIResponse(resp, nil, "POST", "http://api.example.com/clusters")

	require.Error(t, err)

	errMsg := err.Error()
	assert.Contains(t, errMsg, "POST")
	assert.Contains(t, errMsg, "http://api.example.com/clusters")
	assert.Contains(t, errMsg, "503")
	assert.Contains(t, errMsg, "3") // attempts
}

func TestValidateAPIResponse_APIErrorHelpers(t *testing.T) {
	t.Run("IsServerError", func(t *testing.T) {
		resp := &hyperfleetapi.Response{StatusCode: 500, Status: "500 Internal Server Error"}
		err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

		apiErr, _ := apierrors.IsAPIError(err)
		assert.True(t, apiErr.IsServerError())
		assert.False(t, apiErr.IsClientError())
	})

	t.Run("IsClientError", func(t *testing.T) {
		resp := &hyperfleetapi.Response{StatusCode: 400, Status: "400 Bad Request"}
		err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

		apiErr, _ := apierrors.IsAPIError(err)
		assert.True(t, apiErr.IsClientError())
		assert.False(t, apiErr.IsServerError())
	})

	t.Run("IsNotFound", func(t *testing.T) {
		resp := &hyperfleetapi.Response{StatusCode: 404, Status: "404 Not Found"}
		err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

		apiErr, _ := apierrors.IsAPIError(err)
		assert.True(t, apiErr.IsNotFound())
	})

	t.Run("IsUnauthorized", func(t *testing.T) {
		resp := &hyperfleetapi.Response{StatusCode: 401, Status: "401 Unauthorized"}
		err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

		apiErr, _ := apierrors.IsAPIError(err)
		assert.True(t, apiErr.IsUnauthorized())
	})

	t.Run("IsForbidden", func(t *testing.T) {
		resp := &hyperfleetapi.Response{StatusCode: 403, Status: "403 Forbidden"}
		err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

		apiErr, _ := apierrors.IsAPIError(err)
		assert.True(t, apiErr.IsForbidden())
	})

	t.Run("IsRateLimited", func(t *testing.T) {
		resp := &hyperfleetapi.Response{StatusCode: 429, Status: "429 Too Many Requests"}
		err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

		apiErr, _ := apierrors.IsAPIError(err)
		assert.True(t, apiErr.IsRateLimited())
	})

	t.Run("IsBadRequest", func(t *testing.T) {
		resp := &hyperfleetapi.Response{StatusCode: 400, Status: "400 Bad Request"}
		err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

		apiErr, _ := apierrors.IsAPIError(err)
		assert.True(t, apiErr.IsBadRequest())
	})

	t.Run("IsConflict", func(t *testing.T) {
		resp := &hyperfleetapi.Response{StatusCode: 409, Status: "409 Conflict"}
		err := ValidateAPIResponse(resp, nil, "POST", "http://example.com")

		apiErr, _ := apierrors.IsAPIError(err)
		assert.True(t, apiErr.IsConflict())
	})
}

func TestValidateAPIResponse_DoesNotRetainResponseBody(t *testing.T) {
	resp := &hyperfleetapi.Response{
		StatusCode: 500,
		Status:     "500 Internal Server Error",
		Body:       []byte(`{"error":"database timeout","code":"DB_TIMEOUT"}`),
	}

	err := ValidateAPIResponse(resp, nil, "GET", "http://example.com")

	apiErr, _ := apierrors.IsAPIError(err)

	assert.NotContains(t, apiErr.Error(), `{"error":"database timeout","code":"DB_TIMEOUT"}`)
}

// TestToConditionDefs tests the conversion of configloader conditions to criteria definitions
func TestToConditionDefs(t *testing.T) {
	tests := []struct {
		name       string
		expected   []criteria.ConditionDef
		conditions []configloader.Condition
	}{
		{
			name:       "empty conditions",
			conditions: []configloader.Condition{},
			expected:   []criteria.ConditionDef{},
		},
		{
			name: "single condition",
			conditions: []configloader.Condition{
				{Field: "status.phase", Operator: "equals", Value: "Running"},
			},
			expected: []criteria.ConditionDef{
				{Field: "status.phase", Operator: criteria.OperatorEquals, Value: "Running"},
			},
		},
		{
			name: "multiple conditions with camelCase operators",
			conditions: []configloader.Condition{
				{Field: "status.phase", Operator: "equals", Value: "Running"},
				{Field: "replicas", Operator: "greaterThan", Value: 0},
				{Field: "metadata.labels.app", Operator: "notEquals", Value: ""},
			},
			expected: []criteria.ConditionDef{
				{Field: "status.phase", Operator: criteria.OperatorEquals, Value: "Running"},
				{Field: "replicas", Operator: criteria.OperatorGreaterThan, Value: 0},
				{Field: "metadata.labels.app", Operator: criteria.OperatorNotEquals, Value: ""},
			},
		},
		{
			name: "all operator types",
			conditions: []configloader.Condition{
				{Field: "f1", Operator: "equals", Value: "v1"},
				{Field: "f2", Operator: "notEquals", Value: "v2"},
				{Field: "f3", Operator: "greaterThan", Value: 10},
				{Field: "f4", Operator: "lessThan", Value: 5},
				{Field: "f5", Operator: "contains", Value: "test"},
				{Field: "f6", Operator: "in", Value: []string{"a", "b"}},
			},
			expected: []criteria.ConditionDef{
				{Field: "f1", Operator: criteria.OperatorEquals, Value: "v1"},
				{Field: "f2", Operator: criteria.OperatorNotEquals, Value: "v2"},
				{Field: "f3", Operator: criteria.OperatorGreaterThan, Value: 10},
				{Field: "f4", Operator: criteria.OperatorLessThan, Value: 5},
				{Field: "f5", Operator: criteria.OperatorContains, Value: "test"},
				{Field: "f6", Operator: criteria.OperatorIn, Value: []string{"a", "b"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ToConditionDefs(tt.conditions)

			require.Len(t, result, len(tt.expected))
			for i, expected := range tt.expected {
				assert.Equal(t, expected.Field, result[i].Field)
				assert.Equal(t, expected.Operator, result[i].Operator)
				assert.Equal(t, expected.Value, result[i].Value)
			}
		})
	}
}

// TestExecutionErrorToMap tests conversion of ExecutionError to map
func TestExecutionErrorToMap(t *testing.T) {
	tests := []struct {
		expected interface{}
		execErr  *ExecutionError
		name     string
	}{
		{
			name:     "nil error",
			execErr:  nil,
			expected: nil,
		},
		{
			name: "error with all fields",
			execErr: &ExecutionError{
				Phase:   "preconditions",
				Step:    "check-cluster",
				Message: "Cluster not found",
			},
			expected: map[string]interface{}{
				"phase":   "preconditions",
				"step":    "check-cluster",
				"message": "Cluster not found",
			},
		},
		{
			name: "error with empty fields",
			execErr: &ExecutionError{
				Phase:   "",
				Step:    "",
				Message: "",
			},
			expected: map[string]interface{}{
				"phase":   "",
				"step":    "",
				"message": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := executionErrorToMap(tt.execErr)

			if tt.expected == nil {
				assert.Nil(t, result)
				return
			}

			expectedMap := tt.expected.(map[string]interface{})
			resultMap := result.(map[string]interface{})
			assert.Equal(t, expectedMap["phase"], resultMap["phase"])
			assert.Equal(t, expectedMap["step"], resultMap["step"])
			assert.Equal(t, expectedMap["message"], resultMap["message"])
		})
	}
}

// TestAdapterMetadataToMap tests conversion of AdapterMetadata to map
func TestAdapterMetadataToMap(t *testing.T) {
	tests := []struct {
		expected map[string]interface{}
		adapter  *AdapterMetadata
		name     string
	}{
		{
			name:     "nil adapter",
			adapter:  nil,
			expected: map[string]interface{}{},
		},
		{
			name: "success status",
			adapter: &AdapterMetadata{
				ExecutionStatus:  "success",
				ResourcesSkipped: false,
				SkipReason:       "",
				ErrorReason:      "",
				ErrorMessage:     "",
				ExecutionError:   nil,
			},
			expected: map[string]interface{}{
				"executionStatus":  "success",
				"resourcesSkipped": false,
				"skipReason":       "",
				"errorReason":      "",
				"errorMessage":     "",
				"executionError":   nil,
			},
		},
		{
			name: "skipped status",
			adapter: &AdapterMetadata{
				ExecutionStatus:  "success",
				ResourcesSkipped: true,
				SkipReason:       "Precondition 'check-status' not met",
				ErrorReason:      "",
				ErrorMessage:     "",
				ExecutionError:   nil,
			},
			expected: map[string]interface{}{
				"executionStatus":  "success",
				"resourcesSkipped": true,
				"skipReason":       "Precondition 'check-status' not met",
				"errorReason":      "",
				"errorMessage":     "",
				"executionError":   nil,
			},
		},
		{
			name: "failed status with error",
			adapter: &AdapterMetadata{
				ExecutionStatus:  "failed",
				ResourcesSkipped: false,
				SkipReason:       "",
				ErrorReason:      "APIError",
				ErrorMessage:     "API returned 500",
				ExecutionError: &ExecutionError{
					Phase:   "preconditions",
					Step:    "fetch-cluster",
					Message: "Connection refused",
				},
			},
			expected: map[string]interface{}{
				"executionStatus":  "failed",
				"resourcesSkipped": false,
				"skipReason":       "",
				"errorReason":      "APIError",
				"errorMessage":     "API returned 500",
				"executionError": map[string]interface{}{
					"phase":   "preconditions",
					"step":    "fetch-cluster",
					"message": "Connection refused",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := adapterMetadataToMap(tt.adapter)

			assert.Equal(t, tt.expected["executionStatus"], result["executionStatus"])
			assert.Equal(t, tt.expected["resourcesSkipped"], result["resourcesSkipped"])
			assert.Equal(t, tt.expected["skipReason"], result["skipReason"])
			assert.Equal(t, tt.expected["errorReason"], result["errorReason"])
			assert.Equal(t, tt.expected["errorMessage"], result["errorMessage"])

			if tt.expected["executionError"] == nil {
				assert.Nil(t, result["executionError"])
			} else {
				expectedErr := tt.expected["executionError"].(map[string]interface{})
				resultErr := result["executionError"].(map[string]interface{})
				assert.Equal(t, expectedErr["phase"], resultErr["phase"])
				assert.Equal(t, expectedErr["step"], resultErr["step"])
				assert.Equal(t, expectedErr["message"], resultErr["message"])
			}
		})
	}
}

// TestExecuteLogAction tests log action execution
func TestExecuteLogAction(t *testing.T) {
	tests := []struct {
		logAction  *configloader.LogAction
		params     map[string]interface{}
		name       string
		expectCall bool
	}{
		{
			name:       "nil log action",
			logAction:  nil,
			params:     map[string]interface{}{},
			expectCall: false,
		},
		{
			name:       "empty message",
			logAction:  &configloader.LogAction{Message: ""},
			params:     map[string]interface{}{},
			expectCall: false,
		},
		{
			name:       "simple message",
			logAction:  &configloader.LogAction{Message: "Hello World", Level: "info"},
			params:     map[string]interface{}{},
			expectCall: true,
		},
		{
			name:       "template message",
			logAction:  &configloader.LogAction{Message: "Processing cluster {{ .clusterId }}", Level: "info"},
			params:     map[string]interface{}{"clusterId": "cluster-123"},
			expectCall: true,
		},
		{
			name:       "debug level",
			logAction:  &configloader.LogAction{Message: "Debug info", Level: "debug"},
			params:     map[string]interface{}{},
			expectCall: true,
		},
		{
			name:       "warning level",
			logAction:  &configloader.LogAction{Message: "Warning message", Level: "warning"},
			params:     map[string]interface{}{},
			expectCall: true,
		},
		{
			name:       "error level",
			logAction:  &configloader.LogAction{Message: "Error occurred", Level: "error"},
			params:     map[string]interface{}{},
			expectCall: true,
		},
		{
			name:       "default level (empty)",
			logAction:  &configloader.LogAction{Message: "Default level", Level: ""},
			params:     map[string]interface{}{},
			expectCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCtx := &ExecutionContext{Params: tt.params}

			// This should not panic
			ExecuteLogAction(context.Background(), tt.logAction, execCtx)

			// We don't verify the exact log output, just that it doesn't error
		})
	}
}

func TestExecuteLogActionInvalidLevelFallsBackToInfo(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ExecuteLogAction(context.Background(), &configloader.LogAction{
		Message: "hello",
		Level:   "not-a-level",
	}, &ExecutionContext{Params: map[string]interface{}{}})

	out := buf.String()
	assert.Contains(t, out, "invalid log level")
	assert.Contains(t, out, "[config] hello")
}

func TestExecuteLogActionTemplateError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ExecuteLogAction(context.Background(), &configloader.LogAction{
		Message: "{{ .unterminated",
		Level:   "info",
	}, &ExecutionContext{Params: map[string]interface{}{}})

	out := buf.String()
	assert.Contains(t, out, "failed to render log message")
	assert.NotContains(t, out, "[config]")
}

// TestGetResourceAsMap tests resource to map conversion
func TestGetResourceAsMap(t *testing.T) {
	tests := []struct {
		expected map[string]interface{}
		resource *unstructured.Unstructured
		name     string
	}{
		{
			name:     "nil resource",
			resource: nil,
			expected: nil,
		},
		{
			name: "simple resource",
			resource: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":      "test-pod",
						"namespace": "default",
					},
				},
			},
			expected: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name":      "test-pod",
					"namespace": "default",
				},
			},
		},
		{
			name: "resource with status",
			resource: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"kind": "Deployment",
					"status": map[string]interface{}{
						"replicas":          3,
						"availableReplicas": 3,
						"conditions": []interface{}{
							map[string]interface{}{"type": "Available", "status": "True"},
						},
					},
				},
			},
			expected: map[string]interface{}{
				"kind": "Deployment",
				"status": map[string]interface{}{
					"replicas":          3,
					"availableReplicas": 3,
					"conditions": []interface{}{
						map[string]interface{}{"type": "Available", "status": "True"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetResourceAsMap(tt.resource)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildHyperfleetAPICallURL(t *testing.T) {
	tests := []struct {
		execCtx  *ExecutionContext
		name     string
		expected string
		url      string
	}{
		{
			name:     "empty URL returns empty",
			url:      "",
			execCtx:  &ExecutionContext{Config: &configloader.Config{}},
			expected: "",
		},
		{
			name:     "nil execCtx returns URL as-is",
			url:      "/clusters/123",
			execCtx:  nil,
			expected: "/clusters/123",
		},
		{
			name:     "nil config returns URL as-is",
			url:      "/clusters/123",
			execCtx:  &ExecutionContext{Config: nil},
			expected: "/clusters/123",
		},
		{
			name: "absolute URL matching baseURL extracts relative path",
			url:  "http://localhost:8000/api/hyperfleet/v1/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "absolute URL with baseURL containing path extracts relative path",
			url:  "http://localhost:8000/api/hyperfleet/v1/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000/api/hyperfleet/v1",
							Version: "v1",
						},
					},
				},
			},
			expected: "/clusters/abc123",
		},
		{
			name: "absolute URL with trailing slash in baseURL",
			url:  "http://localhost:8000/api/hyperfleet/v1/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000/api/hyperfleet/v1/",
							Version: "v1",
						},
					},
				},
			},
			expected: "/clusters/abc123",
		},
		{
			name: "absolute URL with different host returns as-is",
			url:  "http://other-host:9000/api/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "http://other-host:9000/api/clusters/abc123",
		},
		{
			name: "absolute URL with different scheme returns as-is",
			url:  "https://localhost:8000/api/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "https://localhost:8000/api/clusters/abc123",
		},
		{
			name: "relative path with api/ prefix returns with leading slash",
			url:  "api/hyperfleet/v1/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "relative path with /api/ prefix returns as-is",
			url:  "/api/hyperfleet/v1/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "simple relative path gets full API path added",
			url:  "clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "simple relative path with leading slash gets full API path added",
			url:  "/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "relative path with custom version",
			url:  "clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v2",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v2/clusters/abc123",
		},
		{
			name: "relative path with empty version defaults to v1",
			url:  "clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "relative path with empty baseURL returns as-is",
			url:  "clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "",
							Version: "v1",
						},
					},
				},
			},
			expected: "clusters/abc123",
		},
		{
			name: "path with trailing slashes gets cleaned",
			url:  "clusters/abc123/",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "path with dot segments gets cleaned",
			url:  "/clusters/../clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "absolute URL with empty baseURL returns as-is",
			url:  "http://localhost:8000/api/hyperfleet/v1/clusters/abc123",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "",
							Version: "v1",
						},
					},
				},
			},
			expected: "http://localhost:8000/api/hyperfleet/v1/clusters/abc123",
		},
		{
			name: "complex nested path",
			url:  "clusters/abc123/statuses",
			execCtx: &ExecutionContext{
				Config: &configloader.Config{
					Clients: configloader.ClientsConfig{
						HyperfleetAPI: configloader.HyperfleetAPIConfig{
							BaseURL: "http://localhost:8000",
							Version: "v1",
						},
					},
				},
			},
			expected: "/api/hyperfleet/v1/clusters/abc123/statuses",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildHyperfleetAPICallURL(tt.url, tt.execCtx)
			assert.Equal(t, tt.expected, result)
		})
	}
}
