package executorintegrationtest

import (
	"context"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// K8sTestEnv wraps the K8s test environment for executor tests
type K8sTestEnv struct {
	Client  *k8sclient.Client
	Config  *rest.Config
	Ctx     context.Context
	cleanup func()
}

// SetupK8sTestEnv returns the shared K8s test environment
func SetupK8sTestEnv(t *testing.T) *K8sTestEnv {
	t.Helper()

	// Check if shared environment is available
	if setupErr != nil {
		t.Skipf("K8s integration tests require INTEGRATION_ENVTEST_IMAGE: %v", setupErr)
	}

	if sharedK8sEnv == nil {
		t.Skip("K8s integration tests require INTEGRATION_ENVTEST_IMAGE")
	}

	return sharedK8sEnv
}

// Cleanup cleans up the test environment
func (e *K8sTestEnv) Cleanup(t *testing.T) {
	if e.cleanup != nil {
		e.cleanup()
	}
}

// CreateTestNamespace creates a namespace for test isolation
func (e *K8sTestEnv) CreateTestNamespace(t *testing.T, name string) {
	t.Helper()

	ns := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": name,
				"labels": map[string]interface{}{
					"test":                         "executor-integration",
					"hyperfleet.io/test-namespace": "true",
				},
			},
		},
	}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"})

	_, err := e.Client.CreateResource(e.Ctx, ns)
	if err != nil && !isAlreadyExistsError(err) {
		t.Fatalf("Failed to create test namespace %s: %v", name, err)
	}
}

// CleanupTestNamespace deletes a test namespace
func (e *K8sTestEnv) CleanupTestNamespace(t *testing.T, name string) {
	t.Helper()

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"}
	err := e.Client.DeleteResource(e.Ctx, gvk, "", name, nil, nil)
	if err != nil {
		t.Logf("Warning: failed to cleanup namespace %s: %v", name, err)
	}
}

// isAlreadyExistsError checks if the error indicates the resource already exists
// Uses Kubernetes API error checking for type-safe error detection that properly unwraps error chains
func isAlreadyExistsError(err error) bool {
	return apierrors.IsAlreadyExists(err)
}
