package desireclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conflictStore injects conflicts into a memory store. Each hook runs before
// the call; a non-nil return replaces the call's result. The counters record
// how often each method ran.
type conflictStore struct {
	*memory.Store
	beforeCreateApply  func(n int) error
	beforeUpdateApply  func(n int) error
	beforeDeleteRead   func(n int) error
	beforeDeleteDelete func(n int) error
	beforeCreateDelete func(n int) error
	beforeCreateRead   func(n int) error
	// staleReadOnce makes the next GetReadDesire report a version one ahead
	// of the stored one, as if another writer changed it in between.
	staleReadOnce     bool
	createApplyCalls  int
	updateApplyCalls  int
	deleteReadCalls   int
	deleteDeleteCalls int
	createDeleteCalls int
	createReadCalls   int
}

func (s *conflictStore) CreateApplyDesire(ctx context.Context, d desire.ApplyDesire) (desire.ApplyDesire, error) {
	s.createApplyCalls++
	if s.beforeCreateApply != nil {
		if err := s.beforeCreateApply(s.createApplyCalls); err != nil {
			return desire.ApplyDesire{}, err
		}
	}
	return s.Store.CreateApplyDesire(ctx, d)
}

func (s *conflictStore) UpdateApplyDesireSpec(
	ctx context.Context, id desire.Identity, spec desire.ApplySpec, owner string, version int64,
) (desire.ApplyDesire, error) {
	s.updateApplyCalls++
	if s.beforeUpdateApply != nil {
		if err := s.beforeUpdateApply(s.updateApplyCalls); err != nil {
			return desire.ApplyDesire{}, err
		}
	}
	return s.Store.UpdateApplyDesireSpec(ctx, id, spec, owner, version)
}

func (s *conflictStore) GetReadDesire(ctx context.Context, id desire.Identity) (desire.ReadDesire, error) {
	d, err := s.Store.GetReadDesire(ctx, id)
	if err == nil && s.staleReadOnce {
		s.staleReadOnce = false
		d.Version++
	}
	return d, err
}

func (s *conflictStore) CreateDeleteDesire(ctx context.Context, d desire.DeleteDesire) (desire.DeleteDesire, error) {
	s.createDeleteCalls++
	if s.beforeCreateDelete != nil {
		if err := s.beforeCreateDelete(s.createDeleteCalls); err != nil {
			return desire.DeleteDesire{}, err
		}
	}
	return s.Store.CreateDeleteDesire(ctx, d)
}

func (s *conflictStore) CreateReadDesire(ctx context.Context, d desire.ReadDesire) (desire.ReadDesire, error) {
	s.createReadCalls++
	if s.beforeCreateRead != nil {
		if err := s.beforeCreateRead(s.createReadCalls); err != nil {
			return desire.ReadDesire{}, err
		}
	}
	return s.Store.CreateReadDesire(ctx, d)
}

func (s *conflictStore) DeleteReadDesire(ctx context.Context, id desire.Identity, owner string, version int64) error {
	s.deleteReadCalls++
	if s.beforeDeleteRead != nil {
		if err := s.beforeDeleteRead(s.deleteReadCalls); err != nil {
			return err
		}
	}
	return s.Store.DeleteReadDesire(ctx, id, owner, version)
}

func (s *conflictStore) DeleteDeleteDesire(ctx context.Context, id desire.Identity, owner string, version int64) error {
	s.deleteDeleteCalls++
	if s.beforeDeleteDelete != nil {
		if err := s.beforeDeleteDelete(s.deleteDeleteCalls); err != nil {
			return err
		}
	}
	return s.Store.DeleteDeleteDesire(ctx, id, owner, version)
}

func storedGeneration(t *testing.T, store desire.SpecStore) int64 {
	t.Helper()
	d, err := store.GetApplyDesire(t.Context(), testID.Apply())
	require.NoError(t, err)
	return generationFromKubeContent(d.Spec.KubeContent)
}

func TestApplyResource_CreateConflictReReadsAndUpdates(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	store.beforeCreateApply = func(n int) error {
		if n > 1 {
			return nil
		}
		// Another writer of the same owner creates generation 1 first.
		_, err := store.Store.CreateApplyDesire(ctx, desire.ApplyDesire{
			Identity: testID.Apply(), Owner: testOwner,
			Spec: desire.ApplySpec{KubeContent: json.RawMessage(configMapManifest(1))},
		})
		require.NoError(t, err)
		return desire.ErrAlreadyExists
	}

	result, err := newTestClient(store).ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationUpdate, result.Operation)
	assert.Equal(t, int64(2), storedGeneration(t, store))
	assert.Equal(t, 1, store.createApplyCalls)
	assert.Equal(t, 1, store.updateApplyCalls)
}

