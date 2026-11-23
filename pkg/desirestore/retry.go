package desirestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore/internal/wire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

type response struct {
	header http.Header
	body   []byte
	status int
}

// retrier runs the attempts of one store call within one retry window.
type retrier struct {
	s       *Store
	start   time.Time
	attempt int
}

func (s *Store) retrier() *retrier {
	return &retrier{s: s, start: s.now()}
}

// send makes one attempt. A 429, 502, 503 or 504 comes back as *ProblemError
// and a failure without a response as *TransportError. Any other response
// comes back for the caller to interpret.
func (r *retrier) send(ctx context.Context, method, path string, body []byte) (*response, error) {
	resp, err := r.s.roundTrip(ctx, method, path, body, r.start.Add(defaultRetryWindow))
	if err != nil {
		var usage *UsageError
		if errors.As(err, &usage) {
			return nil, err
		}
		return nil, &TransportError{Cause: err}
	}
	if retryableStatus(resp.status) {
		return nil, problemFromResponse(resp)
	}
	return resp, nil
}

// get sends a GET and retries it within the window.
func (r *retrier) get(ctx context.Context, path string) (*response, error) {
	for {
		resp, err := r.send(ctx, http.MethodGet, path, nil)
		if err == nil || !r.wait(ctx, http.MethodGet, err) {
			return resp, err
		}
	}
}

// getRecord reads one record within the window.
func (r *retrier) getRecord(ctx context.Context, id desire.Identity) (*wire.Desire, error) {
	resp, err := r.get(ctx, desirePath(id))
	if err != nil {
		return nil, err
	}
	return r.s.decodeRecord(ctx, resp, http.StatusOK)
}

// wait sleeps before the next attempt. It returns false without sleeping when
// err is final, or when the wait would pass the retry window or the context
// deadline.
func (r *retrier) wait(ctx context.Context, method string, err error) bool {
	if !retryable(ctx, err) {
		r.s.logger.DebugContext(ctx, "store API request failed", "method", method, "error", err, "cause", rawCause(err))
		return false
	}
	r.s.logger.DebugContext(ctx, "store API request failed, retrying",
		"method", method, "attempt", r.attempt+1, "error", err)
	delay := retryAfterOf(err)
	if delay <= 0 {
		delay = r.s.backoff(r.attempt)
	}
	r.attempt++
	now := r.s.now()
	stop := now.Add(delay).After(r.start.Add(defaultRetryWindow))
	if deadline, ok := ctx.Deadline(); ok && now.Add(delay).After(deadline) {
		stop = true
	}
	if !stop && r.s.sleep(ctx, delay) != nil {
		stop = true
	}
	if stop {
		r.s.logger.DebugContext(ctx, "store API request failed, not retrying further",
			"attempts", r.attempt, "error", err, "cause", rawCause(err))
		return false
	}
	return true
}

// retryable reports whether an error from send may pass on a later attempt.
func retryable(ctx context.Context, err error) bool {
	var te *TransportError
	if errors.As(err, &te) {
		return ctx.Err() == nil && !isTLSVerification(te.Cause)
	}
	var pe *ProblemError
	return errors.As(err, &pe) && retryableStatus(pe.StatusCode)
}

// ambiguous reports whether a retryable error leaves it unknown if the
// request reached the server.
func ambiguous(err error) bool {
	var pe *ProblemError
	if errors.As(err, &pe) {
		return pe.StatusCode != http.StatusTooManyRequests
	}
	var te *TransportError
	return errors.As(err, &te)
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryAfterOf returns the Retry-After delay of a 429 error, or 0.
func retryAfterOf(err error) time.Duration {
	var pe *ProblemError
	if errors.As(err, &pe) && pe.StatusCode == http.StatusTooManyRequests {
		return pe.RetryAfter
	}
	return 0
}

// backoff returns a full-jitter exponential delay for the given attempt.
func (s *Store) backoff(attempt int) time.Duration {
	ceiling := defaultMaxDelay
	if attempt < 32 {
		if d := defaultInitialDelay << attempt; d > 0 && d < ceiling {
			ceiling = d
		}
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(s.jitter(int64(ceiling) + 1))
}

// roundTrip makes one HTTP request and reads the body. The request ends after
// the per-attempt timeout or at windowEnd, whichever comes first.
func (s *Store) roundTrip(
	ctx context.Context, method, path string, body []byte, windowEnd time.Time,
) (*response, error) {
	timeout := s.requestTimeout
	if remaining := windowEnd.Sub(s.now()); remaining < timeout {
		timeout = remaining
	}
	if timeout <= 0 {
		return nil, errRetryWindowClosed
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(actx, method, s.base+path, reader)
	if err != nil {
		return nil, &UsageError{Reason: "cannot build the store API request", Err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, application/problem+json")
	if authErr := s.creds.Authenticate(req); authErr != nil {
		return nil, &UsageError{Reason: "cannot authenticate the store API request", Err: authErr}
	}
	httpResp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := httpResp.Body.Close(); closeErr != nil {
			s.logger.DebugContext(ctx, "failed to close store API response body", "error", closeErr)
		}
	}()
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	return &response{status: httpResp.StatusCode, header: httpResp.Header, body: data}, nil
}

// rawCause returns the unfiltered text of a transport error's cause for logs.
func rawCause(err error) string {
	var te *TransportError
	if errors.As(err, &te) && te.Cause != nil {
		return te.Cause.Error()
	}
	var ue *UsageError
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return ""
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// defaultJitter returns a uniform value in [0, n).
func defaultJitter(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return n - 1
	}
	return v.Int64()
}
