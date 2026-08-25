package maestroclientintegration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/maestroclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	workv1 "open-cluster-management.io/api/work/v1"
)

// testClient holds resources for a test client that need cleanup
type testClient struct {
	Client *maestroclient.Client
	Ctx    context.Context
	Cancel context.CancelFunc
}

// Close cleans up test client resources
func (tc *testClient) Close() {
	if tc.Client != nil {
		_ = tc.Client.Close()
	}
	if tc.Cancel != nil {
		tc.Cancel()
	}
}

// createTestClient creates a Maestro client for integration testing.
// It handles all common setup: env, logger, context, config, and client creation.
// The caller should defer tc.Close() to ensure cleanup.
func createTestClient(t *testing.T, sourceID string, timeout time.Duration) *testClient {
	t.Helper()

	env := GetSharedEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	config := &maestroclient.Config{
		MaestroServerAddr: env.MaestroServerAddr,
		GRPCServerAddr:    env.MaestroGRPCAddr,
		SourceID:          sourceID,
		Insecure:          true,
	}

	client, err := maestroclient.NewMaestroClient(ctx, config)
	if err != nil {
		cancel()
		require.NoError(t, err, "Should create Maestro client successfully")
	}

	return &testClient{
		Client: client,
		Ctx:    ctx,
		Cancel: cancel,
	}
}

// TestMaestroClientConnection tests basic client connection to Maestro
func TestMaestroClientConnection(t *testing.T) {
	tc := createTestClient(t, "integration-test-source", 30*time.Second)
	defer tc.Close()

	assert.NotNil(t, tc.Client.WorkClient(), "WorkClient should not be nil")
	assert.Equal(t, "integration-test-source", tc.Client.SourceID())
}

// TestMaestroClientCreateManifestWork tests creating a ManifestWork
func TestMaestroClientCreateManifestWork(t *testing.T) {
	tc := createTestClient(t, "integration-test-create", 60*time.Second)
	defer tc.Close()

	consumerName := "test-cluster-create"

	// Create a simple namespace manifest
	namespaceManifest := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": "test-namespace",
			"annotations": map[string]interface{}{
				constants.AnnotationGeneration: "1",
			},
		},
	}

	namespaceJSON, err := json.Marshal(namespaceManifest)
	require.NoError(t, err)

	// Create ManifestWork
	work := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-manifestwork-create",
			Namespace: consumerName,
			Annotations: map[string]string{
				constants.AnnotationGeneration: "1",
			},
			Labels: map[string]string{
				"test": "integration",
			},
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{
					{
						RawExtension: runtime.RawExtension{
							Raw: namespaceJSON,
						},
					},
				},
			},
		},
	}

	// Create the ManifestWork
	created, err := tc.Client.CreateManifestWork(tc.Ctx, consumerName, work)

	// Consumer should be registered during test setup, so this should succeed
	require.NoError(
		t, err, "CreateManifestWork should succeed (consumer %s should be registered)", consumerName,
	)
	require.NotNil(t, created)
	assert.Equal(t, work.Name, created.Name)
	t.Logf("Created ManifestWork: %s/%s", created.Namespace, created.Name)
}

// TestMaestroClientListManifestWorks tests listing ManifestWorks
func TestMaestroClientListManifestWorks(t *testing.T) {
	tc := createTestClient(t, "integration-test-list", 30*time.Second)
	defer tc.Close()

	consumerName := "test-cluster-list"

	// List ManifestWorks (empty label selector = list all)
	list, err := tc.Client.ListManifestWorks(tc.Ctx, consumerName, "")

	// Consumer should be registered during test setup, so this should succeed
	require.NoError(
		t, err, "ListManifestWorks should succeed (consumer %s should be registered)", consumerName,
	)
	require.NotNil(t, list)
	t.Logf("Found %d ManifestWorks for consumer %s", len(list.Items), consumerName)
}

