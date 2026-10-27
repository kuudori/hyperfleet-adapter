package configloader

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/utils"
	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------------------------
// Constants
// -----------------------------------------------------------------------------

// Environment variable for config file paths
const (
	EnvAdapterConfig  = "HYPERFLEET_ADAPTER_CONFIG" // Path to deployment config
	EnvTaskConfigPath = "HYPERFLEET_TASK_CONFIG"    // Path to task config
)

// ValidHTTPMethods defines allowed HTTP methods for API calls
var ValidHTTPMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// -----------------------------------------------------------------------------
// Load Options (Functional Options Pattern)
// -----------------------------------------------------------------------------

// LoadOption configures the loading behavior
type LoadOption func(*loadOptions)

type loadOptions struct {
	flags                  interface{} // *pflag.FlagSet
	ctx                    context.Context
	adapterConfigPath      string
	taskConfigPath         string
	adapterVersion         string
	skipSemanticValidation bool
}

// WithAdapterConfigPath sets the path to the deployment config file
func WithAdapterConfigPath(path string) LoadOption {
	return func(o *loadOptions) {
		o.adapterConfigPath = path
	}
}

// WithTaskConfigPath sets the path to the task config file
func WithTaskConfigPath(path string) LoadOption {
	return func(o *loadOptions) {
		o.taskConfigPath = path
	}
}

// WithFlags sets the CLI flags for Viper binding
func WithFlags(flags interface{}) LoadOption {
	return func(o *loadOptions) {
		o.flags = flags
	}
}

// WithAdapterVersion validates config against expected adapter version
func WithAdapterVersion(version string) LoadOption {
	return func(o *loadOptions) {
		o.adapterVersion = version
	}
}

// WithSkipSemanticValidation skips CEL, template, and K8s manifest validation
func WithSkipSemanticValidation() LoadOption {
	return func(o *loadOptions) {
		o.skipSemanticValidation = true
	}
}

// WithContext sets the context for logging during config loading
func WithContext(ctx context.Context) LoadOption {
	return func(o *loadOptions) {
		o.ctx = ctx
	}
}

type TaskLoadOption func(*taskLoadOptions)

type taskLoadOptions struct {
	fsys        fs.FS
	adapterName string
}

func WithAdapterName(name string) TaskLoadOption {
	return func(o *taskLoadOptions) {
		o.adapterName = name
	}
}

func WithFS(fsys fs.FS) TaskLoadOption {
	return func(o *taskLoadOptions) {
		o.fsys = fsys
	}
}

// -----------------------------------------------------------------------------
// Public API
// -----------------------------------------------------------------------------

// LoadConfig loads both deployment and task configurations, validates them,
// and returns a unified Config struct.
// Priority for deployment config values: CLI flags > Environment variables > Config file > Defaults
func LoadConfig(opts ...LoadOption) (*Config, error) {
	o := &loadOptions{}
	for _, opt := range opts {
		opt(o)
	}
	if o.ctx == nil {
		o.ctx = context.Background()
	}

	// 1. Load AdapterConfig with Viper (env/CLI overrides)
	// resolvedAdapterConfigPath is the actual path used (may come from standardConfigPaths fallback)
	resolvedAdapterConfigPath, adapterCfg, err := loadAdapterConfigWithViperGeneric(o.adapterConfigPath, o.flags)
	if err != nil {
		return nil, fmt.Errorf("failed to load adapter config: %w", err)
	}

	// Get base directory from the resolved config path for file references
	adapterBaseDir, errBaseDir := getBaseDir(resolvedAdapterConfigPath)
	if errBaseDir != nil {
		return nil, fmt.Errorf("failed to get base directory for adapter config: %w", errBaseDir)
	}

	// Validate AdapterConfig structure
	adapterValidator := NewAdapterConfigValidator(adapterCfg, adapterBaseDir)
	if err = adapterValidator.ValidateStructure(); err != nil {
		return nil, fmt.Errorf("adapter config validation failed: %w", err)
	}

	// Validate adapter version if specified
	if o.adapterVersion != "" {
		if err = ValidateAdapterVersion(o.ctx, adapterCfg, o.adapterVersion); err != nil {
			return nil, fmt.Errorf("adapter version validation failed: %w", err)
		}
	}

	// 2. Load AdapterTaskConfig from YAML (no env binding)
	taskConfigPath, taskData, err := readTaskConfigFile(o.taskConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load task config: %w", err)
	}

	// Get base directory from task config path
	taskBaseDir, err := getBaseDir(taskConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get base directory for task config: %w", err)
	}

	return buildConfig(o.ctx, taskData, adapterCfg, dirTaskFiles(taskBaseDir), o.skipSemanticValidation, false)
}

// LoadTaskConfig loads and validates a task config from data, without a
// deployment config. ctx must be non-nil. File references resolve against the
// WithFS file system, and any reference fails when none is supplied. It does not
// check that required env params exist in the process environment.
func LoadTaskConfig(ctx context.Context, data []byte, opts ...TaskLoadOption) (*Config, error) {
	o := &taskLoadOptions{}
	for _, opt := range opts {
		opt(o)
	}

	adapterCfg := &AdapterConfig{Adapter: AdapterInfo{Name: o.adapterName}}
	return buildConfig(ctx, data, adapterCfg, fsTaskFiles(o.fsys), false, true)
}

