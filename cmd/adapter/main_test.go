package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/dryrun"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/executor"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportregistry"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/constants"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/health"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDryRunLogOptionsDefaults(t *testing.T) {
	level, format := buildDryRunLogOptions()

	require.Equal(t, "warn", level)
	require.Equal(t, "text", format)
}

func TestLoadConfigRejectsTaskSchemaBeforeRuntimeSetup(t *testing.T) {
	dir := t.TempDir()
	adapterFile := filepath.Join(dir, "adapter.yaml")
	taskFile := filepath.Join(dir, "task.yaml")
	require.NoError(t, os.WriteFile(adapterFile, []byte("adapter: {name: test}\n"), 0o600))
	require.NoError(t, os.WriteFile(taskFile, []byte("schema_version: 2.0\n"), 0o600))
	previousConfig, previousTask := configPath, taskConfigPath
	configPath, taskConfigPath = adapterFile, taskFile
	t.Cleanup(func() { configPath, taskConfigPath = previousConfig, previousTask })

	_, err := loadConfig(t.Context(), nil)
	require.ErrorContains(t, err, "schema_version must be a string")
}

func TestMetricsConfigForAdapter(t *testing.T) {
	tests := []struct {
		component string
		version   string
		commit    string
		want      health.MetricsConfig
		wantError bool
	}{
		{
			component: "test-adapter",
			version:   "v1.2.3",
			commit:    "abc123",
			want: health.MetricsConfig{
				Component: "test-adapter", Version: "v1.2.3", Commit: "abc123", AdapterName: "test",
			},
		},
		{
			component: "validation-adapter ",
			version:   "v1.2.3",
			commit:    "abc123",
			want: health.MetricsConfig{
				Component: "validation-adapter ", Version: "v1.2.3", Commit: "abc123", AdapterName: "validation",
			},
		},
		{component: "adapter-", wantError: true},
		{component: "hyperfleet-adapter-", wantError: true},
		{component: "adapter- ", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.component, func(t *testing.T) {
			got, err := metricsConfigForAdapter(tt.component, tt.version, tt.commit)
			if tt.wantError {
				require.ErrorContains(t, err, "adapter.name")
				require.ErrorContains(t, err, tt.component)
				require.ErrorContains(t, err, "produces an empty metrics identity")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDryRunLogOptionsHonorsLevelOverride(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")

	level, _ := buildDryRunLogOptions()

	require.Equal(t, "debug", level)
}

func TestLogOptionsFlagOverridesEnv(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")
	prev := logLevel
	logLevel = "error"
	t.Cleanup(func() { logLevel = prev })

	level, _, _ := buildLogOptions(nil)

	require.Equal(t, "error", level, "CLI flag must take precedence over LOG_LEVEL")
}

func TestLogOptionsBootstrapDefaults(t *testing.T) {
	prevLevel, prevFormat, prevOutput := logLevel, logFormat, logOutput
	logLevel, logFormat, logOutput = "", "", ""
	t.Cleanup(func() {
		logLevel, logFormat, logOutput = prevLevel, prevFormat, prevOutput
	})

	level, format, output := buildLogOptions(nil)

	require.Empty(t, level, "bootstrap level is passed to hfl.ParseLevel")
	require.Empty(t, format, "bootstrap format is passed to hfl.ParseFormat")
	require.Empty(t, output, "bootstrap output is passed to hfl.ParseOutput")
}

func TestBuildExecutor_DryRunNamedRemoteTransport(t *testing.T) {
	config := &configloader.Config{
		SchemaVersion: "2.0",
		Adapter:       configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"remote-primary": {
				Type: configloader.TransportTypeRemote, Store: "desired-memory", TargetCluster: "cluster-1",
				ResourcePlurals: map[string]string{"v1/ConfigMap": "configmaps"},
			},
		},
		Resources: []configloader.Resource{{
			Name:      "test-resource",
			Transport: configloader.NamedTransport("remote-primary"),
			Manifest: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "test-config", "namespace": "default",
					"annotations": map[string]interface{}{constants.AnnotationGeneration: "1"},
				},
			},
		}},
	}
	recorder := dryrun.NewDryrunTransportClient()
	runtime, err := transportregistry.BuildRecording(config, recorder)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close()) })
	apiClient, err := dryrun.NewDryrunAPIClient(nil)
	require.NoError(t, err)

	exec, err := buildExecutor(config, apiClient, runtime.Registry, nil)
	require.NoError(t, err)
	result := exec.Execute(context.Background(), map[string]interface{}{"id": "cluster-1", "kind": "Cluster"})

	require.Equal(t, executor.StatusSuccess, result.Status)
	require.Len(t, recorder.Records, 1)
	require.Equal(t, "apply", recorder.Records[0].Operation)
	require.Equal(t, "test-resource", recorder.Records[0].Resource)
	require.Equal(t, "cluster-1", recorder.Records[0].TargetCluster)
	require.Equal(t, "configmaps", recorder.Records[0].TargetResource)
}

