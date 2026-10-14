package manifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestStaticGVK(t *testing.T) {
	configMap := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	tests := []struct {
		name     string
		manifest string
		want     schema.GroupVersionKind
		wantErr  string
	}{
		{
			name:     "plain manifest",
			manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n",
			want:     configMap,
		},
		{
			name:     "grouped apiVersion",
			manifest: "apiVersion: apps/v1\nkind: Deployment\n",
			want:     schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		},
		{
			name: "template actions and blocks in other fields",
			manifest: "{{- $name := .clusterId }}\napiVersion: v1\nkind: ConfigMap\n{{- if .labels }}\nlabels:\n" +
				"  team: {{ .team | lower | quote }}\n{{- end }}\nmetadata:\n  name: \"{{ $name }}\"\n",
			want: configMap,
		},
		{
			name:     "whole document indented",
			manifest: "  apiVersion: v1\n  kind: ConfigMap\n  metadata:\n    name: test\n",
			want:     configMap,
		},
		{
			name: "nested apiVersion and kind are ignored",
			manifest: "metadata:\n  ownerReferences:\n    - apiVersion: apps/v1\n      kind: Deployment\n" +
				"apiVersion: v1\nkind: ConfigMap\ndata:\n  doc: |\n    kind: Secret\n",
			want: configMap,
		},
		{
			name: "grouped kind with nested manifests",
			manifest: "apiVersion: bundles.example.com/v1\nkind: Bundle\nspec:\n  workload:\n" +
				"    manifests:\n      - apiVersion: v1\n        kind: Namespace\n",
			want: schema.GroupVersionKind{Group: "bundles.example.com", Version: "v1", Kind: "Bundle"},
		},
		{
			name:     "comments, quotes, and a document start",
			manifest: "# header\n---\napiVersion: \"apps/v1\" # group\nkind: 'Deployment'\n",
			want:     schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		},
		{
			// Only this kind can exist; when the branch is off, apply fails on
			// the rendered manifest instead.
			name:     "kind in a single template branch",
			manifest: "apiVersion: v1\n{{ if .x }}\nkind: ConfigMap\n{{ end }}\n",
			want:     configMap,
		},
		{
			name:     "missing kind",
			manifest: "apiVersion: v1\nmetadata:\n  kind: ConfigMap\n",
			wantErr:  "kind must be set as a top-level field",
		},
		{
			name:     "missing apiVersion",
			manifest: "kind: ConfigMap\n",
			wantErr:  "apiVersion must be set as a top-level field",
		},
		{
			name:     "templated kind",
			manifest: "apiVersion: v1\nkind: \"{{ .kind }}\"\n",
			wantErr:  "line 2: kind must be a literal value",
		},
		{
			name:     "kind completed by a template action",
			manifest: "apiVersion: v1\nkind: Config{{ .suffix }}\n",
			wantErr:  "line 2: kind must be a literal value",
		},
		{
			name:     "different kinds in template branches",
			manifest: "apiVersion: v1\n{{ if .x }}\nkind: ConfigMap\n{{ else }}\nkind: Secret\n{{ end }}\n",
			wantErr:  "line 5: kind is set more than once",
		},
		{
			name:     "same kind in template branches",
			manifest: "apiVersion: v1\n{{ if .x }}\nkind: ConfigMap\n{{ else }}\nkind: ConfigMap\n{{ end }}\n",
			wantErr:  "line 5: kind is set more than once",
		},
		{
			name:     "multiple documents",
			manifest: "apiVersion: v1\nkind: ConfigMap\n---\napiVersion: v1\nkind: Secret\n",
			wantErr:  "line 4: apiVersion is set more than once",
		},
		{
			name:     "invalid apiVersion",
			manifest: "apiVersion: a/b/c\nkind: ConfigMap\n",
			wantErr:  "apiVersion: unexpected GroupVersion string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gvk, err := StaticGVK(tt.manifest)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, gvk)
		})
	}
}
