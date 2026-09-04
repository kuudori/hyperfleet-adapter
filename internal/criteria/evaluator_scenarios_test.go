package criteria

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRealWorldScenario tests a realistic scenario similar to the adapter config template
func TestRealWorldScenario(t *testing.T) {
	// Simulate cluster details from an API response
	ctx := NewEvaluationContext()

	// Set up cluster details (as would be returned from HyperFleet API)
	clusterDetails := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name": "prod-cluster-01",
		},
		"status": map[string]interface{}{
			"phase": "Ready",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   "Available",
					"status": "True",
				},
			},
		},
		"spec": map[string]interface{}{
			"provider":   "aws",
			"region":     "us-east-1",
			"vpc_id":     "vpc-12345",
			"node_count": 5,
		},
	}

	ctx.Set("clusterDetails", clusterDetails)

	// Extract fields (as done in precondition.extract)
	ctx.Set("clusterPhase", "Ready")
	ctx.Set("cloudProvider", "aws")
	ctx.Set("vpcId", "vpc-12345")
	ctx.Set("nodeCount", 5)

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	// Test precondition conditions from the template
	t.Run("clusterPhase in valid phases", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"clusterPhase",
			OperatorIn,
			[]interface{}{"Provisioning", "Installing", "Ready"},
		)
		require.NoError(t, err)
		assert.True(t, result.Matched, "cluster phase should be in valid phases")
	})

	t.Run("cloudProvider in allowed providers", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"cloudProvider",
			OperatorIn,
			[]interface{}{"aws", "gcp", "azure"},
		)
		require.NoError(t, err)
		assert.True(t, result.Matched, "cloud provider should be in allowed providers")
	})

	t.Run("vpcId exists", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"vpcId",
			OperatorExists,
			nil,
		)
		require.NoError(t, err)
		assert.True(t, result.Matched, "vpcId should exist")
	})

	t.Run("evaluate all preconditions together", func(t *testing.T) {
		conditions := []ConditionDef{
			{Field: "clusterPhase", Operator: OperatorIn, Value: []interface{}{"Provisioning", "Installing", "Ready"}},
			{Field: "cloudProvider", Operator: OperatorIn, Value: []interface{}{"aws", "gcp", "azure"}},
			{Field: "vpcId", Operator: OperatorExists, Value: nil},
		}

		result, err := evaluator.EvaluateConditions(conditions)
		require.NoError(t, err)
		assert.True(t, result.Matched, "all preconditions should pass")
	})
}

// TestResourceStatusEvaluation tests evaluating resource status conditions
func TestResourceStatusEvaluation(t *testing.T) {
	ctx := NewEvaluationContext()

	// Simulate tracked resources after creation
	resources := map[string]interface{}{
		"clusterNamespace": map[string]interface{}{
			"status": map[string]interface{}{
				"phase": "Active",
			},
		},
		"clusterController": map[string]interface{}{
			"status": map[string]interface{}{
				"replicas":      3,
				"readyReplicas": 3,
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Available",
						"status": "True",
						"reason": "DeploymentAvailable",
					},
				},
			},
		},
	}

	ctx.Set("resources", resources)

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	t.Run("namespace is active", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"resources.clusterNamespace.status.phase",
			OperatorEquals,
			"Active",
		)
		require.NoError(t, err)
		assert.True(t, result.Matched)
	})

	t.Run("replicas equal ready replicas", func(t *testing.T) {
		// Create isolated context for this subtest to avoid shared state mutation
		localCtx := NewEvaluationContext()
		localCtx.Set("replicas", 3)
		localCtx.Set("readyReplicas", 3)
		localEvaluator, err := NewEvaluator(context.Background(), localCtx)
		require.NoError(t, err)
		result, err := localEvaluator.EvaluateCondition(
			"replicas",
			OperatorEquals,
			3,
		)
		require.NoError(t, err)
		assert.True(t, result.Matched)

		result, err = localEvaluator.EvaluateCondition(
			"readyReplicas",
			OperatorGreaterThan,
			0,
		)
		require.NoError(t, err)
		assert.True(t, result.Matched)
	})
}

