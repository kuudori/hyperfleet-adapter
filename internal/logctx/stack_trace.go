package logctx

import (
	"context"
	"errors"
	"io"
	"log/slog"

	apperrors "github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// -----------------------------------------------------------------------------
// Stack Trace Capture Filter
// -----------------------------------------------------------------------------

// skipStackTraceCheckers is a list of functions that check if an error should skip stack trace capture.
// Each checker returns true if the error is an expected operational error.
// Add new error types here to extend the blocklist.
var skipStackTraceCheckers = []func(error) bool{
	// Context errors (expected in graceful shutdown)
	func(err error) bool { return errors.Is(err, context.Canceled) },
	func(err error) bool { return errors.Is(err, context.DeadlineExceeded) },
	func(err error) bool { return errors.Is(err, io.EOF) },

	// Network/transient errors (expected in distributed systems)
	apperrors.IsNetworkError,

	// HyperFleet API errors (HTTP 4xx/5xx responses)
	isExpectedAPIError,

	// K8s resource data errors
	isK8sResourceDataError,

	// K8s API errors
	apierrors.IsNotFound,
	apierrors.IsConflict,
	apierrors.IsAlreadyExists,
	apierrors.IsForbidden,
	apierrors.IsUnauthorized,
	apierrors.IsInvalid,
	apierrors.IsBadRequest,
	apierrors.IsGone,
	apierrors.IsResourceExpired,
	apierrors.IsServiceUnavailable,
	apierrors.IsTimeout,
	apierrors.IsTooManyRequests,
}

// isExpectedAPIError checks if the error is an expected HyperFleet API error
func isExpectedAPIError(err error) bool {
	apiErr, ok := apperrors.IsAPIError(err)
	if !ok {
		return false
	}
	return apiErr.IsNotFound() ||
		apiErr.IsUnauthorized() ||
		apiErr.IsForbidden() ||
		apiErr.IsBadRequest() ||
		apiErr.IsConflict() ||
		apiErr.IsRateLimited() ||
		apiErr.IsTimeout() ||
		apiErr.IsServerError()
}

// isK8sResourceDataError checks if the error is an expected K8s resource data error
func isK8sResourceDataError(err error) bool {
	var k8sKeyNotFound *apperrors.K8sResourceKeyNotFoundError
	if errors.As(err, &k8sKeyNotFound) {
		return true
	}
	var k8sInvalidPath *apperrors.K8sInvalidPathError
	if errors.As(err, &k8sInvalidPath) {
		return true
	}
	var k8sDataErr *apperrors.K8sResourceDataError
	return errors.As(err, &k8sDataErr)
}

// StackTraceFilter determines whether a stack trace should be attached to the
// given log record. It is intended to be passed to hfl.WithStackTrace
// at handler construction time.
//
// It extracts the error from the record's "error" attribute (if any) and
// returns false for expected operational errors (high frequency, known
// causes) to avoid performance overhead during error storms. Returns true for
// unexpected errors that indicate bugs or require investigation.
func StackTraceFilter(_ context.Context, r slog.Record) bool {
	var err error
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			if e, ok := a.Value.Any().(error); ok {
				err = e
			}
			return false
		}
		return true
	})

	if err == nil {
		return false
	}

	// Check all blocklist conditions
	for _, check := range skipStackTraceCheckers {
		if check(err) {
			return false
		}
	}

	// Capture stack trace for unexpected/internal errors
	return true
}
