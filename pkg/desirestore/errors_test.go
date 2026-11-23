package desirestore

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
)

func TestDesireIDEncoding(t *testing.T) {
	tests := []struct {
		id       desire.Identity
		wantID   string
		wantPath string
	}{
		{
			id: desire.Identity{ManagementCluster: "mc-01", Type: desire.TypeApply, Group: "rbac.authorization.k8s.io",
				Resource: "clusterroles", Name: "system:controller:x"},
			wantID:   "apply:rbac.authorization.k8s.io:clusterroles::system:controller:x",
			wantPath: apiPrefix + "mc-01/desires/apply:rbac.authorization.k8s.io:clusterroles::system:controller:x",
		},
		{
			id: desire.Identity{ManagementCluster: "mc-01", Type: desire.TypeRead, Resource: testResource,
				Namespace: testNamespace, Name: "cm"},
			wantID:   "read::configmaps:default:cm",
			wantPath: apiPrefix + "mc-01/desires/read::configmaps:default:cm",
		},
		{
			id: desire.Identity{ManagementCluster: "mc 01", Type: desire.TypeDelete, Resource: testResource,
				Namespace: testNamespace, Name: "a b"},
			wantID:   "delete::configmaps:default:a b",
			wantPath: apiPrefix + "mc%2001/desires/delete::configmaps:default:a%20b",
		},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.wantID, desireID(tt.id))
		assert.Equal(t, tt.wantPath, desirePath(tt.id))
	}
	assert.Equal(t, apiPrefix+"mc-01/desires", listPath("mc-01"))
}

func TestProblemMapping(t *testing.T) {
	tests := []struct {
		want   error
		code   string
		status int
	}{
		{desire.ErrNotFound, codeNotFound, 404},
		{desire.ErrAlreadyExists, codeAlreadyExists, 409},
		{desire.ErrVersionConflict, codeVersionConflict, 409},
		{desire.ErrDeletePending, codeDeletePending, 409},
		{desire.ErrOwnerConflict, codeOwnerConflict, 409},
		{nil, "HYPERFLEET-NTF-000", 404},
		{nil, "HYPERFLEET-CNF-999", 409},
		{nil, codeOwnerConflict, 400},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d %s", tt.status, tt.code), func(t *testing.T) {
			pe := problemFromResponse(&response{status: tt.status, header: http.Header{},
				body: []byte(problemBody(tt.status, tt.code))})
			assert.Equal(t, tt.code, pe.Code)
			assert.Equal(t, tt.want, pe.Unwrap())
			assert.Equal(t, fmt.Sprintf("store API returned HTTP %d %s", tt.status, tt.code), pe.Error())
			assert.Contains(t, pe.Detail, "Desire", "the detail is kept for logs")
		})
	}

	pe := problemFromResponse(&response{status: 401, header: http.Header{}})
	assert.Equal(t, "store API returned HTTP 401", pe.Error())
	assert.NoError(t, pe.Unwrap())
}

func TestProblemErrorFields(t *testing.T) {
	body := `{"type":"https://api.hyperfleet.io/errors/validation-error","title":"Validation Error","status":400,` +
		`"code":"HYPERFLEET-VAL-005","detail":"Desire id is invalid","errors":[{"field":"desire-id","message":"m"}]}`
	pe := problemFromResponse(&response{status: 400, header: http.Header{}, body: []byte(body)})
	assert.Equal(t, "store API returned HTTP 400 HYPERFLEET-VAL-005 desire-id", pe.Error())

	body = `{"type":"t","title":"t","status":400,"code":"HYPERFLEET-VAL-001",` +
		`"errors":[{"field":"resource_id","message":"m"},{"field":"target_version","message":"m"},` +
		`{"field":"desireType","message":"m"}]}`
	pe = problemFromResponse(&response{status: 400, header: http.Header{}, body: []byte(body)})
	assert.Equal(t, "store API returned HTTP 400 HYPERFLEET-VAL-001 resource_id, target_version, desireType",
		pe.Error())

	// Fields are shown only for 400 and 422.
	pe = problemFromResponse(&response{status: 500, header: http.Header{}, body: []byte(body)})
	assert.Equal(t, "store API returned HTTP 500 HYPERFLEET-VAL-001", pe.Error())
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, 7*time.Second, parseRetryAfter("7", now))
	assert.Equal(t, 30*time.Second, parseRetryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now))
	assert.Equal(t, time.Duration(0), parseRetryAfter("", now))
	assert.Equal(t, time.Duration(0), parseRetryAfter("soon", now))
	assert.Equal(t, time.Duration(0), parseRetryAfter("-3", now))
	assert.Equal(t, time.Duration(0), parseRetryAfter(now.Add(-time.Minute).Format(http.TimeFormat), now))
}

func TestTransportErrorText(t *testing.T) {
	urlErr := func(err error) error {
		return &url.Error{Op: "Get", URL: "https://desire-store.example:8443" + apiPrefix + "mc/desires", Err: err}
	}
	tests := []struct {
		name  string
		cause error
		want  string
	}{
		{
			"DNS keeps its cause",
			urlErr(&net.DNSError{Err: "no such host", Name: testStoreHost, IsNotFound: true}),
			"store API request failed: lookup " + testStoreHost + ": no such host",
		},
		{
			"connection refused",
			urlErr(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}),
			"store API request failed: dial tcp: connect: connection refused",
		},
		{
			"TLS cause",
			urlErr(x509.UnknownAuthorityError{}),
			"store API request failed: x509: certificate signed by unknown authority",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &TransportError{Cause: tt.cause}
			assert.Equal(t, tt.want, err.Error())
		})
	}
	var dnsErr *net.DNSError
	err := &TransportError{Cause: urlErr(&net.DNSError{Err: "x", Name: "y"})}
	assert.ErrorAs(t, err, &dnsErr, "the cause stays reachable")
}

func TestRecoveryErrorSentinels(t *testing.T) {
	for _, sentinel := range []error{desire.ErrNotFound, desire.ErrAlreadyExists, desire.ErrVersionConflict,
		desire.ErrOwnerConflict} {
		err := &recoveryError{reason: "has changed", sentinels: []error{sentinel}}
		assert.ErrorIs(t, err, sentinel)
	}
}