// TestComplexNestedConditions tests complex nested field evaluation
func TestComplexNestedConditions(t *testing.T) {
	ctx := NewEvaluationContext()

	// Simulate complex nested data
	ctx.Set("adapter", map[string]interface{}{
		"executionStatus": "success",
		"resources": map[string]interface{}{
			"created": []string{"namespace", "configmap", "secret"},
			"failed":  []string{},
		},
	})

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	t.Run("adapter execution successful", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"adapter.executionStatus",
			OperatorEquals,
			"success",
		)
		require.NoError(t, err)
		assert.True(t, result.Matched)
	})

	t.Run("resources were created", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"adapter.resources.created",
			OperatorContains,
			"namespace",
		)
		require.NoError(t, err)
		assert.True(t, result.Matched)
	})
}

// TestMapKeyContainment tests the contains operator with map types
func TestMapKeyContainment(t *testing.T) {
	ctx := NewEvaluationContext()

	// Set up a map with labels (common Kubernetes pattern)
	ctx.Set("labels", map[string]interface{}{
		"app":         "myapp",
		"environment": "production",
		"team":        "platform",
	})

	// Also test with map[string]string (typed map)
	ctx.Set("annotations", map[string]string{
		"description": "My application",
		"owner":       "team-a",
	})

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	t.Run("map contains key - found", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"labels",
			OperatorContains,
			"app",
		)
		require.NoError(t, err)
		assert.True(t, result.Matched, "map should contain key 'app'")
	})

	t.Run("map contains key - not found", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"labels",
			OperatorContains,
			"nonexistent",
		)
		require.NoError(t, err)
		assert.False(t, result.Matched, "map should not contain key 'nonexistent'")
	})

	t.Run("typed map contains key", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition(
			"annotations",
			OperatorContains,
			"owner",
		)
		require.NoError(t, err)
		assert.True(t, result.Matched, "typed map should contain key 'owner'")
	})

	t.Run("check label exists pattern", func(t *testing.T) {
		// Common pattern: check if a required label key exists
		result, err := evaluator.EvaluateCondition(
			"labels",
			OperatorContains,
			"environment",
		)
		require.NoError(t, err)
		assert.True(t, result.Matched, "labels should contain 'environment' key")
	})
}

// TestContainsOperatorEdgeCases tests edge cases for the contains operator
func TestContainsOperatorEdgeCases(t *testing.T) {
	ctx := NewEvaluationContext()

	// String field
	ctx.Set("message", "hello world")

	// Slice field
	ctx.Set("tags", []interface{}{"alpha", "beta", "gamma"})

	// Integer slice
	ctx.Set("numbers", []int{1, 2, 3, 4, 5})

	// Unsupported type (int)
	ctx.Set("count", 42)

	// Nil value
	ctx.Set("nilField", nil)

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	t.Run("string contains substring - found", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition("message", OperatorContains, "world")
		require.NoError(t, err)
		assert.True(t, result.Matched)
	})

	t.Run("string contains substring - not found", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition("message", OperatorContains, "foo")
		require.NoError(t, err)
		assert.False(t, result.Matched)
	})

	t.Run("string with non-string needle - error", func(t *testing.T) {
		_, err := evaluator.EvaluateCondition("message", OperatorContains, 123)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "needle must be a string")
	})

	t.Run("slice contains element - found", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition("tags", OperatorContains, "beta")
		require.NoError(t, err)
		assert.True(t, result.Matched)
	})

	t.Run("slice contains element - not found", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition("tags", OperatorContains, "delta")
		require.NoError(t, err)
		assert.False(t, result.Matched)
	})

	t.Run("int slice contains element", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition("numbers", OperatorContains, 3)
		require.NoError(t, err)
		assert.True(t, result.Matched)
	})

	t.Run("int slice does not contain element", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition("numbers", OperatorContains, 99)
		require.NoError(t, err)
		assert.False(t, result.Matched)
	})

	t.Run("unsupported field type - error", func(t *testing.T) {
		_, err := evaluator.EvaluateCondition("count", OperatorContains, 1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "contains operator requires string, slice, array, or map")
	})

	t.Run("nil field value - returns not matched", func(t *testing.T) {
		// When field is nil, EvaluateCondition continues with nil value
		// evaluateContains should return error for nil fieldValue
		result, err := evaluator.EvaluateCondition("nilField", OperatorContains, "test")
		// The condition evaluation handles nil - it gets the value as nil
		// The contains operator returns error for nil
		require.Error(t, err)
		assert.Nil(t, result)
	})

	t.Run("missing field - returns not matched", func(t *testing.T) {
		result, err := evaluator.EvaluateCondition("missingField", OperatorContains, "test")
		// Missing field returns nil value, contains with nil returns error
		require.Error(t, err)
		assert.Nil(t, result)
	})
}

