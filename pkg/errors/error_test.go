package errors

import (
	"net/http"
	"testing"
)

func TestFind(t *testing.T) {
	tests := []struct {
		name       string
		wantReason string
		code       ServiceErrorCode
		wantHTTP   int
		shouldFind bool
	}{
		{
			name:       "find_not_found_error",
			code:       ErrorNotFound,
			shouldFind: true,
			wantReason: "Resource not found",
			wantHTTP:   http.StatusNotFound,
		},
		{
			name:       "find_validation_error",
			code:       ErrorValidation,
			shouldFind: true,
			wantReason: "General validation failure",
			wantHTTP:   http.StatusBadRequest,
		},
		{
			name:       "find_kubernetes_error",
			code:       ErrorKubernetesError,
			shouldFind: true,
			wantReason: "Kubernetes API error",
			wantHTTP:   http.StatusInternalServerError,
		},
		{
			name:       "find_nonexistent_error",
			code:       ServiceErrorCode(999),
			shouldFind: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, err := Find(tt.code)

			if found != tt.shouldFind {
				t.Errorf("Find() found = %v, want %v", found, tt.shouldFind)
			}

			if tt.shouldFind {
				if err == nil {
					t.Fatal("Find() returned nil error for valid code")
				}
				if err.Code != tt.code {
					t.Errorf("Expected code %d, got %d", tt.code, err.Code)
				}
				if err.Reason != tt.wantReason {
					t.Errorf("Expected reason '%s', got '%s'", tt.wantReason, err.Reason)
				}
				if err.HTTPCode != tt.wantHTTP {
					t.Errorf("Expected HTTP code %d, got %d", tt.wantHTTP, err.HTTPCode)
				}
			}
		})
	}
}

func TestErrors(t *testing.T) {
	t.Run("all_errors_defined", func(t *testing.T) {
		errors := Errors()

		// Should have 16 error codes defined
		expectedCount := 16
		if len(errors) != expectedCount {
			t.Errorf("Expected %d errors, got %d", expectedCount, len(errors))
		}

		// Check some key errors exist
		codes := map[ServiceErrorCode]bool{}
		for _, err := range errors {
			codes[err.Code] = true
		}

		requiredCodes := []ServiceErrorCode{
			ErrorNotFound,
			ErrorValidation,
			ErrorConflict,
			ErrorForbidden,
			ErrorUnauthorized,
			ErrorUnauthenticated,
			ErrorBadRequest,
			ErrorMalformedRequest,
			ErrorNotImplemented,
			ErrorGeneral,
			ErrorAdapterConfigNotFound,
			ErrorBrokerConnectionError,
			ErrorKubernetesError,
			ErrorHyperFleetAPIError,
			ErrorInvalidCloudEvent,
		}

		for _, code := range requiredCodes {
			if !codes[code] {
				t.Errorf("Error code %d not found in Errors()", code)
			}
		}
	})
}

func TestNew(t *testing.T) {
	tests := []struct {
		name         string
		reason       string
		wantReason   string
		values       []interface{}
		code         ServiceErrorCode
		wantCode     ServiceErrorCode
		wantHTTPCode int
	}{
		{
			name:         "create_simple_error",
			code:         ErrorNotFound,
			reason:       "Resource not found",
			values:       nil,
			wantCode:     ErrorNotFound,
			wantReason:   "Resource not found",
			wantHTTPCode: http.StatusNotFound,
		},
		{
			name:         "create_formatted_error",
			code:         ErrorKubernetesError,
			reason:       "Failed to create %s in namespace %s",
			values:       []interface{}{"Pod", "default"},
			wantCode:     ErrorKubernetesError,
			wantReason:   "Failed to create Pod in namespace default",
			wantHTTPCode: http.StatusInternalServerError,
		},
		{
			name:         "create_error_without_reason",
			code:         ErrorValidation,
			reason:       "",
			values:       nil,
			wantCode:     ErrorValidation,
			wantReason:   "General validation failure", // Should use default reason
			wantHTTPCode: http.StatusBadRequest,
		},
		{
			name:         "create_error_with_undefined_code",
			code:         ServiceErrorCode(999),
			reason:       "Custom reason",
			values:       nil,
			wantCode:     ErrorGeneral, // Should fallback to general error
			wantReason:   "Custom reason",
			wantHTTPCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := New(tt.code, tt.reason, tt.values...)

			if err == nil {
				t.Fatal("New() returned nil")
			}

			if err.Code != tt.wantCode {
				t.Errorf("Expected code %d, got %d", tt.wantCode, err.Code)
			}

			if err.Reason != tt.wantReason {
				t.Errorf("Expected reason '%s', got '%s'", tt.wantReason, err.Reason)
			}

			if err.HTTPCode != tt.wantHTTPCode {
				t.Errorf("Expected HTTP code %d, got %d", tt.wantHTTPCode, err.HTTPCode)
			}
		})
	}
}

