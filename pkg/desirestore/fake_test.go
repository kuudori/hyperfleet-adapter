package desirestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore/internal/wire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	dsschemas "github.com/openshift-hyperfleet/hyperfleet-desire-store-spec/schemas"
)

var (
	contractOnce   sync.Once
	contractRouter routers.Router
	contractErr    error
)

// loadContract parses the REST v1 specification that the spec module embeds.
func loadContract(t *testing.T) routers.Router {
	t.Helper()
	contractOnce.Do(func() {
		data, err := dsschemas.FS.ReadFile("desire-store/openapi.yaml")
		if err != nil {
			contractErr = err
			return
		}
		doc, err := openapi3.NewLoader().LoadFromData(data)
		if err != nil {
			contractErr = err
			return
		}
		contractRouter, contractErr = legacy.NewRouter(doc)
	})
	if contractErr != nil {
		t.Fatalf("loading the REST v1 specification: %v", contractErr)
	}
	return contractRouter
}

// testCreds sends the identity as the bearer token; the fake takes the owner
// from it, as the gateway takes it from the verified token.
type testCreds struct{ identity string }

func (c testCreds) Identity() string { return c.identity }

func (c testCreds) Authenticate(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+c.identity)
	return nil
}

// fault scripts the answer to one request.
type fault struct {
	// status, header and body form the answer when status is set.
	header map[string]string
	body   string
	// method selects the request; "" matches any.
	method string
	// delay holds the answer, past the client's per-attempt timeout.
	delay  time.Duration
	status int
	// commit runs the real operation before the fault takes effect.
	commit bool
	// drop closes the connection without an answer.
	drop bool
	// skipResponseValidation marks a gateway-style answer that the spec does
	// not describe.
	skipResponseValidation bool
}

type recordedRequest struct {
	method  string
	rawPath string
	query   string
	auth    string
	body    string
}

type fakeServer struct {
	t     *testing.T
	store *memory.Store
	srv   *httptest.Server
	// afterGet, when set, runs once after the next single-record GET.
	afterGet func()
	prefix   string
	faults   []fault
	requests []recordedRequest
	pageSize int
	mu       sync.Mutex
}

// newFakeServer starts a TLS-less server backed by the applier memory store.
// It serves the API under prefix and validates every request and every
// specification-shaped response against the embedded spec.
func newFakeServer(t *testing.T, prefix string) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t, store: memory.New(), prefix: prefix}
	// Load the spec on the test goroutine, so that a load failure stops the
	// test and the first request does not pay for it.
	loadContract(t)
	f.srv = httptest.NewServer(f.handler())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) handler() http.Handler {
	inner := http.HandlerFunc(f.serve)
	if f.prefix == "" {
		return f.record(inner)
	}
	return f.record(http.StripPrefix(f.prefix, inner))
}

func (f *fakeServer) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(f.t, r)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			method: r.Method, rawPath: r.URL.EscapedPath(), query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), body: string(body),
		})
		f.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (f *fakeServer) script(faults ...fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, faults...)
}

func (f *fakeServer) nextFault(method string) (fault, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, ft := range f.faults {
		if ft.method == "" || ft.method == method {
			f.faults = append(f.faults[:i], f.faults[i+1:]...)
			return ft, true
		}
	}
	return fault{}, false
}

