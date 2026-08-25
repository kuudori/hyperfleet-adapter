package maestroclient

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openshift-online/ocm-sdk-go/logging"
)

// Ensure ocmLoggerAdapter implements the OCM SDK logging.Logger interface
var _ logging.Logger = &ocmLoggerAdapter{}

// ocmLoggerAdapter bridges slog.Default() to the OCM SDK logging.Logger interface.
// This allows using the standard slog logger with Maestro's grpcsource client.
type ocmLoggerAdapter struct{}

// newOCMLoggerAdapter creates a new OCM SDK compatible logger adapter
func newOCMLoggerAdapter() *ocmLoggerAdapter {
	return &ocmLoggerAdapter{}
}

// DebugEnabled returns true if the debug level is enabled.
func (a *ocmLoggerAdapter) DebugEnabled() bool {
	return slog.Default().Enabled(context.Background(), slog.LevelDebug)
}

// InfoEnabled returns true if the information level is enabled.
func (a *ocmLoggerAdapter) InfoEnabled() bool {
	return slog.Default().Enabled(context.Background(), slog.LevelInfo)
}

// WarnEnabled returns true if the warning level is enabled.
func (a *ocmLoggerAdapter) WarnEnabled() bool {
	return slog.Default().Enabled(context.Background(), slog.LevelWarn)
}

// ErrorEnabled returns true if the error level is enabled.
func (a *ocmLoggerAdapter) ErrorEnabled() bool {
	return slog.Default().Enabled(context.Background(), slog.LevelError)
}

// logf formats and logs a message at the given level, defaulting to a
// background context when the OCM SDK passes a nil one.
func (a *ocmLoggerAdapter) logf(ctx context.Context, level slog.Level, format string, args ...interface{}) {
	if ctx == nil {
		ctx = context.Background()
	}
	slog.Log(ctx, level, fmt.Sprintf(format, args...))
}

// Debug logs at debug level with formatting.
func (a *ocmLoggerAdapter) Debug(ctx context.Context, format string, args ...interface{}) {
	a.logf(ctx, slog.LevelDebug, format, args...)
}

// Info logs at info level with formatting.
func (a *ocmLoggerAdapter) Info(ctx context.Context, format string, args ...interface{}) {
	a.logf(ctx, slog.LevelInfo, format, args...)
}

// Warn logs at warn level with formatting.
func (a *ocmLoggerAdapter) Warn(ctx context.Context, format string, args ...interface{}) {
	a.logf(ctx, slog.LevelWarn, format, args...)
}

// Error logs at error level with formatting.
func (a *ocmLoggerAdapter) Error(ctx context.Context, format string, args ...interface{}) {
	a.logf(ctx, slog.LevelError, format, args...)
}

// Fatal logs at error level with formatting.
// Note: Does not exit - matches the current behavior of this adapter.
func (a *ocmLoggerAdapter) Fatal(ctx context.Context, format string, args ...interface{}) {
	a.logf(ctx, slog.LevelError, "FATAL: "+format, args...)
}
