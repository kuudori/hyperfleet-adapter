package executor

import (
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
)

// testTransportRegistry registers a client under the default Kubernetes transport name.
func testTransportRegistry(client transportclient.TransportClient) transportclient.Registry {
	return transportclient.Registry{configloader.TransportClientKubernetes: {Client: client}}
}
