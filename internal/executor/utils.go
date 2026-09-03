package executor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/criteria"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	apierrors "github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/errors"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/utils"
	hfl "github.com/openshift-hyperfleet/hyperfleet-logger"
)

// ToConditionDefs converts configloader.Condition slice to criteria.ConditionDef slice.
// This centralizes the conversion logic that was previously repeated in multiple places.
func ToConditionDefs(conditions []configloader.Condition) []criteria.ConditionDef {
	defs := make([]criteria.ConditionDef, len(conditions))
	for i, cond := range conditions {
		defs[i] = criteria.ConditionDef{
			Field:    cond.Field,
			Operator: criteria.Operator(cond.Operator),
			Value:    cond.Value,
		}
	}
	return defs
}

// apiAuthFailureStatusCode returns the status code for wrapped API authorization errors.
func apiAuthFailureStatusCode(err error) (int, bool) {
	apiErr, ok := apierrors.IsAPIError(err)
	if !ok {
		return 0, false
	}

	if !apiErr.IsUnauthorized() && !apiErr.IsForbidden() {
		return 0, false
	}

	return apiErr.StatusCode, true
}

// logAPIAuthFailure records a structured log for an API authentication or
// authorization failure. Callers add context specific to their execution phase.
func logAPIAuthFailure(ctx context.Context, err error, attrs ...any) {
	statusCode, ok := apiAuthFailureStatusCode(err)
	if !ok {
		return
	}

	attrs = append([]any{"http_status", statusCode}, attrs...)
	slog.ErrorContext(ctx, "hyperfleet api authentication or authorization failed", attrs...)
}

// ExecuteLogAction executes a log action with the given context
// The message is rendered as a Go template with access to all params
// This is a shared utility function used by both PreconditionExecutor and PostActionExecutor
func ExecuteLogAction(
	ctx context.Context,
	logAction *configloader.LogAction,
	execCtx *ExecutionContext,
) {
	if logAction == nil || logAction.Message == "" {
		return
	}

	// Render the message template
	message, err := utils.RenderTemplate(logAction.Message, execCtx.Params)
	if err != nil {
		slog.ErrorContext(ctx, "failed to render log message", "error", err)
		return
	}

	// Log at the specified level; unknown levels fall back to info.
	level, err := hfl.ParseLevel(logAction.Level)
	if err != nil {
		slog.WarnContext(ctx, "invalid log level", "error", err)
	}
	slog.Log(ctx, level, "[config] "+message)
}

