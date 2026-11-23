package desirestore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

const (
	defaultRequestTimeout = 10 * time.Second
	defaultInitialDelay   = time.Second
	defaultMaxDelay       = 30 * time.Second
	defaultRetryWindow    = 2 * time.Minute
)

// Store implements desire.SpecStore over the Desire Store REST v1 API for one
// owner. It is safe for concurrent use.
type Store struct {
	creds          Credentials
	client         *http.Client
	sleep          func(context.Context, time.Duration) error
	now            func() time.Time
	jitter         func(int64) int64
	base           string
	owner          string
	requestTimeout time.Duration
}

var _ desire.SpecStore = (*Store)(nil)

type options struct {
	caFile string
}

// Option configures a Store.
type Option func(*options)

// WithCAFile adds the PEM certificates in path to the system roots for this
// client only. The file is read once, in New.
func WithCAFile(path string) Option {
	return func(o *options) { o.caFile = path }
}

// ParseBaseURL checks a store API base URL: absolute http or https, with a
// host and without user info, query or fragment. It trims a trailing "/" and
// keeps any other path as a prefix of every request path.
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("url is not a valid URL")
	}
	switch {
	case u.User != nil:
		// Checked first so that later messages never echo credentials.
		return nil, errors.New("url must not contain user info")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("url %q must use http or https", raw)
	case u.Host == "":
		return nil, fmt.Errorf("url %q must have a host", raw)
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("url %q must not have a query", raw)
	case strings.Contains(raw, "#"):
		return nil, fmt.Errorf("url %q must not have a fragment", raw)
	}
	// Trim on the escaped form, so that an encoded slash (%2F) at the end of
	// the prefix stays part of it.
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	unescaped, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, fmt.Errorf("url %q has an invalid path", raw)
	}
	u.Path, u.RawPath = unescaped, escaped
	return u, nil
}

// New builds a Store for baseURL. The owner is creds.Identity(). New sends no
// request.
func New(baseURL string, creds Credentials, opts ...Option) (*Store, error) {
	if creds == nil {
		return nil, errors.New("credentials are required")
	}
	owner := creds.Identity()
	if owner == "" {
		return nil, errors.New("credentials have no identity")
	}
	base, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	client, err := newHTTPClient(o.caFile)
	if err != nil {
		return nil, err
	}
	return &Store{
		base:           base.String(),
		creds:          creds,
		owner:          owner,
		client:         client,
		requestTimeout: defaultRequestTimeout,
		sleep:          defaultSleep,
		now:            time.Now,
		jitter:         defaultJitter,
	}, nil
}

// newHTTPClient builds the client's HTTP client. It does not follow
// redirects: Go replays a redirected PUT or DELETE as a GET, whose success
// would read as a successful write. A 3xx response therefore surfaces as an
// error.
func newHTTPClient(caFile string) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}
	transport := base.Clone()
	if caFile != "" {
		pem, err := os.ReadFile(filepath.Clean(caFile))
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA roots: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s contains no PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// Owner returns the identity that this Store writes as.
func (s *Store) Owner() string {
	return s.owner
}

func (s *Store) checkOwner(owner string) error {
	if owner != s.owner {
		return &UsageError{Reason: "owner does not match the client identity"}
	}
	return nil
}
