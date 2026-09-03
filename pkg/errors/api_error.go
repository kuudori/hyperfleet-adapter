package errors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// -----------------------------------------------------------------------------
// API Error Type
// -----------------------------------------------------------------------------

// APIError represents an error from an HTTP API call with detailed context.
// This allows the adapter runtime to capture and handle request errors properly.
type APIError struct {
	// Err is the underlying error
	Err error
	// Method is the HTTP method used (GET, POST, PUT, PATCH, DELETE)
	Method string
	// URL is the request URL
	URL string
	// Status is the HTTP status string (e.g., "503 Service Unavailable")
	Status string
	// brokenEndpoint reports whether a 404 response came from the API's
	// catch-all route rather than a missing resource.
	brokenEndpoint bool
	// Duration is the total duration including retries
	Duration time.Duration
	// StatusCode is the HTTP status code (0 if request failed before getting response)
	StatusCode int
	// Attempts is how many attempts were made (including retries)
	Attempts int
}

// brokenEndpointCode is the RFC 9457 error code the HyperFleet API returns
// from its catch-all 404 handler when no route matched the request URL.
const brokenEndpointCode = "HYPERFLEET-NTF-000"

// problemDetails is a subset of RFC 9457 Problem Details used to distinguish
// resource-not-found from broken-endpoint 404 responses.
type problemDetails struct {
	Code string `json:"code"`
}

// isBrokenEndpointResponse reports whether an RFC 9457 response identifies the
// API's catch-all route. The response body is inspected only while constructing
// an APIError and is never retained on the error.
func isBrokenEndpointResponse(body []byte) bool {
	var pd problemDetails
	if err := json.Unmarshal(body, &pd); err != nil {
		return false
	}
	return pd.Code == brokenEndpointCode
}

// Error implements the error interface.
// Note: Err should always be non-nil when APIError is created in production code.
// The client.Do() method always sets lastErr before returning an APIError.
func (e *APIError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("API request failed: %s %s returned %d %s after %d attempt(s): %v",
			e.Method, e.URL, e.StatusCode, e.Status, e.Attempts, e.Err)
	}
	return fmt.Sprintf("API request failed: %s %s after %d attempt(s): %v",
		e.Method, e.URL, e.Attempts, e.Err)
}

// Unwrap returns the underlying error for errors.Is/As support
func (e *APIError) Unwrap() error {
	return e.Err
}

// -----------------------------------------------------------------------------
// Status Code Helpers
// -----------------------------------------------------------------------------

// IsTimeout returns true if the error was caused by a timeout
func (e *APIError) IsTimeout() bool {
	return e.StatusCode == 408 || errors.Is(e.Err, context.DeadlineExceeded)
}

// IsServerError returns true if the error was a server error (5xx)
func (e *APIError) IsServerError() bool {
	return e.StatusCode >= 500 && e.StatusCode < 600
}

// IsClientError returns true if the error was a client error (4xx)
func (e *APIError) IsClientError() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500
}

// IsNotFound returns true if the error was a 404 Not Found
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == 404
}

// IsResourceNotFound returns true when the 404 represents a real resource that
// was not found, as opposed to a broken/misconfigured URL.
// It defaults to true for any 404 (safe fallback if proxies strip the response
// body), and only returns false when the RFC 9457 response contains the catch-all
// error code HYPERFLEET-NTF-000, which signals no route matched the request URL.
func (e *APIError) IsResourceNotFound() bool {
	if !e.IsNotFound() {
		return false
	}
	return !e.brokenEndpoint
}

// IsUnauthorized returns true if the error was a 401 Unauthorized
func (e *APIError) IsUnauthorized() bool {
	return e.StatusCode == 401
}

// IsForbidden returns true if the error was a 403 Forbidden
func (e *APIError) IsForbidden() bool {
	return e.StatusCode == 403
}

// IsRateLimited returns true if the error was a 429 Too Many Requests
func (e *APIError) IsRateLimited() bool {
	return e.StatusCode == 429
}

// IsBadRequest returns true if the error was a 400 Bad Request
func (e *APIError) IsBadRequest() bool {
	return e.StatusCode == 400
}

// IsConflict returns true if the error was a 409 Conflict
func (e *APIError) IsConflict() bool {
	return e.StatusCode == 409
}

// -----------------------------------------------------------------------------
// Constructor and Helper Functions
// -----------------------------------------------------------------------------

// NewAPIError creates a new APIError. For 404 responses, body is inspected to
// preserve resource-not-found classification, but is not retained.
func NewAPIError(
	method, url string,
	statusCode int,
	status string,
	body []byte,
	attempts int,
	duration time.Duration,
	err error,
) *APIError {
	return &APIError{
		Method:         method,
		URL:            url,
		StatusCode:     statusCode,
		Status:         status,
		brokenEndpoint: statusCode == 404 && isBrokenEndpointResponse(body),
		Attempts:       attempts,
		Duration:       duration,
		Err:            err,
	}
}

// IsAPIError checks if an error is an APIError and returns it.
// This function supports wrapped errors via errors.As.
func IsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// IsNotFoundError checks whether err (or any error in its chain) is
// an APIError with a 404 status code.
func IsNotFoundError(err error) bool {
	apiErr, ok := IsAPIError(err)
	return ok && apiErr.IsNotFound()
}

// IsResourceNotFoundError checks whether err is a 404 APIError that represents
// a real resource not found (not a broken/misconfigured endpoint URL).
func IsResourceNotFoundError(err error) bool {
	apiErr, ok := IsAPIError(err)
	return ok && apiErr.IsResourceNotFound()
}