func (f *fakeServer) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *fakeServer) count(method string) int {
	n := 0
	for _, r := range f.recorded() {
		if r.method == method {
			n++
		}
	}
	return n
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	body := readBody(f.t, r)
	route, pathParams := f.validateRequest(r, body)

	ft, faulted := f.nextFault(r.Method)
	if faulted && ft.commit && ft.status == 0 && ft.delay == 0 && !ft.drop {
		// Commit alone passes the request through.
		faulted = false
	}
	if faulted && !ft.commit {
		f.answerFault(w, r, route, pathParams, ft)
		return
	}
	rec := httptest.NewRecorder()
	f.dispatch(rec, r, body)
	if route != nil {
		f.validateResponse(r, route, pathParams, rec.Code, rec.Header(), rec.Body.Bytes())
	}
	if faulted {
		f.answerFault(w, r, route, pathParams, ft)
		return
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	writeBody(f.t, w, rec.Body.Bytes())
}

func (f *fakeServer) answerFault(
	w http.ResponseWriter, r *http.Request, route *routers.Route, pathParams map[string]string, ft fault,
) {
	f.t.Helper()
	if ft.delay > 0 {
		select {
		case <-r.Context().Done():
		case <-time.After(ft.delay):
		}
	}
	if ft.drop {
		hj, ok := w.(http.Hijacker)
		if !ok {
			f.t.Errorf("response writer cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			f.t.Errorf("hijack: %v", err)
			return
		}
		if err := conn.Close(); err != nil {
			f.t.Logf("close hijacked connection: %v", err)
		}
		return
	}
	if ft.status == 0 {
		// A delay with no status: the client gave up already.
		w.WriteHeader(http.StatusGatewayTimeout)
		return
	}
	for k, v := range ft.header {
		w.Header().Set(k, v)
	}
	if ft.body != "" && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/problem+json")
	}
	if route != nil && !ft.skipResponseValidation {
		f.validateResponse(r, route, pathParams, ft.status, w.Header(), []byte(ft.body))
	}
	w.WriteHeader(ft.status)
	writeBody(f.t, w, []byte(ft.body))
}

func readBody(t testing.TB, r *http.Request) []byte {
	t.Helper()
	if r.Body == nil {
		return nil
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		t.Errorf("read request body: %v", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	return buf.Bytes()
}

func (f *fakeServer) validateRequest(r *http.Request, body []byte) (*routers.Route, map[string]string) {
	f.t.Helper()
	route, pathParams, err := loadContract(f.t).FindRoute(r)
	if err != nil {
		f.t.Errorf("%s %s: no specification route: %v", r.Method, r.URL, err)
		return nil, nil
	}
	input := &openapi3filter.RequestValidationInput{
		Request: r, PathParams: pathParams, Route: route,
		Options: &openapi3filter.Options{
			MultiError: true,
			AuthenticationFunc: func(_ context.Context, in *openapi3filter.AuthenticationInput) error {
				if !strings.HasPrefix(in.RequestValidationInput.Request.Header.Get("Authorization"), "Bearer ") {
					return errors.New("no bearer token")
				}
				return nil
			},
		},
	}
	if err := openapi3filter.ValidateRequest(context.Background(), input); err != nil {
		f.t.Errorf("%s %s: the request violates the REST v1 specification: %v\nbody: %s", r.Method, r.URL, err, body)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return route, pathParams
}

func (f *fakeServer) validateResponse(
	r *http.Request, route *routers.Route, pathParams map[string]string, status int, header http.Header, body []byte,
) {
	f.t.Helper()
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: r, PathParams: pathParams, Route: route},
		Status:                 status,
		Header:                 header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	input.SetBodyBytes(body)
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		f.t.Errorf("%s %s: the %d response violates the REST v1 specification: %v\nbody: %s",
			r.Method, r.URL, status, err, body)
	}
}

// dispatch serves the API from the memory store.
func (f *fakeServer) dispatch(w http.ResponseWriter, r *http.Request, body []byte) {
	owner := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), apiPrefix)
	if !ok {
		writeProblem(f.t, w, http.StatusNotFound, "HYPERFLEET-NTF-000")
		return
	}
	segs := strings.Split(rest, "/")
	partition, err := url.PathUnescape(segs[0])
	if err != nil {
		writeProblem(f.t, w, http.StatusBadRequest, "HYPERFLEET-VAL-005")
		return
	}
	if len(segs) == 2 && segs[1] == "desires" && r.Method == http.MethodGet {
		f.list(w, r, partition)
		return
	}
	if len(segs) != 3 || segs[1] != "desires" {
		writeProblem(f.t, w, http.StatusNotFound, "HYPERFLEET-NTF-000")
		return
	}
	rawID, err := url.PathUnescape(segs[2])
	if err != nil {
		writeProblem(f.t, w, http.StatusBadRequest, "HYPERFLEET-VAL-005")
		return
	}
	id, err := parseID(partition, rawID)
	if err != nil {
		writeProblem(f.t, w, http.StatusBadRequest, "HYPERFLEET-VAL-005")
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.serveGet(w, r, id)
	case http.MethodPut:
		f.servePut(w, r, id, owner, body)
	case http.MethodDelete:
		f.serveDelete(w, r, id, owner)
	}
}