func TestBuildExecutor_ReadsProcessEnvAndHostFiles(t *testing.T) {
	t.Setenv("BUILD_EXECUTOR_REGION", "eu-west-9")
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("host-token\n"), 0o600))
	config := &configloader.Config{
		SchemaVersion: "2.0",
		Adapter:       configloader.AdapterInfo{Name: "test-adapter"},
		Params: []configloader.Parameter{
			{Name: "region", Source: configloader.StringSource("env.BUILD_EXECUTOR_REGION"), Required: true},
			{
				Name:     "token",
				Source:   configloader.FileSource(&configloader.FileSourceConfig{Path: tokenPath}),
				Required: true,
			},
		},
	}

	_, _, result := runRecordedEvents(t, config, map[string]interface{}{"id": "cluster-1", "kind": "Cluster"})

	assert.Equal(t, "eu-west-9", result.Params["region"])
	assert.Equal(t, "host-token", result.Params["token"])
}

// configMapManifest renders a ConfigMap with the generation annotation remote
// routes require.
func configMapManifest(namespace, name string, labels, data map[string]interface{}) map[string]interface{} {
	metadata := map[string]interface{}{
		"name": name, "namespace": namespace,
		"annotations": map[string]interface{}{constants.AnnotationGeneration: "1"},
	}
	if labels != nil {
		metadata["labels"] = labels
	}
	return map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata, "data": data}
}

// remoteTransport is a named remote route to targetCluster for ConfigMaps.
func remoteTransport(targetCluster string) configloader.TransportDefinition {
	return configloader.TransportDefinition{
		Type: configloader.TransportTypeRemote, Store: "desired-memory", TargetCluster: targetCluster,
		ResourcePlurals: map[string]string{"v1/ConfigMap": "configmaps"},
	}
}

// runRecordedEvents executes each event through the same executor and
// recording transport wiring runDryRun uses, and returns the recorder and the
// last result.
func runRecordedEvents(
	t *testing.T,
	config *configloader.Config,
	events ...map[string]interface{},
) (*dryrun.DryrunTransportClient, *dryrun.DryrunAPIClient, *executor.ExecutionResult) {
	t.Helper()
	recorder := dryrun.NewDryrunTransportClient()
	runtime, err := transportregistry.BuildRecording(config, recorder)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close()) })
	apiClient, err := dryrun.NewDryrunAPIClient(nil)
	require.NoError(t, err)
	exec, err := buildExecutor(config, apiClient, runtime.Registry, nil)
	require.NoError(t, err)

	var result *executor.ExecutionResult
	for _, event := range events {
		result = exec.Execute(context.Background(), event)
		require.Equal(t, executor.StatusSuccess, result.Status, "errors=%v", result.Errors)
	}
	return recorder, apiClient, result
}