// ExecuteAPICall executes an API call with the given configuration and returns the response and rendered URL
// This is a shared utility function used by both PreconditionExecutor and PostActionExecutor
// On error, it returns an APIError with request metadata. Response bodies are inspected only
// to classify 404 errors and are not retained.
// Returns: response, renderedURL, error
func ExecuteAPICall(
	ctx context.Context,
	apiCall *configloader.APICall,
	execCtx *ExecutionContext,
	apiClient hyperfleetapi.Client,
) (*hyperfleetapi.Response, string, error) {
	if apiCall == nil {
		return nil, "", fmt.Errorf("apiCall is nil")
	}

	// First render the URL template to resolve variables like {{ .hyperfleetApiBaseUrl }}
	renderedURL, err := utils.RenderTemplate(apiCall.URL, execCtx.Params)
	if err != nil {
		return nil, "", fmt.Errorf("failed to render URL template: %w", err)
	}

	// Then build the final URL - this handles absolute URLs vs relative paths
	url := buildHyperfleetAPICallURL(renderedURL, execCtx)

	slog.InfoContext(ctx, "making api call", "method", apiCall.Method)

	// Build request options
	opts := make([]hyperfleetapi.RequestOption, 0)

	// Add headers
	headers := make(map[string]string)
	for _, h := range apiCall.Headers {
		headerValue, headerErr := utils.RenderTemplate(h.Value, execCtx.Params)
		if headerErr != nil {
			return nil, url, fmt.Errorf("failed to render header '%s' template: %w", h.Name, headerErr)
		}
		headers[h.Name] = headerValue
	}
	if len(headers) > 0 {
		opts = append(opts, hyperfleetapi.WithHeaders(headers))
	}

	// Set timeout if specified
	if apiCall.Timeout != "" {
		timeout, timeoutErr := time.ParseDuration(apiCall.Timeout)
		if timeoutErr == nil {
			opts = append(opts, hyperfleetapi.WithRequestTimeout(timeout))
		} else {
			slog.WarnContext(ctx, "failed to parse timeout, using default timeout",
				"timeout", apiCall.Timeout, "error", timeoutErr)
		}
	}

	// Set retry configuration
	if apiCall.RetryAttempts > 0 {
		opts = append(opts, hyperfleetapi.WithRequestRetryAttempts(apiCall.RetryAttempts))
	}
	if apiCall.RetryBackoff != "" {
		backoff := hyperfleetapi.BackoffStrategy(apiCall.RetryBackoff)
		opts = append(opts, hyperfleetapi.WithRequestRetryBackoff(backoff))
	}

	// Execute request based on method
	var resp *hyperfleetapi.Response
	switch strings.ToUpper(apiCall.Method) {
	case http.MethodGet:
		resp, err = apiClient.Get(ctx, url, opts...)
	case http.MethodPost:
		body := []byte(apiCall.Body)
		if apiCall.Body != "" {
			body, err = utils.RenderTemplateBytes(apiCall.Body, execCtx.Params)
			if err != nil {
				return nil, url, fmt.Errorf("failed to render body template: %w", err)
			}
		}
		resp, err = apiClient.Post(ctx, url, body, opts...)
		if err != nil {
			slog.ErrorContext(ctx, "post request failed", "error", err)
		} else if resp != nil && !resp.IsSuccess() {
			slog.ErrorContext(ctx, "post request returned non-success status",
				"status_code", resp.StatusCode, "status", resp.Status)
		}
	case http.MethodPut:
		body := []byte(apiCall.Body)
		if apiCall.Body != "" {
			body, err = utils.RenderTemplateBytes(apiCall.Body, execCtx.Params)
			if err != nil {
				return nil, "", fmt.Errorf("failed to render body template: %w", err)
			}
		}
		resp, err = apiClient.Put(ctx, url, body, opts...)
		if err != nil {
			slog.ErrorContext(ctx, "put request failed", "error", err)
		} else if resp != nil && !resp.IsSuccess() {
			slog.ErrorContext(ctx, "put request returned non-success status",
				"status_code", resp.StatusCode, "status", resp.Status)
		}
	case http.MethodPatch:
		body := []byte(apiCall.Body)
		if apiCall.Body != "" {
			body, err = utils.RenderTemplateBytes(apiCall.Body, execCtx.Params)
			if err != nil {
				return nil, "", fmt.Errorf("failed to render body template: %w", err)
			}
		}
		resp, err = apiClient.Patch(ctx, url, body, opts...)
	case http.MethodDelete:
		resp, err = apiClient.Delete(ctx, url, opts...)
	default:
		return nil, url, fmt.Errorf("unsupported HTTP method: %s", apiCall.Method)
	}

	if err != nil {
		// Return response AND error - response may contain useful details even on error
		// (e.g., HTTP status code, response body)
		if resp != nil {
			slog.WarnContext(ctx, "api call failed",
				"status_code", resp.StatusCode,
				"status", resp.Status,
				"error", err,
			)
			// Wrap as APIError with request and response metadata.
			apiErr := apierrors.NewAPIError(
				apiCall.Method,
				url,
				resp.StatusCode,
				resp.Status,
				resp.Body,
				resp.Attempts,
				resp.Duration,
				err,
			)
			return resp, url, apiErr
		} else {
			slog.WarnContext(ctx, "api call failed", "error", err)
			// No response - create APIError with minimal context
			apiErr := apierrors.NewAPIError(
				apiCall.Method,
				url,
				0,
				"",
				nil,
				0,
				0,
				err,
			)
			return resp, url, apiErr
		}
	}
	if resp == nil {
		nilErr := fmt.Errorf("API client returned nil response without error")
		return nil, url, apierrors.NewAPIError(apiCall.Method, url, 0, "", nil, 0, 0, nilErr)
	}

	slog.InfoContext(ctx, "api call completed", "status_code", resp.StatusCode, "status", resp.Status)
	return resp, url, nil
}

