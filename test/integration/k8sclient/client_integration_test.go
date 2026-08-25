// This file contains integration tests for the K8s client.

package k8sclientintegration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// gvk provides commonly used GroupVersionKinds for integration tests.
// This is a local copy to avoid depending on test-only exports from k8sclient.
var gvk = struct {
	Namespace schema.GroupVersionKind
	Pod       schema.GroupVersionKind
	Service   schema.GroupVersionKind
	ConfigMap schema.GroupVersionKind
	Secret    schema.GroupVersionKind
}{
	Namespace: schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"},
	Pod:       schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
	Service:   schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"},
	ConfigMap: schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
	Secret:    schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"},
}

// TestIntegration_NewClient tests client initialization with real K8s API
func TestIntegration_NewClient(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("client is properly initialized", func(t *testing.T) {
		assert.NotNil(t, env.GetClient())
		assert.NotNil(t, env.GetContext())
	})
}

// TestIntegration_CreateResource tests creating resources in K8s
func TestIntegration_CreateResource(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("create namespace", func(t *testing.T) {
		// Create namespace resource
		ns := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Namespace",
				"metadata": map[string]interface{}{
					"name": "test-namespace-" + time.Now().Format("20060102150405"),
					"labels": map[string]interface{}{
						"test": "integration",
						"app":  "k8s_client",
					},
				},
			},
		}
		ns.SetGroupVersionKind(gvk.Namespace)

		// Create the namespace
		created, err := env.GetClient().CreateResource(env.GetContext(), ns)
		require.NoError(t, err)
		require.NotNil(t, created)

		// Verify the namespace was created
		assert.Equal(t, "Namespace", created.GetKind())
		assert.Equal(t, ns.GetName(), created.GetName())

		// Verify labels
		labels := created.GetLabels()
		assert.Equal(t, "integration", labels["test"])
		assert.Equal(t, "k8s_client", labels["app"])
	})

	t.Run("create configmap", func(t *testing.T) {
		cmName := "test-configmap-" + time.Now().Format("20060102150405")

		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"key1": "value1",
					"key2": "value2",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		created, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)
		require.NotNil(t, created)

		assert.Equal(t, "ConfigMap", created.GetKind())
		assert.Equal(t, cmName, created.GetName())
		assert.Equal(t, "default", created.GetNamespace())
	})
}

// TestIntegration_GetResource tests getting resources from K8s
func TestIntegration_GetResource(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("get existing namespace", func(t *testing.T) {
		nsName := "test-get-ns-" + time.Now().Format("20060102150405")

		// Create namespace first
		ns := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Namespace",
				"metadata": map[string]interface{}{
					"name": nsName,
				},
			},
		}
		ns.SetGroupVersionKind(gvk.Namespace)

		_, err := env.GetClient().CreateResource(env.GetContext(), ns)
		require.NoError(t, err)

		// Get the namespace
		retrieved, err := env.GetClient().GetResource(env.GetContext(), gvk.Namespace, "", nsName, nil)
		require.NoError(t, err)
		require.NotNil(t, retrieved)

		assert.Equal(t, "Namespace", retrieved.GetKind())
		assert.Equal(t, nsName, retrieved.GetName())
	})

	t.Run("get non-existent resource returns error", func(t *testing.T) {
		_, err := env.GetClient().GetResource(env.GetContext(), gvk.Namespace, "", "non-existent-namespace-12345", nil)
		require.Error(t, err)
	})
}

// TestIntegration_ListResources tests listing resources with selectors
func TestIntegration_ListResources(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("list configmaps with label selector", func(t *testing.T) {
		timestamp := time.Now().Format("20060102150405")

		// Create multiple configmaps with labels
		for i := 1; i <= 3; i++ {
			cm := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name":      "test-cm-" + timestamp + "-" + string(rune('a'+i)),
						"namespace": "default",
						"labels": map[string]interface{}{
							"test-group": timestamp,
							"test-index": string(rune('0' + i)),
						},
					},
					"data": map[string]interface{}{
						"index": string(rune('0' + i)),
					},
				},
			}
			cm.SetGroupVersionKind(gvk.ConfigMap)

			_, err := env.GetClient().CreateResource(env.GetContext(), cm)
			require.NoError(t, err)
		}

		// List configmaps with label selector
		selector := "test-group=" + timestamp
		list, err := env.GetClient().ListResources(env.GetContext(), gvk.ConfigMap, "default", selector)
		require.NoError(t, err)
		require.NotNil(t, list)

		// UnstructuredList has Items field directly
		assert.GreaterOrEqual(t, len(list.Items), 3, "Should find at least 3 configmaps")
	})
}

