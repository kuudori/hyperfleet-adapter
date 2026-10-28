package transportclient

import "errors"

var (
	// ErrNotSyncedYet is returned by GetResource and DiscoverResources when the
	// transport cannot establish the object's current state yet. The executor
	// records the resource as unsynced instead of failing the event.
	ErrNotSyncedYet = errors.New("resource not synced yet")

	// ErrDeletionPending means the transport has not confirmed a deletion yet.
	// CleanupAfterDeletion returns it while it waits for confirmation, and
	// ApplyResource returns it when a pending delete of the same object blocks
	// the apply. The executor waits for a later event in both cases.
	ErrDeletionPending = errors.New("deletion pending")
)
