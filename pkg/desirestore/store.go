// Package desirestore implements desire.SpecStore over the HyperFleet Desire Store REST v1 API.
package desirestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore/internal/wire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

func (s *Store) create(
	ctx context.Context, id desire.Identity, owner string, body wire.DesireWrite, matches func(*wire.Desire) bool,
) (*wire.Desire, error) {
	if err := s.checkOwner(owner); err != nil {
		return nil, err
	}
	if body.ResourceId == "" {
		return nil, &UsageError{Reason: "resource_id is required by the store API and the event has no id"}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, &UsageError{Reason: "cannot encode the store API request", Err: err}
	}
	r := s.retrier()
	for {
		resp, err := r.send(ctx, http.MethodPut, desirePath(id), data)
		if err == nil {
			return s.decodeRecord(ctx, resp, http.StatusCreated, http.StatusOK)
		}
		if !r.wait(ctx, http.MethodPut, err) {
			return nil, err
		}
		if !ambiguous(err) {
			continue
		}
		w, readErr := r.getRecord(ctx, id)
		switch {
		case errors.Is(readErr, desire.ErrNotFound):
		case readErr != nil:
			return nil, readErr
		case w.Owner != s.owner:
			return nil, &recoveryError{reason: "belongs to another owner", sentinels: []error{desire.ErrOwnerConflict}}
		case matches(w):
			return w, nil
		default:
			return nil, &recoveryError{reason: "differs from the request", sentinels: []error{desire.ErrAlreadyExists}}
		}
	}
}

// Delete recovery compares versions. Recreating a record can reuse the original version.
func (s *Store) delete(ctx context.Context, id desire.Identity, owner string, version int64) error {
	if err := s.checkOwner(owner); err != nil {
		return err
	}
	path := desirePath(id) + "?version=" + strconv.FormatInt(version, 10)
	r := s.retrier()
	wasAmbiguous := false
	for {
		resp, err := r.send(ctx, http.MethodDelete, path, nil)
		if err == nil {
			return s.deleteResult(ctx, resp, wasAmbiguous)
		}
		if !r.wait(ctx, http.MethodDelete, err) {
			return err
		}
		if !ambiguous(err) {
			continue
		}
		wasAmbiguous = true
		w, readErr := r.getRecord(ctx, id)
		switch {
		case errors.Is(readErr, desire.ErrNotFound):
			return nil
		case readErr != nil:
			return readErr
		case w.Version != version:
			return &recoveryError{
				reason: "has a different version", sentinels: []error{desire.ErrVersionConflict, ErrRecreated},
			}
		}
	}
}

// deleteResult interprets the final response to a delete. A 404 after an
// ambiguous attempt is success: that attempt may have deleted the record.
func (s *Store) deleteResult(ctx context.Context, resp *response, wasAmbiguous bool) error {
	if resp.status == http.StatusNoContent {
		return nil
	}
	pe := s.problem(ctx, resp)
	if wasAmbiguous && errors.Is(pe, desire.ErrNotFound) {
		return nil
	}
	if wasAmbiguous && errors.Is(pe, desire.ErrVersionConflict) {
		return errors.Join(pe, ErrRecreated)
	}
	return pe
}

// CreateApplyDesire implements desire.SpecStore.
func (s *Store) CreateApplyDesire(ctx context.Context, d desire.ApplyDesire) (desire.ApplyDesire, error) {
	w, err := s.create(ctx, d.Identity, d.Owner,
		wire.DesireWrite{ResourceId: d.OriginID, KubeContent: d.Spec.KubeContent},
		func(w *wire.Desire) bool { return jsonEqual(w.KubeContent, d.Spec.KubeContent) })
	if err != nil {
		return desire.ApplyDesire{}, fmt.Errorf("create apply record: %w", err)
	}
	return toApply(d.Identity.ManagementCluster, w)
}

// GetApplyDesire implements desire.SpecStore.
func (s *Store) GetApplyDesire(ctx context.Context, id desire.Identity) (desire.ApplyDesire, error) {
	w, err := s.retrier().getRecord(ctx, id)
	if err != nil {
		return desire.ApplyDesire{}, fmt.Errorf("get apply record: %w", err)
	}
	return toApply(id.ManagementCluster, w)
}