// TestIntegration_UpdateResource tests updating resources
func TestIntegration_UpdateResource(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("update configmap data", func(t *testing.T) {
		cmName := "test-update-cm-" + time.Now().Format("20060102150405")

		// Create configmap
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"key1": "original-value",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		created, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)

		// Update the data
		err = unstructured.SetNestedField(created.Object, "updated-value", "data", "key1")
		require.NoError(t, err)
		err = unstructured.SetNestedField(created.Object, "new-value", "data", "key2")
		require.NoError(t, err)

		updated, err := env.GetClient().UpdateResource(env.GetContext(), created)
		require.NoError(t, err)
		require.NotNil(t, updated)

		// Verify the update
		data, found, err := unstructured.NestedStringMap(updated.Object, "data")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "updated-value", data["key1"])
		assert.Equal(t, "new-value", data["key2"])
	})
}

// TestIntegration_DeleteResource tests deleting resources
func TestIntegration_DeleteResource(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("delete namespace", func(t *testing.T) {
		nsName := "test-delete-ns-" + time.Now().Format("20060102150405")

		// Create namespace
		ns := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Namespace",
				"metadata": map[string]interface{}{
					"name": nsName,
				},
			},
		}
		ns.SetGroupVersionKind(gvk.Namespace)

		_, err := env.GetClient().CreateResource(env.GetContext(), ns)
		require.NoError(t, err)

		// Verify it exists
		_, err = env.GetClient().GetResource(env.GetContext(), gvk.Namespace, "", nsName, nil)
		require.NoError(t, err)

		// Delete the namespace
		err = env.GetClient().DeleteResource(env.GetContext(), gvk.Namespace, "", nsName, nil, nil)
		require.NoError(t, err)

		// Verify it's being deleted (namespaces go into Terminating phase)
		time.Sleep(100 * time.Millisecond)
		deletedNs, err := env.GetClient().GetResource(env.GetContext(), gvk.Namespace, "", nsName, nil)
		if err == nil {
			// Namespace still exists, should have deletionTimestamp set (Terminating state)
			deletionTimestamp := deletedNs.GetDeletionTimestamp()
			assert.NotNil(t, deletionTimestamp, "Namespace should have deletionTimestamp set (Terminating state)")
		} else {
			// Namespace already deleted completely
			require.True(t, k8serrors.IsNotFound(err), "Expected NotFound error for deleted namespace")
		}
	})
}

// TestIntegration_ResourceLifecycle tests full CRUD lifecycle
func TestIntegration_ResourceLifecycle(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("full configmap lifecycle", func(t *testing.T) {
		cmName := "lifecycle-cm-" + time.Now().Format("20060102150405")

		// 1. Create
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
					"labels": map[string]interface{}{
						"lifecycle": "test",
					},
				},
				"data": map[string]interface{}{
					"stage": "created",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		created, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)
		assert.Equal(t, cmName, created.GetName())

		// 2. Get and verify
		retrieved, err := env.GetClient().GetResource(env.GetContext(), gvk.ConfigMap, "default", cmName, nil)
		require.NoError(t, err)
		data, _, _ := unstructured.NestedString(retrieved.Object, "data", "stage")
		assert.Equal(t, "created", data)

		// 3. Update
		err = unstructured.SetNestedField(retrieved.Object, "updated", "data", "stage")
		require.NoError(t, err)
		updated, err := env.GetClient().UpdateResource(env.GetContext(), retrieved)
		require.NoError(t, err)
		data, _, _ = unstructured.NestedString(updated.Object, "data", "stage")
		assert.Equal(t, "updated", data)

		// 4. Get and verify update
		retrieved2, err := env.GetClient().GetResource(env.GetContext(), gvk.ConfigMap, "default", cmName, nil)
		require.NoError(t, err)
		data, _, _ = unstructured.NestedString(retrieved2.Object, "data", "stage")
		assert.Equal(t, "updated", data)

		// 5. Delete
		err = env.GetClient().DeleteResource(env.GetContext(), gvk.ConfigMap, "default", cmName, nil, nil)
		require.NoError(t, err)

		// 6. Verify deletion
		_, err = env.GetClient().GetResource(env.GetContext(), gvk.ConfigMap, "default", cmName, nil)
		assert.Error(t, err)
	})
}

