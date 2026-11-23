package desirestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore/internal/wire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

// listPageLimit is the spec maximum for the list route's limit.
const listPageLimit = 1000

// list reads the records of type t in a partition and converts each one.
// When keep is not nil, it reads only the records whose identity keep accepts.
func list[T any](
	ctx context.Context, s *Store, managementCluster string, t desire.DesireType, keep func(desire.Identity) bool,
	convert func(string, *wire.Desire) (T, error),
) ([]T, error) {
	out := []T{}
	cursor := ""
	for {
		page, err := s.listPage(ctx, managementCluster, t, cursor)
		if err != nil {
			return nil, err
		}
		for _, ref := range page.Items {
			id := desire.Identity{
				ManagementCluster: managementCluster,
				Type:              t,
				Group:             ref.Group,
				Resource:          ref.Resource,
				Namespace:         ref.Namespace,
				Name:              ref.Name,
			}
			if keep != nil && !keep(id) {
				continue
			}
			w, found, err := s.getReferenced(ctx, id)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			d, err := convert(managementCluster, w)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// listPage reads one page of references of type t.
func (s *Store) listPage(
	ctx context.Context, managementCluster string, t desire.DesireType, cursor string,
) (*wire.DesireList, error) {
	q := url.Values{"type": {string(t)}, "limit": {strconv.Itoa(listPageLimit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	resp, err := s.retrier().get(ctx, listPath(managementCluster)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK {
		return nil, s.problem(ctx, resp)
	}
	var page wire.DesireList
	if err := json.Unmarshal(resp.body, &page); err != nil {
		return nil, fmt.Errorf("store API returned an undecodable list page: %w", err)
	}
	return &page, nil
}

// getReferenced reads the full record of a list reference. found is false
// for a record deleted between the list and the get.
func (s *Store) getReferenced(ctx context.Context, id desire.Identity) (w *wire.Desire, found bool, err error) {
	w, err = s.retrier().getRecord(ctx, id)
	switch {
	case errors.Is(err, desire.ErrNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return w, true, nil
}

// ListApplyDesires implements desire.SpecStore. The list route returns
// references, so it reads each record; one deleted in between is skipped.
func (s *Store) ListApplyDesires(ctx context.Context, managementCluster string) ([]desire.ApplyDesire, error) {
	return list(ctx, s, managementCluster, desire.TypeApply, nil, toApply)
}

// ListDeleteDesires implements desire.SpecStore, as ListApplyDesires.
func (s *Store) ListDeleteDesires(ctx context.Context, managementCluster string) ([]desire.DeleteDesire, error) {
	return list(ctx, s, managementCluster, desire.TypeDelete, nil, toDelete)
}

// ListReadDesires implements desire.SpecStore, as ListApplyDesires.
func (s *Store) ListReadDesires(ctx context.Context, managementCluster string) ([]desire.ReadDesire, error) {
	return list(ctx, s, managementCluster, desire.TypeRead, nil, toRead)
}

// ListReadDesiresMatching is ListReadDesires for only the records whose
// identity keep accepts. It skips the other references without reading them.
func (s *Store) ListReadDesiresMatching(
	ctx context.Context, managementCluster string, keep func(desire.Identity) bool,
) ([]desire.ReadDesire, error) {
	return list(ctx, s, managementCluster, desire.TypeRead, keep, toRead)
}

// DeleteByPrefix implements desire.SpecStore. REST v1 has no prefix delete,
// so it returns an error that wraps errors.ErrUnsupported.
func (s *Store) DeleteByPrefix(context.Context, string, desire.PrefixSelector) error {
	return &UsageError{Reason: "DeleteByPrefix is not supported by the store REST v1 API", Err: errors.ErrUnsupported}
}
