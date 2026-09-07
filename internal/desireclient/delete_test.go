package desireclient

import (
	"context"
	"errors"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteResource_RemovesApplyKeepsRead(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)

	applyID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeApply,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	readID := applyID
	readID.Type = desire.TypeRead
	deleteID := applyID
	deleteID.Type = desire.TypeDelete

	err = c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext())
	require.NoError(t, err)

	_, err = store.GetApplyDesire(ctx, applyID)
	assert.True(t, errors.Is(err, desire.ErrNotFound), "apply desire must be removed so nothing re-applies")

	_, err = store.GetDeleteDesire(ctx, deleteID)
	require.NoError(t, err, "delete desire must be posted")

	_, err = store.GetReadDesire(ctx, readID)
	require.NoError(t, err, "read desire must be left in place so disappearance stays observable")
}

func TestDeleteResource_WithoutApply_PostsDelete(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	err := c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext())
	require.NoError(t, err)

	deleteID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeDelete,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	_, err = store.GetDeleteDesire(ctx, deleteID)
	require.NoError(t, err)
}

func TestDeleteResource_WithoutApply_PairsRead(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	// No ApplyResource call first: this identity was never applied through
	// this client, so no read desire was ever paired in.
	err := c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext())
	require.NoError(t, err)

	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	readDesire, err := store.GetReadDesire(ctx, readID)
	require.NoError(t, err, "paired read desire must be auto-created even without a prior apply")
	assert.Equal(t, "v1", readDesire.TargetVersion)
	assert.Equal(t, testOwner, readDesire.Owner)
}

func TestDeleteResource_ReadFailure_ReturnsError(t *testing.T) {
	ctx := context.Background()
	store := &failingReadDesireStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	err := c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext())
	require.Error(t, err, "a read-desire pairing failure must surface as an error, not be swallowed")
	assert.Contains(t, err.Error(), "paired read desire")
}

// TestDeleteResource_ReadFailure_LeavesApplyIntact verifies
// the failure-injection contract for the read-before-delete ordering: an
// existing apply desire must survive a read-desire pairing failure untouched,
// and no delete desire must have been created, since the apply->delete
// transition (CreateDeleteDesire) never runs if the paired read desire can't
// be ensured first.
func TestDeleteResource_ReadFailure_LeavesApplyIntact(t *testing.T) {
	ctx := context.Background()
	inner := newMemoryStore()

	// Set up the pre-existing apply desire through a working client first —
	// ApplyResource itself pairs a read desire via the same store call the
	// failing wrapper below targets, so it must succeed here.
	c := newTestClient(inner)
	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)

	applyID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeApply,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	deleteID := applyID
	deleteID.Type = desire.TypeDelete

	failingClient := newTestClient(&failingReadDesireStore{SpecStore: inner})
	err = failingClient.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext())
	require.Error(t, err, "a read-desire pairing failure must surface as an error")

	_, err = inner.GetApplyDesire(ctx, applyID)
	assert.NoError(t, err,
		"apply desire must be untouched: the apply->delete transition must not run before the read desire is ensured")

	_, err = inner.GetDeleteDesire(ctx, deleteID)
	assert.True(t, errors.Is(err, desire.ErrNotFound),
		"delete desire must not exist: CreateDeleteDesire must not run when read-desire pairing fails first")
}

// TestDeleteResource_DeleteFailure_LeavesApplyAndReadIntact
// verifies the second failure-injection path: if CreateDeleteDesire fails
// after the read desire has already been ensured, the apply desire must
// still be present (CreateDeleteDesire never got a chance to atomically
// remove it) and the read desire must remain — a safe, retryable state
// rather than an orphaned delete desire with no observable disappearance.
func TestDeleteResource_DeleteFailure_LeavesApplyAndReadIntact(t *testing.T) {
	ctx := context.Background()
	store := &failingCreateDeleteDesireStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	_, err := c.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)

	applyID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeApply,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	readID := applyID
	readID.Type = desire.TypeRead
	deleteID := applyID
	deleteID.Type = desire.TypeDelete

	err = c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext())
	require.Error(t, err, "a delete-desire creation failure must surface as an error")
	assert.Contains(t, err.Error(), "failed to create delete desire")

	_, err = store.GetApplyDesire(ctx, applyID)
	assert.NoError(t, err,
		"apply desire must still exist: CreateDeleteDesire failed before it could atomically remove it")

	_, err = store.GetReadDesire(ctx, readID)
	assert.NoError(t, err, "read desire must still exist: it was ensured before the failed transition")

	_, err = store.GetDeleteDesire(ctx, deleteID)
	assert.True(t, errors.Is(err, desire.ErrNotFound), "delete desire must not exist since its creation failed")
}

func TestDeleteResource_DoesNotRecreateExistingReadDesire(t *testing.T) {
	ctx := context.Background()
	store := &spyDeleteReadDesireStore{SpecStore: newMemoryStore()}
	c := newTestClient(store)

	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	// Pre-existing read desire targets a different version than the GVK
	// DeleteResource will be called with below.
	require.NoError(t, c.ensureReadDesire(ctx, readID, "v1", false))

	deleteGVK := testGVK()
	deleteGVK.Version = "v1beta1"

	err := c.DeleteResource(ctx, deleteGVK, testNamespace, testName, nil, testTransportContext())
	require.NoError(t, err)

	assert.Zero(t, store.deleteReadDesireCalls,
		"DeleteResource must never recreate the read desire, even on a version mismatch")

	rd, err := store.GetReadDesire(ctx, readID)
	require.NoError(t, err)
	assert.Equal(t, "v1", rd.TargetVersion, "read desire targetVersion must be left untouched by delete")
}

func TestDeleteResource_RecreatesReadDesireIfExternallyDeleted(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	c := newTestClient(store)

	readID := desire.Identity{
		ManagementCluster: testManagementCluster, Type: desire.TypeRead,
		Resource: testResource, Namespace: testNamespace, Name: testName,
	}
	require.NoError(t, c.ensureReadDesire(ctx, readID, "v1", false))

	rd, err := store.GetReadDesire(ctx, readID)
	require.NoError(t, err)
	require.NoError(t, store.DeleteReadDesire(ctx, readID, c.owner, rd.Version),
		"simulate an external deletion of the read desire")

	err = c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext())
	require.NoError(t, err)

	_, err = store.GetReadDesire(ctx, readID)
	require.NoError(t, err,
		"an externally deleted read desire must be re-created by delete, even though recreate=false")
}

func TestDeleteResource_RequiresTransportContext(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(newMemoryStore())

	err := c.DeleteResource(ctx, testGVK(), testNamespace, testName, nil, nil)
	require.Error(t, err)
}
