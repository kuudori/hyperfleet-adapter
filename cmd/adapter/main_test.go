package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDryRunLogOptionsDefaults(t *testing.T) {
	level, format := buildDryRunLogOptions()

	require.Equal(t, "warn", level)
	require.Equal(t, "text", format)
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
