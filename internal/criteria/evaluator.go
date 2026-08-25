package criteria

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
)

// EvaluationResult contains the result of evaluating a condition
type EvaluationResult struct {
	// FieldValue is the actual value of the field that was evaluated
	FieldValue interface{}
	// ExpectedValue is the value the condition was compared against
	ExpectedValue interface{}
	// Field is the field path that was evaluated
	Field string
	// Operator is the operator used
	Operator Operator
	// Matched indicates if the condition was satisfied
	Matched bool
}

// ConditionsResult contains the result of evaluating multiple conditions
type ConditionsResult struct {
	// ExtractedFields maps field paths to their values
	ExtractedFields map[string]interface{}
	// Results contains individual results for each condition
	Results []EvaluationResult
	// FailedCondition is the index of the first failed condition (-1 if all passed)
	FailedCondition int
	// Matched indicates if all conditions were satisfied
	Matched bool
}

// Evaluator evaluates criteria against an evaluation context
type Evaluator struct {
	evalCtx *EvaluationContext
	ctx     context.Context

	// Lazily cached CEL evaluator for repeated CEL evaluations
	// Recreated when context version changes
	celEval        *CELEvaluator
	celEvalVersion int64 // Track which context version the CEL eval was created with
	mu             sync.Mutex
}

// NewEvaluator creates a new criteria evaluator.
// All parameters are required - ctx for logging correlation, evalCtx for CEL data.
// Returns an error if any required parameter is nil.
func NewEvaluator(ctx context.Context, evalCtx *EvaluationContext) (*Evaluator, error) {
	if ctx == nil {
		return nil, fmt.Errorf("ctx is required for Evaluator")
	}
	if evalCtx == nil {
		return nil, fmt.Errorf("evalCtx is required for Evaluator")
	}
	return &Evaluator{
		evalCtx: evalCtx,
		ctx:     ctx,
	}, nil
}

// getCELEvaluator returns a cached CEL evaluator, creating it lazily on first use.
// If the context has been modified (version changed), the CEL evaluator is recreated
// to ensure the CEL environment stays in sync with the context data.
// This prevents "undeclared reference" errors when variables are added after first evaluation.
func (e *Evaluator) getCELEvaluator() (*CELEvaluator, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	currentVersion := e.evalCtx.Version()

	// Recreate CEL evaluator if context changed or not yet created
	if e.celEval == nil || e.celEvalVersion != currentVersion {
		celEval, err := newCELEvaluator(e.evalCtx)
		if err != nil {
			return nil, err
		}
		e.celEval = celEval
		e.celEvalVersion = currentVersion
	}

	return e.celEval, nil
}

// EvaluateField extracts a field from the evaluation context using JSONPath.
//
// Supports both simple dot notation and full JSONPath syntax:
//   - Simple: "metadata.name", "status.phase"
//   - JSONPath: "{.items[*].metadata.name}"
//   - JSONPath with filter: "{.items[?(@.adapter=='landing-zone-adapter')].data.namespace.status}"
//
// Returns *FieldResult containing the extracted value or error.
// This is the field extraction counterpart to EvaluateCEL.
func (e *Evaluator) EvaluateField(field string) (*FieldResult, error) {
	return ExtractField(e.evalCtx.Data(), field)
}

// EvaluateCondition evaluates a single condition and returns detailed result
func (e *Evaluator) EvaluateCondition(field string, operator Operator, value interface{}) (*EvaluationResult, error) {
	// Get the field value from context
	fieldResult, err := e.evalCtx.GetField(field)
	if err != nil {
		return nil, err
	}

	result := &EvaluationResult{
		Field:         field,
		FieldValue:    fieldResult.Value,
		Operator:      operator,
		ExpectedValue: value,
	}

	// Evaluate based on operator
	var matched bool
	if operator == OperatorExists {
		// Exists is special - only checks fieldValue, no expected value
		matched = evaluateExists(fieldResult.Value)
	} else if evalFn, ok := operatorFuncs[operator]; ok {
		matched, err = evalFn(fieldResult.Value, value)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, &EvaluationError{
			Field:   field,
			Message: fmt.Sprintf("unsupported operator: %s", operator),
		}
	}

	result.Matched = matched
	return result, nil
}

// EvaluateConditions evaluates multiple conditions and returns detailed results
func (e *Evaluator) EvaluateConditions(conditions []ConditionDef) (*ConditionsResult, error) {
	result := &ConditionsResult{
		Matched:         true,
		Results:         make([]EvaluationResult, 0, len(conditions)),
		FailedCondition: -1,
		ExtractedFields: make(map[string]interface{}),
	}

	for i, cond := range conditions {
		evalResult, err := e.EvaluateCondition(cond.Field, cond.Operator, cond.Value)
		if err != nil {
			return nil, err
		}

		result.Results = append(result.Results, *evalResult)
		result.ExtractedFields[cond.Field] = evalResult.FieldValue

		if !evalResult.Matched && result.Matched {
			result.Matched = false
			result.FailedCondition = i
		}
	}

	return result, nil
}

