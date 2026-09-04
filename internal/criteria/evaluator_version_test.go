package criteria

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContextVersionTracking verifies that the CEL evaluator is recreated
// when the context is modified after the first evaluation
func TestContextVersionTracking(t *testing.T) {
	ctx := NewEvaluationContext()
	ctx.Set("status", "Ready")

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	// First CEL evaluation - creates CEL env with only "status"
	result1, err1 := evaluator.EvaluateCEL("status == 'Ready'")
	require.NoError(t, err1)
	require.NotNil(t, result1)
	assert.True(t, result1.Matched)
	assert.False(t, result1.HasError())

	// Add new variable AFTER first evaluation
	ctx.Set("replicas", 3)

	// Second CEL evaluation - CEL env should be recreated with "replicas"
	// This would fail with "undeclared reference to 'replicas'" before the fix
	result2, err2 := evaluator.EvaluateCEL("replicas > 0")
	require.NoError(t, err2)
	require.NotNil(t, result2)
	assert.True(t, result2.Matched)
	assert.False(t, result2.HasError(), "Should recreate CEL env and recognize new variable")

	// Verify both old and new variables work
	result3, err3 := evaluator.EvaluateCEL("status == 'Ready' && replicas == 3")
	require.NoError(t, err3)
	require.NotNil(t, result3)
	assert.True(t, result3.Matched)
	assert.False(t, result3.HasError())
}

// TestSetVariablesFromMapVersionTracking verifies version tracking with SetVariablesFromMap
func TestSetVariablesFromMapVersionTracking(t *testing.T) {
	ctx := NewEvaluationContext()
	ctx.SetVariablesFromMap(map[string]interface{}{
		"env": "production",
	})

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	// First evaluation
	result1, err1 := evaluator.EvaluateCEL("env == 'production'")
	require.NoError(t, err1)
	require.NotNil(t, result1)
	assert.True(t, result1.Matched)

	// Add more variables using SetVariablesFromMap
	ctx.SetVariablesFromMap(map[string]interface{}{
		"region":   "us-west-2",
		"replicas": 5,
	})

	// Should be able to use new variables
	result2, err2 := evaluator.EvaluateCEL("region == 'us-west-2' && replicas == 5")
	require.NoError(t, err2)
	require.NotNil(t, result2)
	assert.True(t, result2.Matched)
	assert.False(t, result2.HasError(), "Should recognize variables added via SetVariablesFromMap")
}

// TestMergeVersionTracking verifies version tracking with Merge
func TestMergeVersionTracking(t *testing.T) {
	ctx1 := NewEvaluationContext()
	ctx1.Set("a", 1)

	evaluator, err := NewEvaluator(context.Background(), ctx1)
	require.NoError(t, err)

	// First evaluation
	result1, err1 := evaluator.EvaluateCEL("a == 1")
	require.NoError(t, err1)
	require.NotNil(t, result1)
	assert.True(t, result1.Matched)

	// Merge another context
	ctx2 := NewEvaluationContext()
	ctx2.Set("b", 2)
	ctx2.Set("c", 3)
	ctx1.Merge(ctx2)

	// Should be able to use merged variables
	result2, err2 := evaluator.EvaluateCEL("a == 1 && b == 2 && c == 3")
	require.NoError(t, err2)
	require.NotNil(t, result2)
	assert.True(t, result2.Matched)
	assert.False(t, result2.HasError(), "Should recognize variables from merged context")
}

// TestVersionIncrements verifies that version increments correctly
func TestVersionIncrements(t *testing.T) {
	ctx := NewEvaluationContext()
	assert.Equal(t, int64(0), ctx.Version(), "Initial version should be 0")

	ctx.Set("a", 1)
	assert.Equal(t, int64(1), ctx.Version(), "Version should increment after Set with new key")

	// Same value should NOT increment version
	ctx.Set("a", 1)
	assert.Equal(t, int64(1), ctx.Version(), "Version should NOT increment when setting same value")

	// Different value SHOULD increment version
	ctx.Set("a", 100)
	assert.Equal(t, int64(2), ctx.Version(), "Version should increment when value changes")

	ctx.SetVariablesFromMap(map[string]interface{}{"b": 2, "c": 3})
	assert.Equal(t, int64(3), ctx.Version(), "Version should increment after SetVariablesFromMap with new keys")

	// Same values should NOT increment
	ctx.SetVariablesFromMap(map[string]interface{}{"b": 2, "c": 3})
	assert.Equal(t, int64(3), ctx.Version(), "Version should NOT increment when SetVariablesFromMap has same values")

	ctx2 := NewEvaluationContext()
	ctx2.Set("d", 4)
	ctx.Merge(ctx2)
	assert.Equal(t, int64(4), ctx.Version(), "Version should increment after Merge with new data")

	// Merge with same data should NOT increment
	ctx3 := NewEvaluationContext()
	ctx3.Set("d", 4)
	ctx.Merge(ctx3)
	assert.Equal(t, int64(4), ctx.Version(), "Version should NOT increment after Merge with same data")

	// New keys should still increment
	ctx.Set("e", 5)
	ctx.Set("f", 6)
	assert.Equal(t, int64(6), ctx.Version(), "Version should increment for each new Set")
}

// TestNoVersionChangeNoRecreate verifies CEL evaluator is not recreated unnecessarily
func TestNoVersionChangeNoRecreate(t *testing.T) {
	ctx := NewEvaluationContext()
	ctx.Set("status", "Ready")

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	// First evaluation
	result1, err1 := evaluator.EvaluateCEL("status == 'Ready'")
	require.NoError(t, err1)
	require.NotNil(t, result1)
	assert.True(t, result1.Matched)

	// Get the CEL evaluator pointer
	celEval1, err := evaluator.getCELEvaluator()
	require.NoError(t, err)

	// Second evaluation WITHOUT context change
	result2, err2 := evaluator.EvaluateCEL("status == 'Ready'")
	require.NoError(t, err2)
	require.NotNil(t, result2)
	assert.True(t, result2.Matched)

	// Get the CEL evaluator pointer again
	celEval2, err := evaluator.getCELEvaluator()
	require.NoError(t, err)

	// Should be the same instance (not recreated)
	assert.Same(t, celEval1, celEval2, "CEL evaluator should not be recreated when context unchanged")

	// Now modify context
	ctx.Set("replicas", 3)

	// Get the CEL evaluator pointer after context change
	celEval3, err := evaluator.getCELEvaluator()
	require.NoError(t, err)

	// Should be a different instance (recreated)
	assert.NotSame(t, celEval1, celEval3, "CEL evaluator should be recreated when context changes")
}
