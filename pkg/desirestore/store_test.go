package desirestore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testOwner     = "system:serviceaccount:hyperfleet:test-adapter"
	testPartition = "mc-01"
	testOrigin    = "0b5c1a6e-6f53-4a7a-9d3c-2f1e0c7b9a11"
	testResource  = "configmaps"
	testNamespace = "default"
	testStoreHost = "desire-store.example"
	retryAfterKey = "Retry-After"
)

// fakeClock advances only when the store sleeps, so retry windows close
// without real waiting.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
	mu     sync.Mutex
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

func (c *fakeClock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// shortTimeout is the per-attempt timeout of tests whose fake server holds
// an answer past it. Other tests keep the default, so that a slow machine
// does not turn a normal answer into a timeout.
const shortTimeout = 200 * time.Millisecond

// newTestStore builds a Store against f with a fake clock and jitter that
// always picks the full backoff delay.
func newTestStore(t *testing.T, baseURL string) (*Store, *fakeClock) {
	t.Helper()
	s, err := New(baseURL, testCreds{identity: testOwner})
	require.NoError(t, err)
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	s.now = clock.Now
	s.sleep = clock.Sleep
	s.jitter = func(n int64) int64 { return n - 1 }
	return s, clock
}

func applyID(name string) desire.Identity {
	return desire.Identity{
		ManagementCluster: testPartition, Type: desire.TypeApply, Group: "rbac.authorization.k8s.io",
		Resource: "clusterroles", Namespace: "", Name: name,
	}
}

func coreID(t desire.DesireType, name string) desire.Identity {
	return desire.Identity{
		ManagementCluster: testPartition, Type: t, Group: "", Resource: testResource, Namespace: testNamespace, Name: name,
	}
}

func clusterRole(name string, rev int) json.RawMessage {
	return mustJSON(map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": name, "annotations": map[string]any{"rev": rev}},
	})
}

func configMap(name string, rev int) json.RawMessage {
	return mustJSON(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": testNamespace},
		"data":     map[string]any{"rev": rev},
	})
}

func newApply(id desire.Identity, content json.RawMessage) desire.ApplyDesire {
	return desire.ApplyDesire{
		Identity: id, Owner: testOwner, OriginID: testOrigin, Spec: desire.ApplySpec{KubeContent: content},
	}
}

func TestRoundTripEachType(t *testing.T) {
	f := newFakeServer(t, "")
	s, _ := newTestStore(t, f.srv.URL)
	ctx := t.Context()

	// Colon name and empty namespace on a cluster-scoped object.
	aID := applyID("system:controller:x")
	created, err := s.CreateApplyDesire(ctx, newApply(aID, clusterRole("system:controller:x", 1)))
	require.NoError(t, err)
	assert.Equal(t, aID, created.Identity)
	assert.Equal(t, testOwner, created.Owner)
	assert.Equal(t, testOrigin, created.OriginID)
	assert.Equal(t, int64(1), created.Version)

	got, err := s.GetApplyDesire(ctx, aID)
	require.NoError(t, err)
	assert.JSONEq(t, string(clusterRole("system:controller:x", 1)), string(got.Spec.KubeContent))

	updated, err := s.UpdateApplyDesireSpec(ctx, aID,
		desire.ApplySpec{KubeContent: clusterRole("system:controller:x", 2)}, testOwner, got.Version)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Version)
	assert.Equal(t, testOrigin, updated.OriginID)

	// Empty group on a core resource.
	rID := coreID(desire.TypeRead, "cm")
	read, err := s.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: rID, Owner: testOwner, OriginID: testOrigin, TargetVersion: "v1",
	})
	require.NoError(t, err)
	assert.Equal(t, "v1", read.TargetVersion)
	gotRead, err := s.GetReadDesire(ctx, rID)
	require.NoError(t, err)
	assert.Equal(t, rID, gotRead.Identity)

	dID := coreID(desire.TypeDelete, "cm")
	del, err := s.CreateDeleteDesire(ctx, desire.DeleteDesire{Identity: dID, Owner: testOwner, OriginID: testOrigin})
	require.NoError(t, err)
	gotDel, err := s.GetDeleteDesire(ctx, dID)
	require.NoError(t, err)
	assert.Equal(t, del.Version, gotDel.Version)

	require.NoError(t, s.DeleteApplyDesire(ctx, aID, testOwner, updated.Version))
	require.NoError(t, s.DeleteReadDesire(ctx, rID, testOwner, read.Version))
	require.NoError(t, s.DeleteDeleteDesire(ctx, dID, testOwner, del.Version))

	_, err = s.GetApplyDesire(ctx, aID)
	assert.ErrorIs(t, err, desire.ErrNotFound)
	err = s.DeleteReadDesire(ctx, rID, testOwner, read.Version)
	assert.ErrorIs(t, err, desire.ErrNotFound)

	// The colon survives in the raw request path.
	assert.Contains(t, f.recorded()[0].rawPath,
		"/desires/apply:rbac.authorization.k8s.io:clusterroles::system:controller:x")
}

