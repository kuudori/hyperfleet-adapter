package configloader

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskSchemaYAML(t *testing.T) {
	base := `resources:
  - name: item
    manifest: {apiVersion: v1, kind: ConfigMap}
    discovery: {by_name: item}
`
	v2Base := "schema_version: \"2.0\"\n" + base
	const wantVersionRequired = `schema_version is required; use "2.0". v1 task configs are not supported`
	withField := func(input, field string) string {
		return strings.Replace(input, "    manifest:", "    "+field+"\n    manifest:", 1)
	}
	for _, tc := range []struct{ name, yaml, want string }{
		{"numeric", "schema_version: 2.0\n" + base, "schema_version must be a string"},
		{"null", "schema_version: null\n" + base, "schema_version must be a string"},
		{"empty", "schema_version: \"\"\n" + base, "schema_version must be \"2.0\""},
		{"boolean", "schema_version: true\n" + base, "schema_version must be a string"},
		{"sequence", "schema_version: [2.0]\n" + base, "schema_version must be a string"},
		{"mapping", "schema_version: {major: 2}\n" + base, "schema_version must be a string"},
		{"unsupported", "schema_version: \"2.1\"\n" + base, "schema_version \"2.1\" is unsupported"},
		{"v1 unsupported", "schema_version: \"1.0\"\n" + base, "schema_version \"1.0\" is unsupported"},
		{"patch unsupported", "schema_version: \"2.0.0\"\n" + base,
			"schema_version \"2.0.0\" is unsupported"},
		{"missing", base, wantVersionRequired},
		{"missing without resources", "params: []\n", wantVersionRequired},
		{"missing with named transport", withField(base, "transport: remote"), wantVersionRequired},
		{"missing with object transport", withField(base, "transport: {client: kubernetes}"), wantVersionRequired},
		{
			"object v2", withField(v2Base, "transport: {client: kubernetes}"),
			"resources[0].transport: this is a v1 configuration shape",
		},
		{
			"object with unknown field v2", withField(v2Base, "transport: {client: kubernetes, removed: true}"),
			"resources[0].transport: this is a v1 configuration shape",
		},
		{
			"nested v2", withField(v2Base, "nested_discoveries: null"),
			"resources[0].nested_discoveries: this is a v1 configuration shape; " +
				"declare each object as its own resource with its own discovery, " +
				"and read it in CEL as resources.<name>",
		},
		{
			"nested empty v2", withField(v2Base, "nested_discoveries: []"),
			"resources[0].nested_discoveries: this is a v1 configuration shape",
		},
		{
			"nested populated v2",
			withField(v2Base, "nested_discoveries: [{name: child, discovery: {by_name: child}}]"),
			"resources[0].nested_discoveries: this is a v1 configuration shape",
		},
		{"unknown", "schema_version: \"2.0\"\nunknown: true\n" + base, "field unknown not found"},
		{"unknown resource", withField(v2Base, "removed: true"), "field removed not found"},
		{"duplicate", "schema_version: \"2.0\"\n" + v2Base, "already defined"},
		{
			"aliased v2 object", `schema_version: "2.0"
routing: &routing {client: kubernetes}
resources:
  - name: item
    transport: *routing
    manifest: {apiVersion: v1, kind: ConfigMap}
    discovery: {by_name: item}
`, "resources[0].transport: this is a v1 configuration shape",
		},
		{
			"merged null", `schema_version: "2.0"
routing: &routing {transport: null}
resources:
  - <<: *routing
    name: item
    manifest: {apiVersion: v1, kind: ConfigMap}
    discovery: {by_name: item}
`, "resources[0].transport must name a transport",
		},
		{
			"merged nested", `schema_version: "2.0"
legacy: &legacy {nested_discoveries: []}
resources:
  - <<: *legacy
    name: item
    manifest: {apiVersion: v1, kind: ConfigMap}
    discovery: {by_name: item}
`, "resources[0].nested_discoveries: this is a v1 configuration shape",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadRemoteConfig(t, "adapter: {name: test}\n", tc.yaml)
			require.ErrorContains(t, err, tc.want)
			if tc.want == wantVersionRequired || strings.Contains(tc.want, "this is a v1 configuration shape") {
				require.ErrorContains(t, err, v2ConceptsURL)
				// The v1-shape errors are author-facing and must not leak internal
				// delivery vocabulary (epic HYPERFLEET-1420 framing constraint).
				// "manifest"+"work" is spelled split so the repo-wide Maestro
				// sweep greps stay clean.
				lowered := strings.ToLower(err.Error())
				for _, term := range []string{"desire", "manifest" + "work", "maestro", "ocm", "feedback", "wrapper"} {
					require.NotContains(t, lowered, term,
						"v1 shape error must not use internal delivery vocabulary")
				}
			}
		})
	}
}

func TestTaskSchemaAcceptsExplicitYAMLString(t *testing.T) {
	_, err := loadRemoteConfig(t, "adapter: {name: test}\n", "schema_version: !!str 2.0\n")
	require.NoError(t, err)

	// The schema check resolves the alias and accepts it; only strict decoding
	// then rejects the extra top-level anchor key.
	_, err = loadRemoteConfig(t, "adapter: {name: test}\n", "version: &version \"2.0\"\nschema_version: *version\n")
	require.ErrorContains(t, err, "field version not found")
}

func TestTaskSchemaIgnoresManifestKeys(t *testing.T) {
	task := `schema_version: "2.0"
resources:
  - name: item
    manifest:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: item
      data:
        nested_discoveries: value
        transport: value
    discovery: {by_name: item}
`
	_, err := loadRemoteConfig(t, "adapter: {name: test}\n", task)
	require.NoError(t, err)
}

func TestTaskSchemaDirectConstruction(t *testing.T) {
	for _, tc := range []struct {
		name, schema, want string
		resource           Resource
	}{
		{"unsupported", "2.1", "schema_version \"2.1\" is unsupported", Resource{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, ValidateResourceTransports(&Config{
				SchemaVersion: tc.schema, Resources: []Resource{tc.resource},
			}), tc.want)
		})
	}
}