func TestApplyResource_CreateConflictAtSameGenerationSkips(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	store.beforeCreateApply = func(n int) error {
		if n > 1 {
			return nil
		}
		_, err := store.Store.CreateApplyDesire(ctx, desire.ApplyDesire{
			Identity: testID.Apply(), Owner: testOwner,
			Spec: desire.ApplySpec{KubeContent: json.RawMessage(configMapManifest(1))},
		})
		require.NoError(t, err)
		return desire.ErrAlreadyExists
	}

	result, err := newTestClient(store).ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationSkip, result.Operation, "the stored record already holds this generation")
}

func TestApplyResource_UpdateConflictReReadsOnce(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)
	store.beforeUpdateApply = func(n int) error {
		if n > 1 {
			return nil
		}
		// A status write advances the version under the client.
		current, getErr := store.GetApplyDesire(ctx, testID.Apply())
		require.NoError(t, getErr)
		_, statusErr := store.UpdateApplyDesireStatus(ctx, testID.Apply(), desire.Status{}, current.Version)
		require.NoError(t, statusErr)
		return desire.ErrVersionConflict
	}

	result, err := c.ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationUpdate, result.Operation)
	assert.Equal(t, int64(2), storedGeneration(t, store))
	assert.Equal(t, 2, store.updateApplyCalls)
}