// TestTerminatingClusterScenario tests a scenario where cluster is terminating
func TestTerminatingClusterScenario(t *testing.T) {
	ctx := NewEvaluationContext()
	ctx.Set("clusterPhase", "Terminating")
	ctx.Set("cloudProvider", "aws")
	ctx.Set("vpcId", "vpc-12345")

	evaluator, err := NewEvaluator(context.Background(), ctx)
	require.NoError(t, err)

	t.Run("terminating cluster fails preconditions", func(t *testing.T) {
		// Cluster in "Terminating" phase should NOT be in allowed phases
		result, err := evaluator.EvaluateCondition(
			"clusterPhase",
			OperatorIn,
			[]interface{}{"Provisioning", "Installing", "Ready"},
		)
		require.NoError(t, err)
		assert.False(t, result.Matched, "terminating cluster should not pass preconditions")
	})

	t.Run("notIn blocks terminating cluster", func(t *testing.T) {
		// Precondition: clusterPhase notIn ["Terminating", "Failed"]
		// Since clusterPhase IS "Terminating", this should return false (blocked)
		result, err := evaluator.EvaluateCondition(
			"clusterPhase",
			OperatorNotIn,
			[]interface{}{"Terminating", "Failed"},
		)
		require.NoError(t, err)
		assert.False(t, result.Matched, "terminating cluster should be blocked by notIn precondition")
	})
}

// TestNodeCountValidation tests node count validation scenarios
func TestNodeCountValidation(t *testing.T) {
	tests := []struct {
		name      string
		nodeCount int
		minNodes  int
		maxNodes  int
		valid     bool
	}{
		{
			name:      "valid node count",
			nodeCount: 5,
			minNodes:  1,
			maxNodes:  10,
			valid:     true,
		},
		{
			name:      "node count below minimum",
			nodeCount: 0,
			minNodes:  1,
			maxNodes:  10,
			valid:     false,
		},
		{
			name:      "node count above maximum",
			nodeCount: 15,
			minNodes:  1,
			maxNodes:  10,
			valid:     false,
		},
		{
			name:      "node count at minimum",
			nodeCount: 1,
			minNodes:  1,
			maxNodes:  10,
			valid:     true,
		},
		{
			name:      "node count at maximum",
			nodeCount: 10,
			minNodes:  1,
			maxNodes:  10,
			valid:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create isolated context and evaluator per subtest for parallel safety
			ctx := NewEvaluationContext()
			ctx.Set("nodeCount", tt.nodeCount)
			ctx.Set("minNodes", tt.minNodes)
			ctx.Set("maxNodes", tt.maxNodes)

			evaluator, err := NewEvaluator(context.Background(), ctx)
			require.NoError(t, err)

			// Check if nodeCount >= minNodes
			result1, err := evaluator.EvaluateCondition(
				"nodeCount",
				OperatorGreaterThan,
				tt.minNodes-1,
			)
			require.NoError(t, err)

			// Check if nodeCount <= maxNodes
			result2, err := evaluator.EvaluateCondition(
				"nodeCount",
				OperatorLessThan,
				tt.maxNodes+1,
			)
			require.NoError(t, err)

			if tt.valid {
				assert.True(t, result1.Matched && result2.Matched, "node count should be within valid range")
			} else {
				assert.False(t, result1.Matched && result2.Matched, "node count should be outside valid range")
			}
		})
	}
}
