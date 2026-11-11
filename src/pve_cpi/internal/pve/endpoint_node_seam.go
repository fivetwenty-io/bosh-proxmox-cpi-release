package pve

import "sync/atomic"

// endpointNodesOverride maps a configured endpoint's host:port to the name of
// the Proxmox node it serves. It stands in for what discovery records once it
// exists, so a wire test can build a pool whose entries know their node. It is
// nil outside tests, and then no configured endpoint carries a node.
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
