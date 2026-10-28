package executor

import (
	"maps"
	"os"
	"strings"
)

// EnvFunc returns the environment variables a run can read as env.* and through env. params.
type EnvFunc func() map[string]string

// ProcessEnv returns an EnvFunc that reads every process environment variable on each call.
func ProcessEnv() EnvFunc {
	return func() map[string]string {
		environ := os.Environ()
		env := make(map[string]string, len(environ))
		for _, kv := range environ {
			if name, value, ok := strings.Cut(kv, "="); ok && name != "" {
				env[name] = value
			}
		}
		return env
	}
}

// EnvFromMap returns an EnvFunc that returns a copy of vars as they were when EnvFromMap is called.
func EnvFromMap(vars map[string]string) EnvFunc {
	snapshot := maps.Clone(vars)
	return func() map[string]string {
		return maps.Clone(snapshot)
	}
}

// EnvAllowlist returns an EnvFunc that reads only the named process environment variables on each call.
func EnvAllowlist(names ...string) EnvFunc {
	return func() map[string]string {
		env := make(map[string]string, len(names))
		for _, name := range names {
			if value, ok := os.LookupEnv(name); ok {
				env[name] = value
			}
		}
		return env
	}
}

func envSnapshot(env EnvFunc) map[string]any {
	if env == nil {
		return make(map[string]any)
	}
	vars := env()
	snapshot := make(map[string]any, len(vars))
	for name, value := range vars {
		snapshot[name] = value
	}
	return snapshot
}
