package desirestore

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// projectedMountDir is the directory of the chart's projected token mount.
const projectedMountDir = "/var/run/secrets/hyperfleet-desire-store"

// DefaultTokenPath is where the adapter chart mounts the projected token for
// the store API (adapterConfig.desireStore.auth.tokenPath).
const DefaultTokenPath = projectedMountDir + "/token"

// DefaultAuthScheme is the Authorization scheme that ProjectedToken uses when
// none is configured.
const DefaultAuthScheme = "Bearer"

// Credentials authenticates requests and names the caller that the API
// records as owner.
type Credentials interface {
	// Identity is the caller identity that the API derives from the
	// credentials, for example system:serviceaccount:<namespace>:<name>.
	Identity() string
	// Authenticate sets the credential headers on a request.
	Authenticate(req *http.Request) error
}

// ProjectedToken sends a Kubernetes projected service-account token. It reads
// the file on every request, so a token that the kubelet rotates is picked up
// at once.
type ProjectedToken struct {
	path     string
	scheme   string
	identity string
}

// NewProjectedToken reads the token at path and takes the identity from its
// "sub" claim. The token is not verified: the gateway verifies it. An empty
// path means DefaultTokenPath, and an empty scheme means DefaultAuthScheme.
func NewProjectedToken(path, scheme string) (*ProjectedToken, error) {
	if path == "" {
		path = DefaultTokenPath
	}
	if scheme == "" {
		scheme = DefaultAuthScheme
	}
	token, err := readToken(path)
	if err != nil {
		return nil, err
	}
	sub, err := tokenSubject(token)
	if err != nil {
		return nil, fmt.Errorf("token file %s: %w", path, err)
	}
	return &ProjectedToken{path: path, scheme: scheme, identity: sub}, nil
}

// Identity returns the token's "sub" claim as read at construction.
func (p *ProjectedToken) Identity() string {
	return p.identity
}

// Authenticate sets Authorization: <scheme> <token> from the current file.
func (p *ProjectedToken) Authenticate(req *http.Request) error {
	token, err := readToken(p.path)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", p.scheme+" "+token)
	return nil
}

func readToken(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// tokenSubject decodes the JWT payload without verifying it and returns "sub".
// Errors never include the token.
func tokenSubject(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("token payload is not base64url")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("token payload is not a JSON object")
	}
	if claims.Sub == "" {
		return "", fmt.Errorf("token has no sub claim")
	}
	return claims.Sub, nil
}
