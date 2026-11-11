package endpointpool

import "strings"

// nodeName returns the name of the Proxmox node the endpoint serves, or empty
// when the pool does not know it. A name that SetEndpointNode recorded wins
// over the one the endpoint was built with. It is safe to call while
// SetEndpointNode runs.
func (e *endpoint) nodeName() string {
	if learned := e.learned.Load(); learned != nil {
		return *learned
	}
	return e.node
}

// SetEndpointNode records node as the name of the Proxmox node the configured
// endpoint at addr serves, so a request whose path names that node counts as
// the endpoint's own for the attribution rule and the response-header
// deadline. A discovery refresh that read the name from /cluster/status calls
// it, so the walks in the same process know the name at once, without the next
// process having to load it from the cache.
//
// It does nothing for an address the pool does not hold, for a discovered
// endpoint or a cold-start candidate, whose names come from the cache or the
// az_map, and for an empty name. A later call replaces an earlier name. The
// name is stored atomically, so a walk that reads it concurrently sees either
// the old name or the new one and takes no lock for it. The pool compares the
// name exactly, so a caller passes the spelling PVE reported.
func (p *Pool) SetEndpointNode(addr, node string) {
	node = strings.TrimSpace(node)
	if node == "" {
		return
	}
	canonical, err := canonicalAddress(addr)
	if err != nil {
		return
	}
	ep := p.byAddr[canonical]
	if ep == nil || ep.kind != KindConfigured {
		return
	}
	ep.learned.Store(&node)
}
