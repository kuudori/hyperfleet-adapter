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
		"remote-primary":   nil,
		"remote-secondary": nil,
	}

	assert.Len(t, registry, 2)
	assert.Contains(t, registry, "remote-primary")
	assert.Contains(t, registry, "remote-secondary")
}

func TestRegistryGetRejectsUnknownAndNilEntries(t *testing.T) {
	registry := Registry{
		"configured-but-nil": nil,
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
			client, err := registry.Get(tt.key)
			require.Error(t, err)
			assert.Nil(t, client)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

func TestRegistryGetReturnsRegisteredClient(t *testing.T) {
	registered := &stubTransportClient{}
	registry := Registry{"remote-primary": registered}

	client, err := registry.Get("remote-primary")

	require.NoError(t, err)
	assert.Same(t, registered, client)
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