// TestIntegration_PatchResource tests patching resources with strategic merge
func TestIntegration_PatchResource(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("patch configmap adds new data field", func(t *testing.T) {
		cmName := "test-patch-cm-" + time.Now().Format("20060102150405")

		// Create initial configmap
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
					"labels": map[string]interface{}{
						"app": "test",
					},
				},
				"data": map[string]interface{}{
					"key1": "original-value",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		created, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)
		require.NotNil(t, created)

		// Patch to add new data field and label
		patchData := []byte(`{
			"metadata": {
				"labels": {
					"patched": "true"
				}
			},
			"data": {
				"key2": "patched-value"
			}
		}`)

		patched, err := env.GetClient().PatchResource(env.GetContext(), gvk.ConfigMap, "default", cmName, patchData)
		require.NoError(t, err)
		require.NotNil(t, patched)

		// Verify original data is preserved
		data, found, err := unstructured.NestedStringMap(patched.Object, "data")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "original-value", data["key1"], "Original key1 should be preserved")
		assert.Equal(t, "patched-value", data["key2"], "New key2 should be added")

		// Verify labels are merged
		labels := patched.GetLabels()
		assert.Equal(t, "test", labels["app"], "Original app label should be preserved")
		assert.Equal(t, "true", labels["patched"], "New patched label should be added")
	})

	t.Run("patch configmap updates existing field", func(t *testing.T) {
		cmName := "test-patch-update-cm-" + time.Now().Format("20060102150405")

		// Create configmap
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"key1": "original",
					"key2": "keep-this",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		_, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)

		// Patch to update key1, keep key2
		patchData := []byte(`{
			"data": {
				"key1": "updated"
			}
		}`)

		patched, err := env.GetClient().PatchResource(env.GetContext(), gvk.ConfigMap, "default", cmName, patchData)
		require.NoError(t, err)

		data, _, _ := unstructured.NestedStringMap(patched.Object, "data")
		assert.Equal(t, "updated", data["key1"], "key1 should be updated")
		assert.Equal(t, "keep-this", data["key2"], "key2 should be preserved")
	})

	t.Run("patch non-existent resource returns error", func(t *testing.T) {
		patchData := []byte(`{"data": {"key": "value"}}`)
		_, err := env.GetClient().PatchResource(
			env.GetContext(), gvk.ConfigMap, "default", "non-existent-cm-12345", patchData,
		)
		require.Error(t, err)
		assert.True(t, k8serrors.IsNotFound(err), "Should return NotFound error")
	})

	t.Run("patch with invalid JSON returns error", func(t *testing.T) {
		cmName := "test-patch-invalid-" + time.Now().Format("20060102150405")

		// Create configmap
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		_, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)

		// Try to patch with invalid JSON
		invalidPatchData := []byte(`{invalid json}`)
		_, err = env.GetClient().PatchResource(env.GetContext(), gvk.ConfigMap, "default", cmName, invalidPatchData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid patch data", "Should return invalid patch data error")
	})
}

// TestIntegration_ErrorScenarios tests various error conditions
func TestIntegration_ErrorScenarios(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("create duplicate resource returns AlreadyExists error", func(t *testing.T) {
		cmName := "test-duplicate-cm-" + time.Now().Format("20060102150405")

		// Create first configmap
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		_, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)

		// Try to create duplicate
		cm2 := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
				},
			},
		}
		cm2.SetGroupVersionKind(gvk.ConfigMap)

		_, err = env.GetClient().CreateResource(env.GetContext(), cm2)
		require.Error(t, err)
		assert.True(t, k8serrors.IsAlreadyExists(err), "Should return AlreadyExists error")
	})

	t.Run("list with invalid label selector returns error", func(t *testing.T) {
		// Invalid selector syntax - use an actually invalid one that will fail parsing
		// controller-runtime is lenient with some selectors, so use one that's truly invalid
		invalidSelector := "app===invalid"
		_, err := env.GetClient().ListResources(env.GetContext(), gvk.ConfigMap, "default", invalidSelector)
		require.Error(t, err)
	})

	t.Run("get with empty name returns error", func(t *testing.T) {
		_, err := env.GetClient().GetResource(env.GetContext(), gvk.ConfigMap, "default", "", nil)
		require.Error(t, err)
	})

	t.Run("delete already deleted resource succeeds", func(t *testing.T) {
		cmName := "test-delete-twice-cm-" + time.Now().Format("20060102150405")

		// Create and delete
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		_, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)

		err = env.GetClient().DeleteResource(env.GetContext(), gvk.ConfigMap, "default", cmName, nil, nil)
		require.NoError(t, err)

		// Try to delete again - should succeed (idempotent)
		err = env.GetClient().DeleteResource(env.GetContext(), gvk.ConfigMap, "default", cmName, nil, nil)
		require.NoError(t, err, "Deleting already deleted resource should succeed")
	})

	t.Run("update with missing resourceVersion still works", func(t *testing.T) {
		cmName := "test-update-no-rv-cm-" + time.Now().Format("20060102150405")

		// Create configmap
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      cmName,
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"key": "original",
				},
			},
		}
		cm.SetGroupVersionKind(gvk.ConfigMap)

		created, err := env.GetClient().CreateResource(env.GetContext(), cm)
		require.NoError(t, err)

		// Update without proper resourceVersion (controller-runtime handles this gracefully)
		created.SetResourceVersion("")
		err = unstructured.SetNestedField(created.Object, "updated", "data", "key")
		require.NoError(t, err)

		_, err = env.GetClient().UpdateResource(env.GetContext(), created)
		// Controller-runtime typically rejects updates without resourceVersion
		// but behavior may vary - verify at least that it doesn't silently corrupt data
		if err == nil {
			// If it succeeded, verify the update was applied
			retrieved, getErr := env.GetClient().GetResource(env.GetContext(), gvk.ConfigMap, "default", cmName, nil)
			require.NoError(t, getErr)
			data, _, _ := unstructured.NestedString(retrieved.Object, "data", "key")
			assert.Equal(t, "updated", data)
		}
	})
}

