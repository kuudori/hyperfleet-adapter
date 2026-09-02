package transportclient

import "fmt"

// Registry associates configured transport client names with their implementations.
// It deliberately performs no routing or fallback; callers choose the client name.
type Registry map[string]TransportClient

// Get returns the transport client registered under name.
func (r Registry) Get(name string) (TransportClient, error) {
	client, ok := r[name]
	if !ok || client == nil {
		return nil, fmt.Errorf("transport client %q not configured", name)
	}

	return client, nil
}
