package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/logctx"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	apierrors "github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/errors"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/metrics"
	hfl "github.com/openshift-hyperfleet/hyperfleet-logger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// ResourceNotFoundReason indicates the API returned 404 for the target resource.
const ResourceNotFoundReason = "ResourceNotFound"

// NewExecutor creates a new Executor with the given configuration
func NewExecutor(config *ExecutorConfig) (*Executor, error) {
	if err := validateExecutorConfig(config); err != nil {
		return nil, err
	}

	return &Executor{
		config:             config,
		precondExecutor:    newPreconditionExecutor(config),
		resourceExecutor:   newResourceExecutor(config),
		postActionExecutor: newPostActionExecutor(config),
	}, nil
}

func validateExecutorConfig(config *ExecutorConfig) error {
	if config == nil {
		return fmt.Errorf("config is required")
	}

	if config.Config == nil {
		return fmt.Errorf("config is required")
	}

	if config.TransportRegistry == nil {
		return fmt.Errorf("field TransportRegistry is required")
	}

	if config.APIClient == nil {
		if steps := apiCallSteps(config.Config); len(steps) > 0 {
			return fmt.Errorf("task config makes API calls but no API client was supplied: %s", strings.Join(steps, ", "))
		}
	}

	return nil
}

func apiCallSteps(cfg *configloader.Config) []string {
	var steps []string
	for i, param := range cfg.Params {
		if param.Source.IsAPICall() {
			steps = append(steps, fmt.Sprintf("%s[%d].%s.%s (%s)",
				configloader.FieldParams, i, configloader.FieldSource, configloader.FieldAPICall, param.Name))
		}
	}
	for i, precond := range cfg.Preconditions {
		if precond.APICall != nil {
			steps = append(steps, fmt.Sprintf("%s[%d].%s (%s)",
				configloader.FieldPreconditions, i, configloader.FieldAPICall, precond.Name))
		}
	}
	if cfg.Post != nil {
		for i, action := range cfg.Post.PostActions {
			if action.APICall != nil {
				steps = append(steps, fmt.Sprintf("%s.%s[%d].%s (%s)",
					configloader.FieldPost, configloader.FieldPostActions, i, configloader.FieldAPICall, action.Name))
			}
		}
	}
	return steps
}

