package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/criteria"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"
)

// PreconditionExecutor evaluates preconditions
type PreconditionExecutor struct {
	apiClient hyperfleetapi.Client
}

// newPreconditionExecutor creates a new precondition executor
// NOTE: Caller (NewExecutor) is responsible for config validation
func newPreconditionExecutor(config *ExecutorConfig) *PreconditionExecutor {
	return &PreconditionExecutor{
		apiClient: config.APIClient,
	}
}

// ExecuteAll executes all preconditions in sequence
// Returns a high-level outcome with match status and individual results
func (pe *PreconditionExecutor) ExecuteAll(
	ctx context.Context,
	preconditions []configloader.Precondition,
	execCtx *ExecutionContext,
) *PreconditionsOutcome {
	results := make([]PreconditionResult, 0, len(preconditions))

	for _, precond := range preconditions {
		result, err := pe.executePrecondition(ctx, precond, execCtx)
		results = append(results, result)

		if err != nil {
			// Execution error (API call failed, parse error, etc.)
			slog.ErrorContext(ctx, "precondition evaluated: failed",
				"precondition", precond.Name,
				"error", err,
			)
			return &PreconditionsOutcome{
				AllMatched: false,
				Results:    results,
				Error:      err,
			}
		}

		if !result.Matched {
			// Business outcome: precondition not satisfied
			slog.InfoContext(ctx, "precondition evaluated: not met",
				"precondition", precond.Name, "matched", false)
			return &PreconditionsOutcome{
				AllMatched:   false,
				Results:      results,
				Error:        nil,
				NotMetReason: fmt.Sprintf("precondition '%s' not met: %s", precond.Name, formatConditionDetails(result)),
			}
		}

		slog.InfoContext(ctx, "precondition evaluated: met", "precondition", precond.Name)
	}

	// All preconditions matched
	return &PreconditionsOutcome{
		AllMatched: true,
		Results:    results,
		Error:      nil,
	}
}

