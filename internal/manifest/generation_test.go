package manifest

import (
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/constants"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCompareGenerations(t *testing.T) {
	tests := []struct {
		name              string
		expectedReason    string
		expectedOperation Operation
		newGen            int64
		existingGen       int64
		exists            bool
	}{
		{
			name:              "resource does not exist - create",
			newGen:            5,
			existingGen:       0,
			exists:            false,
			expectedOperation: OperationCreate,
			expectedReason:    "resource not found",
		},
		{
			name:              "generations match - skip",
			newGen:            5,
			existingGen:       5,
			exists:            true,
			expectedOperation: OperationSkip,
			expectedReason:    "generation 5 unchanged",
		},
		{
			name:              "newer generation - update",
			newGen:            6,
			existingGen:       5,
			exists:            true,
			expectedOperation: OperationUpdate,
			expectedReason:    "generation changed 5->6",
		},
		{
			name:              "older generation (rollback) - update",
			newGen:            4,
			existingGen:       5,
			exists:            true,
			expectedOperation: OperationUpdate,
			expectedReason:    "generation changed 5->4",
		},
		{
			name:              "large generation difference - update",
			newGen:            100,
			existingGen:       1,
			exists:            true,
			expectedOperation: OperationUpdate,
			expectedReason:    "generation changed 1->100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CompareGenerations(tt.newGen, tt.existingGen, tt.exists)

			if result.Operation != tt.expectedOperation {
				t.Errorf("Operation = %v, want %v", result.Operation, tt.expectedOperation)
			}

			if result.Reason != tt.expectedReason {
				t.Errorf("Reason = %v, want %v", result.Reason, tt.expectedReason)
			}

			if result.NewGeneration != tt.newGen {
				t.Errorf("NewGeneration = %v, want %v", result.NewGeneration, tt.newGen)
			}

			if tt.exists && result.ExistingGeneration != tt.existingGen {
				t.Errorf("ExistingGeneration = %v, want %v", result.ExistingGeneration, tt.existingGen)
			}
		})
	}
}

