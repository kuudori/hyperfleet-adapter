package desirestore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore/internal/wire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

const apiPrefix = "/api/hyperfleet/desire-store/v1/partitions/"

// desireID encodes an identity as the API's desire identifier:
// type:group:resource:namespace:name. The name comes last and may contain
// colons, so the encoding stays reversible.
func desireID(id desire.Identity) string {
	return strings.Join([]string{string(id.Type), id.Group, id.Resource, id.Namespace, id.Name}, ":")
}

// desirePath returns the escaped path of one record, without the base path.
func desirePath(id desire.Identity) string {
	return apiPrefix + url.PathEscape(id.ManagementCluster) + "/desires/" + url.PathEscape(desireID(id))
}

// listPath returns the escaped path of a partition's list route, without the
// base path.
func listPath(managementCluster string) string {
	return apiPrefix + url.PathEscape(managementCluster) + "/desires"
}

func identityOf(managementCluster string, w *wire.Desire) desire.Identity {
	return desire.Identity{
		ManagementCluster: managementCluster,
		Type:              desire.DesireType(w.Type),
		Group:             w.Group,
		Resource:          w.Resource,
		Namespace:         w.Namespace,
		Name:              w.Name,
	}
}

func toApply(managementCluster string, w *wire.Desire) (desire.ApplyDesire, error) {
	var status desire.Status
	if err := decodeStatus(w.Status, &status); err != nil {
		return desire.ApplyDesire{}, err
	}
	return desire.ApplyDesire{
		Identity:   identityOf(managementCluster, w),
		Owner:      w.Owner,
		OriginID:   w.ResourceId,
		Spec:       desire.ApplySpec{KubeContent: w.KubeContent},
		Status:     status,
		Generation: w.Generation,
		Version:    w.Version,
	}, nil
}

func toDelete(managementCluster string, w *wire.Desire) (desire.DeleteDesire, error) {
	var status desire.Status
	if err := decodeStatus(w.Status, &status); err != nil {
		return desire.DeleteDesire{}, err
	}
	return desire.DeleteDesire{
		Identity:   identityOf(managementCluster, w),
		Owner:      w.Owner,
		OriginID:   w.ResourceId,
		Status:     status,
		Generation: w.Generation,
		Version:    w.Version,
	}, nil
}

func toRead(managementCluster string, w *wire.Desire) (desire.ReadDesire, error) {
	var status desire.ReadStatus
	if err := decodeStatus(w.Status, &status); err != nil {
		return desire.ReadDesire{}, err
	}
	return desire.ReadDesire{
		Identity:      identityOf(managementCluster, w),
		Owner:         w.Owner,
		OriginID:      w.ResourceId,
		TargetVersion: w.TargetVersion,
		Status:        status,
		Generation:    w.Generation,
		Version:       w.Version,
	}, nil
}

func decodeStatus(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("store API returned an undecodable status: %w", err)
	}
	return nil
}

// jsonEqual reports whether two JSON documents hold the same value, ignoring
// whitespace and object-key order. Numbers compare by their literal text.
func jsonEqual(a, b json.RawMessage) bool {
	va, errA := decodeExact(a)
	vb, errB := decodeExact(b)
	if errA != nil || errB != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// decodeExact decodes JSON with numbers kept as their literal text, so that
// large integers do not round to the same float64.
func decodeExact(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Store) decodeRecord(ctx context.Context, r *response, ok ...int) (*wire.Desire, error) {
	for _, code := range ok {
		if r.status == code {
			var w wire.Desire
			if err := json.Unmarshal(r.body, &w); err != nil {
				return nil, fmt.Errorf("store API returned an undecodable record: %w", err)
			}
			return &w, nil
		}
	}
	return nil, s.problem(ctx, r)
}
