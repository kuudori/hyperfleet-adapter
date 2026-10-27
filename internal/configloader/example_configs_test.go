package configloader

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShippedV2ConfigsLoad(t *testing.T) {
	root := filepath.Join("..", "..")
	cases := []struct {
		name        string
		adapterPath string
		taskPath    string
		resources   int
	}{
		{
			name: "template", adapterPath: "configs/adapter-config-template.yaml",
			taskPath: "configs/adapter-task-config-template.yaml", resources: 1,
		},
		{
			name: "kubernetes", adapterPath: "charts/examples/kubernetes/adapter-config.yaml",
			taskPath: "charts/examples/kubernetes/adapter-task-config.yaml", resources: 2,
		},
		{
			name: "two resources", adapterPath: "charts/examples/remote-two-resources/adapter-config.yaml",
			taskPath: "charts/examples/remote-two-resources/adapter-task-config.yaml", resources: 2,
		},
		{
			name: "CEL showcase", adapterPath: "test/testdata/dryrun/dryrun-kubernetes-adapter-config.yaml",
			taskPath: "test/testdata/dryrun/cel-showcase/dryrun-cel-showcase-task-config.yaml", resources: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, err := LoadConfig(
				WithAdapterConfigPath(filepath.Join(root, tc.adapterPath)),
				WithTaskConfigPath(filepath.Join(root, tc.taskPath)),
			)
			require.NoError(t, err)
			require.Equal(t, "2.0", config.SchemaVersion)
			require.Len(t, config.Resources, tc.resources)
			for _, resource := range config.Resources {
				require.NotNil(t, resource.Manifest, "resource %q manifest", resource.Name)
			}
		})
	}
}

func TestShippedTaskConfigLoadsWithoutDeploymentConfig(t *testing.T) {
	dir := filepath.Join("..", "..", "charts", "examples", "kubernetes")
	taskPath := filepath.Join(dir, "adapter-task-config.yaml")
	data, err := os.ReadFile(taskPath)
	require.NoError(t, err)

	fromTask, err := LoadTaskConfig(t.Context(), data,
		WithFS(os.DirFS(dir)), WithAdapterName("kubernetes-example"))
	require.NoError(t, err)

	fromLoad, err := LoadConfig(
		WithAdapterConfigPath(filepath.Join(dir, "adapter-config.yaml")),
		WithTaskConfigPath(taskPath),
	)
	require.NoError(t, err)

	require.Equal(t, "kubernetes-example", fromTask.Adapter.Name)
	require.Equal(t, ClientsConfig{}, fromTask.Clients)
	require.Equal(t, fromLoad.SchemaVersion, fromTask.SchemaVersion)
	require.Equal(t, fromLoad.Params, fromTask.Params)
	require.Equal(t, fromLoad.Preconditions, fromTask.Preconditions)
	require.Equal(t, fromLoad.Resources, fromTask.Resources)
	require.Equal(t, fromLoad.Post, fromTask.Post)

	job, err := os.ReadFile(filepath.Join(dir, "job.yaml"))
	require.NoError(t, err)
	resource := fromTask.GetResourceByName("helloWorldJob")
	require.NotNil(t, resource)
	require.Equal(t, string(job), resource.Manifest)
}