// TestDryRun_RecordsResourceNames verifies that every transport call the
// executor makes, through pre-discovery, apply, post-apply discovery and delete,
// records the task config resource it was made for. Each resource renders into
// its own namespace, so a record's namespace tells which resource it belongs to.
func TestDryRun_RecordsResourceNames(t *testing.T) {
	deleteWhenDeleting := &configloader.ResourceLifecycle{Delete: &configloader.LifecycleDelete{
		When: &configloader.LifecycleWhen{Expression: "event.?deleting.orValue(false)"},
	}}
	config := &configloader.Config{
		SchemaVersion: "2.0",
		Adapter:       configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{"remote-primary": remoteTransport("cluster-1")},
		Resources: []configloader.Resource{
			{
				Name:      "remoteByName",
				Transport: configloader.NamedTransport("remote-primary"),
				Manifest:  configMapManifest("ns-a", "cm-a", nil, nil),
				Discovery: &configloader.DiscoveryConfig{Namespace: "ns-a", ByName: "cm-a"},
				Lifecycle: deleteWhenDeleting,
			},
			{
				Name:      "remoteBySelector",
				Transport: configloader.NamedTransport("remote-primary"),
				Manifest:  configMapManifest("ns-b", "cm-b", map[string]interface{}{"app": "b"}, nil),
				Discovery: &configloader.DiscoveryConfig{
					Namespace:   "ns-b",
					BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{"app": "b"}},
				},
			},
			{
				Name:     "local",
				Manifest: configMapManifest("ns-c", "cm-c", map[string]interface{}{"app": "c"}, nil),
				Discovery: &configloader.DiscoveryConfig{
					Namespace:   "ns-c",
					BySelectors: &configloader.SelectorConfig{LabelSelector: map[string]string{"app": "c"}},
				},
				Lifecycle: deleteWhenDeleting,
			},
		},
	}

	recorder, _, _ := runRecordedEvents(t, config,
		map[string]interface{}{"id": "cluster-1", "kind": "Cluster"},
		map[string]interface{}{"id": "cluster-1", "kind": "Cluster", "deleting": true},
	)

	resourceByNamespace := map[string]string{"ns-a": "remoteByName", "ns-b": "remoteBySelector", "ns-c": "local"}
	operations := make(map[string]map[string]bool)
	for _, record := range recorder.Records {
		want, ok := resourceByNamespace[record.Namespace]
		require.True(t, ok, "unexpected record namespace %q", record.Namespace)
		assert.Equal(t, want, record.Resource, "%s %s/%s", record.Operation, record.Namespace, record.Name)
		if operations[record.Resource] == nil {
			operations[record.Resource] = make(map[string]bool)
		}
		operations[record.Resource][record.Operation] = true
	}
	// Each resource went through the calls it should have, so none of them is
	// missing from the check above.
	assert.Equal(t, map[string]bool{"get": true, "apply": true, "delete": true}, operations["remoteByName"])
	assert.Equal(t, map[string]bool{"discover": true, "apply": true}, operations["remoteBySelector"])
	assert.Equal(t, map[string]bool{"discover": true, "apply": true, "delete": true}, operations["local"])
}

// TestDryRun_TraceShowsEachResourcesOwnManifest verifies that resources which
// render the same object each show their own route and rendered manifest,
// whether they share a route or not.
func TestDryRun_TraceShowsEachResourcesOwnManifest(t *testing.T) {
	sharedObject := func(transport, step string) configloader.Resource {
		resource := configloader.Resource{
			Name:      step,
			Manifest:  configMapManifest("default", "shared", nil, map[string]interface{}{"step": step}),
			Discovery: &configloader.DiscoveryConfig{Namespace: "default", ByName: "shared"},
		}
		if transport != "" {
			resource.Transport = configloader.NamedTransport(transport)
		}
		return resource
	}
	config := &configloader.Config{
		SchemaVersion: "2.0",
		Adapter:       configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"remote-a": remoteTransport("cluster-a"),
			"remote-b": remoteTransport("cluster-b"),
		},
		Resources: []configloader.Resource{
			sharedObject("remote-a", "firstOnA"),
			sharedObject("remote-b", "onB"),
			sharedObject("remote-a", "secondOnA"),
			sharedObject("", "firstLocal"),
			sharedObject("", "secondLocal"),
		},
	}

	recorder, apiClient, result := runRecordedEvents(t, config,
		map[string]interface{}{"id": "cluster-1", "kind": "Cluster"})
	trace := &dryrun.ExecutionTrace{Result: result, APIClient: apiClient, Transport: recorder, Verbose: true}
	output := trace.FormatText()

	targets := map[string]string{
		"firstOnA": "cluster-a", "onB": "cluster-b", "secondOnA": "cluster-a", "firstLocal": "", "secondLocal": "",
	}
	for i, resource := range config.Resources {
		_, section, found := strings.Cut(output, fmt.Sprintf("[%d/5] %s", i+1, resource.Name))
		require.True(t, found, "trace has no section for %s", resource.Name)
		section, _, _ = strings.Cut(section, fmt.Sprintf("[%d/5]", i+2))
		section, _, _ = strings.Cut(section, "Phase 3.5")

		for step := range targets {
			if step == resource.Name {
				assert.Contains(t, section, fmt.Sprintf(`"step": %q`, step))
			} else {
				assert.NotContains(t, section, fmt.Sprintf(`"step": %q`, step), "%s shows %s's manifest", resource.Name, step)
			}
		}
		if target := targets[resource.Name]; target != "" {
			assert.Contains(t, section, "Target: cluster "+target+",")
		} else {
			assert.NotContains(t, section, "Target:")
		}
	}
}