// executePrecondition executes a single precondition
func (pe *PreconditionExecutor) executePrecondition(
	ctx context.Context,
	precond configloader.Precondition,
	execCtx *ExecutionContext,
) (PreconditionResult, error) {
	result := PreconditionResult{
		Name:           precond.Name,
		Status:         StatusSuccess,
		CapturedFields: make(map[string]interface{}),
	}

	// Step 1: Execute log action if configured
	if precond.Log != nil {
		ExecuteLogAction(ctx, precond.Log, execCtx)
	}

	// Step 2: Make API call if configured
	if precond.APICall != nil {
		apiResult, err := pe.executeAPICall(ctx, precond.APICall, execCtx)
		if err != nil {
			result.Status = StatusFailed
			result.Error = err
			return result, execCtx.failPhase(PhasePreconditions, precond.Name, "API call failed", err)
		}
		result.APICallMade = true
		result.APIResponse = apiResult

		// Parse response as JSON
		var responseData map[string]interface{}
		if err := json.Unmarshal(apiResult, &responseData); err != nil {
			result.Status = StatusFailed
			result.Error = fmt.Errorf("failed to parse API response as JSON: %w", err)
			return result, execCtx.failPhase(PhasePreconditions, precond.Name, "failed to parse API response", err)
		}

		// Store full response under precondition name for condition digging
		// e.g., conditions can access "check-cluster.status.conditions"
		execCtx.Params[precond.Name] = responseData

		// Capture fields from response
		if len(precond.Capture) > 0 {
			slog.DebugContext(ctx, "capturing fields from api response", "field_count", len(precond.Capture))

			// Create evaluator with response data only.
			// Both field (JSONPath) and expression (CEL) work on the same source.
			captureCtx := criteria.NewEvaluationContext()
			captureCtx.SetVariablesFromMap(responseData)
			// Option 1: also expose the full response as a named map variable so capture
			// expressions can safely navigate optional fields without an "undeclared reference"
			// error, e.g.: dig(checkClusterState, "deleted_time") != null
			//              has(checkClusterState.deleted_time)
			captureCtx.Set(precond.Name, responseData)

			captureEvaluator, evalErr := criteria.NewEvaluator(ctx, captureCtx)
			if evalErr != nil {
				slog.WarnContext(ctx, "failed to create capture evaluator", "error", evalErr)
				result.Status = StatusFailed
				result.Error = fmt.Errorf("failed to create capture evaluator: %w", evalErr)
				return result, execCtx.failPhase(PhasePreconditions, precond.Name, "failed to create capture evaluator", evalErr)
			}

			for _, capture := range precond.Capture {
				extractResult, err := captureEvaluator.ExtractValue(capture.Field, capture.Expression)
				if err != nil {
					return result, err
				}

				value := extractResult.Value

				// Option 2: default handling for field: captures only.
				// When a field is absent from the response (extractResult.Error != nil) and
				// a Default is configured, use it silently. Without a Default, log a WARN.
				// Expression captures are unaffected — their errors surface as-is.
				if capture.Field != "" && extractResult.Error != nil {
					if capture.Default != nil {
						slog.DebugContext(ctx, "field absent from response, using default",
							"field", capture.Name)
						value = capture.Default
					} else {
						slog.WarnContext(ctx, "failed to capture field", "field", capture.Name, "error", extractResult.Error)
					}
				}

				result.CapturedFields[capture.Name] = value
				execCtx.Params[capture.Name] = value
				slog.DebugContext(ctx, "captured field",
					"field", capture.Name, "source", extractResult.Source)
			}
		}
	}

	// Step 3: Evaluate conditions
	// Create evaluation context with all CEL variables (params, adapter, resources)
	// Note: resources will be empty during preconditions since they haven't been created yet
	evalCtx := criteria.NewEvaluationContext()
	evalCtx.SetVariablesFromMap(execCtx.GetCELVariables())

	evaluator, err := criteria.NewEvaluator(ctx, evalCtx)
	if err != nil {
		result.Status = StatusFailed
		result.Error = err
		return result, NewExecutorError(PhasePreconditions, precond.Name, "failed to create evaluator", err)
	}

	// Evaluate using structured conditions or CEL expression
	switch {
	case len(precond.Conditions) > 0:
		slog.DebugContext(ctx, "evaluating structured conditions", "condition_count", len(precond.Conditions))
		condDefs := ToConditionDefs(precond.Conditions)

		condResult, err := evaluator.EvaluateConditions(condDefs)
		if err != nil {
			result.Status = StatusFailed
			result.Error = err
			return result, NewExecutorError(PhasePreconditions, precond.Name, "condition evaluation failed", err)
		}

		result.Matched = condResult.Matched
		result.ConditionResults = condResult.Results

		// Log individual condition results
		for _, cr := range condResult.Results {
			slog.DebugContext(ctx, "condition evaluated",
				"field", cr.Field, "operator", cr.Operator, "matched", cr.Matched)
		}

		// Record evaluation in execution context - reuse criteria.EvaluationResult directly
		fieldResults := make(map[string]criteria.EvaluationResult, len(condResult.Results))
		for _, cr := range condResult.Results {
			fieldResults[cr.Field] = cr
		}
		execCtx.AddConditionsEvaluation(PhasePreconditions, precond.Name, condResult.Matched, fieldResults)
	case precond.Expression != "":
		// Evaluate CEL expression
		slog.DebugContext(ctx, "evaluating cel expression", "precondition", precond.Name)
		celResult, err := evaluator.EvaluateCEL(strings.TrimSpace(precond.Expression))
		if err != nil {
			result.Status = StatusFailed
			result.Error = err
			return result, NewExecutorError(PhasePreconditions, precond.Name, "CEL expression evaluation failed", err)
		}

		result.Matched = celResult.Matched
		result.CELResult = celResult
		slog.DebugContext(ctx, "cel result", "matched", celResult.Matched)

		// Record CEL evaluation in execution context
		execCtx.AddCELEvaluation(PhasePreconditions, precond.Name, precond.Expression, celResult.Matched)
	default:
		// No conditions specified - consider it matched
		slog.DebugContext(ctx, "no conditions specified, auto-matched")
		result.Matched = true
	}

	return result, nil
}

// executeAPICall executes an API call and returns the response body for field capture
func (pe *PreconditionExecutor) executeAPICall(
	ctx context.Context,
	apiCall *configloader.APICall,
	execCtx *ExecutionContext,
) ([]byte, error) {
	resp, url, err := ExecuteAPICall(ctx, apiCall, execCtx, pe.apiClient)

	// Validate response - returns APIError with full metadata if validation fails
	if validationErr := ValidateAPIResponse(resp, err, apiCall.Method, url); validationErr != nil {
		return nil, validationErr
	}

	return resp.Body, nil
}

// formatConditionDetails formats condition evaluation details for error messages
func formatConditionDetails(result PreconditionResult) string {
	var details []string

	if result.CELResult != nil && result.CELResult.HasError() {
		details = append(details, fmt.Sprintf("CEL error: %v", result.CELResult.Error))
	}

	for _, condResult := range result.ConditionResults {
		if !condResult.Matched {
			details = append(details, fmt.Sprintf("%s %s %v (actual: %v)",
				condResult.Field, condResult.Operator, condResult.ExpectedValue, condResult.FieldValue))
		}
	}

	if len(details) == 0 {
		return "no specific details available"
	}

	return strings.Join(details, "; ")
}
