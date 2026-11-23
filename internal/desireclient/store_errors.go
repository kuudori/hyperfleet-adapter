package desireclient

import (
	"errors"
	"strings"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore"
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
	// The store API client writes its own error text.
	var pe *desirestore.ProblemError
	var te *desirestore.TransportError
	var ue *desirestore.UsageError
	if errors.As(e.err, &pe) || errors.As(e.err, &te) || errors.As(e.err, &ue) {
		return e.err.Error()
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

// reReadable reports whether a fresh read can resolve a store error: a version
// conflict, except a record that a timed-out delete found changed or created
// again.
func reReadable(err error) bool {
	return errors.Is(err, desire.ErrVersionConflict) && !errors.Is(err, desirestore.ErrRecreated)
}
