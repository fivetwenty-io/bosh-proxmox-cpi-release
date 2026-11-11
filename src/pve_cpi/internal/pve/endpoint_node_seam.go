package pve

import "sync/atomic"

// endpointNodesOverride maps a configured endpoint's host:port to the name of
// the Proxmox node it serves. It lets a wire test build a pool whose entries
// know their node without running discovery and a cache. Production never sets
// it: a configured endpoint gets its node from the cache file, which a
// discovery refresh writes from /cluster/status, and from the refresh itself.
// It is nil outside tests, and then only the cache names a configured
// endpoint.
var endpointNodesOverride atomic.Pointer[map[string]string]

// configuredEndpointNode returns the node the override names for the
// configured endpoint at address, or "" when there is none.
func configuredEndpointNode(address string) string {
	nodes := endpointNodesOverride.Load()
	if nodes == nil {
		return ""
	}
	return (*nodes)[address]
}

// SetEndpointNodesForTest makes every client built afterwards give each
// configured endpoint whose host:port is a key of nodes the matching node
// name, and returns a restore function. The map is copied. It exists for
// tests in other packages, which cannot reach the package's test files.
//
//	defer pve.SetEndpointNodesForTest(map[string]string{"127.0.0.1:8006": "pve1"})()
func SetEndpointNodesForTest(nodes map[string]string) func() {
	copied := make(map[string]string, len(nodes))
	for address, node := range nodes {
		copied[address] = node
	}
	prev := endpointNodesOverride.Swap(&copied)
	return func() { endpointNodesOverride.Store(prev) }
}
