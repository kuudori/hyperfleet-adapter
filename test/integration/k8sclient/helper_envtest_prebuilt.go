// This file contains helper functions for setting up a pre-built image integration test environment.

package k8sclientintegration

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/test/integration/testutil"
)

const (
	// EnvtestAPIServerPort is the port the kube-apiserver listens on
	EnvtestAPIServerPort = "6443/tcp"

	// EnvtestReadyLog is the log message indicating envtest is ready
	EnvtestReadyLog = "Envtest is running"
)

// EnvtestBearerToken is the token used for authenticating with the envtest API server
const EnvtestBearerToken = "test-token"

// waitForAPIServerReady polls the API server's health endpoint until it returns 200
// or the timeout is reached. It uses short backoff retries with TLS verification disabled.
func waitForAPIServerReady(kubeAPIServer string, timeout time.Duration) error {
	// Create HTTP client with TLS verification disabled (for self-signed certs)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // Required for envtest self-signed certs
			},
		},
	}

	healthURL := kubeAPIServer + "/healthz"
	deadline := time.Now().Add(timeout)
	backoff := 500 * time.Millisecond

	var lastErr error
	var lastStatus int

	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+EnvtestBearerToken)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			lastStatus = resp.StatusCode
			if closeErr := resp.Body.Close(); closeErr != nil {
				lastErr = closeErr
			} else if lastStatus == http.StatusOK {
				return nil
			} else {
				lastErr = nil
			}
		}

		time.Sleep(backoff)
	}

	if lastErr != nil {
		return fmt.Errorf(
			"API server not ready after %v: last error: %w", timeout, lastErr)
	}
	return fmt.Errorf(
		"API server not ready after %v: last status code: %d", timeout, lastStatus)
}

// TestEnvPrebuilt holds the test environment for pre-built image integration tests
type TestEnvPrebuilt struct {
	Container testcontainers.Container
	Client    *k8sclient.Client
	Config    *rest.Config
	Ctx       context.Context
}

// Cleanup terminates the container (no-op for shared containers, use CleanupSharedEnv)
func (e *TestEnvPrebuilt) Cleanup(t *testing.T) {
	t.Helper()
	// No-op for shared containers - cleanup is handled by TestMain via CleanupSharedEnv
}

// setupSharedTestEnv creates a shared test environment for use across all tests.
// This is called from TestMain and doesn't require a *testing.T.
// It uses testutil.StartSharedContainer for container lifecycle management.
// Returns an error instead of panicking to allow graceful handling.
func setupSharedTestEnv() (*TestEnvPrebuilt, error) {
	ctx := context.Background()

	// Check that INTEGRATION_ENVTEST_IMAGE is set
	imageName := os.Getenv("INTEGRATION_ENVTEST_IMAGE")
	if imageName == "" {
		return nil, fmt.Errorf("INTEGRATION_ENVTEST_IMAGE environment variable is not set")
	}

	// Configure proxy settings from environment
	httpProxy := os.Getenv("HTTP_PROXY")
	httpsProxy := os.Getenv("HTTPS_PROXY")
	noProxy := os.Getenv("NO_PROXY")

	// Use testutil.StartSharedContainer for container lifecycle
	config := testutil.ContainerConfig{
		Name:         "envtest",
		Image:        imageName,
		ExposedPorts: []string{EnvtestAPIServerPort},
		Env: map[string]string{
			"HTTP_PROXY":  httpProxy,
			"HTTPS_PROXY": httpsProxy,
			"NO_PROXY":    noProxy,
		},
		WaitStrategy: wait.ForAll(
			wait.ForListeningPort(EnvtestAPIServerPort).WithPollInterval(500*time.Millisecond),
			wait.ForLog(EnvtestReadyLog).WithPollInterval(500*time.Millisecond),
		).WithDeadline(120 * time.Second),
		MaxRetries:     3,
		StartupTimeout: 3 * time.Minute,
	}

	sharedContainer, err := testutil.StartSharedContainer(config)
	if err != nil {
		return nil, fmt.Errorf("failed to start envtest container: %w", err)
	}

	// Get the kube-apiserver endpoint
	kubeAPIServer := fmt.Sprintf("https://%s", sharedContainer.GetEndpoint(EnvtestAPIServerPort))
	println(fmt.Sprintf("   Kube-apiserver available at: %s", kubeAPIServer))

	// Wait for API server to be fully ready with auth
	println("   Waiting for API server to be fully ready...")
	if waitErr := waitForAPIServerReady(kubeAPIServer, 30*time.Second); waitErr != nil {
		sharedContainer.Cleanup()
		return nil, fmt.Errorf("API server failed to become ready: %w", waitErr)
	}
	println("   ✅ API server is ready!")

	// Create rest.Config for the client
	restConfig := &rest.Config{
		Host:        kubeAPIServer,
		BearerToken: EnvtestBearerToken,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	// Create client
	client, err := k8sclient.NewClientFromConfig(ctx, restConfig)
	if err != nil {
		sharedContainer.Cleanup()
		return nil, fmt.Errorf("failed to create K8s client: %w", err)
	}

	// Create default namespace
	println("   Creating default namespace...")
	if err := createDefaultNamespaceNoTest(client, ctx); err != nil {
		println(fmt.Sprintf("   Warning: Could not create default namespace: %v", err))
	}

	return &TestEnvPrebuilt{
		Container: sharedContainer.Container,
		Client:    client,
		Config:    restConfig,
		Ctx:       ctx,
	}, nil
}

// CleanupSharedEnv terminates the shared container
func (e *TestEnvPrebuilt) CleanupSharedEnv() {
	if e == nil || e.Container == nil {
		return
	}
	shared := &testutil.SharedContainer{
		Container: e.Container,
		Name:      "envtest",
	}
	shared.Cleanup()
}

// createDefaultNamespaceNoTest creates the default namespace without requiring *testing.T
func createDefaultNamespaceNoTest(client *k8sclient.Client, ctx context.Context) error {
	ns := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": "default",
			},
		},
	}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"})

	_, err := client.CreateResource(ctx, ns)
	// Ignore error if namespace already exists
	if err != nil && !isAlreadyExistsError(err) {
		return err
	}
	return nil
}