func TestGetGeneration(t *testing.T) {
	tests := []struct {
		name     string
		meta     metav1.ObjectMeta
		expected int64
	}{
		{
			name: "with valid generation annotation",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "42",
				},
			},
			expected: 42,
		},
		{
			name:     "with no annotations",
			meta:     metav1.ObjectMeta{},
			expected: 0,
		},
		{
			name: "with empty generation annotation",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "",
				},
			},
			expected: 0,
		},
		{
			name: "with invalid generation annotation",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "not-a-number",
				},
			},
			expected: 0,
		},
		{
			name: "with other annotations only (no generation)",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"other": "value",
				},
			},
			expected: 0,
		},
		{
			name: "with generation and other annotations",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"other":                        "value",
					"another/annotation":           "foo",
					constants.AnnotationGeneration: "5",
				},
			},
			expected: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetGeneration(tt.meta)
			if result != tt.expected {
				t.Errorf("GetGeneration() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestGetGenerationFromUnstructured(t *testing.T) {
	tests := []struct {
		obj      *unstructured.Unstructured
		name     string
		expected int64
	}{
		{
			name: "with valid generation",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"annotations": map[string]interface{}{
							constants.AnnotationGeneration: "100",
						},
					},
				},
			},
			expected: 100,
		},
		{
			name:     "nil object",
			obj:      nil,
			expected: 0,
		},
		{
			name: "no annotations",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{},
				},
			},
			expected: 0,
		},
		{
			name: "with generation and other annotations",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"annotations": map[string]interface{}{
							"other":                        "value",
							constants.AnnotationGeneration: "42",
						},
					},
				},
			},
			expected: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetGenerationFromUnstructured(tt.obj)
			if result != tt.expected {
				t.Errorf("GetGenerationFromUnstructured() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestValidateGeneration(t *testing.T) {
	tests := []struct {
		name        string
		meta        metav1.ObjectMeta
		expectError bool
	}{
		{
			name: "valid generation annotation",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "42",
				},
			},
			expectError: false,
		},
		{
			name: "generation 0 is invalid (must be > 0)",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "0",
				},
			},
			expectError: true,
		},
		{
			name: "large generation is valid",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "9999999999",
				},
			},
			expectError: false,
		},
		{
			name: "valid generation with other annotations",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"other":                        "value",
					constants.AnnotationGeneration: "10",
				},
			},
			expectError: false,
		},
		{
			name:        "missing annotations",
			meta:        metav1.ObjectMeta{},
			expectError: true,
		},
		{
			name: "missing generation annotation",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"other": "annotation",
				},
			},
			expectError: true,
		},
		{
			name: "empty generation annotation",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "",
				},
			},
			expectError: true,
		},
		{
			name: "invalid generation value",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "not-a-number",
				},
			},
			expectError: true,
		},
		{
			name: "negative generation",
			meta: metav1.ObjectMeta{
				Annotations: map[string]string{
					constants.AnnotationGeneration: "-5",
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGeneration(tt.meta)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateGenerationFromUnstructured(t *testing.T) {
	tests := []struct {
		obj         *unstructured.Unstructured
		name        string
		expectError bool
	}{
		{
			name: "valid generation annotation",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata": map[string]interface{}{
						"name": "test",
						"annotations": map[string]interface{}{
							constants.AnnotationGeneration: "5",
						},
					},
				},
			},
			expectError: false,
		},
		{
			name:        "nil object",
			obj:         nil,
			expectError: true,
		},
		{
			name: "missing annotations",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata": map[string]interface{}{
						"name": "test",
					},
				},
			},
			expectError: true,
		},
		{
			name: "invalid generation value",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata": map[string]interface{}{
						"name": "test",
						"annotations": map[string]interface{}{
							constants.AnnotationGeneration: "invalid",
						},
					},
				},
			},
			expectError: true,
		},
		{
			name: "negative generation",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata": map[string]interface{}{
						"name": "test",
						"annotations": map[string]interface{}{
							constants.AnnotationGeneration: "-10",
						},
					},
				},
			},
			expectError: true,
		},
		{
			name: "generation 0 is invalid (must be > 0)",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata": map[string]interface{}{
						"name": "test",
						"annotations": map[string]interface{}{
							constants.AnnotationGeneration: "0",
						},
					},
				},
			},
			expectError: true,
		},
		{
			name: "valid generation with other annotations",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata": map[string]interface{}{
						"name": "test",
						"annotations": map[string]interface{}{
							"other":                        "value",
							constants.AnnotationGeneration: "15",
						},
					},
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGenerationFromUnstructured(tt.obj)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestGetLatestGenerationFromList(t *testing.T) {
	tests := []struct {
		name         string
		list         *unstructured.UnstructuredList
		expectedName string
		expectNil    bool
	}{
		{
			name:      "nil list returns nil",
			list:      nil,
			expectNil: true,
		},
		{
			name: "empty list returns nil",
			list: &unstructured.UnstructuredList{
				Items: []unstructured.Unstructured{},
			},
			expectNil: true,
		},
		{
			name: "returns resource with highest generation",
			list: &unstructured.UnstructuredList{
				Items: []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"metadata": map[string]interface{}{
								"name": "resource1",
								"annotations": map[string]interface{}{
									constants.AnnotationGeneration: "10",
								},
							},
						},
					},
					{
						Object: map[string]interface{}{
							"metadata": map[string]interface{}{
								"name": "resource2",
								"annotations": map[string]interface{}{
									constants.AnnotationGeneration: "42",
								},
							},
						},
					},
					{
						Object: map[string]interface{}{
							"metadata": map[string]interface{}{
								"name": "resource3",
								"annotations": map[string]interface{}{
									constants.AnnotationGeneration: "5",
								},
							},
						},
					},
				},
			},
			expectedName: "resource2",
		},
		{
			name: "sorts by name when generations are equal",
			list: &unstructured.UnstructuredList{
				Items: []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"metadata": map[string]interface{}{
								"name": "resource-c",
								"annotations": map[string]interface{}{
									constants.AnnotationGeneration: "10",
								},
							},
						},
					},
					{
						Object: map[string]interface{}{
							"metadata": map[string]interface{}{
								"name": "resource-a",
								"annotations": map[string]interface{}{
									constants.AnnotationGeneration: "10",
								},
							},
						},
					},
					{
						Object: map[string]interface{}{
							"metadata": map[string]interface{}{
								"name": "resource-b",
								"annotations": map[string]interface{}{
									constants.AnnotationGeneration: "10",
								},
							},
						},
					},
				},
			},
			expectedName: "resource-a", // Alphabetically first
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetLatestGenerationFromList(tt.list)

			if tt.expectNil {
				if result != nil {
					t.Errorf("GetLatestGenerationFromList() = %v, want nil", result)
				}
				return
			}

			if result == nil {
				t.Errorf("GetLatestGenerationFromList() = nil, want non-nil")
				return
			}

			if result.GetName() != tt.expectedName {
				t.Errorf("GetLatestGenerationFromList() name = %s, want %s", result.GetName(), tt.expectedName)
			}
		})
	}
}
