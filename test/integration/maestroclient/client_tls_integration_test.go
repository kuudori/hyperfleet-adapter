package maestroclientintegration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/maestroclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTLSTestClient creates a Maestro client configured for TLS.
func createTLSTestClient(t *testing.T, config *maestroclient.Config, timeout time.Duration) *testClient {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	client, err := maestroclient.NewMaestroClient(ctx, config)
	if err != nil {
		cancel()
		require.NoError(t, err, "Should create TLS Maestro client successfully")
	}

	return &testClient{
		Client: client,
		Ctx:    ctx,
		Cancel: cancel,
	}
}

// TestTLSClientWithCAOnly tests connecting to TLS Maestro with only a CA cert
// (server verification, no client auth). This exercises the "Option 3: CA only"
// path in configureTLS.
func TestTLSClientWithCAOnly(t *testing.T) {
	env := GetSharedEnv(t)
	requireTLSEnv(t, env)

	config := &maestroclient.Config{
		MaestroServerAddr: env.TLSMaestroServerAddr,
		GRPCServerAddr:    env.TLSMaestroGRPCAddr,
		SourceID:          "tls-test-ca-only",
		CAFile:            env.TLSCerts.CAFilePath(),
		Insecure:          false,
	}

	tc := createTLSTestClient(t, config, 30*time.Second)
	defer tc.Close()

	assert.NotNil(t, tc.Client.WorkClient(), "WorkClient should not be nil with CA-only TLS")
	t.Log("CA-only TLS connection succeeded")
}

// TestTLSClientWithMutualTLS tests connecting to TLS Maestro with CA + client cert/key.
// This exercises the "Option 1: Mutual TLS with certificates" path in configureTLS.
func TestTLSClientWithMutualTLS(t *testing.T) {
	env := GetSharedEnv(t)
	requireTLSEnv(t, env)

	config := &maestroclient.Config{
		MaestroServerAddr: env.TLSMaestroServerAddr,
		GRPCServerAddr:    env.TLSMaestroGRPCAddr,
		SourceID:          "tls-test-mtls",
		CAFile:            env.TLSCerts.CAFilePath(),
		ClientCertFile:    env.TLSCerts.ClientCertFilePath(),
		ClientKeyFile:     env.TLSCerts.ClientKeyFilePath(),
		Insecure:          false,
	}

	tc := createTLSTestClient(t, config, 30*time.Second)
	defer tc.Close()

	assert.NotNil(t, tc.Client.WorkClient(), "WorkClient should not be nil with mTLS")
	t.Log("Mutual TLS connection succeeded")
}

// TestTLSClientWithToken tests connecting to TLS Maestro with CA + token file.
// This exercises the "Option 2: Token-based authentication with CA" path in configureTLS.
func TestTLSClientWithToken(t *testing.T) {
	env := GetSharedEnv(t)
	requireTLSEnv(t, env)

	// Write a dummy token file (Maestro mock auth doesn't validate tokens)
	tokenDir := t.TempDir()
	tokenFile := filepath.Join(tokenDir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("test-bearer-token"), 0o600))

	config := &maestroclient.Config{
		MaestroServerAddr: env.TLSMaestroServerAddr,
		GRPCServerAddr:    env.TLSMaestroGRPCAddr,
		SourceID:          "tls-test-token",
		CAFile:            env.TLSCerts.CAFilePath(),
		TokenFile:         tokenFile,
		Insecure:          false,
	}

	tc := createTLSTestClient(t, config, 30*time.Second)
	defer tc.Close()

	assert.NotNil(t, tc.Client.WorkClient(), "WorkClient should not be nil with token auth")
	t.Log("Token-based TLS connection succeeded")
}

// TestTLSClientWithWrongCA tests that a wrong CA cert causes HTTP API calls to fail.
// Client creation is lazy (no immediate connection), so the failure surfaces on the
// first real HTTP request when the server cert cannot be verified.
func TestTLSClientWithWrongCA(t *testing.T) {
	env := GetSharedEnv(t)
	requireTLSEnv(t, env)

	wrongCAFile := writeWrongCA(t)

	config := &maestroclient.Config{
		MaestroServerAddr: env.TLSMaestroServerAddr,
		GRPCServerAddr:    env.TLSMaestroGRPCAddr,
		SourceID:          "tls-test-wrong-ca",
		CAFile:            wrongCAFile,
		Insecure:          false,
	}

	// Client creation succeeds (connections are lazy)
	tc := createTLSTestClient(t, config, 15*time.Second)
	defer tc.Close()

	// First real HTTP call should fail: server cert signed by our test CA,
	// but client uses wrong CA → x509 verification error
	_, err := tc.Client.ListManifestWorks(tc.Ctx, "test-cluster-list", "")
	require.Error(t, err, "ListManifestWorks should fail with wrong CA cert")
	t.Logf("Wrong CA correctly rejected on HTTP call: %v", err)
}