func TestListAcrossPagesAndSkipsDeletedReference(t *testing.T) {
	f := newFakeServer(t, "")
	f.pageSize = 2
	s, _ := newTestStore(t, f.srv.URL)
	ctx := t.Context()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		_, err := s.CreateReadDesire(ctx, desire.ReadDesire{
			Identity: coreID(desire.TypeRead, name), Owner: testOwner, OriginID: testOrigin, TargetVersion: "v1",
		})
		require.NoError(t, err)
	}
	_, err := s.CreateApplyDesire(ctx, newApply(coreID(desire.TypeApply, "a"), configMap("a", 1)))
	require.NoError(t, err)

	reads, err := s.ListReadDesires(ctx, testPartition)
	require.NoError(t, err)
	names := make([]string, 0, len(reads))
	for _, r := range reads {
		names = append(names, r.Identity.Name)
	}
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, names)
	pages := 0
	for _, r := range f.recorded() {
		if r.method == http.MethodGet && strings.HasSuffix(r.rawPath, "/desires") {
			pages++
			assert.Contains(t, r.query, "type=read")
			assert.Contains(t, r.query, "limit=1000")
		}
	}
	assert.Equal(t, 3, pages)

	applies, err := s.ListApplyDesires(ctx, testPartition)
	require.NoError(t, err)
	require.Len(t, applies, 1)
	assert.Equal(t, "a", applies[0].Identity.Name)
	deletes, err := s.ListDeleteDesires(ctx, testPartition)
	require.NoError(t, err)
	assert.Empty(t, deletes)

	// A reference deleted between the list and the get is skipped.
	f.script(fault{method: http.MethodGet, commit: true}) // the list page itself
	f.script(fault{method: http.MethodGet, status: http.StatusNotFound, body: problemBody(404, codeNotFound)})
	reads, err = s.ListReadDesires(ctx, testPartition)
	require.NoError(t, err)
	assert.Len(t, reads, 4)
}

func TestListReadDesiresMatchingReadsOnlyKeptRecords(t *testing.T) {
	f := newFakeServer(t, "")
	s, _ := newTestStore(t, f.srv.URL)
	ctx := t.Context()
	for _, name := range []string{"a", "b", "c"} {
		_, err := s.CreateReadDesire(ctx, desire.ReadDesire{
			Identity: coreID(desire.TypeRead, name), Owner: testOwner, OriginID: testOrigin, TargetVersion: "v1",
		})
		require.NoError(t, err)
	}
	before := len(f.recorded())

	reads, err := s.ListReadDesiresMatching(ctx, testPartition, func(id desire.Identity) bool {
		return id.Name == "b"
	})
	require.NoError(t, err)
	require.Len(t, reads, 1)
	assert.Equal(t, "b", reads[0].Identity.Name)
	var gets []string
	for _, r := range f.recorded()[before:] {
		if r.method == http.MethodGet && !strings.HasSuffix(r.rawPath, "/desires") {
			gets = append(gets, r.rawPath)
		}
	}
	require.Len(t, gets, 1)
	assert.True(t, strings.HasSuffix(gets[0], ":b"), gets[0])
}

func TestConflictCodesMapToSentinels(t *testing.T) {
	tests := []struct {
		want error
		code string
	}{
		{desire.ErrAlreadyExists, codeAlreadyExists},
		{desire.ErrVersionConflict, codeVersionConflict},
		{desire.ErrDeletePending, codeDeletePending},
		{desire.ErrOwnerConflict, codeOwnerConflict},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			f := newFakeServer(t, "")
			s, _ := newTestStore(t, f.srv.URL)
			f.script(fault{method: http.MethodPut, status: http.StatusConflict, body: problemBody(409, tt.code)})
			_, err := s.CreateApplyDesire(t.Context(), newApply(coreID(desire.TypeApply, "x"), configMap("x", 1)))
			require.ErrorIs(t, err, tt.want)
			var pe *ProblemError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, "create apply record: store API returned HTTP 409 "+tt.code, err.Error())
			// Conflicts are never retried by the store.
			assert.Equal(t, 1, f.count(http.MethodPut))
		})
	}
}

