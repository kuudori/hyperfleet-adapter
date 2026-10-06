package configloader

import (
	"fmt"
	"slices"
)

// -----------------------------------------------------------------------------
// Built-in Variables
// -----------------------------------------------------------------------------

// builtinVariables lists the built-in variables that the runtime adds to every
// template and CEL context.
var builtinVariables = []string{
	FieldAdapter, FieldConfig, FieldEnv, FieldEvent,
}

// BuiltinVariables returns the list of built-in variables always available in templates/CEL
func BuiltinVariables() []string {
	return builtinVariables
}

// ReservedVariableNames returns every name that the runtime adds to CEL: the
// built-in variables plus resources and resource_states. Params, API-call
// preconditions, captures and post payloads cannot use these names.
func ReservedVariableNames() []string {
	return slices.Concat(builtinVariables, []string{FieldResources, FieldResourceStates})
}

// builtinVariableSet returns a new set that contains the built-in variables.
func builtinVariableSet() map[string]bool {
	set := make(map[string]bool, len(builtinVariables))
	for _, name := range builtinVariables {
		set[name] = true
	}
	return set
}

// -----------------------------------------------------------------------------
// Config Accessors (Unified Configuration)
// -----------------------------------------------------------------------------

// definedVariables returns the built-in variables, every author-defined name and
// the resource aliases (resources.<name>). The author-defined names are the params,
// API-call precondition responses and their captures, and post payloads. The
// executor stores them in Params, so each one is a top-level template and CEL variable.
func definedVariables(
	params []Parameter, preconditions []Precondition, post *PostConfig, resources []Resource,
) map[string]bool {
	vars := builtinVariableSet()

	for _, p := range params {
		if p.Name != "" {
			vars[p.Name] = true
		}
	}

	for _, precond := range preconditions {
		// Only API-call preconditions store values: the response under the
		// precondition name, plus its captures.
		if precond.APICall == nil {
			continue
		}
		if precond.Name != "" {
			vars[precond.Name] = true
		}
		for _, capture := range precond.Capture {
			if capture.Name != "" {
				vars[capture.Name] = true
			}
		}
	}

	if post != nil {
		for _, p := range post.Payloads {
			if p.Name != "" {
				vars[p.Name] = true
			}
		}
	}

	for _, r := range resources {
		if r.Name != "" {
			vars[FieldResources+"."+r.Name] = true
		}
	}

	return vars
}

// GetParamByName returns a parameter by name from params, or nil if not found
func (c *Config) GetParamByName(name string) *Parameter {
	if c == nil {
		return nil
	}
	for i := range c.Params {
		if c.Params[i].Name == name {
			return &c.Params[i]
		}
	}
	return nil
}

// GetRequiredParams returns all parameters marked as required from params
func (c *Config) GetRequiredParams() []Parameter {
	if c == nil {
		return nil
	}
	var required []Parameter
	for _, p := range c.Params {
		if p.Required {
			required = append(required, p)
		}
	}
	return required
}

// GetResourceByName returns a resource by name, or nil if not found
func (c *Config) GetResourceByName(name string) *Resource {
	if c == nil {
		return nil
	}
	for i := range c.Resources {
		if c.Resources[i].Name == name {
			return &c.Resources[i]
		}
	}
	return nil
}

// GetPreconditionByName returns a precondition by name, or nil if not found
func (c *Config) GetPreconditionByName(name string) *Precondition {
	if c == nil {
		return nil
	}
	for i := range c.Preconditions {
		if c.Preconditions[i].Name == name {
			return &c.Preconditions[i]
		}
	}
	return nil
}

// GetPostActionByName returns a post action by name, or nil if not found
func (c *Config) GetPostActionByName(name string) *PostAction {
	if c == nil || c.Post == nil {
		return nil
	}
	for i := range c.Post.PostActions {
		if c.Post.PostActions[i].Name == name {
			return &c.Post.PostActions[i]
		}
	}
	return nil
}

// ParamNames returns all parameter names in order
func (c *Config) ParamNames() []string {
	if c == nil {
		return nil
	}
	names := make([]string, len(c.Params))
	for i, p := range c.Params {
		names[i] = p.Name
	}
	return names
}

// ResourceNames returns all resource names in order
func (c *Config) ResourceNames() []string {
	if c == nil {
		return nil
	}
	names := make([]string, len(c.Resources))
	for i, r := range c.Resources {
		names[i] = r.Name
	}
	return names
}

// -----------------------------------------------------------------------------
// Resource Accessors
// -----------------------------------------------------------------------------

// GetTransportName returns the named transport, defaulting to local Kubernetes.
// An explicitly blank name remains blank so validation can reject it.
func (r *Resource) GetTransportName() string {
	if r == nil || r.Transport == nil {
		return TransportClientKubernetes
	}
	return r.Transport.Name
}

// IsMaestroTransport returns true if this resource uses the maestro transport client
// TODO(HYPERFLEET-1504): remove with the Maestro transport.
func (r *Resource) IsMaestroTransport() bool {
	return r.GetTransportName() == TransportClientMaestro
}

// HasManifestRef returns true if the manifest uses a ref (single file reference)
func (r *Resource) HasManifestRef() bool {
	if r == nil || r.Manifest == nil {
		return false
	}
	manifest := normalizeToStringKeyMap(r.Manifest)
	if manifest == nil {
		return false
	}
	_, hasRef := manifest["ref"]
	return hasRef
}

// GetManifestRef returns the ref path if set, empty string otherwise
func (r *Resource) GetManifestRef() string {
	if r == nil || r.Manifest == nil {
		return ""
	}
	manifest := normalizeToStringKeyMap(r.Manifest)
	if manifest == nil {
		return ""
	}

	if ref, ok := manifest["ref"].(string); ok {
		return ref
	}

	return ""
}

// UnmarshalManifest attempts to unmarshal the manifest as a map
// Returns nil, nil if resource is nil or manifest is nil
// Returns error if manifest cannot be converted to map
func (r *Resource) UnmarshalManifest() (map[string]interface{}, error) {
	if r == nil || r.Manifest == nil {
		return nil, nil
	}

	// Try to normalize the manifest to map[string]interface{}
	if m := normalizeToStringKeyMap(r.Manifest); m != nil {
		return m, nil
	}

	// If manifest cannot be normalized, return an error with type info
	return nil, fmt.Errorf("manifest is not a map, got %T", r.Manifest)
}

// -----------------------------------------------------------------------------
// Helper Functions
// -----------------------------------------------------------------------------

// normalizeToStringKeyMap converts various map types to map[string]interface{}.
// This handles both map[string]interface{} (from yaml.v3) and map[interface{}]interface{}
// (from yaml.v2 or other sources) for robustness.
// Returns nil if the input is not a map type.
func normalizeToStringKeyMap(v interface{}) map[string]interface{} {
	switch m := v.(type) {
	case map[string]interface{}:
		return m
	case map[interface{}]interface{}:
		result := make(map[string]interface{}, len(m))
		for k, val := range m {
			if keyStr, ok := k.(string); ok {
				result[keyStr] = val
			} else {
				// Convert non-string keys to string representation
				result[fmt.Sprintf("%v", k)] = val
			}
		}
		return result
	default:
		return nil
	}
}
