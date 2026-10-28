package transportclient

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Registry associates configured transport client names with their entries.
// It deliberately performs no fallback between names; callers choose the client name.
type Registry map[string]Entry

// Entry is one configured transport. Target builds the TransportContext for
// each call to Client; a nil Target gives the client a nil context.
type Entry struct {
	Client TransportClient
	Target TargetFunc
}

// TargetFunc builds the TransportContext for one call to a transport, from the
// resource's static GVK and the execution's template variables.
type TargetFunc func(gvk schema.GroupVersionKind, vars map[string]any) (TransportContext, error)

// Get returns the entry registered under name.
func (r Registry) Get(name string) (Entry, error) {
	entry, ok := r[name]
	if !ok || entry.Client == nil {
		return Entry{}, fmt.Errorf("transport client %q not configured", name)
	}

	return entry, nil
}