func TestRealConflictsFromTheStore(t *testing.T) {
	f := newFakeServer(t, "")
	s, _ := newTestStore(t, f.srv.URL)
	ctx := t.Context()
	id := coreID(desire.TypeApply, "x")
	created, err := s.CreateApplyDesire(ctx, newApply(id, configMap("x", 1)))
	require.NoError(t, err)

	_, err = s.CreateApplyDesire(ctx, newApply(id, configMap("x", 1)))
	assert.ErrorIs(t, err, desire.ErrAlreadyExists)

	_, err = s.UpdateApplyDesireSpec(ctx, id, desire.ApplySpec{KubeContent: configMap("x", 2)}, testOwner,
		created.Version+5)
	assert.ErrorIs(t, err, desire.ErrVersionConflict)
	assert.ErrorIs(t, s.DeleteApplyDesire(ctx, id, testOwner, created.Version+5), desire.ErrVersionConflict)

	other, err := New(f.srv.URL, testCreds{identity: "system:serviceaccount:hyperfleet:other"})
	require.NoError(t, err)
	before := f.count(http.MethodPut)
	_, err = other.CreateReadDesire(ctx, desire.ReadDesire{
		Identity: coreID(desire.TypeRead, "x"), Owner: other.Owner(), OriginID: testOrigin, TargetVersion: "v1",
	})
	assert.ErrorIs(t, err, desire.ErrOwnerConflict)
	assert.Equal(t, before+1, f.count(http.MethodPut), "owner conflict makes exactly one request")
}

func TestTimedOutCreate(t *testing.T) {
	id := coreID(desire.TypeApply, "x")
	t.Run("change present after commit is success", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		f.script(fault{method: http.MethodPut, commit: true, delay: time.Second})
		got, err := s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		require.NoError(t, err)
		assert.Equal(t, int64(1), got.Version)
		assert.Equal(t, 1, f.count(http.MethodPut))
		assert.Equal(t, 1, f.count(http.MethodGet))
	})
	t.Run("dropped connection after commit is success", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		f.script(fault{method: http.MethodPut, commit: true, drop: true})
		_, err := s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		require.NoError(t, err)
		assert.Equal(t, 1, f.count(http.MethodPut))
	})
	t.Run("absent record is retried", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		f.script(fault{method: http.MethodPut, delay: time.Second})
		_, err := s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		require.NoError(t, err)
		assert.Equal(t, 2, f.count(http.MethodPut))
	})
	t.Run("different content is already exists", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		_, err := f.store.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 9)))
		require.NoError(t, err)
		f.script(fault{method: http.MethodPut, status: http.StatusServiceUnavailable,
			body: problemBody(503, "HYPERFLEET-SVC-001")})
		_, err = s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		require.ErrorIs(t, err, desire.ErrAlreadyExists)
		assert.NotContains(t, strings.ToLower(err.Error()), "desire")
		assert.Equal(t, 1, f.count(http.MethodPut))
	})
	t.Run("record of another owner is owner conflict", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		foreign := newApply(id, configMap("x", 1))
		foreign.Owner = "someone-else"
		_, err := f.store.CreateApplyDesire(t.Context(), foreign)
		require.NoError(t, err)
		f.script(fault{method: http.MethodPut, status: http.StatusGatewayTimeout, body: "upstream timeout",
			header: map[string]string{"Content-Type": "text/plain"}, skipResponseValidation: true})
		_, err = s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		require.ErrorIs(t, err, desire.ErrOwnerConflict)
	})
	t.Run("read target version decides", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		f.script(fault{method: http.MethodPut, commit: true, status: http.StatusBadGateway, body: "bad gateway",
			header: map[string]string{"Content-Type": "text/plain"}, skipResponseValidation: true})
		got, err := s.CreateReadDesire(t.Context(), desire.ReadDesire{
			Identity: coreID(desire.TypeRead, "x"), Owner: testOwner, OriginID: testOrigin, TargetVersion: "v1",
		})
		require.NoError(t, err)
		assert.Equal(t, "v1", got.TargetVersion)
		assert.Equal(t, 1, f.count(http.MethodPut))
	})
	t.Run("delete desire present is success", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		f.script(fault{method: http.MethodPut, commit: true, delay: time.Second})
		_, err := s.CreateDeleteDesire(t.Context(), desire.DeleteDesire{
			Identity: coreID(desire.TypeDelete, "x"), Owner: testOwner, OriginID: testOrigin,
		})
		require.NoError(t, err)
		assert.Equal(t, 1, f.count(http.MethodPut))
	})
}

