package desireclient

import (
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient/desiretest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestProbeDeletion_ReportsDeleteDesireStatusOnly(t *testing.T) {
	tests := []struct {
		seed func(t *testing.T, store *memory.Store)
		name string
		want transportclient.DeletionState
	}{
		{
			name: "no desires",
			want: transportclient.DeletionNone,
		},
		{
			name: "apply and NotFound read without a delete",
			seed: func(t *testing.T, store *memory.Store) {
				desiretest.PutConfirmedAbsentReadDesire(t, t.Context(), store, testID.Read(), testOwner)
				_, err := store.CreateApplyDesire(t.Context(), desire.ApplyDesire{
					Identity: testID.Apply(), Owner: testOwner,
					Spec: desire.ApplySpec{KubeContent: configMapManifest(1)},
				})
				require.NoError(t, err)
			},
			want: transportclient.DeletionNone,
		},
		{
			name: "old NotFound read does not confirm a pending delete",
			seed: func(t *testing.T, store *memory.Store) {
				desiretest.PutConfirmedAbsentReadDesire(t, t.Context(), store, testID.Read(), testOwner)
				desiretest.PutDeleteDesire(t, t.Context(), store, testID.Delete(), testOwner,
					metav1.ConditionFalse, desire.ReasonWaitingForDeletion)
			},
			want: transportclient.DeletionPending,
		},
		{
			name: "confirmed delete ignores a stale present read",
			seed: func(t *testing.T, store *memory.Store) {
				desiretest.PutSyncedReadDesire(t, t.Context(), store, testID.Read(), testOwner, configMapManifest(1))
				desiretest.PutConfirmedDeleteDesire(t, t.Context(), store, testID.Delete(), testOwner)
			},
			want: transportclient.DeletionConfirmed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemoryStore()
			if tt.seed != nil {
				tt.seed(t, store)
			}
			client := newTestClient(store)

			state, err := client.ProbeDeletion(t.Context(), testGVK(), testNamespace, testName, testTransportContext())

			require.NoError(t, err)
			require.Equal(t, tt.want, state)
		})
	}
}

func TestProbeDeletion_StoreErrorIsReturned(t *testing.T) {
	client := newTestClient(&failingGetDeleteDesireStore{SpecStore: newMemoryStore()})

	_, err := client.ProbeDeletion(t.Context(), testGVK(), testNamespace, testName, testTransportContext())

	require.ErrorContains(t, err, "probe delete desire")
}