// buildHyperfleetAPICallURL builds a full HyperFleet API URL when a relative path is provided.
// It uses hyperfleet API client settings from execution context config.
// Since the hyperfleetapi.Client always prepends its baseURL to the path,
// this function returns a relative path that the client can use correctly.
// If the URL is absolute and contains the baseURL, the relative path is extracted.
func buildHyperfleetAPICallURL(apiCallURL string, execCtx *ExecutionContext) string {
	if apiCallURL == "" {
		return apiCallURL
	}
	if execCtx == nil || execCtx.Config == nil {
		return apiCallURL
	}

	// Parse the input URL to check if it's absolute
	parsedURL, err := url.Parse(apiCallURL)
	if err != nil {
		return apiCallURL
	}

	// If the URL is absolute (has a scheme like http:// or https://)
	if parsedURL.Scheme != "" {
		// Parse the baseURL to extract its path for comparison
		baseURLStr := execCtx.Config.Clients.HyperfleetAPI.BaseURL
		if baseURLStr == "" {
			return apiCallURL
		}

		baseURL, err := url.Parse(baseURLStr)
		if err != nil {
			return apiCallURL
		}

		// Check if the absolute URL starts with our baseURL (same scheme, host, and path prefix)
		if parsedURL.Scheme == baseURL.Scheme && parsedURL.Host == baseURL.Host {
			// Extract the relative path by removing the baseURL's path prefix
			basePath := strings.TrimSuffix(baseURL.Path, "/")
			relativePath := parsedURL.Path
			if basePath != "" && strings.HasPrefix(relativePath, basePath) {
				relativePath = strings.TrimPrefix(relativePath, basePath)
			}
			// Ensure the path starts with /
			if !strings.HasPrefix(relativePath, "/") {
				relativePath = "/" + relativePath
			}
			return relativePath
		}

		// For absolute URLs not matching our baseURL, return as-is
		return apiCallURL
	}

	// For relative URLs, ensure proper formatting
	baseURLStr := execCtx.Config.Clients.HyperfleetAPI.BaseURL
	if baseURLStr == "" {
		return apiCallURL
	}

	// Clean the path and check if it already has the api/ prefix
	cleanPath := path.Clean(parsedURL.Path)
	cleanPath = strings.TrimPrefix(cleanPath, "/")

	if strings.HasPrefix(cleanPath, "api/") {
		// Already has api/ prefix, return with leading slash
		return "/" + cleanPath
	}

	// Build the full API path using path.Join for clean path handling
	version := execCtx.Config.Clients.HyperfleetAPI.Version
	if version == "" {
		version = "v1"
	}
	return path.Join("/api/hyperfleet", version, cleanPath)
}

// ValidateAPIResponse checks if an API response is valid and successful
// Returns an APIError with full context if response is nil or unsuccessful
// method and url are used to construct APIError with proper context
func ValidateAPIResponse(resp *hyperfleetapi.Response, err error, method, url string) error {
	if err != nil {
		// If it's already an APIError, return it as-is
		if _, ok := apierrors.IsAPIError(err); ok { //nolint:errcheck // checking type only, not using the value
			return err
		}
		// Otherwise wrap it as APIError
		return apierrors.NewAPIError(method, url, 0, "", nil, 0, 0, err)
	}

	if resp == nil {
		nilErr := fmt.Errorf("API response is nil")
		return apierrors.NewAPIError(method, url, 0, "", nil, 0, 0, nilErr)
	}

	if !resp.IsSuccess() {
		baseErr := fmt.Errorf("API returned non-success status: %d %s", resp.StatusCode, resp.Status)
		return apierrors.NewAPIError(
			method,
			url,
			resp.StatusCode,
			resp.Status,
			resp.Body,
			resp.Attempts,
			resp.Duration,
			baseErr,
		)
	}

	return nil
}

// executionErrorToMap converts an ExecutionError struct to a map for CEL evaluation
// Returns nil if the ExecutionError pointer is nil
func executionErrorToMap(execErr *ExecutionError) interface{} {
	if execErr == nil {
		return nil
	}

	return map[string]interface{}{
		"phase":   execErr.Phase,
		"step":    execErr.Step,
		"message": execErr.Message,
	}
}

// buildEnvMap returns all OS environment variables as a map[string]interface{} for CEL evaluation.
func buildEnvMap() map[string]interface{} {
	envMap := make(map[string]interface{})
	for _, kv := range os.Environ() {
		if i := strings.Index(kv, "="); i > 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	return envMap
}

// adapterMetadataToMap converts AdapterMetadata struct to a map for CEL evaluation
func adapterMetadataToMap(adapter *AdapterMetadata) map[string]interface{} {
	if adapter == nil {
		return map[string]interface{}{}
	}

	resourceErrors := make(map[string]interface{}, len(adapter.ResourceErrors))
	for name, execErr := range adapter.ResourceErrors {
		execErrCopy := execErr
		resourceErrors[name] = executionErrorToMap(&execErrCopy)
	}

	return map[string]interface{}{
		"executionStatus":  adapter.ExecutionStatus,
		"resourcesSkipped": adapter.ResourcesSkipped,
		"skipReason":       adapter.SkipReason,
		"errorReason":      adapter.ErrorReason,
		"errorMessage":     adapter.ErrorMessage,
		"executionError":   executionErrorToMap(adapter.ExecutionError),
		"resourceErrors":   resourceErrors,
	}
}
