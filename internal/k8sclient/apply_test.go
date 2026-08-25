package k8sclient

import (
	"context"
	"fmt"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestClient() *Client {
	scheme := runtime.NewScheme()
	builder := fake.NewClientBuilder().WithScheme(scheme)
	return &Client{
		client: builder.Build(),
	}
}

func newConfigMap(name, namespace string, generation int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(CommonResourceKinds.ConfigMap)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.SetAnnotations(map[string]string{
		"hyperfleet.io/generation": fmt.Sprintf("%d", generation),
	})
	obj.Object["data"] = map[string]any{
		"key": "value",
	}
	return obj
}

func TestApplyManifest_CreateAlreadyExists(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	cm := newConfigMap("test-cm", "default", 1)

	// First create should succeed
	result1, err := c.ApplyManifest(ctx, cm, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationCreate, result1.Operation)

	// Second create with nil existing (simulates concurrent create race)
	// ApplyManifest sees existing=nil so decides to create, but resource already exists
	cm2 := newConfigMap("test-cm", "default", 1)
	result2, err := c.ApplyManifest(ctx, cm2, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationSkip, result2.Operation)
	assert.Equal(t, "already exists (concurrent create)", result2.Reason)
}

func TestApplyManifest_CreateSuccess(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	cm := newConfigMap("new-cm", "default", 1)
	result, err := c.ApplyManifest(ctx, cm, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationCreate, result.Operation)
}

func TestApplyManifest_SkipSameGeneration(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	cm := newConfigMap("existing-cm", "default", 1)

	// Create the resource first
	_, err := c.CreateResource(ctx, cm)
	require.NoError(t, err)

	// Get existing to pass to ApplyManifest
	existing, err := c.GetResource(ctx, CommonResourceKinds.ConfigMap, "default", "existing-cm", nil)
	require.NoError(t, err)

	// Apply with same generation should skip
	newCm := newConfigMap("existing-cm", "default", 1)
	result, err := c.ApplyManifest(ctx, newCm, existing, nil)
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationSkip, result.Operation)
}

func TestApplyManifest_NilManifest(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	result, err := c.ApplyManifest(ctx, nil, nil, nil)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "new manifest cannot be nil")
}
