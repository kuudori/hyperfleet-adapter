package configloader

import (
	"fmt"
	"slices"
	"strings"
	"text/template"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/utils"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

// canonicalGVKKey normalises a resource_plurals key the same way
// NormalizeRegistryName normalises a registry name, so a GVK key and a
// registry name canonicalise identically.
func canonicalGVKKey(key string) string { return NormalizeRegistryName(key) }

// ValidateResourcePluralKeyCollisions rejects keys that would resolve to the
// same GVK after case normalization.
func ValidateResourcePluralKeyCollisions(transports map[string]TransportDefinition) error {
	for _, name := range utils.SortedMapKeys(transports) {
		if err := validateCollisions(fmt.Sprintf("%s.%s.resource_plurals", FieldTransports, name),
			"GVK", transports[name].ResourcePlurals); err != nil {
			return err
		}
	}
	return nil
}

func validateResourcePlurals(path string, plurals map[string]string) error {
	if len(plurals) == 0 {
		return fmt.Errorf("%s.resource_plurals is required for remote transport", path)
	}
	for _, key := range utils.SortedMapKeys(plurals) {
		if key != strings.TrimSpace(key) {
			return fmt.Errorf("%s.resource_plurals key %q must not have surrounding whitespace", path, key)
		}
		// Grouped apiVersions contain a slash, so split on the final slash.
		last := strings.LastIndex(key, "/")
		if last < 0 {
			return fmt.Errorf("%s.resource_plurals key %q must be a static apiVersion/Kind", path, key)
		}
		apiVersion, kind := strings.TrimSpace(key[:last]), strings.TrimSpace(key[last+1:])
		if apiVersion == "" || kind == "" || apiVersion != key[:last] || kind != key[last+1:] ||
			strings.ContainsAny(kind, "/ \t\n") ||
			strings.Contains(key, "{{") {
			return fmt.Errorf("%s.resource_plurals key %q must be a static apiVersion/Kind", path, key)
		}
		if errs := validation.IsQualifiedName(kind); len(errs) > 0 {
			return fmt.Errorf("%s.resource_plurals key %q has invalid kind: %s", path, key, strings.Join(errs, ", "))
		}
		if _, err := schema.ParseGroupVersion(apiVersion); err != nil {
			return fmt.Errorf("%s.resource_plurals key %q has invalid apiVersion: %w", path, key, err)
		}
		if errs := validation.IsDNS1035Label(plurals[key]); len(errs) > 0 {
			return fmt.Errorf("%s.resource_plurals[%q] must be a Kubernetes DNS-1035 label: %s",
				path, key, strings.Join(errs, ", "))
		}
	}
	return nil
}

func validateTargetClusterSyntax(path, target string) error {
	if _, err := template.New("target_cluster").Funcs(utils.TemplateFuncs).Parse(target); err != nil {
		return fmt.Errorf("%s.target_cluster template: %w", path, err)
	}
	if !strings.Contains(target, "{{") {
		if errs := validation.IsDNS1123Label(target); len(errs) > 0 {
			return fmt.Errorf("%s.target_cluster must be a Kubernetes DNS-1123 label: %s",
				path, strings.Join(errs, ", "))
		}
	}
	return nil
}

// StaticGVK returns the manifest GVK without rendering the manifest; see
// manifest.StaticGVK. A map manifest is checked as the YAML text the executor
// renders from it, so both manifest forms follow the same rules.
func (r Resource) StaticGVK() (schema.GroupVersionKind, error) {
	text, err := manifest.ToYAMLString(r.Manifest)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	return manifest.StaticGVK(text)
}

// PluralForGVK looks up a static manifest kind on a remote route. Viper folds
// map keys to lower case, so loaded configs hit the direct lookup; direct Config
// construction may preserve their case and falls back to a scan.
func (d TransportDefinition) PluralForGVK(gvk schema.GroupVersionKind) (string, bool) {
	key := canonicalGVKKey(gvk.GroupVersion().String() + "/" + gvk.Kind)
	if plural, ok := d.ResourcePlurals[key]; ok {
		return plural, true
	}
	for configured, plural := range d.ResourcePlurals {
		if canonicalGVKKey(configured) == key {
			return plural, true
		}
	}
	return "", false
}

// ValidateResourceTransports checks resource routing in the merged config after manifest refs load.
// These are structural safety checks and also run when semantic checks are skipped.
func ValidateResourceTransports(config *Config) error {
	if err := validateTaskSchema(config.SchemaVersion, config.Resources); err != nil {
		return err
	}
	available := utils.SortedMapKeys(config.Transports)
	if !slices.ContainsFunc(available, func(name string) bool {
		return NormalizeRegistryName(name) == TransportClientKubernetes
	}) {
		available = append(available, TransportClientKubernetes)
		slices.Sort(available)
	}
	vars := targetClusterVariables(config)
	for i, resource := range config.Resources {
		path := fmt.Sprintf("resources[%d]", i)
		if resource.Manifest == nil {
			return fmt.Errorf("%s.manifest is required", path)
		}
		// Every transport discovers and deletes by this identity before rendering.
		gvk, err := resource.StaticGVK()
		if err != nil {
			return fmt.Errorf("%s.manifest: %w", path, err)
		}
		if resource.Transport != nil && resource.Transport.Legacy {
			maestro, err := validateLegacyTransport(config, resource, path, vars)
			if err != nil {
				return err
			}
			if maestro {
				continue
			}
		}
		name := resource.GetTransportName()
		if NormalizeRegistryName(name) == "" {
			return fmt.Errorf("%s.transport must name a transport; available: %v", path, available)
		}
		definition, configured := TransportDefinitionByName(config.Transports, name)
		if !configured && NormalizeRegistryName(name) != TransportClientKubernetes {
			return fmt.Errorf("%s.transport references unknown transport %q; available: %v",
				path, name, available)
		}
		if !configured || definition.Type != TransportTypeRemote {
			continue
		}
		if _, ok := definition.PluralForGVK(gvk); !ok {
			return fmt.Errorf("%s.manifest GVK %s has no resource_plurals mapping in transport %q",
				path, gvk.GroupVersion().String()+"/"+gvk.Kind, name)
		}
		// Mirrors the executor's runtime guard: delete desires are keyed by name.
		if resource.Lifecycle != nil && resource.Lifecycle.Delete != nil &&
			(resource.Discovery == nil || resource.Discovery.ByName == "") {
			return fmt.Errorf("%s.lifecycle.delete: %s", path, ErrMsgDesireSelectorDeleteUnsupported)
		}
		// The remote client never reads ApplyOptions, so recreate_on_change would be
		// silently ignored there while dry runs still report a recreate. Fail at load
		// instead. TODO(HYPERFLEET-1194): remove with recreate_on_change when
		// lifecycle.recreate.when replaces it.
		if resource.RecreateOnChange {
			return fmt.Errorf("%s.recreate_on_change is unsupported for remote transport", path)
		}
		for _, match := range templateVarRegex.FindAllStringSubmatch(definition.TargetCluster, -1) {
			if !isVariableDefinedIn(match[1], vars) {
				return fmt.Errorf("%s.transport %q target_cluster uses undefined variable %q", path, name, match[1])
			}
		}
	}
	return nil
}

// ValidateConfigRouting applies the loader's transport, store, and resource
// routing checks to a merged Config, for callers that did not use LoadConfig.
func ValidateConfigRouting(config *Config) error {
	adapter := &AdapterConfig{
		Transports: config.Transports,
		Stores:     config.Stores,
		Clients:    config.Clients,
	}
	if err := NewAdapterConfigValidator(adapter, "").validateTransportRegistry(); err != nil {
		return err
	}
	return ValidateResourceTransports(config)
}

// targetClusterVariables returns the template variables that exist when the
// executor renders target_cluster: the built-in variables plus the names in Params.
// Unlike GetDefinedVariables, it excludes post payloads and resource aliases,
// because they do not exist yet at that point.
func targetClusterVariables(config *Config) map[string]bool {
	return definedVariables(config.Params, config.Preconditions, nil, nil)
}
