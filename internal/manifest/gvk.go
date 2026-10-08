package manifest

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// StaticGVK reads a manifest template's apiVersion and kind without rendering
// it, as discovery, deletion, and remote routing need them before rendering.
//
// It reads the "apiVersion:" and "kind:" lines at the manifest's top level,
// the indentation of its first field. Each must appear exactly once, with a
// literal value. Appearing once rules out a kind set in both branches of an
// {{ if }}/{{ else }}, and a second YAML document in the manifest. The
// executor rejects a rendered manifest whose GVK differs.
func StaticGVK(manifest string) (schema.GroupVersionKind, error) {
	var apiVersion, kind string
	indent := -1
	lineNo := 0
	for line := range strings.SplitSeq(manifest, "\n") {
		lineNo++
		content := strings.TrimSpace(line)
		if content == "" || strings.HasPrefix(content, "#") || strings.HasPrefix(content, "---") ||
			strings.HasPrefix(content, "{{") {
			continue
		}
		lineIndent := len(line) - len(strings.TrimLeft(line, " "))
		if indent < 0 {
			indent = lineIndent
		}
		key, value, _ := strings.Cut(content, ":")
		if lineIndent != indent {
			continue
		}
		var target *string
		switch key {
		case "apiVersion":
			target = &apiVersion
		case "kind":
			target = &kind
		default:
			continue
		}
		if *target != "" {
			return schema.GroupVersionKind{}, fmt.Errorf(
				"line %d: %s is set more than once (in several template branches or YAML documents)", lineNo, key)
		}
		value, _, _ = strings.Cut(value, " #")
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value == "" || strings.Contains(value, "{{") {
			return schema.GroupVersionKind{}, fmt.Errorf("line %d: %s must be a literal value", lineNo, key)
		}
		*target = value
	}

	if apiVersion == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("apiVersion must be set as a top-level field")
	}
	if kind == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("kind must be set as a top-level field")
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("apiVersion: %w", err)
	}
	return gv.WithKind(kind), nil
}