// Execute processes event data according to the adapter configuration
// The caller is responsible for:
// - Adding event ID to context for logging correlation using hfl.Set(ctx, logctx.EventIDKey, id)
func (e *Executor) Execute(ctx context.Context, data interface{}) *ExecutionResult {
	// Start OTel span and add trace context to logs
	ctx, span := e.startTracedExecution(ctx)
	defer span.End()

	// Parse event data
	eventData, rawData, err := ParseEventData(data)
	if err != nil {
		parseErr := fmt.Errorf("failed to parse event data: %w", err)
		slog.ErrorContext(ctx, "failed to parse event data", "error", parseErr)
		return &ExecutionResult{
			Status:       StatusFailed,
			CurrentPhase: PhaseParamExtraction,
			Errors:       map[ExecutionPhase]error{PhaseParamExtraction: parseErr},
		}
	}

	// Map the event ID to resource_id. For an owned resource, also map its kind
	// to resource_type and its owner's kind and ID to owner_resource_type and owner_resource_id.
	if eventData.OwnerReferences != nil {
		ctx = hfl.WithResourceType(ctx, eventData.Kind)
		ctx = hfl.WithResourceID(ctx, eventData.ID)
		ctx = hfl.Set(ctx, logctx.OwnerResourceTypeKey, eventData.OwnerReferences.Kind)
		ctx = hfl.Set(ctx, logctx.OwnerResourceIDKey, eventData.OwnerReferences.ID)
	} else {
		ctx = hfl.WithResourceID(ctx, eventData.ID)
	}

	execCtx := NewExecutionContext(ctx, rawData, e.config.Config)

	// Initialize execution result
	result := &ExecutionResult{
		Status:       StatusSuccess,
		Params:       make(map[string]interface{}),
		Errors:       make(map[ExecutionPhase]error),
		CurrentPhase: PhaseParamExtraction,
	}

	slog.InfoContext(ctx, "processing event")

	// Phase 1: Parameter Extraction
	slog.InfoContext(ctx, "phase running", "phase", result.CurrentPhase)
	paramErr := e.executeParamExtraction(execCtx)
	result.APIAuthFailureStatusCodes = execCtx.APIAuthFailureStatusCodes
	if paramErr != nil {
		result.Status = StatusFailed
		result.Errors[PhaseParamExtraction] = paramErr
		execCtx.SetError("ParameterExtractionFailed", paramErr.Error())
		resErr := fmt.Errorf("parameter extraction failed: %w", paramErr)
		slog.ErrorContext(ctx, "phase failed", "phase", PhaseParamExtraction, "error", resErr)
		result.ExecutionContext = execCtx
		result.Params = execCtx.Params
		return result
	}
	result.Params = execCtx.Params
	slog.DebugContext(ctx, "parameter extraction completed", "param_count", len(execCtx.Params))

	// Phase 2: Preconditions
	result.CurrentPhase = PhasePreconditions
	preconditions := e.config.Config.Preconditions
	slog.InfoContext(ctx, "phase running", "phase", result.CurrentPhase, "configured_count", len(preconditions))
	precondOutcome := e.precondExecutor.ExecuteAll(ctx, preconditions, execCtx)
	result.PreconditionResults = precondOutcome.Results

	switch {
	case precondOutcome.Error != nil && apierrors.IsResourceNotFoundError(precondOutcome.Error):
		// Resource no longer exists (e.g. deleted externally, wrong ID in event).
		// Stop processing gracefully.
		slog.InfoContext(ctx, "phase: resource not found, stopping processing gracefully",
			"phase", result.CurrentPhase)
		result.ResourcesSkipped = true
		result.SkipReason = ResourceNotFoundReason
		execCtx.SetSkipped(ResourceNotFoundReason, "")
		execCtx.Adapter.ExecutionError = nil
		result.ExecutionContext = execCtx
		result.Params = execCtx.Params
		return result
	case precondOutcome.Error != nil:
		// Process execution error: precondition evaluation failed
		result.Status = StatusFailed
		precondErr := fmt.Errorf("precondition evaluation failed: error=%w", precondOutcome.Error)
		result.Errors[result.CurrentPhase] = precondErr
		execCtx.SetError("PreconditionFailed", precondOutcome.Error.Error())
		slog.ErrorContext(ctx, "phase failed",
			"phase", result.CurrentPhase,
			"error", precondOutcome.Error,
		)
		result.ResourcesSkipped = true
		result.SkipReason = "PreconditionFailed"
		// Set skip metadata on adapter context without overwriting the failed execution status
		// Note: SetSkipped() is NOT called here because it resets ExecutionStatus to "success",
		// which would mask the precondition failure in CEL expressions (e.g., Health condition)
		execCtx.Adapter.ResourcesSkipped = true
		execCtx.Adapter.SkipReason = precondOutcome.Error.Error()
		// Continue to post actions for error reporting
	case !precondOutcome.AllMatched:
		// Business outcome: precondition not satisfied
		result.ResourcesSkipped = true
		result.SkipReason = precondOutcome.NotMetReason
		execCtx.SetSkipped("PreconditionNotMet", precondOutcome.NotMetReason)
		slog.InfoContext(ctx, "phase success: not met", "phase", result.CurrentPhase, "reason", precondOutcome.NotMetReason)
	default:
		// All preconditions matched
		slog.InfoContext(ctx, "phase success: met", "phase", result.CurrentPhase, "passed_count", len(precondOutcome.Results))
	}

	// Phase 3: Resources (skip if preconditions not met or previous error)
	result.CurrentPhase = PhaseResources
	resources := e.config.Config.Resources
	slog.InfoContext(ctx, "phase running", "phase", result.CurrentPhase, "configured_count", len(resources))
	if !result.ResourcesSkipped {
		resourceResults, resourceErr := e.resourceExecutor.ExecuteAll(ctx, resources, execCtx)
		result.ResourceResults = resourceResults

		if resourceErr != nil {
			result.Status = StatusFailed
			resErr := fmt.Errorf("resource execution failed: %w", resourceErr)
			result.Errors[result.CurrentPhase] = resErr
			execCtx.SetError("ResourceFailed", resourceErr.Error())
			slog.ErrorContext(ctx, "phase failed", "phase", result.CurrentPhase, "error", resourceErr)
			// Continue to post actions for error reporting
		} else {
			slog.InfoContext(ctx, "phase success", "phase", result.CurrentPhase, "processed_count", len(resourceResults))
		}
	} else {
		slog.InfoContext(ctx, "phase skipped", "phase", result.CurrentPhase, "reason", result.SkipReason)
	}

	// Phase 4: Post Actions (always execute for error reporting)
	result.CurrentPhase = PhasePostActions
	postConfig := e.config.Config.Post
	postActionCount := 0
	if postConfig != nil {
		postActionCount = len(postConfig.PostActions)
	}
	slog.InfoContext(ctx, "phase running", "phase", result.CurrentPhase, "configured_count", postActionCount)
	postResults, err := e.postActionExecutor.ExecuteAll(ctx, postConfig, execCtx)
	result.PostActionResults = postResults

	if err != nil {
		if apierrors.IsResourceNotFoundError(err) {
			// Resource no longer exists. Log and continue, don't fail.
			slog.InfoContext(ctx, "phase: resource not found, skipping remaining post-actions",
				"phase", result.CurrentPhase)
			result.ResourcesSkipped = true
			// ResourceNotFound takes precedence: the resource no longer exists,
			// making the original skip reason moot.
			result.SkipReason = ResourceNotFoundReason
			// The PostActionExecutor set the step to StatusFailed before the error
			// reached us. Now that we've decided this 404 is a graceful stop (not a
			// real failure), correct the step to match that decision.
			if len(result.PostActionResults) > 0 {
				last := &result.PostActionResults[len(result.PostActionResults)-1]
				last.Status = StatusSkipped
				last.Skipped = true
				last.SkipReason = ResourceNotFoundReason
				last.Error = nil
			}
			if result.Status == StatusSuccess {
				execCtx.SetSkipped(ResourceNotFoundReason, "")
				execCtx.Adapter.ExecutionError = nil
			}
		} else {
			result.Status = StatusFailed
			postErr := fmt.Errorf("post action execution failed: %w", err)
			result.Errors[result.CurrentPhase] = postErr
			slog.ErrorContext(ctx, "phase failed", "phase", result.CurrentPhase, "error", err)
		}
	} else {
		slog.InfoContext(ctx, "phase success", "phase", result.CurrentPhase, "executed_count", len(postResults))
	}

	// Finalize
	result.ExecutionContext = execCtx

	if result.Status == StatusSuccess {
		slog.InfoContext(ctx, "event execution finished",
			"execution_status", "success", "resources_skipped", result.ResourcesSkipped, "reason", result.SkipReason)
	} else {
		// Combine all errors into a single error for logging
		var errMsgs []string
		for phase, err := range result.Errors {
			errMsgs = append(errMsgs, fmt.Sprintf("%s: %v", phase, err))
		}
		combinedErr := fmt.Errorf("execution failed: %s", strings.Join(errMsgs, "; "))
		slog.ErrorContext(ctx, "event execution finished", "execution_status", "failed", "error", combinedErr)
	}
	return result
}