func TestApplyResource_SecondConflictIsReturned(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)
	store.beforeUpdateApply = func(int) error { return desire.ErrVersionConflict }
	_, err = c.ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.ErrorIs(t, err, desire.ErrVersionConflict)
	assertNoDesireText(t, err)
	assert.Equal(t, 2, store.updateApplyCalls)

	store.beforeCreateApply = func(int) error { return desire.ErrAlreadyExists }
	store.createApplyCalls = 0
	other := testTransportContext()
	_, err = c.ApplyResource(ctx, []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"other",`+
		`"namespace":"default","annotations":{"hyperfleet.io/generation":"1"}}}`), nil, other)
	require.ErrorIs(t, err, desire.ErrAlreadyExists)
	assert.Equal(t, 2, store.createApplyCalls)
}

func TestEnsureReadDesire_DeleteConflictReReadsOnce(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	readID := testID.Read()
	require.NoError(t, c.ensureReadDesire(ctx, readID, "v1", "", false))
	// The first read reports a version that is not the stored one, so the
	// memory store itself rejects the delete. Only the re-read version works.
	store.staleReadOnce = true

	require.NoError(t, c.ensureReadDesire(ctx, readID, "v2", "", true))
	got, err := store.GetReadDesire(ctx, readID)
	require.NoError(t, err)
	assert.Equal(t, "v2", got.TargetVersion)
	assert.Equal(t, 2, store.deleteReadCalls)

	store.beforeDeleteRead = func(int) error { return desire.ErrVersionConflict }
	store.deleteReadCalls = 0
	err = c.ensureReadDesire(ctx, readID, "v3", "", true)
	require.ErrorIs(t, err, desire.ErrVersionConflict)
	assert.Equal(t, 2, store.deleteReadCalls)
}

func TestEnsureReadDesire_DeleteConflictWithMirrorGone(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	readID := testID.Read()
	require.NoError(t, c.ensureReadDesire(ctx, readID, "v1", "", false))
	store.beforeDeleteRead = func(n int) error {
		if n > 1 {
			return nil
		}
		current, err := store.GetReadDesire(ctx, readID)
		require.NoError(t, err)
		require.NoError(t, store.Store.DeleteReadDesire(ctx, readID, testOwner, current.Version))
		return desire.ErrVersionConflict
	}

	require.NoError(t, c.ensureReadDesire(ctx, readID, "v2", "", true))
	got, err := store.GetReadDesire(ctx, readID)
	require.NoError(t, err)
	assert.Equal(t, "v2", got.TargetVersion)
}

func TestCleanupAfterDeletion_DeleteConflictReReadsOnce(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	desiretest.PutConfirmedDeleteDesire(t, ctx, store.Store, testID.Delete(), testOwner)
	store.beforeDeleteDelete = func(n int) error {
		if n > 1 {
			return nil
		}
		return desire.ErrVersionConflict
	}

	require.NoError(t, c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext()))
	_, err := store.GetDeleteDesire(ctx, testID.Delete())
	require.ErrorIs(t, err, desire.ErrNotFound)
	assert.Equal(t, 2, store.deleteDeleteCalls)

	desiretest.PutConfirmedDeleteDesire(t, ctx, store.Store, testID.Delete(), testOwner)
	store.beforeDeleteDelete = func(int) error { return desire.ErrVersionConflict }
	store.deleteDeleteCalls = 0
	err = c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.ErrorIs(t, err, desire.ErrVersionConflict)
	assert.Equal(t, 2, store.deleteDeleteCalls)
}

func TestOriginIDIsStoredOnEveryCreate(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	c := newTestClient(store)
	tc := testTransportContext()
	tc.ResourceID = "2b7f4c3a-cluster-id"

	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, tc)
	require.NoError(t, err)
	apply, err := store.GetApplyDesire(ctx, testID.Apply())
	require.NoError(t, err)
	assert.Equal(t, tc.ResourceID, apply.OriginID)
	read, err := store.GetReadDesire(ctx, testID.Read())
	require.NoError(t, err)
	assert.Equal(t, tc.ResourceID, read.OriginID)

	require.NoError(t, c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, tc))
	del, err := store.GetDeleteDesire(ctx, testID.Delete())
	require.NoError(t, err)
	assert.Equal(t, tc.ResourceID, del.OriginID)
}

type staticCreds struct{ identity string }

func (c staticCreds) Identity() string { return c.identity }

func (c staticCreds) Authenticate(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer test")
	return nil
}

func TestAPIStore_MissingResourceIDHidesStoreText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		body := `{"type":"t","title":"Not Found","status":404,"code":"HYPERFLEET-NTF-001",` +
			`"detail":"Desire 'read::configmaps:default:my-config' not found."}`
		if _, err := w.Write([]byte(body)); err != nil {
			t.Logf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	store, err := desirestore.New(srv.URL, staticCreds{identity: testOwner})
	require.NoError(t, err)

	_, err = NewClient(store, store.Owner()).ApplyResource(t.Context(), configMapManifest(1), nil,
		testTransportContext())
	assertNoDesireText(t, err)
	assert.Contains(t, err.Error(), "resource_id is required")
}

func TestDeleteResource_CreateConflictIsDone(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	store.beforeCreateDelete = func(int) error {
		_, err := store.Store.CreateDeleteDesire(ctx, desire.DeleteDesire{Identity: testID.Delete(), Owner: testOwner})
		require.NoError(t, err)
		return desire.ErrAlreadyExists
	}

	require.NoError(t, c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext()))
	_, err := store.GetDeleteDesire(ctx, testID.Delete())
	require.NoError(t, err)
}

func TestEnsureReadDesire_CreateConflictRecreatesAtTargetVersion(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	readID := testID.Read()
	store.beforeCreateRead = func(n int) error {
		if n > 1 {
			return nil
		}
		// Another writer creates the mirror at another target version first.
		_, err := store.Store.CreateReadDesire(ctx, desire.ReadDesire{
			Identity: readID, Owner: testOwner, TargetVersion: "v1",
		})
		require.NoError(t, err)
		return desire.ErrAlreadyExists
	}

	require.NoError(t, c.ensureReadDesire(ctx, readID, "v2", "", true))
	got, err := store.Store.GetReadDesire(ctx, readID)
	require.NoError(t, err)
	assert.Equal(t, "v2", got.TargetVersion)
	assert.Equal(t, 1, store.deleteReadCalls)
	assert.Equal(t, 2, store.createReadCalls)
}

func TestCleanupAfterDeletion_RecreatedRecordIsNotDeletedAgain(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	desiretest.PutConfirmedDeleteDesire(t, ctx, store.Store, testID.Delete(), testOwner)
	recreated := fmt.Errorf("outcome unknown: %w", errors.Join(desire.ErrVersionConflict, desirestore.ErrRecreated))
	store.beforeDeleteDelete = func(int) error { return recreated }

	err := c.CleanupAfterDeletion(ctx, testGVK(), testNamespace, testName, testTransportContext())
	require.ErrorIs(t, err, desire.ErrVersionConflict)
	assert.Equal(t, 1, store.deleteDeleteCalls)
}

func TestEnsureReadDesire_RecreatedMirrorIsNotDeletedAgain(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	readID := testID.Read()
	require.NoError(t, c.ensureReadDesire(ctx, readID, "v1", "", false))
	store.beforeDeleteRead = func(int) error {
		return errors.Join(desire.ErrVersionConflict, desirestore.ErrRecreated)
	}

	err := c.ensureReadDesire(ctx, readID, "v2", "", true)
	require.ErrorIs(t, err, desire.ErrVersionConflict)
	assert.Equal(t, 1, store.deleteReadCalls)
}

func TestMirrorConflictRechecksApplyGeneration(t *testing.T) {
	ctx := t.Context()
	store := &conflictStore{Store: memory.New()}
	c := newTestClient(store)
	require.NoError(t, c.ensureReadDesire(ctx, testID.Read(), "v2", "", false))
	store.beforeDeleteRead = func(n int) error {
		if n > 1 {
			return nil
		}
		_, err := store.Store.CreateApplyDesire(ctx, desire.ApplyDesire{Identity: testID.Apply(), Owner: testOwner,
			Spec: desire.ApplySpec{KubeContent: json.RawMessage(configMapManifest(3))}})
		require.NoError(t, err)
		return desire.ErrVersionConflict
	}
	result, err := c.ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.NoError(t, err)
	require.Equal(t, manifest.OperationSkip, result.Operation)
	mirror, err := store.GetReadDesire(ctx, testID.Read())
	require.NoError(t, err)
	require.Equal(t, "v2", mirror.TargetVersion)
}
