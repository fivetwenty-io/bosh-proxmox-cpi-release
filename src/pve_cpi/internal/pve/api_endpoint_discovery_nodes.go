package pve

import (
	"context"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// statusEndpointNodes returns the name /cluster/status reported as local,
// paired with the configured endpoint that answered that read, in the key's
// spelling. It is empty when there is nothing to attribute, which is a status
// that was not read, one that marks no node local, a client that reports no
// answering endpoint, an answering endpoint that is none of the key's
// endpoints, and a name that fails the pve-node format or that a resolver could
// misread. A name from the API is never trusted as it stands, so one that the
// cache would refuse is left out here, and the refresh goes on without it.
func statusEndpointNodes(key endpointcache.Key, reads DiscoveryReads) map[string]string {
	if !reads.Status.Available || reads.Status.Local == "" {
		return nil
	}
	via, ok := keyEndpoint(key, reads.StatusVia)
	if !ok || endpointcache.ValidateNodeName(reads.Status.Local) != nil {
		return nil
	}
	return map[string]string{via: reads.Status.Local}
}

// learnEndpointNodes hands the pool the node each configured endpoint's status
// answer named, so the walks of this process attribute a node-scoped request
// to the endpoint at once, and returns the same pairs for the cache write. The
// pool applies a name whether or not the write goes on, because the name was
// read from the endpoint just now, and it is no trust decision. Debug lines say
// what the pool learned, and when a name from the API is left out because the
// cache would refuse it, which never echoes the name.
func (w *DiscoveryWorker) learnEndpointNodes(
	ctx context.Context, b DiscoveryBinding, reads DiscoveryReads,
) map[string]string {
	nodes := statusEndpointNodes(w.store.Key(), reads)
	if len(nodes) == 0 {
		if reads.Status.Available && reads.Status.Local != "" && reads.StatusVia != "" &&
			endpointcache.ValidateNodeName(reads.Status.Local) != nil {
			w.logger.LogAttrs(ctx, log.LevelDebug,
				"api endpoint discovery: the local node name from /cluster/status is not a usable node name; "+
					"no node name is recorded for the endpoint",
				log.String("endpoint", reads.StatusVia))
		}
		return nil
	}
	for endpoint, node := range nodes {
		b.Pool.SetEndpointNode(endpoint, node)
		w.logger.LogAttrs(ctx, log.LevelDebug,
			"api endpoint discovery: configured endpoint "+endpoint+" serves node "+node,
			log.String("endpoint", endpoint), log.String("node", node))
	}
	return nodes
}