// executeParamExtraction extracts parameters from the event and environment
func (e *Executor) executeParamExtraction(execCtx *ExecutionContext) error {
	return extractParams(execCtx, e.config.Config, e.config.APIClient)
}

// extractParams sets the config variable and extracts the configured params.
func extractParams(execCtx *ExecutionContext, config *configloader.Config, apiClient hyperfleetapi.Client) error {
	configMap, err := configToMap(config)
	if err != nil {
		return NewExecutorError(PhaseParamExtraction, "config", "failed to marshal config", err)
	}

	// Templates and CEL read a redacted config, so rendered manifests and logs do
	// not show sensitive values such as store credentials.
	redactedMap, err := configToMap(config.Redacted())
	if err != nil {
		return NewExecutorError(PhaseParamExtraction, "config", "failed to marshal redacted config", err)
	}
	execCtx.configVariable = redactedMap

	// config.* param sources resolve against the real (unredacted) config so that
	// sensitive fields like store URLs can still be explicitly extracted when needed.
	return extractConfigParams(execCtx.Ctx, config, execCtx, configMap, apiClient)
}

// startTracedExecution creates an OTel span and adds trace context to logs.
// Returns the enriched context and span. Caller must call span.End() when done.
//
// This method:
//   - Creates an OTel span with trace_id and span_id (for distributed tracing)
//   - Adds trace_id and span_id to logger context (for log correlation)
//   - The trace context is automatically propagated to outgoing HTTP requests
func (e *Executor) startTracedExecution(ctx context.Context) (context.Context, trace.Span) {
	componentName := e.config.Config.Adapter.Name
	ctx, span := otel.Tracer(componentName).Start(ctx, "Execute")

	// Add trace_id and span_id to logger context for log correlation
	ctx = logctx.WithOTelTraceContext(ctx)

	return ctx, span
}

