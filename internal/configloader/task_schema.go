package configloader

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	schemaVersionV2 = "2.0"
	v2ConceptsURL   = "https://github.com/openshift-hyperfleet/hyperfleet-adapter/" +
		"blob/main/docs/configuration.md#v2-concepts-changed"

	// removedFieldNestedDiscoveries names the removed v1 key, kept only for
	// the load-time rejection that tells authors what replaced it.
	removedFieldNestedDiscoveries = "nested_discoveries"
)

const (
	v1ShapeTransportReplacement = "v2 resources name a transport and use plain live resources"
	v1ShapeNestedReplacement    = "declare each object as its own resource with its own discovery, " +
		"and read it in CEL as resources.<name>"
)

func v1ResourceShapeError(index int, field, replacement string) error {
	return fmt.Errorf("resources[%d].%s: this is a v1 configuration shape; "+
		"%s; see %s", index, field, replacement, v2ConceptsURL)
}

func schemaVersionRequiredError() error {
	return fmt.Errorf("schema_version is required; use %q. v1 task configs are not supported; see %s",
		schemaVersionV2, v2ConceptsURL)
}

// taskSchemaDocument holds the only task config parts checkTaskSchemaYAML reads.
// Resource values stay as nodes, so inline manifests are not decoded here.
// yaml.v3 still applies merge keys when it fills the resource maps.
type taskSchemaDocument struct {
	Resources     []map[string]yaml.Node `yaml:"resources"`
	SchemaVersion yaml.Node              `yaml:"schema_version"`
}

// checkTaskSchemaYAML requires schema_version and inspects YAML value types before
// strict decoding, which would otherwise accept a numeric schema_version or treat a
// null transport as omitted.
func checkTaskSchemaYAML(data []byte) error {
	var document taskSchemaDocument
	if err := yaml.Unmarshal(data, &document); err != nil {
		// The strict decoder reports malformed YAML and duplicate keys.
		return nil
	}
	schemaNode := resolveYAMLAlias(&document.SchemaVersion)
	if schemaNode.Kind == 0 {
		return schemaVersionRequiredError()
	}
	if schemaNode.Kind != yaml.ScalarNode || schemaNode.ShortTag() != "!!str" {
		return fmt.Errorf("schema_version must be a string containing %q", schemaVersionV2)
	}
	if err := validateSupportedSchemaVersion(schemaNode.Value); err != nil {
		return err
	}
	for i, resource := range document.Resources {
		// An omitted transport leaves a zero node, which matches none of the checks below.
		transportNode := resource[FieldTransport]
		transport := resolveYAMLAlias(&transportNode)
		if transport.Kind == yaml.ScalarNode && transport.ShortTag() == "!!null" {
			return fmt.Errorf("resources[%d].%s must name a transport", i, FieldTransport)
		}
		if _, old := resource[removedFieldNestedDiscoveries]; old {
			return v1ResourceShapeError(i, removedFieldNestedDiscoveries, v1ShapeNestedReplacement)
		}
		if transport.Kind == yaml.MappingNode {
			return v1ResourceShapeError(i, FieldTransport, v1ShapeTransportReplacement)
		}
	}
	return nil
}

func resolveYAMLAlias(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

func validateSupportedSchemaVersion(value string) error {
	if value == schemaVersionV2 {
		return nil
	}
	if value == "" {
		return fmt.Errorf("schema_version must be %q", schemaVersionV2)
	}
	return fmt.Errorf("schema_version %q is unsupported; use %q", value, schemaVersionV2)
}

func validateTaskSchema(value string) error {
	if value != "" {
		if err := validateSupportedSchemaVersion(value); err != nil {
			return err
		}
	}
	return nil
}