func (f *fakeServer) serveGet(w http.ResponseWriter, r *http.Request, id desire.Identity) {
	rec, err := f.get(r.Context(), id)
	writeResult(f.t, w, http.StatusOK, rec, err)
	f.mu.Lock()
	hook := f.afterGet
	f.afterGet = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (f *fakeServer) servePut(w http.ResponseWriter, r *http.Request, id desire.Identity, owner string, body []byte) {
	var in wire.DesireWrite
	if err := json.Unmarshal(body, &in); err != nil {
		writeProblem(f.t, w, http.StatusBadRequest, "HYPERFLEET-VAL-001")
		return
	}
	if id.Type != desire.TypeApply && in.Version != 0 {
		// ReadDesire and DeleteDesire have no update.
		writeProblem(f.t, w, http.StatusBadRequest, "HYPERFLEET-VAL-001")
		return
	}
	rec, status, err := f.put(r.Context(), id, owner, in)
	writeResult(f.t, w, status, rec, err)
}

func (f *fakeServer) serveDelete(w http.ResponseWriter, r *http.Request, id desire.Identity, owner string) {
	ctx := r.Context()
	version, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
	if err != nil {
		writeProblem(f.t, w, http.StatusBadRequest, "HYPERFLEET-VAL-001")
		return
	}
	switch id.Type {
	case desire.TypeApply:
		err = f.store.DeleteApplyDesire(ctx, id, owner, version)
	case desire.TypeDelete:
		err = f.store.DeleteDeleteDesire(ctx, id, owner, version)
	case desire.TypeRead:
		err = f.store.DeleteReadDesire(ctx, id, owner, version)
	}
	if err != nil {
		writeResult(f.t, w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseID(partition, raw string) (desire.Identity, error) {
	parts := strings.SplitN(raw, ":", 5)
	if len(parts) != 5 {
		return desire.Identity{}, errors.New("bad id")
	}
	return desire.Identity{
		ManagementCluster: partition, Type: desire.DesireType(parts[0]), Group: parts[1],
		Resource: parts[2], Namespace: parts[3], Name: parts[4],
	}, nil
}

func (f *fakeServer) get(ctx context.Context, id desire.Identity) (*wire.Desire, error) {
	switch id.Type {
	case desire.TypeApply:
		d, err := f.store.GetApplyDesire(ctx, id)
		return fromApply(d), err
	case desire.TypeDelete:
		d, err := f.store.GetDeleteDesire(ctx, id)
		return fromDelete(d), err
	case desire.TypeRead:
		d, err := f.store.GetReadDesire(ctx, id)
		return fromRead(d), err
	default:
		return nil, fmt.Errorf("unknown type %q", id.Type)
	}
}

func (f *fakeServer) put(
	ctx context.Context, id desire.Identity, owner string, in wire.DesireWrite,
) (*wire.Desire, int, error) {
	switch {
	case id.Type == desire.TypeApply && in.Version > 0:
		d, err := f.store.UpdateApplyDesireSpec(ctx, id, desire.ApplySpec{KubeContent: in.KubeContent}, owner, in.Version)
		return fromApply(d), http.StatusOK, err
	case id.Type == desire.TypeApply:
		d, err := f.store.CreateApplyDesire(ctx, desire.ApplyDesire{
			Identity: id, Owner: owner, OriginID: in.ResourceId, Spec: desire.ApplySpec{KubeContent: in.KubeContent},
		})
		return fromApply(d), http.StatusCreated, err
	case id.Type == desire.TypeDelete:
		d, err := f.store.CreateDeleteDesire(ctx, desire.DeleteDesire{Identity: id, Owner: owner, OriginID: in.ResourceId})
		return fromDelete(d), http.StatusCreated, err
	default:
		d, err := f.store.CreateReadDesire(ctx, desire.ReadDesire{
			Identity: id, Owner: owner, OriginID: in.ResourceId, TargetVersion: in.TargetVersion,
		})
		return fromRead(d), http.StatusCreated, err
	}
}

func (f *fakeServer) list(w http.ResponseWriter, r *http.Request, partition string) {
	f.t.Helper()
	refs, err := f.references(r.Context(), partition, desire.DesireType(r.URL.Query().Get("type")))
	if err != nil {
		f.t.Errorf("list: %v", err)
		writeProblem(f.t, w, http.StatusInternalServerError, "HYPERFLEET-INT-001")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > listPageLimit {
			writeProblem(f.t, w, http.StatusBadRequest, "HYPERFLEET-VAL-001")
			return
		}
		limit = parsed
	}
	if f.pageSize > 0 && f.pageSize < limit {
		limit = f.pageSize
	}
	writeJSON(f.t, w, http.StatusOK, paginate(refs, r.URL.Query().Get("cursor"), limit))
}

// references lists the partition's references of type t, or of every type
// when t is empty, sorted by desire ID.
func (f *fakeServer) references(
	ctx context.Context, partition string, t desire.DesireType,
) ([]wire.DesireReference, error) {
	var refs []wire.DesireReference
	add := func(id desire.Identity, version int64, targetVersion string) {
		refs = append(refs, wire.DesireReference{
			Type: wire.DesireType(id.Type), Group: id.Group, Resource: id.Resource, Namespace: id.Namespace,
			Name: id.Name, Version: version, TargetVersion: targetVersion,
		})
	}
	if t == "" || t == desire.TypeApply {
		ds, err := f.store.ListApplyDesires(ctx, partition)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			add(d.Identity, d.Version, "")
		}
	}
	if t == "" || t == desire.TypeDelete {
		ds, err := f.store.ListDeleteDesires(ctx, partition)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			add(d.Identity, d.Version, "")
		}
	}
	if t == "" || t == desire.TypeRead {
		ds, err := f.store.ListReadDesires(ctx, partition)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			add(d.Identity, d.Version, d.TargetVersion)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refKey(refs[i]) < refKey(refs[j]) })
	return refs, nil
}

func refKey(ref wire.DesireReference) string {
	return desireID(desire.Identity{
		Type: desire.DesireType(ref.Type), Group: ref.Group, Resource: ref.Resource,
		Namespace: ref.Namespace, Name: ref.Name,
	})
}

// paginate returns the page after cursor, with the last key as the next
// cursor when more references follow.
func paginate(refs []wire.DesireReference, cursor string, limit int) wire.DesireList {
	start := 0
	for start < len(refs) && cursor != "" && refKey(refs[start]) <= cursor {
		start++
	}
	page := wire.DesireList{Items: []wire.DesireReference{}}
	for i := start; i < len(refs) && len(page.Items) < limit; i++ {
		page.Items = append(page.Items, refs[i])
	}
	if start+len(page.Items) < len(refs) {
		page.NextCursor = refKey(page.Items[len(page.Items)-1])
	}
	return page
}

func fromIdentity(id desire.Identity) wire.Desire {
	return wire.Desire{
		Id: desireID(id), Type: wire.DesireType(id.Type), Group: id.Group, Resource: id.Resource,
		Namespace: id.Namespace, Name: id.Name,
	}
}

func fromApply(d desire.ApplyDesire) *wire.Desire {
	w := fromIdentity(d.Identity)
	w.Owner, w.ResourceId, w.KubeContent = d.Owner, d.OriginID, d.Spec.KubeContent
	w.Generation, w.Version = d.Generation, d.Version
	if len(d.Status.Conditions) > 0 {
		w.Status = mustJSON(d.Status)
	}
	return &w
}

func fromDelete(d desire.DeleteDesire) *wire.Desire {
	w := fromIdentity(d.Identity)
	w.Owner, w.ResourceId, w.Generation, w.Version = d.Owner, d.OriginID, d.Generation, d.Version
	if len(d.Status.Conditions) > 0 {
		w.Status = mustJSON(d.Status)
	}
	return &w
}

func fromRead(d desire.ReadDesire) *wire.Desire {
	w := fromIdentity(d.Identity)
	w.Owner, w.ResourceId, w.TargetVersion = d.Owner, d.OriginID, d.TargetVersion
	w.Generation, w.Version = d.Generation, d.Version
	if len(d.Status.KubeContent) > 0 || len(d.Status.Conditions) > 0 {
		w.Status = mustJSON(d.Status)
	}
	return &w
}

func writeResult(t testing.TB, w http.ResponseWriter, status int, rec *wire.Desire, err error) {
	t.Helper()
	switch {
	case err == nil:
		writeJSON(t, w, status, rec)
	case errors.Is(err, desire.ErrNotFound):
		writeProblem(t, w, http.StatusNotFound, codeNotFound)
	case errors.Is(err, desire.ErrAlreadyExists):
		writeProblem(t, w, http.StatusConflict, codeAlreadyExists)
	case errors.Is(err, desire.ErrVersionConflict):
		writeProblem(t, w, http.StatusConflict, codeVersionConflict)
	case errors.Is(err, desire.ErrDeletePending):
		writeProblem(t, w, http.StatusConflict, codeDeletePending)
	case errors.Is(err, desire.ErrOwnerConflict):
		writeProblem(t, w, http.StatusConflict, codeOwnerConflict)
	default:
		writeProblem(t, w, http.StatusInternalServerError, "HYPERFLEET-INT-001")
	}
}

func problemBody(status int, code string) string {
	return string(mustJSON(map[string]any{
		"type":   "https://api.hyperfleet.io/errors/test",
		"title":  http.StatusText(status),
		"status": status,
		"code":   code,
		"detail": fmt.Sprintf("Desire problem %s", code),
	}))
}

// mustJSON encodes test values that always encode.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// writeBody writes a response body. A write fails when the client gave up
// after a scripted delay, which the tests expect, so it is only logged.
func writeBody(t testing.TB, w http.ResponseWriter, body []byte) {
	t.Helper()
	if _, err := w.Write(body); err != nil {
		t.Logf("write response: %v", err)
	}
}

func writeProblem(t testing.TB, w http.ResponseWriter, status int, code string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	writeBody(t, w, []byte(problemBody(status, code)))
}

func writeJSON(t testing.TB, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeBody(t, w, mustJSON(v))
}
