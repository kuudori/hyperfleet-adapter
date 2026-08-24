package desireclient

import (
	"fmt"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/constants"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// otherTransportContext stands in for a different transport's context type
// (e.g. *maestroclient.TransportContext) to exercise the type-assertion branch
// of resolveTransportContext without importing another transport package.
type otherTransportContext struct{}

func TestResolveTransportContext_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		target  any
		wantErr string
	}{
		{name: "nil", target: nil, wantErr: "TransportContext"},
		{name: "wrong type", target: &otherTransportContext{}, wantErr: "TransportContext"},
		{
			name:    "missing management cluster",
			target:  &TransportContext{Resource: testResource},
			wantErr: "ManagementCluster",
		},
		{
			name:    "missing resource",
			target:  &TransportContext{ManagementCluster: testManagementCluster},
			wantErr: "Resource",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveTransportContext(tt.target)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestResolveTransportContext_Valid(t *testing.T) {
	in := &TransportContext{ManagementCluster: testManagementCluster, Resource: testResource}
	tc, err := resolveTransportContext(in)
	require.NoError(t, err)
	assert.Same(t, in, tc)
}

func TestBuildIdentity(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		resName   string
		wantErr   string
	}{
		{name: "valid", namespace: testNamespace, resName: testName},
		{
			name:      "invalid namespace is rejected",
			namespace: "Invalid_Namespace",
			resName:   testName,
			wantErr:   "invalid identity",
		},
		{
			name:      "invalid name is rejected",
			namespace: testNamespace,
			resName:   "Invalid_Name",
			wantErr:   "invalid identity",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := buildIdentity(testTransportContext(), desire.TypeRead, testGVK(), tt.namespace, tt.resName)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, desire.Identity{
				ManagementCluster: testManagementCluster,
				Type:              desire.TypeRead,
				Group:             testGVK().Group,
				Resource:          testResource,
				Namespace:         tt.namespace,
				Name:              tt.resName,
			}, id)
		})
	}
}

func TestParseToUnstructured(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
		wantNil bool
	}{
		{name: "valid JSON", data: configMapManifest(1)},
		{
			name: "valid YAML",
			data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: my-config\n  namespace: default\n"),
		},
		{name: "garbage is neither valid JSON nor YAML", data: []byte("{not valid: [json or yaml"), wantErr: true},
		{
			// json.Unmarshal("null", &obj.Object) succeeds with a nil map instead
			// of erroring, and the YAML fallback degrades the same way — so a
			// literal "null" manifest parses successfully into an empty object
			// rather than failing loudly. Pinning this down since it's the one
			// input where the two-stage JSON-then-YAML parse doesn't behave like
			// either "valid" or "invalid".
			name:    "literal null parses to an empty object rather than erroring",
			data:    []byte("null"),
			wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj, err := parseToUnstructured(tt.data)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, obj.Object)
				return
			}
			assert.Equal(t, "ConfigMap", obj.GetKind())
			assert.Equal(t, testName, obj.GetName())
		})
	}
}

// TestParseToUnstructured_JSONAndYAMLProduceEquivalentObject verifies the
// property the store's JSON-only contract depends on: a JSON manifest and its
// YAML equivalent must parse to the identical unstructured object, so
// ApplyResource's re-marshal-to-JSON step doesn't silently diverge in content
// depending on which format the caller happened to render.
func TestParseToUnstructured_JSONAndYAMLProduceEquivalentObject(t *testing.T) {
	jsonManifest := configMapManifest(1)
	yamlManifest := fmt.Appendf(nil, `
apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
  annotations:
    %s: "1"
data:
  key: value
`, testName, testNamespace, constants.AnnotationGeneration)

	jsonObj, err := parseToUnstructured(jsonManifest)
	require.NoError(t, err)
	yamlObj, err := parseToUnstructured(yamlManifest)
	require.NoError(t, err)

	assert.Equal(t, jsonObj.Object, yamlObj.Object,
		"JSON and YAML encodings of the same manifest must parse to the same object")
}

func TestGenerationFromKubeContent(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		want    int64
	}{
		{name: "valid content with generation annotation", content: configMapManifest(5), want: 5},
		{
			name:    "valid content without generation annotation",
			content: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x","namespace":"default"}}`),
			want:    0,
		},
		{name: "unparseable content returns zero rather than erroring", content: []byte("not-json"), want: 0},
		{name: "empty content returns zero rather than erroring", content: nil, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, generationFromKubeContent(tt.content))
		})
	}
}