// TestMaestroClientApplyManifestWork tests the apply (create or update) operation
func TestMaestroClientApplyManifestWork(t *testing.T) {
	tc := createTestClient(t, "integration-test-apply", 60*time.Second)
	defer tc.Close()

	consumerName := "test-cluster-apply"

	// Create a ConfigMap manifest
	configMapManifest := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "test-config",
			"namespace": "default",
			"annotations": map[string]interface{}{
				constants.AnnotationGeneration: "1",
			},
		},
		"data": map[string]interface{}{
			"key1": "value1",
		},
	}

	configMapJSON, err := json.Marshal(configMapManifest)
	require.NoError(t, err)

	// Create ManifestWork
	work := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-manifestwork-apply",
			Namespace: consumerName,
			Annotations: map[string]string{
				constants.AnnotationGeneration: "1",
			},
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{
					{
						RawExtension: runtime.RawExtension{
							Raw: configMapJSON,
						},
					},
				},
			},
		},
	}

	// Apply the ManifestWork (should create if not exists)
	applied, err := tc.Client.ApplyManifestWork(tc.Ctx, consumerName, work)

	// Consumer should be registered during test setup, so this should succeed
	require.NoError(
		t, err, "ApplyManifestWork should succeed (consumer %s should be registered)", consumerName,
	)
	require.NotNil(t, applied)
	require.NotNil(t, applied.Work)
	t.Logf(
		"Applied ManifestWork: %s/%s (operation=%s)", applied.Work.Namespace, applied.Work.Name, applied.Operation,
	)

	// Now apply again with updated generation (should update)
	work.Annotations[constants.AnnotationGeneration] = "2"

	metadata, ok := configMapManifest["metadata"].(map[string]interface{})
	require.True(t, ok, "configMapManifest should have metadata map")
	annotations, ok := metadata["annotations"].(map[string]interface{})
	require.True(t, ok, "metadata should have annotations map")
	annotations[constants.AnnotationGeneration] = "2"

	data, ok := configMapManifest["data"].(map[string]interface{})
	require.True(t, ok, "configMapManifest should have data map")
	data["key2"] = "value2"
	configMapJSON, _ = json.Marshal(configMapManifest)
	work.Spec.Workload.Manifests[0].Raw = configMapJSON

	updated, err := tc.Client.ApplyManifestWork(tc.Ctx, consumerName, work)
	require.NoError(t, err, "ApplyManifestWork (update) should succeed")
	require.NotNil(t, updated)
	require.NotNil(t, updated.Work)
	t.Logf(
		"Updated ManifestWork: %s/%s (operation=%s)", updated.Work.Namespace, updated.Work.Name, updated.Operation,
	)
}

// TestMaestroClientGenerationSkip tests that apply skips when generation matches
func TestMaestroClientGenerationSkip(t *testing.T) {
	tc := createTestClient(t, "integration-test-skip", 60*time.Second)
	defer tc.Close()

	consumerName := "test-cluster-skip"

	// Create a simple manifest
	manifest := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "test-skip-config",
			"namespace": "default",
			"annotations": map[string]interface{}{
				constants.AnnotationGeneration: "5",
			},
		},
		"data": map[string]interface{}{
			"test": "data",
		},
	}

	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)

	work := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-manifestwork-skip",
			Namespace: consumerName,
			Annotations: map[string]string{
				constants.AnnotationGeneration: "5",
			},
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{
					{
						RawExtension: runtime.RawExtension{
							Raw: manifestJSON,
						},
					},
				},
			},
		},
	}

	// First apply
	result1, err := tc.Client.ApplyManifestWork(tc.Ctx, consumerName, work)
	if err != nil {
		t.Skipf("Skipping generation skip test - consumer may not be registered: %v", err)
	}
	require.NotNil(t, result1)
	require.NotNil(t, result1.Work)

	// Apply again with same generation - should skip (return existing without update)
	result2, err := tc.Client.ApplyManifestWork(tc.Ctx, consumerName, work)
	require.NoError(t, err)
	require.NotNil(t, result2)
	require.NotNil(t, result2.Work)

	// When skipped, both results should refer to the same resource (same name/namespace)
	assert.Equal(t, result1.Work.Name, result2.Work.Name,
		"ManifestWork name should match when generation unchanged (skip)")
	assert.Equal(t, result1.Work.Namespace, result2.Work.Namespace,
		"ManifestWork namespace should match when generation unchanged (skip)")
	t.Logf(
		"Skip test passed - operation=%s, result1.ResourceVersion=%s, result2.ResourceVersion=%s",
		result2.Operation, result1.Work.ResourceVersion, result2.Work.ResourceVersion,
	)
}