// TestTLSClientPlaintextHTTPToTLSServer tests that sending plaintext HTTP to a
// TLS-enabled server fails. Uses http:// scheme against the HTTPS port.
func TestTLSClientPlaintextHTTPToTLSServer(t *testing.T) {
	env := GetSharedEnv(t)
	requireTLSEnv(t, env)

	// Use http:// (plaintext) against the HTTPS server port.
	// The server expects a TLS handshake, so the plaintext HTTP request will fail.
	plaintextAddr := fmt.Sprintf("http://127.0.0.1:%s", env.TLSMaestroHTTPPort)

	config := &maestroclient.Config{
		MaestroServerAddr: plaintextAddr,
		GRPCServerAddr:    env.TLSMaestroGRPCAddr,
		SourceID:          "tls-test-plaintext",
		Insecure:          true,
	}

	tc := createTLSTestClient(t, config, 15*time.Second)
	defer tc.Close()

	// Plaintext HTTP to a TLS server → the server does TLS handshake,
	// client sends raw HTTP → connection error
	_, err := tc.Client.ListManifestWorks(tc.Ctx, "test-cluster-list", "")
	require.Error(t, err, "Plaintext HTTP to TLS server should fail")
	t.Logf("Plaintext HTTP to TLS server correctly rejected: %v", err)
}

// TestTLSClientHTTPSWithCA tests that the HTTP transport correctly verifies
// the server certificate using the configured CA.
func TestTLSClientHTTPSWithCA(t *testing.T) {
	env := GetSharedEnv(t)
	requireTLSEnv(t, env)

	config := &maestroclient.Config{
		MaestroServerAddr: env.TLSMaestroServerAddr,
		GRPCServerAddr:    env.TLSMaestroGRPCAddr,
		SourceID:          "tls-test-https-ca",
		CAFile:            env.TLSCerts.CAFilePath(),
		Insecure:          false,
	}

	tc := createTLSTestClient(t, config, 60*time.Second)
	defer tc.Close()

	// List ManifestWorks - exercises the full HTTP+gRPC path with TLS
	list, err := tc.Client.ListManifestWorks(tc.Ctx, "test-cluster-list", "")
	require.NoError(t, err, "ListManifestWorks over TLS should succeed")
	require.NotNil(t, list)
	t.Logf("Listed %d ManifestWorks over TLS", len(list.Items))
}

// TestTLSClientSeparateHTTPCA tests that HTTPCAFile takes precedence over CAFile
// for HTTP connections when both are specified.
func TestTLSClientSeparateHTTPCA(t *testing.T) {
	env := GetSharedEnv(t)
	requireTLSEnv(t, env)

	config := &maestroclient.Config{
		MaestroServerAddr: env.TLSMaestroServerAddr,
		GRPCServerAddr:    env.TLSMaestroGRPCAddr,
		SourceID:          "tls-test-http-ca",
		CAFile:            env.TLSCerts.CAFilePath(),
		HTTPCAFile:        env.TLSCerts.CAFilePath(), // same CA, but exercising the HTTPCAFile path
		Insecure:          false,
	}

	tc := createTLSTestClient(t, config, 60*time.Second)
	defer tc.Close()

	list, err := tc.Client.ListManifestWorks(tc.Ctx, "test-cluster-list", "")
	require.NoError(t, err, "ListManifestWorks with HTTPCAFile should succeed")
	require.NotNil(t, list)
	t.Logf("Separate HTTPCAFile path works - listed %d ManifestWorks", len(list.Items))
}

// TestTLSNoConfigFails tests that Insecure=false without any TLS config
// produces a clear error at client creation time.
func TestTLSNoConfigFails(t *testing.T) {
	env := GetSharedEnv(t)
	requireTLSEnv(t, env)

	config := &maestroclient.Config{
		MaestroServerAddr: env.TLSMaestroServerAddr,
		GRPCServerAddr:    env.TLSMaestroGRPCAddr,
		SourceID:          "tls-test-no-config",
		Insecure:          false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := maestroclient.NewMaestroClient(ctx, config)
	require.Error(t, err, "Should fail when Insecure=false and no TLS config provided")
	assert.Contains(t, err.Error(), "no TLS configuration provided")
	t.Logf("No TLS config correctly rejected: %v", err)
}

// --- helpers ---

func requireTLSEnv(t *testing.T, env *MaestroTestEnv) {
	t.Helper()
	if env.TLSMaestroContainer == nil || env.TLSCerts == nil {
		t.Skip("TLS Maestro environment not available")
	}
}

// writeWrongCA generates a separate self-signed CA that did NOT sign the server cert.
func writeWrongCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(999),
		Subject: pkix.Name{
			Organization: []string{"Wrong CA"},
			CommonName:   "Wrong CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	dir := t.TempDir()
	caFile := filepath.Join(dir, "wrong-ca.crt")
	require.NoError(t, os.WriteFile(caFile, certPEM, 0o600))

	return caFile
}
