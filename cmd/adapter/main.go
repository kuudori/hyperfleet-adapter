package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/dryrun"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/executor"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/logctx"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportregistry"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/health"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/metrics"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/telemetry"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/version"
	"github.com/openshift-hyperfleet/hyperfleet-broker/broker"
	hfl "github.com/openshift-hyperfleet/hyperfleet-logger"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"gopkg.in/yaml.v3"
)

// Command-line flags
var (
	configPath     string // Path to deployment config (adapter-config.yaml)
	taskConfigPath string // Path to task config (adapter-task-config.yaml)
	logLevel       string
	logFormat      string
	logOutput      string

	// Dry-run flags
	dryRunEvent        string // Path to CloudEvent JSON file
	dryRunAPIResponses string // Path to mock API responses JSON file
	dryRunDiscovery    string // Path to mock discovery responses JSON file
	dryRunVerbose      bool   // Show verbose dry-run output
	dryRunOutput       string // Output format: text or json
)

// Timeout constants
const (
	// OTelShutdownTimeout is the timeout for gracefully shutting down the OpenTelemetry TracerProvider
	OTelShutdownTimeout = 5 * time.Second
	// HealthServerShutdownTimeout is the timeout for gracefully shutting down the health server
	HealthServerShutdownTimeout = 5 * time.Second
)

// Server port constants
const (
	// HealthServerPort is the port for /healthz and /readyz endpoints
	HealthServerPort = "8080"
	// MetricsServerPort is the port for /metrics endpoint
	MetricsServerPort = "9090"
)