func TestServiceError_Error(t *testing.T) {
	tests := []struct {
		name     string
		err      *ServiceError
		contains []string
	}{
		{
			name:     "error_string_format",
			err:      NotFound("Resource XYZ not found"),
			contains: []string{"hyperfleet-adapter-1", "Resource XYZ not found"},
		},
		{
			name:     "kubernetes_error_string",
			err:      KubernetesError("API call failed"),
			contains: []string{"hyperfleet-adapter-13", "API call failed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errStr := tt.err.Error()

			for _, expected := range tt.contains {
				if !containsString(errStr, expected) {
					t.Errorf("Error string '%s' doesn't contain '%s'", errStr, expected)
				}
			}
		})
	}
}

func TestServiceError_AsError(t *testing.T) {
	t.Run("convert_to_standard_error", func(t *testing.T) {
		serviceErr := NotFound("Item not found")
		stdErr := serviceErr.AsError()

		if stdErr == nil {
			t.Fatal("AsError() returned nil")
		}

		errMsg := stdErr.Error()
		if !containsString(errMsg, "Item not found") {
			t.Errorf("Standard error message doesn't contain original reason: %s", errMsg)
		}
	})
}

func TestServiceError_Is404(t *testing.T) {
	tests := []struct {
		err  *ServiceError
		name string
		want bool
	}{
		{
			name: "not_found_error_is_404",
			err:  NotFound("Not found"),
			want: true,
		},
		{
			name: "validation_error_is_not_404",
			err:  Validation("Invalid input"),
			want: false,
		},
		{
			name: "kubernetes_error_is_not_404",
			err:  KubernetesError("K8s error"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Is404(); got != tt.want {
				t.Errorf("Is404() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServiceError_IsConflict(t *testing.T) {
	tests := []struct {
		err  *ServiceError
		name string
		want bool
	}{
		{
			name: "conflict_error_is_conflict",
			err:  Conflict("Resource already exists"),
			want: true,
		},
		{
			name: "not_found_error_is_not_conflict",
			err:  NotFound("Not found"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.IsConflict(); got != tt.want {
				t.Errorf("IsConflict() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServiceError_IsForbidden(t *testing.T) {
	tests := []struct {
		err  *ServiceError
		name string
		want bool
	}{
		{
			name: "forbidden_error_is_forbidden",
			err:  Forbidden("Access denied"),
			want: true,
		},
		{
			name: "unauthorized_error_is_not_forbidden",
			err:  Unauthorized("Unauthorized"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.IsForbidden(); got != tt.want {
				t.Errorf("IsForbidden() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodeStr(t *testing.T) {
	tests := []struct {
		name string
		want string
		code ServiceErrorCode
	}{
		{
			name: "not_found_code_string",
			code: ErrorNotFound,
			want: "hyperfleet-adapter-1",
		},
		{
			name: "validation_code_string",
			code: ErrorValidation,
			want: "hyperfleet-adapter-2",
		},
		{
			name: "kubernetes_code_string",
			code: ErrorKubernetesError,
			want: "hyperfleet-adapter-13",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CodeStr(tt.code)
			if got == nil {
				t.Fatal("CodeStr() returned nil")
			}
			if *got != tt.want {
				t.Errorf("CodeStr() = %s, want %s", *got, tt.want)
			}
		})
	}
}

func TestHref(t *testing.T) {
	tests := []struct {
		name string
		want string
		code ServiceErrorCode
	}{
		{
			name: "not_found_href",
			code: ErrorNotFound,
			want: "/api/hyperfleet-adapter/v1/errors/1",
		},
		{
			name: "kubernetes_href",
			code: ErrorKubernetesError,
			want: "/api/hyperfleet-adapter/v1/errors/13",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Href(tt.code)
			if got == nil {
				t.Fatal("Href() returned nil")
			}
			if *got != tt.want {
				t.Errorf("Href() = %s, want %s", *got, tt.want)
			}
		})
	}
}

func TestHelperFunctions(t *testing.T) {
	tests := []struct {
		name         string
		fn           func(string, ...interface{}) *ServiceError
		reason       string
		args         []interface{}
		wantCode     ServiceErrorCode
		wantHTTPCode int
	}{
		{
			name:         "NotFound",
			fn:           NotFound,
			reason:       "Resource %s not found",
			args:         []interface{}{"Pod"},
			wantCode:     ErrorNotFound,
			wantHTTPCode: http.StatusNotFound,
		},
		{
			name:         "GeneralError",
			fn:           GeneralError,
			reason:       "Something went wrong",
			args:         nil,
			wantCode:     ErrorGeneral,
			wantHTTPCode: http.StatusInternalServerError,
		},
		{
			name:         "Unauthorized",
			fn:           Unauthorized,
			reason:       "Missing permissions",
			args:         nil,
			wantCode:     ErrorUnauthorized,
			wantHTTPCode: http.StatusForbidden,
		},
		{
			name:         "Unauthenticated",
			fn:           Unauthenticated,
			reason:       "Invalid token",
			args:         nil,
			wantCode:     ErrorUnauthenticated,
			wantHTTPCode: http.StatusUnauthorized,
		},
		{
			name:         "Forbidden",
			fn:           Forbidden,
			reason:       "Access denied",
			args:         nil,
			wantCode:     ErrorForbidden,
			wantHTTPCode: http.StatusForbidden,
		},
		{
			name:         "NotImplemented",
			fn:           NotImplemented,
			reason:       "Feature not implemented",
			args:         nil,
			wantCode:     ErrorNotImplemented,
			wantHTTPCode: http.StatusMethodNotAllowed,
		},
		{
			name:         "Conflict",
			fn:           Conflict,
			reason:       "Resource already exists",
			args:         nil,
			wantCode:     ErrorConflict,
			wantHTTPCode: http.StatusConflict,
		},
		{
			name:         "Validation",
			fn:           Validation,
			reason:       "Invalid input: %s",
			args:         []interface{}{"name is required"},
			wantCode:     ErrorValidation,
			wantHTTPCode: http.StatusBadRequest,
		},
		{
			name:         "MalformedRequest",
			fn:           MalformedRequest,
			reason:       "Cannot parse JSON",
			args:         nil,
			wantCode:     ErrorMalformedRequest,
			wantHTTPCode: http.StatusBadRequest,
		},
		{
			name:         "BadRequest",
			fn:           BadRequest,
			reason:       "Invalid query parameter",
			args:         nil,
			wantCode:     ErrorBadRequest,
			wantHTTPCode: http.StatusBadRequest,
		},
		{
			name:         "AdapterConfigNotFound",
			fn:           AdapterConfigNotFound,
			reason:       "Config file missing",
			args:         nil,
			wantCode:     ErrorAdapterConfigNotFound,
			wantHTTPCode: http.StatusNotFound,
		},
		{
			name:         "BrokerConnectionError",
			fn:           BrokerConnectionError,
			reason:       "Cannot connect to RabbitMQ",
			args:         nil,
			wantCode:     ErrorBrokerConnectionError,
			wantHTTPCode: http.StatusInternalServerError,
		},
		{
			name:         "KubernetesError",
			fn:           KubernetesError,
			reason:       "Failed to create %s",
			args:         []interface{}{"Namespace"},
			wantCode:     ErrorKubernetesError,
			wantHTTPCode: http.StatusInternalServerError,
		},
		{
			name:         "HyperFleetAPIError",
			fn:           HyperFleetAPIError,
			reason:       "API returned 500",
			args:         nil,
			wantCode:     ErrorHyperFleetAPIError,
			wantHTTPCode: http.StatusInternalServerError,
		},
		{
			name:         "InvalidCloudEvent",
			fn:           InvalidCloudEvent,
			reason:       "Missing required field: %s",
			args:         []interface{}{"type"},
			wantCode:     ErrorInvalidCloudEvent,
			wantHTTPCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn(tt.reason, tt.args...)

			if err == nil {
				t.Fatal("Helper function returned nil")
			}

			if err.Code != tt.wantCode {
				t.Errorf("Expected code %d, got %d", tt.wantCode, err.Code)
			}

			if err.HTTPCode != tt.wantHTTPCode {
				t.Errorf("Expected HTTP code %d, got %d", tt.wantHTTPCode, err.HTTPCode)
			}

			// Check formatted reason
			if len(tt.args) > 0 {
				// Should contain formatted values
				errStr := err.Error()
				for _, arg := range tt.args {
					argStr := ""
					if v, ok := arg.(string); ok {
						argStr = v
					}
					if argStr != "" && !containsString(errStr, argStr) {
						t.Errorf("Error string '%s' doesn't contain arg '%s'", errStr, argStr)
					}
				}
			}
		})
	}
}

func TestErrorCodeConstants(t *testing.T) {
	tests := []struct {
		name string
		code ServiceErrorCode
		want int
	}{
		{"ErrorNotFound", ErrorNotFound, 1},
		{"ErrorValidation", ErrorValidation, 2},
		{"ErrorConflict", ErrorConflict, 3},
		{"ErrorForbidden", ErrorForbidden, 4},
		{"ErrorUnauthorized", ErrorUnauthorized, 5},
		{"ErrorUnauthenticated", ErrorUnauthenticated, 6},
		{"ErrorBadRequest", ErrorBadRequest, 7},
		{"ErrorMalformedRequest", ErrorMalformedRequest, 8},
		{"ErrorNotImplemented", ErrorNotImplemented, 9},
		{"ErrorGeneral", ErrorGeneral, 10},
		{"ErrorAdapterConfigNotFound", ErrorAdapterConfigNotFound, 11},
		{"ErrorBrokerConnectionError", ErrorBrokerConnectionError, 12},
		{"ErrorKubernetesError", ErrorKubernetesError, 13},
		{"ErrorHyperFleetAPIError", ErrorHyperFleetAPIError, 14},
		{"ErrorInvalidCloudEvent", ErrorInvalidCloudEvent, 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if int(tt.code) != tt.want {
				t.Errorf("Expected %s = %d, got %d", tt.name, tt.want, int(tt.code))
			}
		})
	}
}

func TestHTTPCodeMapping(t *testing.T) {
	tests := []struct {
		name     string
		code     ServiceErrorCode
		wantHTTP int
	}{
		{"not_found_maps_to_404", ErrorNotFound, http.StatusNotFound},
		{"validation_maps_to_400", ErrorValidation, http.StatusBadRequest},
		{"conflict_maps_to_409", ErrorConflict, http.StatusConflict},
		{"forbidden_maps_to_403", ErrorForbidden, http.StatusForbidden},
		{"unauthorized_maps_to_403", ErrorUnauthorized, http.StatusForbidden},
		{"unauthenticated_maps_to_401", ErrorUnauthenticated, http.StatusUnauthorized},
		{"bad_request_maps_to_400", ErrorBadRequest, http.StatusBadRequest},
		{"not_implemented_maps_to_405", ErrorNotImplemented, http.StatusMethodNotAllowed},
		{"general_maps_to_500", ErrorGeneral, http.StatusInternalServerError},
		{"kubernetes_error_maps_to_500", ErrorKubernetesError, http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Find(tt.code)
			if err == nil {
				t.Fatal("Find() returned nil")
			}

			if err.HTTPCode != tt.wantHTTP {
				t.Errorf("Expected HTTP code %d, got %d", tt.wantHTTP, err.HTTPCode)
			}
		})
	}
}

// Helper function
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || findInString(s, substr))
}

func findInString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