// TestIntegration_DifferentResourceTypes tests various K8s resource types
func TestIntegration_DifferentResourceTypes(t *testing.T) {
	env := GetSharedEnv(t)

	t.Run("create and get service", func(t *testing.T) {
		svcName := "test-service-" + time.Now().Format("20060102150405")

		svc := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name":      svcName,
					"namespace": "default",
					"labels": map[string]interface{}{
						"app": "test",
					},
				},
				"spec": map[string]interface{}{
					"selector": map[string]interface{}{
						"app": "test",
					},
					"ports": []interface{}{
						map[string]interface{}{
							"protocol":   "TCP",
							"port":       80,
							"targetPort": 8080,
						},
					},
				},
			},
		}
		svc.SetGroupVersionKind(gvk.Service)

		created, err := env.GetClient().CreateResource(env.GetContext(), svc)
		require.NoError(t, err)
		require.NotNil(t, created)

		assert.Equal(t, "Service", created.GetKind())
		assert.Equal(t, svcName, created.GetName())

		// Get the service
		retrieved, err := env.GetClient().GetResource(env.GetContext(), gvk.Service, "default", svcName, nil)
		require.NoError(t, err)
		assert.Equal(t, svcName, retrieved.GetName())
	})

	t.Run("create and list pods", func(t *testing.T) {
		timestamp := time.Now().Format("20060102150405")

		// Create default ServiceAccount if it doesn't exist
		// K3s creates it asynchronously, so we need to wait or create it
		defaultSA := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata": map[string]interface{}{
					"name":      "default",
					"namespace": "default",
				},
			},
		}
		defaultSA.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "",
			Version: "v1",
			Kind:    "ServiceAccount",
		})
		_, err := env.GetClient().CreateResource(env.GetContext(), defaultSA)
		// Ignore AlreadyExists error
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			t.Logf("Warning: Could not create default ServiceAccount: %v", err)
		}
		// Give it a moment to be processed
		time.Sleep(1 * time.Second)

		// Create test pods
		for i := 1; i <= 2; i++ {
			podName := "test-pod-" + timestamp + "-" + string(rune('a'+i))
			pod := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":      podName,
						"namespace": "default",
						"labels": map[string]interface{}{
							"test-group": timestamp,
							"app":        "test-pod",
						},
					},
					"spec": map[string]interface{}{
						"automountServiceAccountToken": false, // Avoid needing default ServiceAccount
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"image": "nginx:latest",
							},
						},
					},
				},
			}
			pod.SetGroupVersionKind(gvk.Pod)

			_, createErr := env.GetClient().CreateResource(env.GetContext(), pod)
			require.NoError(t, createErr)
		}

		// List pods with label selector
		selector := "test-group=" + timestamp
		list, err := env.GetClient().ListResources(env.GetContext(), gvk.Pod, "default", selector)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(list.Items), 2, "Should find at least 2 pods")
	})

	t.Run("create secret with data", func(t *testing.T) {
		secretName := "test-secret-" + time.Now().Format("20060102150405")

		secret := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":      secretName,
					"namespace": "default",
				},
				"type": "Opaque",
				"data": map[string]interface{}{
					"username": "YWRtaW4=",     // base64 encoded "admin"
					"password": "cGFzc3dvcmQ=", // base64 encoded "password"
				},
			},
		}
		secret.SetGroupVersionKind(gvk.Secret)

		created, err := env.GetClient().CreateResource(env.GetContext(), secret)
		require.NoError(t, err)
		require.NotNil(t, created)

		assert.Equal(t, "Secret", created.GetKind())
		assert.Equal(t, secretName, created.GetName())

		// Verify secret data
		data, found, err := unstructured.NestedStringMap(created.Object, "data")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "YWRtaW4=", data["username"])
		assert.Equal(t, "cGFzc3dvcmQ=", data["password"])
	})
}
