// Package desireclient implements transportclient.TransportClient against the
// desire-store contract from github.com/openshift-hyperfleet/hyperfleet-applier.
// It is the producer half of desire-based delivery: the adapter writes intent
// (apply/delete desires) and reads mirrored status (read desires) through the
// store, while a separate applier reconciles that intent against the target
// cluster. See docs/adapter-authoring-guide.md's "Desire transport" section
// for the contract this client implements.
package desireclient

import (
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

// Client implements transportclient.TransportClient against a desire store.
type Client struct {
	store desire.SpecStore
	owner string
}

// NewClient builds a desire-backed transport client. store is the producer
// surface of the desire contract (never StatusStore, which is the applier's
// own status-writing surface). owner identifies this adapter as the writer
// for ownership/CAS checks on the store.
func NewClient(store desire.SpecStore, owner string) *Client {
	return &Client{store: store, owner: owner}
}

var _ transportclient.TransportClient = (*Client)(nil)