func TestFailedRecoveryReadIsRetried(t *testing.T) {
	f := newFakeServer(t, "")
	s, clock := newTestStore(t, f.srv.URL)
	f.script(
		fault{method: http.MethodPut, status: http.StatusServiceUnavailable, body: problemBody(503, "HYPERFLEET-SVC-001")},
		fault{method: http.MethodGet, status: http.StatusServiceUnavailable, body: problemBody(503, "HYPERFLEET-SVC-001")},
	)
	_, err := s.CreateApplyDesire(t.Context(), newApply(coreID(desire.TypeApply, "x"), configMap("x", 1)))
	require.NoError(t, err)
	assert.Equal(t, 2, f.count(http.MethodPut))
	assert.Equal(t, 2, f.count(http.MethodGet))
	assert.Len(t, clock.slept(), 2)
}

func TestTimedOutUpdate(t *testing.T) {
	id := coreID(desire.TypeApply, "x")
	setup := func(t *testing.T) (*fakeServer, *Store, desire.ApplyDesire) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		created, err := s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		require.NoError(t, err)
		return f, s, created
	}
	spec := desire.ApplySpec{KubeContent: configMap("x", 2)}

	t.Run("change present is success", func(t *testing.T) {
		f, s, created := setup(t)
		f.script(fault{method: http.MethodPut, commit: true, delay: time.Second})
		got, err := s.UpdateApplyDesireSpec(t.Context(), id, spec, testOwner, created.Version)
		require.NoError(t, err)
		assert.Equal(t, created.Version+1, got.Version)
		assert.Equal(t, 2, f.count(http.MethodPut))
	})
	t.Run("same version is retried", func(t *testing.T) {
		f, s, created := setup(t)
		f.script(fault{method: http.MethodPut, delay: time.Second})
		got, err := s.UpdateApplyDesireSpec(t.Context(), id, spec, testOwner, created.Version)
		require.NoError(t, err)
		assert.Equal(t, created.Version+1, got.Version)
		assert.Equal(t, 3, f.count(http.MethodPut))
	})
	t.Run("other change is version conflict", func(t *testing.T) {
		f, s, created := setup(t)
		f.script(fault{method: http.MethodPut, status: http.StatusServiceUnavailable,
			body: problemBody(503, "HYPERFLEET-SVC-001")})
		// Another writer changes the record while the client waits.
		_, err := f.store.UpdateApplyDesireSpec(t.Context(), id,
			desire.ApplySpec{KubeContent: configMap("x", 7)}, testOwner, created.Version)
		require.NoError(t, err)
		_, err = s.UpdateApplyDesireSpec(t.Context(), id, spec, testOwner, created.Version)
		require.ErrorIs(t, err, desire.ErrVersionConflict)
		assert.Equal(t, 2, f.count(http.MethodPut))
	})
	t.Run("absent is not found", func(t *testing.T) {
		f, s, created := setup(t)
		f.script(fault{method: http.MethodPut, status: http.StatusServiceUnavailable,
			body: problemBody(503, "HYPERFLEET-SVC-001")})
		// The record goes away while the client waits to recover.
		s.sleep = func(ctx context.Context, _ time.Duration) error {
			if err := f.store.DeleteApplyDesire(ctx, id, testOwner, created.Version); err != nil &&
				!errors.Is(err, desire.ErrNotFound) {
				t.Errorf("delete: %v", err)
			}
			return ctx.Err()
		}
		_, err := s.UpdateApplyDesireSpec(t.Context(), id, spec, testOwner, created.Version)
		require.ErrorIs(t, err, desire.ErrNotFound)
		assert.NotContains(t, strings.ToLower(err.Error()), "desire")
	})
}

func TestUpdateResendsStoredResourceID(t *testing.T) {
	f := newFakeServer(t, "")
	s, _ := newTestStore(t, f.srv.URL)
	id := coreID(desire.TypeApply, "x")
	created, err := s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
	require.NoError(t, err)

	_, err = s.UpdateApplyDesireSpec(t.Context(), id, desire.ApplySpec{KubeContent: configMap("x", 2)}, testOwner,
		created.Version)
	require.NoError(t, err)
	reqs := f.recorded()
	require.Len(t, reqs, 3)
	assert.Equal(t, http.MethodGet, reqs[1].method)
	assert.Equal(t, http.MethodPut, reqs[2].method)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(reqs[2].body), &body))
	assert.Equal(t, testOrigin, body["resource_id"])
	assert.EqualValues(t, created.Version, body["version"])
	assert.NotContains(t, body, "owner")
}