func buildConfig(
	ctx context.Context,
	data []byte,
	adapterCfg *AdapterConfig,
	files *taskFiles,
	skipSemantic, skipHostEnv bool,
) (*Config, error) {
	taskCfg, err := decodeTaskConfig(data)
	if err != nil {
		return nil, fmt.Errorf("failed to load task config: %w", err)
	}

	// Validate AdapterTaskConfig structure
	taskValidator := newTaskConfigValidator(taskCfg, files, skipHostEnv)
	if err := taskValidator.ValidateStructure(); err != nil {
		return nil, fmt.Errorf("task config validation failed: %w", err)
	}

	// Validate and load file references in task config
	if err := taskValidator.ValidateFileReferences(); err != nil {
		return nil, fmt.Errorf("task config file reference validation failed: %w", err)
	}

	if err := loadTaskConfigFileReferences(taskCfg, files); err != nil {
		return nil, fmt.Errorf("failed to load task config file references: %w", err)
	}

	// Merge into unified Config and validate cross-file routing
	config := Merge(adapterCfg, taskCfg)
	if config == nil {
		return nil, fmt.Errorf("failed to merge configurations")
	}
	if err := ValidateResourceTransports(config); err != nil {
		return nil, fmt.Errorf("resource transport validation failed: %w", err)
	}

	// Semantic validation for task config (optional)
	if !skipSemantic {
		if err := taskValidator.ValidateSemantic(); err != nil {
			return nil, fmt.Errorf("task config semantic validation failed: %w", err)
		}
		for _, w := range taskValidator.Warnings() {
			slog.WarnContext(ctx, w)
		}
	}

	return config, nil
}

// -----------------------------------------------------------------------------
// Internal Functions
// -----------------------------------------------------------------------------

type taskFiles struct {
	fsys    fs.FS
	resolve func(ref string) (name, shown string, err error)
}

func dirTaskFiles(baseDir string) *taskFiles {
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		baseAbs = baseDir
	}
	baseAbs = filepath.Clean(baseAbs)
	return &taskFiles{
		fsys: os.DirFS(baseAbs),
		resolve: func(ref string) (string, string, error) {
			fullPath, err := utils.ResolveSecurePath(baseAbs, ref)
			if err != nil {
				return "", "", err
			}
			rel, err := filepath.Rel(baseAbs, fullPath)
			if err != nil {
				return "", "", fmt.Errorf("path %q escapes base directory", ref)
			}
			return filepath.ToSlash(rel), fullPath, nil
		},
	}
}

// fsTaskFiles resolves refs against fsys. A nil fsys rejects every ref.
func fsTaskFiles(fsys fs.FS) *taskFiles {
	return &taskFiles{
		fsys: fsys,
		resolve: func(ref string) (string, string, error) {
			if fsys == nil {
				return "", "", fmt.Errorf("file reference %q needs a file system; none was supplied", ref)
			}
			if path.IsAbs(ref) {
				return "", "", fmt.Errorf("path %q must be relative to the task config file system", ref)
			}
			name := path.Clean(ref)
			if !fs.ValidPath(name) {
				return "", "", fmt.Errorf("path %q escapes base directory", ref)
			}
			return name, name, nil
		},
	}
}

// loadTaskConfigFileReferences loads content from file references into the task config
func loadTaskConfigFileReferences(config *AdapterTaskConfig, files *taskFiles) error {
	// Load manifest.ref in resources as raw strings to support Go template syntax.
	// Files are stored as raw strings so that structural Go templates ({{ if }}, {{ range }}, etc.)
	// are preserved and rendered at execution time before YAML parsing.
	for i := range config.Resources {
		resource := &config.Resources[i]
		ref := resource.GetManifestRef()
		if ref == "" {
			continue
		}

		content, err := loadRawFile(files, ref)
		if err != nil {
			return fmt.Errorf("%s[%d].%s.%s: %w", FieldResources, i, FieldManifest, FieldRef, err)
		}

		// Replace manifest with raw string content for template rendering at execution time
		resource.Manifest = content
	}

	// Load buildRef in post.payloads
	if config.Post != nil {
		for i := range config.Post.Payloads {
			payload := &config.Post.Payloads[i]
			if payload.BuildRef != "" {
				content, err := loadYAMLFile(files, payload.BuildRef)
				if err != nil {
					return fmt.Errorf("%s.%s[%d].%s: %w", FieldPost, FieldPayloads, i, FieldBuildRef, err)
				}
				payload.BuildRefContent = content
			}
		}
	}

	return nil
}

// loadRawFile reads a file and returns its content as a raw string.
// Used for manifest ref files to preserve Go template syntax for later rendering.
func loadRawFile(files *taskFiles, refPath string) (string, error) {
	name, shown, err := files.resolve(refPath)
	if err != nil {
		return "", err
	}

	data, err := fs.ReadFile(files.fsys, name)
	if err != nil {
		return "", fmt.Errorf("failed to read file %q: %w", shown, err)
	}

	return string(data), nil
}

// loadYAMLFile loads and parses a YAML file
func loadYAMLFile(files *taskFiles, refPath string) (map[string]any, error) {
	name, shown, err := files.resolve(refPath)
	if err != nil {
		return nil, err
	}

	data, err := fs.ReadFile(files.fsys, name)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %q: %w", shown, err)
	}

	var content map[string]any
	if err := yaml.Unmarshal(data, &content); err != nil {
		return nil, fmt.Errorf("failed to parse YAML file %q: %w", shown, err)
	}

	return content, nil
}