func main() {
	// Root command
	rootCmd := &cobra.Command{
		Use:   "adapter",
		Short: "HyperFleet Adapter - event-driven Kubernetes resource manager",
		Long: `HyperFleet Adapter listens for events from a message broker and
executes configured actions including Kubernetes resource management
and HyperFleet API calls.`,
		// Disable default completion command
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}

	// Add flags to root command (so they work on all subcommands)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	// Serve command
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the adapter and begin processing events",
		Long: `Start the HyperFleet adapter in serve mode. The adapter will:
- Connect to the configured message broker
- Subscribe to the specified topic
- Process incoming events according to the adapter configuration
- Execute Kubernetes operations and HyperFleet API calls

Dry-run mode:
  Pass --dry-run-event to process a single CloudEvent from a JSON file
  using mock transport clients. No broker, cluster, or API is required.
  Optionally pass --dry-run-api-responses to configure mock API responses.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if isDryRun() {
				return runDryRun(cmd.Flags())
			}
			return runServe(cmd.Flags())
		},
	}
	addConfigPathFlags(serveCmd)
	addOverrideFlags(serveCmd)
	serveCmd.Flags().Bool("debug-config", false,
		"Log the full merged configuration after load. Env: HYPERFLEET_DEBUG_CONFIG")
	serveCmd.Flags().StringVar(&logLevel, "log-level", "",
		"Log level (debug, info, warn, error). Env: LOG_LEVEL")
	serveCmd.Flags().StringVar(&logFormat, "log-format", "",
		"Log format (text, json). Env: LOG_FORMAT")
	serveCmd.Flags().StringVar(&logOutput, "log-output", "",
		"Log output (stdout, stderr). Env: LOG_OUTPUT")
	serveCmd.Flags().StringVar(&dryRunEvent, "dry-run-event", "",
		"Path to CloudEvent JSON file for dry-run mode")
	serveCmd.Flags().StringVar(&dryRunAPIResponses, "dry-run-api-responses", "",
		"Path to mock API responses JSON file for dry-run mode (defaults to 200 OK)")
	serveCmd.Flags().StringVar(&dryRunDiscovery, "dry-run-discovery", "",
		"Path to mock discovery responses JSON file for dry-run mode (overrides applied resources)")
	serveCmd.Flags().BoolVar(&dryRunVerbose, "dry-run-verbose", false,
		"Show rendered manifests, API request/response bodies in dry-run output")
	serveCmd.Flags().StringVar(&dryRunOutput, "dry-run-output", "text",
		"Dry-run output format: text or json")

	// Config-dump command: loads config and prints the merged result as YAML, then exits.
	// Useful for debugging and verifying that config files, env vars, and CLI flags load correctly.
	configDumpCmd := &cobra.Command{
		Use:   "config-dump",
		Short: "Load and print the merged adapter configuration as YAML",
		Long: `Load the adapter configuration from config files, environment variables,
and CLI flags, then print the merged result as YAML to stdout.
Sensitive fields (certificates, keys) are redacted.
Exits with code 0 on success, non-zero on error.

Priority order (lowest to highest): config file < env vars < CLI flags`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigDump(cmd.Flags())
		},
	}
	addConfigPathFlags(configDumpCmd)
	addOverrideFlags(configDumpCmd)
	configDumpCmd.Flags().Bool("debug-config", false,
		"Include debug_config field in output. Env: HYPERFLEET_DEBUG_CONFIG")
	configDumpCmd.Flags().StringVar(&logLevel, "log-level", "",
		"Log level (debug, info, warn, error). Env: LOG_LEVEL")
	configDumpCmd.Flags().StringVar(&logFormat, "log-format", "",
		"Log format (text, json). Env: LOG_FORMAT")
	configDumpCmd.Flags().StringVar(&logOutput, "log-output", "",
		"Log output (stdout, stderr). Env: LOG_OUTPUT")

	// Version command
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			info := version.Info()
			fmt.Printf("Version:    %s\n", info.Version)
			fmt.Printf("Commit:     %s\n", info.Commit)
			fmt.Printf("Build Date: %s\n", info.BuildDate)
		},
	}

	// Add subcommands
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(configDumpCmd)
	rootCmd.AddCommand(versionCmd)

	// Execute
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// isDryRun returns true when dry-run flags are present.
func isDryRun() bool {
	return dryRunEvent != "" || dryRunAPIResponses != ""
}

// -----------------------------------------------------------------------------
// Configuration loading (shared between serve and dry-run)
// -----------------------------------------------------------------------------

// buildLogOptions computes the log level/format/output strings with priority
// (lowest to highest): config file < LOG_* env vars < --log-* CLI flags.
// Pass logCfg=nil for the bootstrap handler (before config is loaded).
func buildLogOptions(logCfg *configloader.LogConfig) (level, format, output string) {
	if logCfg != nil {
		level, format, output = logCfg.Level, logCfg.Format, logCfg.Output
	}

	// Apply environment variables (override config file)
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		level = strings.ToLower(v)
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		format = strings.ToLower(v)
	}
	if v := os.Getenv("LOG_OUTPUT"); v != "" {
		output = v
	}

	// Apply CLI flags (highest priority)
	if logLevel != "" {
		level = logLevel
	}
	if logFormat != "" {
		format = logFormat
	}
	if logOutput != "" {
		output = logOutput
	}

	return level, format, output
}

// buildDryRunLogOptions applies the normal level and format overrides while
// keeping logs on stderr so stdout remains machine-readable trace output.
func buildDryRunLogOptions() (level, format string) {
	level, format, _ = buildLogOptions(&configloader.LogConfig{
		Level:  "warn",
		Format: "text",
	})
	return level, format
}

// initLogging builds the shared hyperfleet-logger handler and installs it as
// the process-wide slog default. Pass logCfg=nil for the bootstrap handler
// (before config is loaded).
func initLogging(component string, logCfg *configloader.LogConfig) error {
	levelStr, formatStr, outputStr := buildLogOptions(logCfg)
	return initLoggingWithOptions(component, levelStr, formatStr, outputStr)
}

// initLoggingWithOptions builds the shared hyperfleet-logger handler and
// installs it as the process-wide slog default using explicit options.
func initLoggingWithOptions(component, levelStr, formatStr, outputStr string) error {
	level, err := hfl.ParseLevel(levelStr)
	if err != nil {
		return fmt.Errorf("invalid log level: %w", err)
	}
	format, err := hfl.ParseFormat(formatStr)
	if err != nil {
		return fmt.Errorf("invalid log format: %w", err)
	}
	output, err := hfl.ParseOutput(outputStr)
	if err != nil {
		return fmt.Errorf("invalid log output: %w", err)
	}

	handler := hfl.NewHandler(component, version.Version,
		hfl.WithLevel(level),
		hfl.WithFormat(format),
		hfl.WithOutput(output),
		hfl.WithContextFields(logctx.ContextFields()...),
		hfl.WithStackTrace(logctx.StackTraceFilter),
	)
	slog.SetDefault(slog.New(handler))
	return nil
}

// loadConfig loads the unified adapter configuration from both config files.
func loadConfig(ctx context.Context, flags *pflag.FlagSet) (*configloader.Config, error) {
	slog.InfoContext(ctx, "loading adapter configuration...")
	config, err := configloader.LoadConfig(
		configloader.WithAdapterConfigPath(configPath),
		configloader.WithTaskConfigPath(taskConfigPath),
		configloader.WithAdapterVersion(version.Version),
		configloader.WithFlags(flags),
		configloader.WithContext(ctx),
	)
	if err != nil {
		slog.ErrorContext(ctx, "failed to load adapter configuration", "error", err)
		return nil, fmt.Errorf("failed to load adapter configuration: %w", err)
	}
	return config, nil
}

// -----------------------------------------------------------------------------
// Client creation (shared between serve and dry-run)
// -----------------------------------------------------------------------------

// createAPIClient creates a HyperFleet API client from the config
func createAPIClient(apiConfig configloader.HyperfleetAPIConfig) (hyperfleetapi.Client, error) {
	var opts []hyperfleetapi.ClientOption

	// Set base URL if configured (env fallback handled in NewClient)
	if apiConfig.BaseURL != "" {
		opts = append(opts, hyperfleetapi.WithBaseURL(apiConfig.BaseURL))
	}

	// Set timeout if configured (0 means use default)
	if apiConfig.Timeout > 0 {
		opts = append(opts, hyperfleetapi.WithTimeout(apiConfig.Timeout))
	}

	// Set retry attempts
	if apiConfig.RetryAttempts > 0 {
		opts = append(opts, hyperfleetapi.WithRetryAttempts(apiConfig.RetryAttempts))
	}

	// Set retry backoff strategy
	if apiConfig.RetryBackoff != "" {
		switch apiConfig.RetryBackoff {
		case hyperfleetapi.BackoffExponential, hyperfleetapi.BackoffLinear, hyperfleetapi.BackoffConstant:
			opts = append(opts, hyperfleetapi.WithRetryBackoff(apiConfig.RetryBackoff))
		default:
			return nil, fmt.Errorf(
				"invalid retry backoff strategy %q (supported: exponential, linear, constant)",
				apiConfig.RetryBackoff,
			)
		}
	}

	// Set retry base delay
	if apiConfig.BaseDelay > 0 {
		opts = append(opts, hyperfleetapi.WithBaseDelay(apiConfig.BaseDelay))
	}

	// Set retry max delay
	if apiConfig.MaxDelay > 0 {
		opts = append(opts, hyperfleetapi.WithMaxDelay(apiConfig.MaxDelay))
	}

	// Set default headers
	for key, value := range apiConfig.DefaultHeaders {
		opts = append(opts, hyperfleetapi.WithDefaultHeader(key, value))
	}

	// Configure bearer token auth if set
	if apiConfig.Auth != nil {
		opts = append(opts, hyperfleetapi.WithAuth(apiConfig.Auth))
	}

	return hyperfleetapi.NewClient(opts...)
}

// buildExecutor creates the executor with the given clients.
func buildExecutor(
	config *configloader.Config,
	apiClient hyperfleetapi.Client,
	tc transportclient.TransportClient,
	metricsRecorder *metrics.Recorder,
) (*executor.Executor, error) {
	return executor.NewBuilder().
		WithConfig(config).
		WithAPIClient(apiClient).
		WithTransportClient(tc).
		WithMetricsRecorder(metricsRecorder).
		Build()
}

// -----------------------------------------------------------------------------
// Serve mode (normal operation)
// -----------------------------------------------------------------------------

// runServe contains the main application logic for the serve command
func runServe(flags *pflag.FlagSet) error {
	// Create context that cancels on system signals
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create bootstrap logging (before config is loaded)
	if err := initLogging("hyperfleet-adapter", nil); err != nil {
		return fmt.Errorf("failed to initialize logging: %w", err)
	}

	slog.InfoContext(ctx, "starting hyperfleet adapter",
		"commit", version.Commit, "built", version.BuildDate)

	// Load unified configuration (deployment + task configs)
	config, err := loadConfig(ctx, flags)
	if err != nil {
		return err
	}

	// Reinitialize logging with component name and log settings from config
	if err = initLogging(config.Adapter.Name, &config.Log); err != nil {
		return fmt.Errorf("failed to initialize logging with adapter config: %w", err)
	}

	slog.InfoContext(ctx, "adapter configuration loaded successfully", "name", config.Adapter.Name)
	slog.InfoContext(ctx, "hyperfleet api client configured",
		"timeout", config.Clients.HyperfleetAPI.Timeout.String(),
		"retry_attempts", config.Clients.HyperfleetAPI.RetryAttempts)
	var redactedConfigBytes []byte
	if config.DebugConfig {
		var data []byte
		data, err = yaml.Marshal(config.Redacted())
		if err != nil {
			slog.WarnContext(ctx, "failed to marshal adapter configuration for logging", "error", err)
		} else {
			redactedConfigBytes = data
			slog.InfoContext(ctx, "loaded adapter configuration", "config", string(redactedConfigBytes))
		}
	}

	// Initialize OpenTelemetry
	tracingEnabled := true
	if tracingEnv := os.Getenv("HYPERFLEET_TRACING_ENABLED"); tracingEnv != "" {
		var enabled bool
		if enabled, err = strconv.ParseBool(tracingEnv); err == nil {
			tracingEnabled = enabled
		} else {
			slog.WarnContext(ctx, "invalid HYPERFLEET_TRACING_ENABLED value, defaulting to true", "value", tracingEnv)
		}
	}

	var tp *sdktrace.TracerProvider
	if tracingEnabled {
		serviceName := config.Adapter.Name
		if svcName := os.Getenv("OTEL_SERVICE_NAME"); svcName != "" {
			serviceName = svcName
		}

		var traceProvider *sdktrace.TracerProvider
		traceProvider, err = telemetry.InitTraceProvider(ctx, serviceName, version.Version)
		if err != nil {
			slog.ErrorContext(ctx, "failed to initialize opentelemetry", "error", err)
			return fmt.Errorf("failed to initialize OpenTelemetry: %w", err)
		}
		tp = traceProvider
		slog.InfoContext(ctx, "opentelemetry initialized", "service_name", serviceName)
	} else {
		slog.InfoContext(ctx, "opentelemetry tracing disabled")
	}
	defer func() {
		if tp != nil {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), OTelShutdownTimeout)
			defer shutdownCancel()
			if shutdownErr := tp.Shutdown(shutdownCtx); shutdownErr != nil {
				slog.WarnContext(ctx, "failed to shutdown opentelemetry", "error", shutdownErr)
			}
		}
	}()

	// Start health server
	healthServer := health.NewServer(HealthServerPort, config.Adapter.Name)
	err = healthServer.Start(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to start health server", "error", err)
		return fmt.Errorf("failed to start health server: %w", err)
	}
	healthServer.SetConfigLoaded()
	if len(redactedConfigBytes) > 0 {
		healthServer.SetConfig(redactedConfigBytes)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), HealthServerShutdownTimeout)
		defer shutdownCancel()
		if shutdownErr := healthServer.Shutdown(shutdownCtx); shutdownErr != nil {
			slog.WarnContext(shutdownCtx, "failed to shutdown health server", "error", shutdownErr)
		}
	}()

	// Start metrics server
	metricsServer := health.NewMetricsServer(MetricsServerPort, health.MetricsConfig{
		Component: config.Adapter.Name,
		Version:   version.Version,
		Commit:    version.Commit,
	})
	err = metricsServer.Start(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to start metrics server", "error", err)
		return fmt.Errorf("failed to start metrics server: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), HealthServerShutdownTimeout)
		defer shutdownCancel()
		if shutdownErr := metricsServer.Shutdown(shutdownCtx); shutdownErr != nil {
			slog.WarnContext(shutdownCtx, "failed to shutdown metrics server", "error", shutdownErr)
		}
	}()

	// Create adapter metrics recorder
	adapterName := metrics.ExtractAdapterName(config.Adapter.Name)
	metricsRecorder := metrics.NewRecorder(config.Adapter.Name, version.Version, adapterName, nil)

	// Create real clients
	slog.InfoContext(ctx, "creating hyperfleet api client...")
	apiClient, err := createAPIClient(config.Clients.HyperfleetAPI)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create hyperfleet api client", "error", err)
		return fmt.Errorf("failed to create HyperFleet API client: %w", err)
	}

	transportRuntime, err := transportregistry.Build(ctx, config)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create transport registry", "error", err)
		return fmt.Errorf("failed to create transport registry: %w", err)
	}
	defer func() {
		if closeErr := transportRuntime.Close(); closeErr != nil {
			slog.WarnContext(ctx, "failed to close transport registry", "error", closeErr)
		}
	}()

	compatibilityKey := configloader.TransportClientKubernetes
	if config.Clients.Maestro != nil {
		compatibilityKey = configloader.TransportClientMaestro
	}
	tc, err := transportRuntime.Registry.Get(compatibilityKey)
	if err != nil {
		return fmt.Errorf("failed to resolve transport client: %w", err)
	}

	// Build executor
	slog.InfoContext(ctx, "creating event executor...")
	exec, err := buildExecutor(config, apiClient, tc, metricsRecorder)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create executor", "error", err)
		return fmt.Errorf("failed to create executor: %w", err)
	}

	// Create the event handler and subscribe to broker
	handler := executor.AlwaysAck(executor.WithMetrics(exec.CreateHandler(), metricsRecorder))

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.InfoContext(ctx, "received signal, initiating graceful shutdown...", "signal", sig.String())
		slog.InfoContext(ctx, "shutdown initiated, marking not ready")
		healthServer.SetShuttingDown(true)
		metricsServer.MarkShuttingDown()
		cancel()

		// Second signal forces immediate exit
		sig = <-sigCh
		slog.InfoContext(ctx, "received second signal, forcing immediate exit", "signal", sig.String())
		os.Exit(1)
	}()

	// Get broker config
	subscriptionID := config.Clients.Broker.SubscriptionID
	if subscriptionID == "" {
		err = fmt.Errorf("clients.broker.subscription_id is required")
		slog.ErrorContext(ctx, "missing required broker configuration", "error", err)
		return err
	}

	topic := config.Clients.Broker.Topic
	if topic == "" {
		err = fmt.Errorf("clients.broker.topic is required")
		slog.ErrorContext(ctx, "missing required broker configuration", "error", err)
		return err
	}

	// Create broker metrics recorder
	brokerMetrics := broker.NewMetricsRecorder(config.Adapter.Name, version.Version, nil)

	// Create broker subscriber and subscribe
	slog.InfoContext(ctx, "creating broker subscriber...")
	subscriber, err := broker.NewSubscriber(slog.Default(), subscriptionID, brokerMetrics)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create subscriber", "error", err)
		return fmt.Errorf("failed to create subscriber: %w", err)
	}
	slog.InfoContext(ctx, "broker subscriber created successfully")

	slog.InfoContext(ctx, "subscribing to broker topic...")
	err = subscriber.Subscribe(ctx, topic, handler)
	if err != nil {
		slog.ErrorContext(ctx, "failed to subscribe to topic", "error", err)
		return fmt.Errorf("failed to subscribe to topic: %w", err)
	}
	slog.InfoContext(ctx, "successfully subscribed to broker topic")

	// Mark as ready
	healthServer.SetBrokerReady(true)
	slog.InfoContext(ctx, "adapter is ready to process events")

	// Monitor subscription errors
	fatalErrCh := make(chan error, 1)
	go func() {
		for subErr := range subscriber.Errors() {
			slog.ErrorContext(ctx, "subscription error", "error", subErr)
			select {
			case fatalErrCh <- subErr:
			default:
			}
		}
	}()

	slog.InfoContext(ctx, "adapter started, waiting for events...")

	// Wait for shutdown signal or fatal subscription error
	select {
	case <-ctx.Done():
		slog.InfoContext(ctx, "context canceled, shutting down...")
	case err := <-fatalErrCh:
		slog.ErrorContext(ctx, "fatal subscription error, shutting down", "error", err)
		healthServer.SetShuttingDown(true)
		metricsServer.MarkShuttingDown()
		cancel()
	}

	// Close subscriber gracefully
	slog.InfoContext(ctx, "closing broker subscriber...")
	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer shutdownCancel()

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- subscriber.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			slog.ErrorContext(ctx, "error closing subscriber", "error", err)
		} else {
			slog.InfoContext(ctx, "subscriber closed successfully")
		}
	case <-shutdownCtx.Done():
		err := fmt.Errorf("subscriber close timed out after 30 seconds")
		slog.ErrorContext(ctx, "subscriber close timed out", "error", err)
	}

	slog.InfoContext(ctx, "adapter shutdown complete")

	return nil
}

// -----------------------------------------------------------------------------
// Dry-run mode
// -----------------------------------------------------------------------------

// runDryRun processes a single CloudEvent from file using mock clients.
func runDryRun(flags *pflag.FlagSet) error {
	ctx := context.Background()

	// Log on stderr so stdout is reserved for trace output
	level, format := buildDryRunLogOptions()
	if err := initLoggingWithOptions("dry-run", level, format, "stderr"); err != nil {
		return fmt.Errorf("failed to initialize logging: %w", err)
	}

	// Load config (same path as serve)
	config, err := loadConfig(ctx, flags)
	if err != nil {
		return err
	}

	// Load CloudEvent from file
	if dryRunEvent == "" {
		return fmt.Errorf("--dry-run-event is required for dry-run mode")
	}
	evt, err := dryrun.LoadCloudEvent(dryRunEvent)
	if err != nil {
		return fmt.Errorf("failed to load event: %w", err)
	}

	// Create dryrun API client
	var dryrunResponsesFile *dryrun.DryrunResponsesFile
	if dryRunAPIResponses != "" {
		dryrunResponsesFile, err = dryrun.LoadDryrunResponses(dryRunAPIResponses)
		if err != nil {
			return fmt.Errorf("failed to load dryrun responses: %w", err)
		}
	}
	dryrunAPI, err := dryrun.NewDryrunAPIClient(dryrunResponsesFile)
	if err != nil {
		return fmt.Errorf("failed to create dryrun API client: %w", err)
	}

	// Create recording transport client
	var dryrunClient *dryrun.DryrunTransportClient
	if dryRunDiscovery != "" {
		var overrides dryrun.DiscoveryOverrides
		overrides, err = dryrun.LoadDiscoveryOverrides(dryRunDiscovery)
		if err != nil {
			return fmt.Errorf("failed to load discovery overrides: %w", err)
		}
		dryrunClient = dryrun.NewDryrunTransportClientWithOverrides(overrides)
	} else {
		dryrunClient = dryrun.NewDryrunTransportClient()
	}

	transportRuntime, err := transportregistry.BuildRecording(config, dryrunClient)
	if err != nil {
		return fmt.Errorf("failed to create recording transport registry: %w", err)
	}
	defer func() {
		if closeErr := transportRuntime.Close(); closeErr != nil {
			slog.WarnContext(ctx, "failed to close recording transport registry", "error", closeErr)
		}
	}()

	compatibilityKey := configloader.TransportClientKubernetes
	if config.Clients.Maestro != nil {
		compatibilityKey = configloader.TransportClientMaestro
	}
	tc, err := transportRuntime.Registry.Get(compatibilityKey)
	if err != nil {
		return fmt.Errorf("failed to resolve recording transport client: %w", err)
	}

	// Build executor with mock clients (same builder as serve, no metrics in dry-run)
	exec, err := buildExecutor(config, dryrunAPI, tc, nil)
	if err != nil {
		return fmt.Errorf("failed to create executor: %w", err)
	}

	// Execute with event data
	result := exec.Execute(ctx, evt.Data())

	// Build and output execution trace
	trace := &dryrun.ExecutionTrace{
		EventID:   evt.ID(),
		EventType: evt.Type(),
		Result:    result,
		APIClient: dryrunAPI,
		Transport: dryrunClient,
		Verbose:   dryRunVerbose,
	}

	switch dryRunOutput {
	case "json":
		data, err := trace.FormatJSON()
		if err != nil {
			return fmt.Errorf("failed to format trace as JSON: %w", err)
		}
		fmt.Println(string(data))
	default:
		fmt.Print(trace.FormatText())
	}

	if result.Status == executor.StatusFailed {
		for phase, err := range result.Errors {
			fmt.Fprintf(os.Stderr, "Error in %s: %v\n", phase, err)
		}
	}

	return nil
}

// -----------------------------------------------------------------------------
// Config-dump mode
// -----------------------------------------------------------------------------

// runConfigDump loads the full adapter configuration and prints it as YAML to stdout.
// Sensitive fields are redacted. Exits 0 on success.
func runConfigDump(flags *pflag.FlagSet) error {
	ctx := context.Background()

	// Log on stderr so stdout remains reserved for the machine-readable YAML output
	level, format, _ := buildLogOptions(nil)
	if err := initLoggingWithOptions("config-dump", level, format, "stderr"); err != nil {
		return fmt.Errorf("failed to initialize logging: %w", err)
	}

	config, err := loadConfig(ctx, flags)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(config.Redacted())
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	fmt.Print(string(data))
	return nil
}

// -----------------------------------------------------------------------------
// Flag registration helpers (shared between serve and config-dump)
// -----------------------------------------------------------------------------

// addConfigPathFlags registers the --config and --task-config path flags.
func addConfigPathFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&configPath, "config", "c", "",
		fmt.Sprintf("Path to adapter deployment config file (can also use %s env var)",
			configloader.EnvAdapterConfig))
	cmd.Flags().StringVarP(&taskConfigPath, "task-config", "t", "",
		fmt.Sprintf("Path to adapter task config file (can also use %s env var)",
			configloader.EnvTaskConfigPath))
}

// addOverrideFlags registers all configuration override flags (Maestro, API, broker, Kubernetes).
// These flags are available on both the serve and config-dump commands.
func addOverrideFlags(cmd *cobra.Command) {
	// Maestro override flags
	cmd.Flags().String("maestro-grpc-server-address", "",
		"Maestro gRPC server address. Env: HYPERFLEET_MAESTRO_GRPC_SERVER_ADDRESS")
	cmd.Flags().String("maestro-http-server-address", "",
		"Maestro HTTP server address. Env: HYPERFLEET_MAESTRO_HTTP_SERVER_ADDRESS")
	cmd.Flags().String("maestro-source-id", "", "Maestro source ID. Env: HYPERFLEET_MAESTRO_SOURCE_ID")
	cmd.Flags().String("maestro-client-id", "", "Maestro client ID. Env: HYPERFLEET_MAESTRO_CLIENT_ID")
	cmd.Flags().String("maestro-auth-type", "", "Maestro auth type (tls, none). Env: HYPERFLEET_MAESTRO_AUTH_TYPE")
	cmd.Flags().String("maestro-ca-file", "", "Maestro gRPC CA certificate file. Env: HYPERFLEET_MAESTRO_CA_FILE")
	cmd.Flags().String("maestro-cert-file", "", "Maestro gRPC client certificate file. Env: HYPERFLEET_MAESTRO_CERT_FILE")
	cmd.Flags().String("maestro-key-file", "", "Maestro gRPC client key file. Env: HYPERFLEET_MAESTRO_KEY_FILE")
	cmd.Flags().String("maestro-http-ca-file", "",
		"Maestro HTTP CA certificate file. Env: HYPERFLEET_MAESTRO_HTTP_CA_FILE")
	cmd.Flags().String("maestro-timeout", "",
		"Maestro client timeout (e.g. 10s). Env: HYPERFLEET_MAESTRO_TIMEOUT")
	cmd.Flags().String("maestro-server-healthiness-timeout", "",
		"Maestro server healthiness check timeout (e.g. 20s). Env: HYPERFLEET_MAESTRO_SERVER_HEALTHINESS_TIMEOUT")
	cmd.Flags().Int("maestro-retry-attempts", 0,
		"Maestro retry attempts. Env: HYPERFLEET_MAESTRO_RETRY_ATTEMPTS")
	cmd.Flags().String("maestro-keepalive-time", "",
		"Maestro gRPC keepalive ping interval (e.g. 30s). Env: HYPERFLEET_MAESTRO_KEEPALIVE_TIME")
	cmd.Flags().String("maestro-keepalive-timeout", "",
		"Maestro gRPC keepalive ping timeout (e.g. 10s). Env: HYPERFLEET_MAESTRO_KEEPALIVE_TIMEOUT")
	cmd.Flags().Bool("maestro-insecure", false,
		"Use insecure connection to Maestro. Env: HYPERFLEET_MAESTRO_INSECURE")

	// HyperFleet API override flags
	cmd.Flags().String("hyperfleet-api-base-url", "", "HyperFleet API base URL. Env: HYPERFLEET_API_BASE_URL")
	cmd.Flags().String("hyperfleet-api-version", "", "HyperFleet API version (e.g. v1). Env: HYPERFLEET_API_VERSION")
	cmd.Flags().String("hyperfleet-api-timeout", "",
		"HyperFleet API timeout (e.g. 10s). Env: HYPERFLEET_API_TIMEOUT")
	cmd.Flags().Int("hyperfleet-api-retry", 0,
		"HyperFleet API retry attempts. Env: HYPERFLEET_API_RETRY_ATTEMPTS")
	cmd.Flags().String("hyperfleet-api-retry-backoff", "",
		"HyperFleet API retry backoff strategy (exponential, linear, constant). Env: HYPERFLEET_API_RETRY_BACKOFF")
	cmd.Flags().String("hyperfleet-api-base-delay", "",
		"HyperFleet API retry base delay (e.g. 1s). Env: HYPERFLEET_API_BASE_DELAY")
	cmd.Flags().String("hyperfleet-api-max-delay", "",
		"HyperFleet API retry max delay (e.g. 30s). Env: HYPERFLEET_API_MAX_DELAY")

	// Broker override flags
	cmd.Flags().String("broker-subscription-id", "", "Broker subscription ID. Env: HYPERFLEET_BROKER_SUBSCRIPTION_ID")
	cmd.Flags().String("broker-topic", "", "Broker topic. Env: HYPERFLEET_BROKER_TOPIC")

	// Kubernetes override flags
	cmd.Flags().String("kubernetes-kube-config-path", "",
		"Path to kubeconfig file (empty = in-cluster auth). Env: HYPERFLEET_KUBERNETES_KUBE_CONFIG_PATH")
	cmd.Flags().String("kubernetes-api-version", "", "Kubernetes API version. Env: HYPERFLEET_KUBERNETES_API_VERSION")
	cmd.Flags().Float64("kubernetes-qps", 0, "Kubernetes client QPS rate limit. Env: HYPERFLEET_KUBERNETES_QPS")
	cmd.Flags().Int("kubernetes-burst", 0, "Kubernetes client burst rate limit. Env: HYPERFLEET_KUBERNETES_BURST")
}