func TestTimedOutDelete(t *testing.T) {
	id := coreID(desire.TypeRead, "x")
	setup := func(t *testing.T) (*fakeServer, *Store, desire.ReadDesire) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		s.requestTimeout = shortTimeout
		created, err := s.CreateReadDesire(t.Context(), desire.ReadDesire{
			Identity: id, Owner: testOwner, OriginID: testOrigin, TargetVersion: "v1",
		})
		require.NoError(t, err)
		return f, s, created
	}
	t.Run("absent is success", func(t *testing.T) {
		f, s, created := setup(t)
		f.script(fault{method: http.MethodDelete, commit: true, delay: time.Second})
		require.NoError(t, s.DeleteReadDesire(t.Context(), id, testOwner, created.Version))
		assert.Equal(t, 1, f.count(http.MethodDelete))
	})
	t.Run("same version is retried", func(t *testing.T) {
		f, s, created := setup(t)
		f.script(fault{method: http.MethodDelete, delay: time.Second})
		require.NoError(t, s.DeleteReadDesire(t.Context(), id, testOwner, created.Version))
		assert.Equal(t, 2, f.count(http.MethodDelete))
		_, err := f.store.GetReadDesire(t.Context(), id)
		assert.ErrorIs(t, err, desire.ErrNotFound)
	})
	t.Run("new version is not retried", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, _ := newTestStore(t, f.srv.URL)
		aID := coreID(desire.TypeApply, "x")
		created, err := s.CreateApplyDesire(t.Context(), newApply(aID, configMap("x", 1)))
		require.NoError(t, err)
		// The delete commits, and another writer recreates and updates the
		// record while the client waits, so it is at a new version.
		f.script(fault{method: http.MethodDelete, commit: true, status: http.StatusServiceUnavailable,
			body: problemBody(503, "HYPERFLEET-SVC-001")})
		s.sleep = func(ctx context.Context, _ time.Duration) error {
			again, createErr := f.store.CreateApplyDesire(ctx, newApply(aID, configMap("x", 2)))
			require.NoError(t, createErr)
			_, updateErr := f.store.UpdateApplyDesireSpec(ctx, aID, desire.ApplySpec{KubeContent: configMap("x", 3)},
				testOwner, again.Version)
			require.NoError(t, updateErr)
			return ctx.Err()
		}
		err = s.DeleteApplyDesire(t.Context(), aID, testOwner, created.Version)
		require.ErrorIs(t, err, desire.ErrVersionConflict)
		require.ErrorIs(t, err, ErrRecreated)
		assert.Equal(t, 1, f.count(http.MethodDelete))
		_, err = f.store.GetApplyDesire(t.Context(), aID)
		require.NoError(t, err, "the recreated record stays")
	})
	t.Run("version conflict on the retried delete", func(t *testing.T) {
		f, s, created := setup(t)
		f.script(fault{method: http.MethodDelete, status: 503, body: problemBody(503, "HYPERFLEET-SVC-001")},
			fault{method: http.MethodDelete, status: 409, body: problemBody(409, codeVersionConflict)})
		err := s.DeleteReadDesire(t.Context(), id, testOwner, created.Version)
		require.ErrorIs(t, err, desire.ErrVersionConflict)
		require.ErrorIs(t, err, ErrRecreated)
	})
	t.Run("404 on the retried delete is success", func(t *testing.T) {
		f, s, created := setup(t)
		f.script(fault{method: http.MethodDelete, status: http.StatusServiceUnavailable,
			body: problemBody(503, "HYPERFLEET-SVC-001")})
		// The recovery read sees the record at the same version. Another
		// caller then deletes it, so the retried delete gets a real 404.
		f.afterGet = func() {
			require.NoError(t, f.store.DeleteReadDesire(t.Context(), id, testOwner, created.Version))
		}
		require.NoError(t, s.DeleteReadDesire(t.Context(), id, testOwner, created.Version))
		assert.Equal(t, 2, f.count(http.MethodDelete))
		_, err := f.store.GetReadDesire(t.Context(), id)
		require.ErrorIs(t, err, desire.ErrNotFound)
	})
	t.Run("404 on the first delete is not found", func(t *testing.T) {
		f, s, created := setup(t)
		require.NoError(t, f.store.DeleteReadDesire(t.Context(), id, testOwner, created.Version))
		err := s.DeleteReadDesire(t.Context(), id, testOwner, created.Version)
		require.ErrorIs(t, err, desire.ErrNotFound)
	})
}