// ExtractValueResult contains the result of value extraction
type ExtractValueResult struct {
	Value  interface{} // Extracted value
	Error  error       // Error if extraction failed
	Source string      // The field path or expression used
}

// ExtractValue extracts a value from the context using either field (JSONPath) or expression (CEL).
// Only one of field or expression should be non-empty.
//
// This provides a unified extraction interface for captures, conditions, and payload building.
//
//   - field: Uses JSONPath/dot notation to extract from context data
//   - expression: Uses CEL expression to compute/extract value
//
// Example:
//
//	result := evaluator.ExtractValue("status.phase", "")           // JSONPath
//	result := evaluator.ExtractValue("", "items.size() > 0")       // CEL
//	result := evaluator.ExtractValue("{.items[0].name}", "")       // Full JSONPath
func (e *Evaluator) ExtractValue(field, expression string) (*ExtractValueResult, error) {
	result := &ExtractValueResult{}
	field = strings.TrimSpace(field)
	expression = strings.TrimSpace(expression)
	// Enforce mutual exclusivity
	if field != "" && expression != "" {
		return result, fmt.Errorf("field and expression are mutually exclusive; only one should be specified")
	}
	if expression != "" {
		// CEL expression mode
		celResult, err := e.EvaluateCEL(expression)
		if err != nil {
			return result, fmt.Errorf("CEL evaluation failed: %w", err)
		}
		if celResult.Error != nil {
			// Caller should handle logging based on CELResult.Error
			// This is NOT a parse error, so we don't return error - caller can use default
			// Only caught field missing or empty value as warn log
			slog.WarnContext(e.ctx, "cel evaluation failed", "expression", expression, "error", celResult.Error)
		}
		result.Value = celResult.Value
		result.Source = expression
		return result, nil
	} else if field != "" {
		// Field extraction mode (JSONPath)
		fieldResult, err := e.EvaluateField(field)
		if err != nil {
			// Parse error - fail fast (indicates bug in config)
			return result, fmt.Errorf("field extraction failed: %w", err)
		}
		// Propagate the extraction error to the caller without logging here.
		// fieldResult.Error means the field was absent from the response — the caller
		// decides whether to warn or fall back to a configured default value.
		result.Error = fieldResult.Error
		result.Value = fieldResult.Value
		result.Source = field
		return result, nil
	}

	return result, fmt.Errorf("neither field nor expression defined")
}

// withCELEvaluator gets the CEL evaluator and applies a function to it
func withCELEvaluator[T any](e *Evaluator, fn func(*CELEvaluator) (T, error)) (T, error) {
	var zero T
	celEval, err := e.getCELEvaluator()
	if err != nil {
		return zero, err
	}
	return fn(celEval)
}

// EvaluateCEL evaluates a CEL expression against the current context
func (e *Evaluator) EvaluateCEL(expression string) (*CELResult, error) {
	return withCELEvaluator(e, func(c *CELEvaluator) (*CELResult, error) {
		return c.EvaluateSafe(expression)
	})
}

// ConditionDef defines a condition to evaluate
type ConditionDef struct {
	Value    interface{}
	Field    string
	Operator Operator
}

// ConditionDefJSON is used for JSON/YAML unmarshaling with string operator
type ConditionDefJSON struct {
	Value    interface{} `json:"value" yaml:"value"`
	Field    string      `json:"field" yaml:"field"`
	Operator string      `json:"operator" yaml:"operator"`
}

// ToConditionDef converts ConditionDefJSON to ConditionDef with typed Operator
func (c ConditionDefJSON) ToConditionDef() ConditionDef {
	return ConditionDef{
		Field:    c.Field,
		Operator: Operator(c.Operator),
		Value:    c.Value,
	}
}

// evalFunc is a function type for operator evaluation
type evalFunc func(fieldValue, expected interface{}) (bool, error)

// operatorFuncs maps operators to their evaluation functions
var operatorFuncs = map[Operator]evalFunc{
	OperatorEquals:      evaluateEquals,
	OperatorNotEquals:   negate(evaluateEquals),
	OperatorIn:          evaluateIn,
	OperatorNotIn:       negate(evaluateIn),
	OperatorContains:    evaluateContains,
	OperatorGreaterThan: evaluateGreaterThan,
	OperatorLessThan:    evaluateLessThan,
}

// negate wraps an evalFunc to return the opposite result
func negate(fn evalFunc) evalFunc {
	return func(a, b interface{}) (bool, error) {
		result, err := fn(a, b)
		if err != nil {
			return false, err
		}
		return !result, nil
	}
}

// evaluateEquals checks if two values are equal
func evaluateEquals(fieldValue, expectedValue interface{}) (bool, error) {
	// Handle nil cases
	if fieldValue == nil && expectedValue == nil {
		return true, nil
	}
	if fieldValue == nil || expectedValue == nil {
		return false, nil
	}

	// Use reflect.DeepEqual for complex types
	return reflect.DeepEqual(fieldValue, expectedValue), nil
}

