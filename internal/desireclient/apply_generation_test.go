package desireclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/constants"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/require"
)

type applyMutationSpy struct {
	desire.SpecStore
	creates, updates, readCreates, readDeletes int
}

func (s *applyMutationSpy) CreateApplyDesire(ctx context.Context, d desire.ApplyDesire) (desire.ApplyDesire, error) {
	s.creates++
	return s.SpecStore.CreateApplyDesire(ctx, d)
}

func (s *applyMutationSpy) UpdateApplyDesireSpec(
	ctx context.Context, id desire.Identity, spec desire.ApplySpec, owner string, version int64,
) (desire.ApplyDesire, error) {
	s.updates++
	return s.SpecStore.UpdateApplyDesireSpec(ctx, id, spec, owner, version)
}

func (s *applyMutationSpy) CreateReadDesire(ctx context.Context, d desire.ReadDesire) (desire.ReadDesire, error) {
	s.readCreates++
	return s.SpecStore.CreateReadDesire(ctx, d)
}

func (s *applyMutationSpy) DeleteReadDesire(
	ctx context.Context, id desire.Identity, owner string, version int64,
) error {
	s.readDeletes++
	return s.SpecStore.DeleteReadDesire(ctx, id, owner, version)
}

func TestDesireGenerationInvalidInputBeforeStoreAccess(t *testing.T) {
	for _, value := range []string{"missing", "", "abc", "0", "-1"} {
		t.Run(value, func(t *testing.T) {
			content := configMapManifest(1)
			if value == "missing" {
				content = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"my-config","namespace":"default"}}`)
			} else {
				content = []byte(strings.Replace(string(content), `"1"`, `"`+value+`"`, 1))
			}
			spy := &applyMutationSpy{SpecStore: newMemoryStore()}
			_, err := newTestClient(spy).ApplyResource(t.Context(), content, nil, testTransportContext())
			require.Error(t, err)
			require.Zero(t, spy.creates+spy.updates+spy.readCreates+spy.readDeletes)
		})
	}
}

func TestDesireGenerationDecisions(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		want                     manifest.Operation
		stored, mirror, incoming int64
		wantWrite                bool
	}{
		{"create", manifest.OperationCreate, 0, 0, 1, true},
		{"update", manifest.OperationUpdate, 1, 0, 2, true},
		{"equal", manifest.OperationSkip, 2, 0, 2, false},
		{"below stored", manifest.OperationSkip, 3, 0, 2, false},
		{"below mirror", manifest.OperationSkip, 1, 3, 2, false},
		{"equal mirror ahead of spec", manifest.OperationUpdate, 1, 2, 2, true},
		{"equal spec lagging mirror", manifest.OperationSkip, 2, 1, 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			store := newMemoryStore()
			if tt.stored > 0 {
				_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{Identity: testID.Apply(), Owner: testOwner,
					Spec: desire.ApplySpec{KubeContent: configMapManifest(tt.stored)}})
				require.NoError(t, err)
			}
			if tt.mirror > 0 {
				desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(tt.mirror))
			} else if tt.stored > 0 {
				desiretest.PutUnsyncedReadDesire(t, ctx, store, testID.Read(), testOwner)
			}
			spy := &applyMutationSpy{SpecStore: store}
			result, err := newTestClient(spy).ApplyResource(ctx, configMapManifest(tt.incoming), nil, testTransportContext())
			require.NoError(t, err)
			require.Equal(t, tt.want, result.Operation)
			if tt.wantWrite {
				require.Equal(t, 1, spy.creates+spy.updates)
			} else {
				require.Zero(t, spy.creates+spy.updates+spy.readCreates+spy.readDeletes)
			}
			if strings.Contains(tt.name, "below") {
				require.Contains(t, result.Reason, "stale generation")
			}
			if tt.wantWrite {
				applied, err := store.GetApplyDesire(ctx, testID.Apply())
				require.NoError(t, err)
				var content map[string]any
				require.NoError(t, json.Unmarshal(applied.Spec.KubeContent, &content))
				metadata := content["metadata"].(map[string]any)
				annotations := metadata["annotations"].(map[string]any)
				require.Len(t, annotations, 1)
				require.Equal(t, tt.incoming, generationFromKubeContent(applied.Spec.KubeContent))
				require.Contains(t, annotations, constants.AnnotationGeneration)
			}
		})
	}
}

func TestDesireGenerationReadPairRepair(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{Identity: testID.Apply(), Owner: testOwner,
		Spec: desire.ApplySpec{KubeContent: configMapManifest(2)}})
	require.NoError(t, err)
	spy := &applyMutationSpy{SpecStore: store}
	result, err := newTestClient(spy).ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.NoError(t, err)
	require.Equal(t, manifest.OperationSkip, result.Operation)
	require.Equal(t, 1, spy.readCreates)
	require.Zero(t, spy.creates+spy.updates+spy.readDeletes)
}

func TestDesireGenerationStaleEventRepairsMissingReadPair(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	client := newTestClient(store)
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{Identity: testID.Apply(), Owner: testOwner,
		Spec: desire.ApplySpec{KubeContent: configMapManifest(3)}})
	require.NoError(t, err)

	result, err := client.ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.NoError(t, err)
	require.Equal(t, manifest.OperationSkip, result.Operation)
	read, err := store.GetReadDesire(ctx, testID.Read())
	require.NoError(t, err, "stale event must restore the missing read pair")
	require.Equal(t, "v1", read.TargetVersion)
	apply, err := store.GetApplyDesire(ctx, testID.Apply())
	require.NoError(t, err)
	require.Equal(t, int64(3), generationFromKubeContent(apply.Spec.KubeContent))
}

func TestDesireGenerationRetainedMirrorBlocksRollback(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	desiretest.PutKubeAPIErrorReadDesire(t, ctx, store, testID.Read(), testOwner, configMapManifest(3))
	spy := &applyMutationSpy{SpecStore: store}
	result, err := newTestClient(spy).ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.NoError(t, err)
	require.Equal(t, manifest.OperationSkip, result.Operation)
	require.Zero(t, spy.creates+spy.updates+spy.readCreates+spy.readDeletes)
}

func TestDesireGenerationMirrorEdgeCases(t *testing.T) {
	for _, tt := range []struct {
		name      string
		content   []byte
		wantError bool
	}{
		{"annotation absent", []byte(`{
			"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"my-config","namespace":"default"}
		}`), false},
		{"invalid annotation", []byte(`{
			"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"my-config","namespace":"default",
			"annotations":{"hyperfleet.io/generation":"bad"}}
		}`), false},
		{"wrong target", []byte(`{
			"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"other","namespace":"default",
			"annotations":{"hyperfleet.io/generation":"9"}}
		}`), true},
		{"malformed JSON", []byte(`{bad`), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			store := newMemoryStore()
			desiretest.PutSyncedReadDesire(t, ctx, store, testID.Read(), testOwner, tt.content)
			spy := &applyMutationSpy{SpecStore: store}
			result, err := newTestClient(spy).ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
			if tt.wantError {
				require.Error(t, err)
				require.Nil(t, result)
				require.Zero(t, spy.creates+spy.updates+spy.readCreates+spy.readDeletes)
				return
			}
			require.NoError(t, err)
			require.Equal(t, manifest.OperationCreate, result.Operation)
		})
	}
}

func TestDesireGenerationRepairsAnnotationFreeApply(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{Identity: testID.Apply(), Owner: testOwner,
		Spec: desire.ApplySpec{KubeContent: []byte(`{
			"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"my-config","namespace":"default"}
		}`)}})
	require.NoError(t, err)
	spy := &applyMutationSpy{SpecStore: store}
	result, err := newTestClient(spy).ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)
	require.Equal(t, manifest.OperationUpdate, result.Operation)
	require.Equal(t, 1, spy.updates)
}

func TestDesireGenerationRejectsForeignOwners(t *testing.T) {
	for _, which := range []string{"apply", "read"} {
		t.Run(which, func(t *testing.T) {
			ctx := t.Context()
			store := newMemoryStore()
			if which == "apply" {
				_, err := store.CreateApplyDesire(ctx, desire.ApplyDesire{Identity: testID.Apply(), Owner: "other",
					Spec: desire.ApplySpec{KubeContent: configMapManifest(2)}})
				require.NoError(t, err)
			} else {
				desiretest.PutUnsyncedReadDesire(t, ctx, store, testID.Read(), "other")
			}
			spy := &applyMutationSpy{SpecStore: store}
			_, err := newTestClient(spy).ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
			require.ErrorIs(t, err, desire.ErrOwnerConflict)
			require.Zero(t, spy.creates+spy.updates+spy.readCreates+spy.readDeletes)
		})
	}
}

func TestDesireGenerationStoreLossRecreatesPair(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	client := newTestClient(store)
	_, err := client.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)
	apply, err := store.GetApplyDesire(ctx, testID.Apply())
	require.NoError(t, err)
	read, err := store.GetReadDesire(ctx, testID.Read())
	require.NoError(t, err)
	require.NoError(t, store.DeleteApplyDesire(ctx, testID.Apply(), testOwner, apply.Version))
	require.NoError(t, store.DeleteReadDesire(ctx, testID.Read(), testOwner, read.Version))
	spy := &applyMutationSpy{SpecStore: store}
	result, err := newTestClient(spy).ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	require.NoError(t, err)
	require.Equal(t, manifest.OperationCreate, result.Operation)
	require.Equal(t, 1, spy.creates)
	require.Equal(t, 1, spy.readCreates)
}