func TestRateLimit(t *testing.T) {
	id := coreID(desire.TypeApply, "x")
	t.Run("waits for Retry-After", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, clock := newTestStore(t, f.srv.URL)
		f.script(fault{method: http.MethodPut, status: http.StatusTooManyRequests,
			header: map[string]string{retryAfterKey: "7"}, body: problemBody(429, "HYPERFLEET-LMT-001")})
		_, err := s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		require.NoError(t, err)
		assert.Equal(t, []time.Duration{7 * time.Second}, clock.slept())
		// 429 is not ambiguous: no recovery read.
		assert.Equal(t, 0, f.count(http.MethodGet))
	})
	t.Run("missing Retry-After falls back to backoff", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, clock := newTestStore(t, f.srv.URL)
		f.script(fault{method: http.MethodPut, status: http.StatusTooManyRequests,
			body: problemBody(429, "HYPERFLEET-LMT-001"), skipResponseValidation: true})
		_, err := s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		require.NoError(t, err)
		assert.Equal(t, []time.Duration{defaultInitialDelay}, clock.slept())
	})
	t.Run("stops when the wait passes the window", func(t *testing.T) {
		f := newFakeServer(t, "")
		s, clock := newTestStore(t, f.srv.URL)
		f.script(fault{method: http.MethodPut, status: http.StatusTooManyRequests,
			header: map[string]string{retryAfterKey: "300"}, body: problemBody(429, "HYPERFLEET-LMT-001")})
		_, err := s.CreateApplyDesire(t.Context(), newApply(id, configMap("x", 1)))
		var pe *ProblemError
		require.ErrorAs(t, err, &pe)
		assert.Equal(t, http.StatusTooManyRequests, pe.StatusCode)
		assert.Equal(t, 300*time.Second, pe.RetryAfter)
		assert.Empty(t, clock.slept())
		assert.Equal(t, 1, f.count(http.MethodPut))
	})
}

func TestRetryWindowCloses(t *testing.T) {
	f := newFakeServer(t, "")
	s, clock := newTestStore(t, f.srv.URL)
	for range 50 {
		f.script(fault{method: http.MethodGet, status: http.StatusServiceUnavailable,
			body: problemBody(503, "HYPERFLEET-SVC-001")})
	}
	_, err := s.GetApplyDesire(t.Context(), coreID(desire.TypeApply, "x"))
	var pe *ProblemError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, http.StatusServiceUnavailable, pe.StatusCode)
	var total time.Duration
	for _, d := range clock.slept() {
		assert.LessOrEqual(t, d, defaultMaxDelay)
		total += d
	}
	assert.LessOrEqual(t, total, defaultRetryWindow)
	assert.Greater(t, f.count(http.MethodGet), 2)
}

func TestNonRetryableErrorsMakeOneRequest(t *testing.T) {
	const badIDBody = `{"type":"https://api.hyperfleet.io/errors/validation-error",` +
		`"title":"Validation Error","status":400,"code":"HYPERFLEET-VAL-005","detail":"invalid Desire id",` +
		`"errors":[{"field":"desire-id","message":"bad Desire id"}]}`
	const unprocessableBody = `{"type":"https://api.hyperfleet.io/errors/validation-error",` +
		`"title":"Validation Error","status":422,"code":"HYPERFLEET-VAL-002",` +
		`"errors":[{"field":"kubeContent","message":"identity mismatch"},{"field":"desireName","message":"x"}]}`
	tests := []struct {
		name   string
		text   string
		fault  fault
		status int
	}{
		{
			name: "400", fault: fault{status: 400, body: badIDBody}, status: 400,
			text: "store API returned HTTP 400 HYPERFLEET-VAL-005 desire-id",
		},
		{
			name: "401 without body", fault: fault{status: 401, skipResponseValidation: true}, status: 401,
			text: "store API returned HTTP 401",
		},
		{
			name: "403", fault: fault{status: 403, body: problemBody(403, "HYPERFLEET-AUZ-001")}, status: 403,
			text: "store API returned HTTP 403 HYPERFLEET-AUZ-001",
		},
		{
			name: "422", fault: fault{status: 422, body: unprocessableBody}, status: 422,
			text: "store API returned HTTP 422 HYPERFLEET-VAL-002 kubeContent, desireName",
		},
		{
			name: "500", fault: fault{status: 500, body: problemBody(500, "HYPERFLEET-INT-001")}, status: 500,
			text: "store API returned HTTP 500 HYPERFLEET-INT-001",
		},
		{
			name: "unknown endpoint 404", fault: fault{status: 404, body: problemBody(404, "HYPERFLEET-NTF-000")},
			status: 404, text: "store API returned HTTP 404 HYPERFLEET-NTF-000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeServer(t, "")
			s, _ := newTestStore(t, f.srv.URL)
			ft := tt.fault
			ft.method = http.MethodPut
			f.script(ft)
			_, err := s.CreateApplyDesire(t.Context(), newApply(coreID(desire.TypeApply, "x"), configMap("x", 1)))
			var pe *ProblemError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, tt.status, pe.StatusCode)
			assert.Equal(t, "create apply record: "+tt.text, err.Error())
			assert.NotErrorIs(t, err, desire.ErrNotFound)
			assert.Equal(t, 1, f.count(http.MethodPut))
			assert.Equal(t, 0, f.count(http.MethodGet))
		})
	}
}

func newTLSServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}})
	}))
	t.Cleanup(srv.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(caFile, certPEM, 0o600))
	return srv, caFile
}

func TestUntrustedTLSIsNotRetried(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}})
	}))
	t.Cleanup(srv.Close)
	s, clock := newTestStore(t, srv.URL)
	_, err := s.ListApplyDesires(t.Context(), testPartition)
	var te *TransportError
	require.ErrorAs(t, err, &te)
	assert.True(t, strings.HasPrefix(err.Error(), "store API request failed: tls: failed to verify certificate"),
		err.Error())
	assert.Empty(t, clock.slept())
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 0, hits, "the handshake fails before any request reaches the handler")
}

func TestCAFileTrustsTheServer(t *testing.T) {
	srv, caFile := newTLSServer(t)
	s, err := New(srv.URL, testCreds{identity: testOwner}, WithCAFile(caFile))
	require.NoError(t, err)
	got, err := s.ListApplyDesires(t.Context(), testPartition)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCAFileErrors(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
	_, err := New("https://store.example", testCreds{identity: testOwner}, WithCAFile(empty))
	require.ErrorContains(t, err, "contains no PEM certificate")

	_, err = New("https://store.example", testCreds{identity: testOwner}, WithCAFile(filepath.Join(t.TempDir(), "x")))
	require.ErrorContains(t, err, "read CA file")
}

func TestUsageErrorsMakeNoRequest(t *testing.T) {
	f := newFakeServer(t, "")
	s, _ := newTestStore(t, f.srv.URL)
	ctx := t.Context()

	d := newApply(coreID(desire.TypeApply, "x"), configMap("x", 1))
	d.Owner = "someone-else"
	_, err := s.CreateApplyDesire(ctx, d)
	var ue *UsageError
	require.ErrorAs(t, err, &ue)
	assert.EqualError(t, err, "create apply record: owner does not match the client identity")

	d = newApply(coreID(desire.TypeApply, "x"), configMap("x", 1))
	d.OriginID = ""
	_, err = s.CreateApplyDesire(ctx, d)
	require.ErrorAs(t, err, &ue)
	assert.EqualError(t, err, "create apply record: resource_id is required by the store API and the event has no id")

	assert.ErrorAs(t, s.DeleteApplyDesire(ctx, d.Identity, "someone-else", 1), &ue)
	_, err = s.UpdateApplyDesireSpec(ctx, d.Identity, d.Spec, "someone-else", 1)
	assert.ErrorAs(t, err, &ue)

	assert.Empty(t, f.recorded())
}

func TestDeleteByPrefixUnsupported(t *testing.T) {
	s, err := New("https://store.example", testCreds{identity: testOwner})
	require.NoError(t, err)
	err = s.DeleteByPrefix(t.Context(), testPartition, desire.PrefixSelector{})
	require.ErrorIs(t, err, errors.ErrUnsupported)
	assert.NotContains(t, strings.ToLower(err.Error()), "desire")
}

func TestAuthorizationHeaderUsesSchemeAndCurrentToken(t *testing.T) {
	f := newFakeServer(t, "")
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(jwtWithSub(testOwner)), 0o600))
	creds, err := NewProjectedToken(path, "")
	require.NoError(t, err)
	s, err := New(f.srv.URL, creds)
	require.NoError(t, err)
	assert.Equal(t, testOwner, s.Owner())

	_, err = s.ListApplyDesires(t.Context(), testPartition)
	require.NoError(t, err)
	rotated := jwtWithSub(testOwner) + "x"
	require.NoError(t, os.WriteFile(path, []byte(rotated+"\n"), 0o600))
	_, err = s.ListApplyDesires(t.Context(), testPartition)
	require.NoError(t, err)

	reqs := f.recorded()
	require.Len(t, reqs, 2)
	assert.Equal(t, "Bearer "+jwtWithSub(testOwner), reqs[0].auth)
	assert.Equal(t, "Bearer "+rotated, reqs[1].auth)

	custom, err := NewProjectedToken(path, "ServiceAccount")
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodGet, "https://store.example", nil)
	require.NoError(t, err)
	require.NoError(t, custom.Authenticate(req))
	assert.Equal(t, "ServiceAccount "+rotated, req.Header.Get("Authorization"))
}

