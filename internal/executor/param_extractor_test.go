package executor

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractConfigParams_OptionalParamFailureIsLogged(t *testing.T) {
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter", Version: "1.0.0"},
		Params: []configloader.Parameter{
			// The expression parses, but evaluating an undeclared function fails.
			{Name: "dropped", Source: configloader.ExpressionSource(`noSuchFunction("x")`)},
			{Name: "defaulted", Source: configloader.ExpressionSource(`noSuchFunction("y")`), Default: "fallback"},
		},
	}
	execCtx := NewExecutionContext(t.Context(), map[string]interface{}{"id": "abc"}, config)
	configMap, err := configToMap(config)
	require.NoError(t, err)

	err = extractConfigParams(context.Background(), config, execCtx, configMap, nil, nil)

	require.NoError(t, err, "a failing optional param must not fail extraction")
	assert.NotContains(t, execCtx.Params, "dropped")
	assert.Equal(t, "fallback", execCtx.Params["defaulted"])
	logs := buf.String()
	assert.Contains(t, logs, `msg="optional parameter not extracted" param=dropped default_applied=false`)
	assert.Contains(t, logs, `msg="optional parameter not extracted" param=defaulted default_applied=true`)
	assert.Contains(t, logs, "noSuchFunction")
}

func TestConvertParamType(t *testing.T) {
	tests := []struct {
		name       string
		want       interface{}
		value      interface{}
		targetType string
		wantErr    bool
	}{
		// String conversions
		{name: "string to string", value: "hello", targetType: "string", want: "hello"},
		{name: "int to string", value: 42, targetType: "string", want: "42"},
		{name: "float to string", value: 3.14, targetType: "string", want: "3.14"},
		{name: "bool to string", value: true, targetType: "string", want: "true"},

		// Int conversions
		{name: "int to int", value: 42, targetType: "int", want: int64(42)},
		{name: "int64 to int64", value: int64(42), targetType: "int64", want: int64(42)},
		{name: "string to int", value: "42", targetType: "int", want: int64(42)},
		{name: "float to int", value: 3.9, targetType: "int", want: int64(3)},
		{name: "string float to int", value: "3.9", targetType: "int", want: int64(3)},
		{name: "bool true to int", value: true, targetType: "int", want: int64(1)},
		{name: "bool false to int", value: false, targetType: "int", want: int64(0)},
		{name: "invalid string to int", value: "abc", targetType: "int", wantErr: true},

		// Float conversions
		{name: "float to float", value: 3.14, targetType: "float", want: 3.14},
		{name: "float64 to float64", value: float64(3.14), targetType: "float64", want: 3.14},
		{name: "int to float", value: 42, targetType: "float", want: float64(42)},
		{name: "string to float", value: "3.14", targetType: "float", want: 3.14},
		{name: "bool true to float", value: true, targetType: "float", want: 1.0},
		{name: "invalid string to float", value: "abc", targetType: "float", wantErr: true},

		// Bool conversions
		{name: "bool to bool", value: true, targetType: "bool", want: true},
		{name: "string true to bool", value: "true", targetType: "bool", want: true},
		{name: "string false to bool", value: "false", targetType: "bool", want: false},
		{name: "string yes to bool", value: "yes", targetType: "bool", want: true},
		{name: "string no to bool", value: "no", targetType: "bool", want: false},
		{name: "string 1 to bool", value: "1", targetType: "bool", want: true},
		{name: "string 0 to bool", value: "0", targetType: "bool", want: false},
		{name: "int 1 to bool", value: 1, targetType: "bool", want: true},
		{name: "int 0 to bool", value: 0, targetType: "bool", want: false},
		{name: "float non-zero to bool", value: 3.14, targetType: "bool", want: true},
		{name: "invalid string to bool", value: "maybe", targetType: "bool", wantErr: true},

		// Unsupported type
		{name: "unsupported type", value: "test", targetType: "unknown", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertParamType(tt.value, tt.targetType)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestConvertToString(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		want  string
	}{
		{name: "string", value: "hello", want: "hello"},
		{name: "int", value: 42, want: "42"},
		{name: "int64", value: int64(123), want: "123"},
		{name: "float64", value: 3.14159, want: "3.14159"},
		{name: "bool true", value: true, want: "true"},
		{name: "bool false", value: false, want: "false"},
		{name: "uint", value: uint(100), want: "100"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertToString(tt.value)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConvertToInt64(t *testing.T) {
	tests := []struct {
		value   interface{}
		name    string
		want    int64
		wantErr bool
	}{
		{name: "int", value: 42, want: 42},
		{name: "int64", value: int64(123), want: 123},
		{name: "float64", value: 3.9, want: 3},
		{name: "string int", value: "42", want: 42},
		{name: "string float", value: "3.9", want: 3},
		{name: "uint", value: uint(100), want: 100},
		{name: "bool true", value: true, want: 1},
		{name: "bool false", value: false, want: 0},
		{name: "invalid string", value: "abc", wantErr: true},
		{name: "invalid type", value: []string{"a"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertToInt64(tt.value)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestConvertToFloat64(t *testing.T) {
	tests := []struct {
		value   interface{}
		name    string
		want    float64
		wantErr bool
	}{
		{name: "float64", value: 3.14, want: 3.14},
		{name: "float32", value: float32(2.5), want: 2.5},
		{name: "int", value: 42, want: 42.0},
		{name: "int64", value: int64(123), want: 123.0},
		{name: "string", value: "3.14", want: 3.14},
		{name: "uint", value: uint(100), want: 100.0},
		{name: "bool true", value: true, want: 1.0},
		{name: "bool false", value: false, want: 0.0},
		{name: "invalid string", value: "abc", wantErr: true},
		{name: "invalid type", value: []string{"a"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertToFloat64(tt.value)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.InDelta(t, tt.want, got, 0.001)
			}
		})
	}
}

func TestConvertToBool(t *testing.T) {
	tests := []struct {
		value   interface{}
		name    string
		wantErr bool
		want    bool
	}{
		{name: "bool true", value: true, want: true},
		{name: "bool false", value: false, want: false},
		{name: "string true", value: "true", want: true},
		{name: "string false", value: "false", want: false},
		{name: "string True", value: "True", want: true},
		{name: "string FALSE", value: "FALSE", want: false},
		{name: "string yes", value: "yes", want: true},
		{name: "string no", value: "no", want: false},
		{name: "string y", value: "y", want: true},
		{name: "string n", value: "n", want: false},
		{name: "string on", value: "on", want: true},
		{name: "string off", value: "off", want: false},
		{name: "string 1", value: "1", want: true},
		{name: "string 0", value: "0", want: false},
		{name: "string empty", value: "", want: false},
		{name: "int 1", value: 1, want: true},
		{name: "int 0", value: 0, want: false},
		{name: "int non-zero", value: 42, want: true},
		{name: "float non-zero", value: 3.14, want: true},
		{name: "float zero", value: 0.0, want: false},
		{name: "invalid string", value: "maybe", wantErr: true},
		{name: "invalid type", value: []string{"a"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertToBool(tt.value)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
