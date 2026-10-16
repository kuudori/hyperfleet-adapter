package desireclient

import (
	"errors"
	"strings"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

// storeError keeps a store error's identity for errors.Is but replaces its
// text. The applier's store errors name desires ("desire: owner conflict"),
// and a wrapped error reaches adapter authors through
// adapter.executionError.message.
type storeError struct {
	err error
}

func (e *storeError) Error() string {
	switch {
	case errors.Is(e.err, desire.ErrOwnerConflict):
		return "owner conflict"
	case errors.Is(e.err, desire.ErrVersionConflict):
		return "version conflict"
	case errors.Is(e.err, desire.ErrDeletePending):
		return "delete pending"
	case errors.Is(e.err, desire.ErrAlreadyExists):
		return "already exists"
	case errors.Is(e.err, desire.ErrNotFound):
		return "not found"
	case errors.Is(e.err, desire.ErrAborted):
		return "aborted"
	}
	// Other errors wrap a cause such as a Redis network error. Report the
	// outermost error in the chain whose text does not name desires.
	for err := e.err; err != nil; err = errors.Unwrap(err) {
		if msg := err.Error(); !strings.Contains(strings.ToLower(msg), "desire") {
			return msg
		}
	}
	return "store request failed"
}

func (e *storeError) Unwrap() error {
	return e.err
}

// storeErr wraps an error returned by the store or by desire.CheckOwner.
func storeErr(err error) error {
	if err == nil {
		return nil
	}
	return &storeError{err: err}
}