// UpdateApplyDesireSpec implements desire.SpecStore. The API requires
// resource_id on every write, so it reads the record first and sends the
// stored value.
func (s *Store) UpdateApplyDesireSpec(
	ctx context.Context, id desire.Identity, spec desire.ApplySpec, owner string, version int64,
) (desire.ApplyDesire, error) {
	if err := s.checkOwner(owner); err != nil {
		return desire.ApplyDesire{}, fmt.Errorf("update apply record: %w", err)
	}
	if version < 1 {
		return desire.ApplyDesire{}, &UsageError{Reason: "an update needs the stored version"}
	}
	current, err := s.retrier().getRecord(ctx, id)
	if err != nil {
		return desire.ApplyDesire{}, fmt.Errorf("update apply record: %w", err)
	}
	data, err := json.Marshal(wire.DesireWrite{
		ResourceId: current.ResourceId, KubeContent: spec.KubeContent, Version: version,
	})
	if err != nil {
		return desire.ApplyDesire{}, &UsageError{Reason: "cannot encode the store API request", Err: err}
	}
	r := s.retrier()
	for {
		resp, err := r.send(ctx, http.MethodPut, desirePath(id), data)
		if err == nil {
			w, decodeErr := s.decodeRecord(ctx, resp, http.StatusOK)
			if decodeErr != nil {
				return desire.ApplyDesire{}, fmt.Errorf("update apply record: %w", decodeErr)
			}
			return toApply(id.ManagementCluster, w)
		}
		if !r.wait(ctx, http.MethodPut, err) {
			return desire.ApplyDesire{}, fmt.Errorf("update apply record: %w", err)
		}
		if !ambiguous(err) {
			continue
		}
		w, readErr := r.getRecord(ctx, id)
		switch {
		case errors.Is(readErr, desire.ErrNotFound):
			return desire.ApplyDesire{}, &recoveryError{reason: "is absent", sentinels: []error{desire.ErrNotFound}}
		case readErr != nil:
			return desire.ApplyDesire{}, fmt.Errorf("update apply record: %w", readErr)
		case w.Version == version:
		case w.Version > version && w.Owner == s.owner && jsonEqual(w.KubeContent, spec.KubeContent):
			return toApply(id.ManagementCluster, w)
		default:
			return desire.ApplyDesire{}, &recoveryError{reason: "has changed", sentinels: []error{desire.ErrVersionConflict}}
		}
	}
}

// DeleteApplyDesire implements desire.SpecStore.
func (s *Store) DeleteApplyDesire(ctx context.Context, id desire.Identity, owner string, version int64) error {
	return s.delete(ctx, id, owner, version)
}

// CreateDeleteDesire implements desire.SpecStore.
func (s *Store) CreateDeleteDesire(ctx context.Context, d desire.DeleteDesire) (desire.DeleteDesire, error) {
	w, err := s.create(ctx, d.Identity, d.Owner, wire.DesireWrite{ResourceId: d.OriginID},
		func(*wire.Desire) bool { return true })
	if err != nil {
		return desire.DeleteDesire{}, fmt.Errorf("create delete record: %w", err)
	}
	return toDelete(d.Identity.ManagementCluster, w)
}

// GetDeleteDesire implements desire.SpecStore.
func (s *Store) GetDeleteDesire(ctx context.Context, id desire.Identity) (desire.DeleteDesire, error) {
	w, err := s.retrier().getRecord(ctx, id)
	if err != nil {
		return desire.DeleteDesire{}, fmt.Errorf("get delete record: %w", err)
	}
	return toDelete(id.ManagementCluster, w)
}

// DeleteDeleteDesire implements desire.SpecStore.
func (s *Store) DeleteDeleteDesire(ctx context.Context, id desire.Identity, owner string, version int64) error {
	return s.delete(ctx, id, owner, version)
}

// CreateReadDesire implements desire.SpecStore.
func (s *Store) CreateReadDesire(ctx context.Context, d desire.ReadDesire) (desire.ReadDesire, error) {
	w, err := s.create(ctx, d.Identity, d.Owner,
		wire.DesireWrite{ResourceId: d.OriginID, TargetVersion: d.TargetVersion},
		func(w *wire.Desire) bool { return w.TargetVersion == d.TargetVersion })
	if err != nil {
		return desire.ReadDesire{}, fmt.Errorf("create read record: %w", err)
	}
	return toRead(d.Identity.ManagementCluster, w)
}

// GetReadDesire implements desire.SpecStore.
func (s *Store) GetReadDesire(ctx context.Context, id desire.Identity) (desire.ReadDesire, error) {
	w, err := s.retrier().getRecord(ctx, id)
	if err != nil {
		return desire.ReadDesire{}, fmt.Errorf("get read record: %w", err)
	}
	return toRead(id.ManagementCluster, w)
}

// DeleteReadDesire implements desire.SpecStore.
func (s *Store) DeleteReadDesire(ctx context.Context, id desire.Identity, owner string, version int64) error {
	return s.delete(ctx, id, owner, version)
}
