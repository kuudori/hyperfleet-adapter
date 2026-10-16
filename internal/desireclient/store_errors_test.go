package desireclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertNoDesireText(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.NotContains(t, strings.ToLower(err.Error()), "desire")
}

func TestApplyResource_OwnerConflictHidesStoreText(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	other := NewClient(store, "other-adapter")
	_, err := other.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)

	_, err = newTestClient(store).ApplyResource(ctx, configMapManifest(2), nil, testTransportContext())
	assertNoDesireText(t, err)
	assert.ErrorIs(t, err, desire.ErrOwnerConflict)
	assert.EqualError(t, err, "remote transport: apply request owner: owner conflict")
}

func TestDeleteResource_OwnerConflictHidesStoreText(t *testing.T) {
	ctx := t.Context()
	store := newMemoryStore()
	other := NewClient(store, "other-adapter")
	_, err := other.ApplyResource(ctx, configMapManifest(1), nil, testTransportContext())
	require.NoError(t, err)

	err = newTestClient(store).DeleteResource(ctx, testGVK(), testNamespace, testName, nil, testTransportContext())
	assertNoDesireText(t, err)
	assert.ErrorIs(t, err, desire.ErrOwnerConflict)
}

func TestStoreError_Text(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"owner conflict", desire.ErrOwnerConflict, "owner conflict"},
		{"version conflict", desire.ErrVersionConflict, "version conflict"},
		{"delete pending", desire.ErrDeletePending, "delete pending"},
		{"already exists", desire.ErrAlreadyExists, "already exists"},
		{"not found", desire.ErrNotFound, "not found"},
		{"aborted", desire.ErrAborted, "aborted"},
		{
			"sentinel wrapped by the store",
			fmt.Errorf("desire: create apply desire mc/v1/configmaps/default/x: %w", desire.ErrOwnerConflict),
			"owner conflict",
		},
		{
			"cause wrapped by the store",
			fmt.Errorf("desire: get desire:{mc/configmaps/default/x}/apply: %w",
				errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")),
			"dial tcp 10.0.0.1:6379: connect: connection refused",
		},
		{
			"validation cause",
			fmt.Errorf("desire: validate read desire: %w",
				fmt.Errorf("ReadDesire: %w", errors.New("identity: field name is required and must be non-empty"))),
			"identity: field name is required and must be non-empty",
		},
		{"no cause without desire text", errors.New("desire: cas noop"), "store request failed"},
		{"error without desire text", errors.New("connection reset"), "connection reset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := storeErr(tt.err)
			assert.EqualError(t, err, tt.want)
			assert.ErrorIs(t, err, tt.err)
		})
	}
	assert.NoError(t, storeErr(nil))
}
