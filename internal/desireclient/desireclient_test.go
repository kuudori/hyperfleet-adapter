package desireclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/constants"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	testManagementCluster = "mgmt-cluster-01"
	testResource          = "configmaps"
	testNamespace         = "default"
	testName              = "my-config"
	testOwner             = "hyperfleet-adapter"
)

func newTestClient(store desire.SpecStore) *Client {
	return NewClient(store, testOwner)
}

func testTransportContext() *TransportContext {
	return &TransportContext{ManagementCluster: testManagementCluster, Resource: testResource}
}

func testGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
}

// configMapManifest builds a minimal ConfigMap manifest carrying the
// hyperfleet.io/generation annotation task authors are required to set
// (docs/adapter-authoring-guide.md:498).
func configMapManifest(generation int64) []byte {
	return fmt.Appendf(nil, `{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": %q,
			"namespace": %q,
			"annotations": {%q: %q}
		},
		"data": {"key": "value"}
	}`, testName, testNamespace, constants.AnnotationGeneration, fmt.Sprint(generation))
}

// failingReadDesireStore wraps a real SpecStore but forces CreateReadDesire
// to fail, simulating a read-desire pairing failure.
type failingReadDesireStore struct {
	desire.SpecStore
}

func (f *failingReadDesireStore) CreateReadDesire(
	_ context.Context, _ desire.ReadDesire,
) (desire.ReadDesire, error) {
	return desire.ReadDesire{}, errors.New("boom: read desire store unavailable")
}

// failingCreateDeleteDesireStore wraps a real SpecStore but forces
// CreateDeleteDesire to fail, simulating a failure in the apply-to-delete
// transition after the paired read desire has already been ensured.
type failingCreateDeleteDesireStore struct {
	desire.SpecStore
}

func (f *failingCreateDeleteDesireStore) CreateDeleteDesire(
	_ context.Context, _ desire.DeleteDesire,
) (desire.DeleteDesire, error) {
	return desire.DeleteDesire{}, errors.New("boom: delete desire store unavailable")
}

func newMemoryStore() *memory.Store {
	return memory.New()
}