// evaluateIn checks if a value is in a list
func evaluateIn(fieldValue, expectedList interface{}) (bool, error) {
	// Guard against nil expectedList
	if expectedList == nil {
		return false, fmt.Errorf("expected list cannot be nil")
	}

	list := reflect.ValueOf(expectedList)

	// Guard against invalid reflect.Value (e.g., from nil interface)
	if !list.IsValid() {
		return false, fmt.Errorf("expected list is invalid")
	}

	// expectedList should be a slice or array
	if list.Kind() != reflect.Slice && list.Kind() != reflect.Array {
		return false, fmt.Errorf("expected list to be a slice or array, got %s", list.Kind())
	}

	// Check if fieldValue is in the list
	for i := 0; i < list.Len(); i++ {
		item := list.Index(i).Interface()
		if reflect.DeepEqual(fieldValue, item) {
			return true, nil
		}
	}

	return false, nil
}

// evaluateContains checks if a value contains another value
func evaluateContains(fieldValue, needle interface{}) (bool, error) {
	// Guard against nil fieldValue
	if fieldValue == nil {
		return false, fmt.Errorf("fieldValue cannot be nil")
	}

	// Guard against nil needle
	if needle == nil {
		return false, fmt.Errorf("needle cannot be nil")
	}

	// For strings
	if str, ok := fieldValue.(string); ok {
		needleStr, ok := needle.(string)
		if !ok {
			return false, fmt.Errorf("needle must be a string when field is a string")
		}
		return strings.Contains(str, needleStr), nil
	}

	value := reflect.ValueOf(fieldValue)

	// Guard against invalid reflect.Value
	if !value.IsValid() {
		return false, fmt.Errorf("fieldValue is invalid")
	}

	// For maps - check if needle is a key in the map
	if value.Kind() == reflect.Map {
		needleVal := reflect.ValueOf(needle)

		// Guard against invalid needle value
		if !needleVal.IsValid() {
			return false, fmt.Errorf("needle is invalid for map key lookup")
		}

		// Check if needle type is compatible with map key type
		if needleVal.Type().AssignableTo(value.Type().Key()) {
			result := value.MapIndex(needleVal)
			return result.IsValid(), nil
		}
		// Try string conversion for interface{} keyed maps (common in YAML/JSON)
		if value.Type().Key().Kind() == reflect.Interface {
			result := value.MapIndex(needleVal)
			return result.IsValid(), nil
		}
		return false, fmt.Errorf("needle type %T not compatible with map key type %v", needle, value.Type().Key())
	}

	// For slices/arrays
	if value.Kind() == reflect.Slice || value.Kind() == reflect.Array {
		for i := 0; i < value.Len(); i++ {
			itemVal := value.Index(i)
			// Guard against invalid item value before calling Interface()
			if !itemVal.IsValid() {
				continue
			}
			item := itemVal.Interface()
			if reflect.DeepEqual(item, needle) {
				return true, nil
			}
		}
		return false, nil
	}

	return false, fmt.Errorf("contains operator requires string, slice, array, or map field type")
}

// evaluateGreaterThan checks if a value is greater than another
func evaluateGreaterThan(fieldValue, threshold interface{}) (bool, error) {
	return compareNumbers(fieldValue, threshold, func(a, b float64) bool {
		return a > b
	})
}

// evaluateLessThan checks if a value is less than another
func evaluateLessThan(fieldValue, threshold interface{}) (bool, error) {
	return compareNumbers(fieldValue, threshold, func(a, b float64) bool {
		return a < b
	})
}

// evaluateExists checks if a value exists (is not nil or empty)
func evaluateExists(fieldValue interface{}) bool {
	if fieldValue == nil {
		return false
	}

	// Check for empty string
	if str, ok := fieldValue.(string); ok {
		return str != ""
	}

	// Check for zero values
	value := reflect.ValueOf(fieldValue)
	switch value.Kind() { //nolint:exhaustive // only checking collection and pointer types
	case reflect.Slice, reflect.Map, reflect.Array:
		return value.Len() > 0
	case reflect.Ptr, reflect.Interface:
		return !value.IsNil()
	}

	return true
}

// compareNumbers compares two numeric values using the provided comparison function
func compareNumbers(a, b interface{}, compare func(float64, float64) bool) (bool, error) {
	aFloat, err := toFloat64(a)
	if err != nil {
		return false, fmt.Errorf("failed to convert field value to number: %w", err)
	}

	bFloat, err := toFloat64(b)
	if err != nil {
		return false, fmt.Errorf("failed to convert comparison value to number: %w", err)
	}

	return compare(aFloat, bFloat), nil
}

// toFloat64 converts various numeric types to float64
func toFloat64(value interface{}) (float64, error) {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		return v.Float(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(v.Uint()), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", value)
	}
}
