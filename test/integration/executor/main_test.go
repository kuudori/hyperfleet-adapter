package executorintegrationtest

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/test/integration/testutil"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	EnvtestAPIServerPort = "6443/tcp"
	EnvtestReadyLog      = "Envtest is running"
	EnvtestBearerToken   = "envtest-token"
)

// sharedK8sEnv holds the shared test environment for executor integration tests
var sharedK8sEnv *K8sTestEnv

// setupErr holds any error that occurred during setup
var setupErr error

// TestMain runs before all tests to set up the shared envtest container
func TestMain(m *testing.M) {
	flag.Parse()

	// Check if we should skip integration tests
	if testing.Short() {
		os.Exit(m.Run())
	}

	// Check if INTEGRATION_ENVTEST_IMAGE is set
	imageName := os.Getenv("INTEGRATION_ENVTEST_IMAGE")
	if imageName == "" {
		println("⚠️  INTEGRATION_ENVTEST_IMAGE not set, K8s tests will be skipped")
		os.Exit(m.Run())
	}

	// Quick check if testcontainers can work
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		cancel()
		setupErr = err
		println("⚠️  Warning: Could not connect to container runtime:", err.Error())
		println("   K8s tests will be skipped")
		os.Exit(m.Run())
	}

	info, err := provider.DaemonHost(ctx)
	_ = provider.Close()
	cancel()

	if err != nil {
		setupErr = err
		println("⚠️  Warning: Could not get container runtime info:", err.Error())
		println("   K8s tests will be skipped")
		os.Exit(m.Run())
	}

	println("✅ Container runtime available:", info)
	println("🚀 Setting up shared envtest for executor tests...")

	// Set up the shared environment
	env, err := setupSharedK8sEnvtestEnv()
	if err != nil {
		setupErr = err
		println("❌ Failed to set up shared environment:", err.Error())
		println("   K8s tests will be skipped")
		os.Exit(m.Run())
	}

	sharedK8sEnv = env
	println("✅ Shared envtest container ready for executor tests!")
	println()

	// Run tests
	exitCode := m.Run()

	// Cleanup after all tests
	if sharedK8sEnv != nil && sharedK8sEnv.cleanup != nil {
		println()
		println("🧹 Cleaning up executor test envtest container...")
		sharedK8sEnv.cleanup()
	}

	os.Exit(exitCode)
}

// setupSharedK8sEnvtestEnv creates the shared envtest environment for executor tests
func setupSharedK8sEnvtestEnv() (*K8sTestEnv, error) {
	ctx := context.Background()

	imageName := os.Getenv("INTEGRATION_ENVTEST_IMAGE")

	// Start envtest container
	config := testutil.ContainerConfig{
		Name:         "executor-envtest",
		Image:        imageName,
		ExposedPorts: []string{EnvtestAPIServerPort},
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

	// Create rest.Config for the client
	restConfig := &rest.Config{
		Host:        kubeAPIServer,
		BearerToken: EnvtestBearerToken,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	// Wait for API server to be ready
	println("   Waiting for API server to be fully ready...")
	if waitErr := waitForAPIServerReady(restConfig, 30*time.Second); waitErr != nil {
		sharedContainer.Cleanup()
		return nil, fmt.Errorf("API server failed to become ready: %w", waitErr)
	}
	println("   ✅ API server is ready!")

	// Create K8s client
	client, err := k8sclient.NewClientFromConfig(ctx, restConfig)
	if err != nil {
		sharedContainer.Cleanup()
		return nil, fmt.Errorf("failed to create K8s client: %w", err)
	}

	println("   ✅ K8s client created successfully")

	// Create default namespace for tests
	println("   Creating default namespace...")
	defaultNS := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]interface{}{"name": "default"},
		},
	}
	defaultNS.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"})
	_, err = client.CreateResource(ctx, defaultNS)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create default namespace: %w", err)
	}

	return &K8sTestEnv{
		Client: client,
		Config: restConfig,
		Ctx:    ctx,
		cleanup: func() {
			sharedContainer.Cleanup()
		},
	}, nil
}

// waitForAPIServerReady waits for the API server to be ready to accept connections
// Uses a simple HTTP health check which is more reliable during startup
func waitForAPIServerReady(config *rest.Config, timeout time.Duration) error {
	// Create HTTP client with TLS verification disabled (for self-signed certs)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // Required for envtest self-signed certs
				MinVersion:         tls.VersionTLS12,
			},
		},
	}

	healthURL := config.Host + "/healthz"
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+config.BearerToken)

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil // API server is ready
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for API server to be ready")
}