func TestBaseURLPaths(t *testing.T) {
	tests := []struct {
		name, prefix, suffix, wantPath string
	}{
		{"trailing slash", "", "/", "/api/hyperfleet/desire-store/v1/partitions/mc-01/desires"},
		{"path prefix", "/prefix", "/prefix/", "/prefix/api/hyperfleet/desire-store/v1/partitions/mc-01/desires"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeServer(t, tt.prefix)
			s, _ := newTestStore(t, f.srv.URL+tt.suffix)
			_, err := s.ListApplyDesires(t.Context(), testPartition)
			require.NoError(t, err)
			reqs := f.recorded()
			require.Len(t, reqs, 1)
			assert.Equal(t, tt.wantPath, reqs[0].rawPath)
		})
	}
}

func TestParseBaseURL(t *testing.T) {
	for _, bad := range []string{
		"http://store.example", "http://10.0.0.1",
		"store.example", "ftp://store.example", "https://", "https://user:pw@store.example",
		"https://store.example?x=1", "https://store.example?", "https://store.example#frag", "://bad",
	} {
		_, err := ParseBaseURL(bad)
		assert.Error(t, err, bad)
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost", "http://LOCALHOST"} {
		_, err := ParseBaseURL(raw)
		assert.NoError(t, err, raw)
	}
	u, err := ParseBaseURL("https://store.example/base/")
	require.NoError(t, err)
	assert.Equal(t, "https://store.example/base", u.String())
}

func TestTLSConfigKeepsSystemRoots(t *testing.T) {
	_, caFile := newTLSServer(t)
	client, err := newHTTPClient(caFile)
	require.NoError(t, err)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	cfg := transport.TLSClientConfig
	require.NotNil(t, cfg)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)

	want, err := x509.SystemCertPool()
	require.NoError(t, err)
	caPEM, err := os.ReadFile(caFile)
	require.NoError(t, err)
	require.True(t, want.AppendCertsFromPEM(caPEM))
	assert.True(t, want.Equal(cfg.RootCAs), "the pool is the system roots plus the CA file")
}

func TestDNSFailureKeepsCause(t *testing.T) {
	s, _ := newTestStore(t, "https://"+testStoreHost)
	s.client = &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.DNSError{Err: "no such host", Name: testStoreHost, IsNotFound: true}
		},
	}}
	_, err := s.GetApplyDesire(t.Context(), coreID(desire.TypeApply, "x"))
	var te *TransportError
	require.ErrorAs(t, err, &te)
	assert.Equal(t, "get apply record: store API request failed: lookup "+testStoreHost+": no such host", err.Error())
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var deletes, gets int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodDelete {
			deletes++
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		gets++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	s, _ := newTestStore(t, srv.URL)
	err := s.DeleteApplyDesire(t.Context(), coreID(desire.TypeApply, "x"), testOwner, 1)
	var pe *ProblemError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, http.StatusFound, pe.StatusCode)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, deletes)
	assert.Equal(t, 0, gets, "a redirected DELETE must not be replayed as a GET")
}

func TestRecoveryReadHonoursRetryAfter(t *testing.T) {
	f := newFakeServer(t, "")
	s, clock := newTestStore(t, f.srv.URL)
	f.script(
		fault{method: http.MethodPut, status: http.StatusServiceUnavailable, body: problemBody(503, "HYPERFLEET-SVC-001")},
		fault{method: http.MethodGet, status: http.StatusTooManyRequests,
			header: map[string]string{retryAfterKey: "20"}, body: problemBody(429, "HYPERFLEET-LMT-001")},
	)
	_, err := s.CreateApplyDesire(t.Context(), newApply(coreID(desire.TypeApply, "x"), configMap("x", 1)))
	require.NoError(t, err)
	assert.Equal(t, []time.Duration{defaultInitialDelay, 20 * time.Second}, clock.slept())
}

func TestAttemptTimeoutStopsAtWindowEnd(t *testing.T) {
	s, clock := newTestStore(t, "https://store.example")
	clock.now = clock.now.Add(time.Hour)
	_, err := s.roundTrip(t.Context(), http.MethodGet, "/x", nil, clock.now.Add(-time.Second))
	require.ErrorIs(t, err, errRetryWindowClosed)
}

func TestParseBaseURLKeepsEncodedSlash(t *testing.T) {
	u, err := ParseBaseURL("https://store.example/team%2F/")
	require.NoError(t, err)
	assert.Equal(t, "https://store.example/team%2F", u.String())
}
