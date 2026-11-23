package desirestore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore/internal/wire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

// ErrRecreated reports that a delete whose outcome was unknown found the record
// at a different version: it was changed or created again, so the caller must
// not delete it again in the same call. It comes with desire.ErrVersionConflict
var ErrRecreated = errors.New("store record changed or was created again")

// errRetryWindowClosed ends an attempt that would start after the retry window.
var errRetryWindowClosed = errors.New("retry window closed")

// Error codes that the API returns and that this client acts on.
const (
	codeNotFound        = "HYPERFLEET-NTF-001"
	codeAlreadyExists   = "HYPERFLEET-CNF-001"
	codeVersionConflict = "HYPERFLEET-CNF-002"
	codeDeletePending   = "HYPERFLEET-CNF-003"
	codeOwnerConflict   = "HYPERFLEET-CNF-004"
)

// ProblemError is an HTTP error response from the store API or its gateway.
// Title, Detail and TraceID are kept for logs and are not part of Error().
type ProblemError struct {
	Code    string
	Title   string
	Detail  string
	TraceID string
	// Fields lists errors[].field of a 400 or 422 response.
	Fields     []string
	StatusCode int
	RetryAfter time.Duration
}

func (e *ProblemError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "store API returned HTTP %d", e.StatusCode)
	if e.Code != "" {
		b.WriteString(" ")
		b.WriteString(e.Code)
	}
	if len(e.Fields) > 0 {
		b.WriteString(" ")
		b.WriteString(strings.Join(e.Fields, ", "))
	}
	return b.String()
}

// Unwrap returns the desire sentinel that the response code stands for, or nil.
func (e *ProblemError) Unwrap() error {
	switch {
	case e.StatusCode == http.StatusNotFound && e.Code == codeNotFound:
		return desire.ErrNotFound
	case e.StatusCode != http.StatusConflict:
		return nil
	}
	switch e.Code {
	case codeAlreadyExists:
		return desire.ErrAlreadyExists
	case codeVersionConflict:
		return desire.ErrVersionConflict
	case codeDeletePending:
		return desire.ErrDeletePending
	case codeOwnerConflict:
		return desire.ErrOwnerConflict
	}
	return nil
}

// problemFromResponse builds a ProblemError from a response. A gateway 401 or
// 403 has no body, so the body is optional.
func problemFromResponse(r *response) *ProblemError {
	pe := &ProblemError{StatusCode: r.status, RetryAfter: parseRetryAfter(r.header.Get("Retry-After"), time.Now())}
	var body wire.ProblemDetails
	if len(r.body) == 0 || json.Unmarshal(r.body, &body) != nil {
		return pe
	}
	pe.Code, pe.Title, pe.Detail, pe.TraceID = body.Code, body.Title, body.Detail, body.TraceId
	if r.status == http.StatusBadRequest || r.status == http.StatusUnprocessableEntity {
		for _, fe := range body.Errors {
			pe.Fields = append(pe.Fields, fe.Field)
		}
	}
	return pe
}

// parseRetryAfter reads delay-seconds or an HTTP date (RFC 9110). It returns 0
// for a missing or invalid value.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// TransportError is a failure without an HTTP response: a network error, a
// timeout or a TLS failure.
type TransportError struct {
	Cause error
}

func (e *TransportError) Error() string {
	cause := e.Cause
	var ue *url.Error
	if errors.As(cause, &ue) {
		// The url.Error text repeats the method and the route path.
		cause = ue.Err
	}
	return "store API request failed: " + cause.Error()
}

func (e *TransportError) Unwrap() error {
	return e.Cause
}

func isTLSVerification(err error) bool {
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &certErr) || errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostname) || errors.As(err, &invalid)
}

// UsageError is a request that the client refuses before sending it.
type UsageError struct {
	Err    error
	Reason string
}

func (e *UsageError) Error() string {
	return e.Reason
}

func (e *UsageError) Unwrap() error {
	return e.Err
}

// recoveryError reports what the recovery read found after an ambiguous
// write or delete, as the sentinel the caller acts on.
type recoveryError struct {
	reason    string
	sentinels []error
}

func (e *recoveryError) Error() string {
	return "store API request outcome unknown, and the stored record " + e.reason
}

func (e *recoveryError) Unwrap() []error {
	return e.sentinels
}

// problem returns the error for a non-success response and logs the server's
// details, which Error() leaves out.
func (s *Store) problem(ctx context.Context, r *response) error {
	pe := problemFromResponse(r)
	s.logger.DebugContext(ctx, "store API returned an error",
		"status", pe.StatusCode, "code", pe.Code, "title", pe.Title, "detail", pe.Detail, "trace_id", pe.TraceID)
	return pe
}
