package desireclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyResource_CreatesApplyAndReadDesire(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	result, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationCreate, result.Operation)

	applyID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeApply,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	applied, err := store.GetApplyDesire(ctx, applyID)
	require.NoError(t, err)
	assert.Equal(t, testOwner, applied.Owner)

	readID := applyID
	readID.Type = desire.TypeRead
	readDesire, err := store.GetReadDesire(ctx, readID)
	require.NoError(t, err, "paired read desire must be auto-created")
	assert.Equal(t, "v1", readDesire.TargetVersion)
	assert.Equal(t, testOwner, readDesire.Owner)
}

func TestApplyResource_UpdatesWithCASVersion(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)

	applyID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeApply,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	before, err := store.GetApplyDesire(ctx, applyID)
	require.NoError(t, err)
	require.Equal(t, int64(1), before.Version)

	result, err := c.ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationUpdate, result.Operation)

	after, err := store.GetApplyDesire(ctx, applyID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), after.Version, "UpdateApplyDesireSpec must have used the fetched CAS version")
	assert.Contains(t, string(after.Spec.KubeContent), `"key":"value"`)
}

func TestApplyResource_AcceptsYAMLManifest(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	yamlManifest := []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config
  namespace: default
  annotations:
    hyperfleet.io/generation: "1"
data:
  key: value
`)

	result, err := c.ApplyResource(ctx, yamlManifest, nil, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationCreate, result.Operation)

	applyID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeApply,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	applied, err := store.GetApplyDesire(ctx, applyID)
	require.NoError(t, err)
	// The store requires KubeContent to be valid JSON; a YAML input must be
	// normalized before being persisted, not stored verbatim.
	assert.True(t, json.Valid(applied.Spec.KubeContent),
		"KubeContent must be valid JSON even when the input manifest was YAML")
	assert.Contains(t, string(applied.Spec.KubeContent), `"key":"value"`)
}

func TestApplyResource_SkipsWhenGenerationUnchanged(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)

	result, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationSkip, result.Operation)
}

func TestApplyResource_ReadDesirePairingFailureIsError(t *testing.T) {
	ctx := context.Background()
	store := &failingReadDesireStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.Error(t, err, "a read-desire pairing failure must surface as an error, not a warning")
	assert.Contains(t, err.Error(), "paired read desire")
}

func TestApplyResource_RequiresTransportContext(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(newMemoryStore())

	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, nil)
	require.Error(t, err)
}

func TestApplyResource_EmptyManifestIsError(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(newMemoryStore())

	_, err := c.ApplyResource(ctx, nil, nil, testTransportContext())
	require.Error(t, err)
}

// spyDeleteReadDesireStore counts DeleteReadDesire calls so tests can assert
// whether ensureReadDesire actually attempted a recreate.
type spyDeleteReadDesireStore struct {
	desire.SpecStore
	deleteReadDesireCalls int
}

func (s *spyDeleteReadDesireStore) DeleteReadDesire(
	ctx context.Context, id desire.Identity, owner string, version int64,
) error {
	s.deleteReadDesireCalls++
	return s.SpecStore.DeleteReadDesire(ctx, id, owner, version)
}

func TestEnsureReadDesire_SameTargetVersionIsNoOp(t *testing.T) {
	ctx := context.Background()
	store := &spyDeleteReadDesireStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}

	require.NoError(t, c.ensureReadDesire(ctx, readID, "v1"))
	require.NoError(t, c.ensureReadDesire(ctx, readID, "v1"))

	assert.Zero(t, store.deleteReadDesireCalls, "matching target version must not trigger a recreate")

	rd, err := store.GetReadDesire(ctx, readID)
	require.NoError(t, err)
	assert.Equal(t, "v1", rd.TargetVersion)
}

func TestEnsureReadDesire_RecreatesOnTargetVersionChange(t *testing.T) {
	ctx := context.Background()
	store := &spyDeleteReadDesireStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}

	require.NoError(t, c.ensureReadDesire(ctx, readID, "v1"))
	require.NoError(t, c.ensureReadDesire(ctx, readID, "v2"),
		"a target version change must delete and recreate the read desire, not surface an error")

	assert.Equal(t, 1, store.deleteReadDesireCalls, "changed target version must delete the stale read desire")

	rd, err := store.GetReadDesire(ctx, readID)
	require.NoError(t, err)
	assert.Equal(t, "v2", rd.TargetVersion, "read desire must be recreated with the new target version")
}

// staleApplyVersionStore always reports version 1 for GetApplyDesire,
// regardless of the real stored version, so the caller's subsequent
// UpdateApplyDesireSpec is exercised against a genuinely stale CAS token.
type staleApplyVersionStore struct {
	desire.SpecStore
}

func (s *staleApplyVersionStore) GetApplyDesire(ctx context.Context, id desire.Identity) (desire.ApplyDesire, error) {
	d, err := s.SpecStore.GetApplyDesire(ctx, id)
	if err != nil {
		return d, err
	}
	d.Version = 1
	return d, nil
}

func TestApplyResource_VersionConflictSurfacesAsError(t *testing.T) {
	ctx := context.Background()
	memStore := newMemoryStore()
	c := newTestClient(memStore)

	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)

	applyID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeApply,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	// Bump the real version out from under the (stale-reporting) view the
	// client will see next, so its CAS write genuinely conflicts.
	_, err = memStore.UpdateApplyDesireSpec(
		ctx, applyID, desire.ApplySpec{KubeContent: configMapManifest(2)}, testOwner, 1)
	require.NoError(t, err)

	staleClient := newTestClient(&staleApplyVersionStore{SpecStore: memStore})
	_, err = staleClient.ApplyResource(ctx, configMapManifest(3), nil, testTransportContext())
	require.Error(t, err)
	assert.True(t, errors.Is(err, desire.ErrVersionConflict))
}
