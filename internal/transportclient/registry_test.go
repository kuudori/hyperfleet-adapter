package transportclient

import (
	"context"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRegistryKeepsEntriesDistinctByConfiguredName(t *testing.T) {
	registry := Registry{
		"remote-primary":   {},
		"remote-secondary": {},
	}

	assert.Len(t, registry, 2)
	assert.Contains(t, registry, "remote-primary")
	assert.Contains(t, registry, "remote-secondary")
}

func TestRegistryGetRejectsUnknownAndNilEntries(t *testing.T) {
	registry := Registry{
		"configured-but-nil": {Target: func(schema.GroupVersionKind, map[string]any) (TransportContext, error) {
			return nil, nil
		}},
	}

	tests := []struct {
		name string
		key  string
	}{
		{
			name: "unknown configured name",
			key:  "not-configured",
		},
		{
			name: "nil configured entry",
			key:  "configured-but-nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := registry.Get(tt.key)
			require.Error(t, err)
			assert.Nil(t, entry.Client)
			assert.Nil(t, entry.Target)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

func TestRegistryGetReturnsRegisteredEntry(t *testing.T) {
	registered := &stubTransportClient{}
	route := &struct{ name string }{name: "remote-route"}
	registry := Registry{"remote-primary": {
		Client: registered,
		Target: func(schema.GroupVersionKind, map[string]any) (TransportContext, error) {
			return route, nil
		},
	}}

	entry, err := registry.Get("remote-primary")

	require.NoError(t, err)
	assert.Same(t, registered, entry.Client)
	require.NotNil(t, entry.Target)
	target, err := entry.Target(schema.GroupVersionKind{}, nil)
	require.NoError(t, err)
	assert.Same(t, route, target)
}

type stubTransportClient struct{}

func (*stubTransportClient) ApplyResource(
	context.Context,
	[]byte,
	*ApplyOptions,
	TransportContext,
) (*ApplyResult, error) {
	return nil, nil
}

func (*stubTransportClient) GetResource(
	context.Context,
	schema.GroupVersionKind,
	string,
	string,
	TransportContext,
) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (*stubTransportClient) DiscoverResources(
	context.Context,
	schema.GroupVersionKind,
	manifest.Discovery,
	TransportContext,
) (*unstructured.UnstructuredList, error) {
	return nil, nil
}

func (*stubTransportClient) DeleteResource(
	context.Context,
	schema.GroupVersionKind,
	string,
	string,
	*DeleteOptions,
	TransportContext,
) error {
	return nil
}