// ParseEventData parses event data from various input types into structured EventData and raw map.
// Accepts: []byte (JSON), map[string]interface{}, or any JSON-serializable type.
// Returns: structured EventData, raw map for flexible access, and any error.
func ParseEventData(data interface{}) (*EventData, map[string]interface{}, error) {
	if data == nil {
		return &EventData{}, make(map[string]interface{}), nil
	}

	var jsonBytes []byte
	var err error

	switch v := data.(type) {
	case []byte:
		if len(v) == 0 {
			return &EventData{}, make(map[string]interface{}), nil
		}
		jsonBytes = v
	case map[string]interface{}:
		// Already a map, marshal to JSON for struct conversion
		jsonBytes, err = json.Marshal(v)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal map data: error=%w", err)
		}
	default:
		// Try to marshal any other type
		jsonBytes, err = json.Marshal(v)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal data: error=%w", err)
		}
	}

	// Parse into structured EventData
	var eventData EventData
	if err := json.Unmarshal(jsonBytes, &eventData); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal to EventData: error=%w", err)
	}

	// Parse into raw map for flexible access
	var rawData map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &rawData); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal to map: error=%w", err)
	}

	return &eventData, rawData, nil
}

// ExecutorBuilder provides a fluent interface for building an Executor
type ExecutorBuilder struct {
	config *ExecutorConfig
}

// NewBuilder creates a new ExecutorBuilder
func NewBuilder() *ExecutorBuilder {
	return &ExecutorBuilder{
		config: &ExecutorConfig{},
	}
}

// WithConfig sets the unified configuration
func (b *ExecutorBuilder) WithConfig(config *configloader.Config) *ExecutorBuilder {
	b.config.Config = config
	return b
}

// WithAPIClient sets the HyperFleet API client
func (b *ExecutorBuilder) WithAPIClient(client hyperfleetapi.Client) *ExecutorBuilder {
	b.config.APIClient = client
	return b
}

// WithTransportRegistry sets the named transport clients for resource application.
func (b *ExecutorBuilder) WithTransportRegistry(registry transportclient.Registry) *ExecutorBuilder {
	b.config.TransportRegistry = registry
	return b
}

// WithTransportClient configures a Kubernetes client for callers that use one transport.
func (b *ExecutorBuilder) WithTransportClient(client transportclient.TransportClient) *ExecutorBuilder {
	return b.WithTransportRegistry(transportclient.Registry{
		configloader.TransportClientKubernetes: {Client: client},
	})
}

// WithMetricsRecorder sets the optional Prometheus metrics recorder
func (b *ExecutorBuilder) WithMetricsRecorder(recorder *metrics.Recorder) *ExecutorBuilder {
	b.config.MetricsRecorder = recorder
	return b
}

// Build creates the Executor
func (b *ExecutorBuilder) Build() (*Executor, error) {
	return NewExecutor(b.config)
}
