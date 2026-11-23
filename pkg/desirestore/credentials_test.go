package desirestore

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jwtWithPayload(payload string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + enc.EncodeToString([]byte(payload)) + ".c2ln"
}

func jwtWithSub(sub string) string {
	return jwtWithPayload(`{"aud":["hyperfleet-desire-store"],"sub":"` + sub + `"}`)
}

func writeToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(token), 0o600))
	return path
}

func TestNewProjectedToken(t *testing.T) {
	creds, err := NewProjectedToken(writeToken(t, jwtWithSub(testOwner)+"\n"), "")
	require.NoError(t, err)
	assert.Equal(t, testOwner, creds.Identity())
	req, err := http.NewRequest(http.MethodGet, "https://store.example", nil)
	require.NoError(t, err)
	require.NoError(t, creds.Authenticate(req))
	assert.Equal(t, "Bearer "+jwtWithSub(testOwner), req.Header.Get("Authorization"))
}

func TestNewProjectedTokenErrors(t *testing.T) {
	secret := "c2VjcmV0LXRva2Vu"
	tests := []struct {
		name, token, want string
	}{
		{"no sub claim", jwtWithPayload(`{"aud":"x"}`), "token has no sub claim"},
		{"not a JWT", secret, "token is not a JWT"},
		{"payload not base64url", "a.!!!." + secret, "token payload is not base64url"},
		{"payload not JSON", jwtWithPayload("not json"), "token payload is not a JSON object"},
		{"empty file", "  \n", "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeToken(t, tt.token)
			_, err := NewProjectedToken(path, "")
			require.ErrorContains(t, err, tt.want)
			assert.Contains(t, err.Error(), path)
			assert.NotContains(t, err.Error(), secret)
		})
	}

	_, err := NewProjectedToken(filepath.Join(t.TempDir(), "missing"), "")
	require.ErrorContains(t, err, "read token file")
}

func TestAuthenticateFailsWhenTheFileGoesAway(t *testing.T) {
	path := writeToken(t, jwtWithSub(testOwner))
	creds, err := NewProjectedToken(path, "")
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	req, err := http.NewRequest(http.MethodGet, "https://store.example", nil)
	require.NoError(t, err)
	assert.Error(t, creds.Authenticate(req))
}
