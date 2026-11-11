package pve

import (
	"context"
	"fmt"
	"sync"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// clusterIdentityWatch is what one client's cache and its discovery worker
// share about the cluster identity. It remembers the identity the pool's
// discovered endpoints were loaded under, so the worker can tell that another
// process replaced it, and it says a standing identity conflict once per call
// whichever of the two finds the conflict first.
type clusterIdentityWatch struct {
	logger *log.Logger
	file   string

	mu sync.Mutex
	// loaded is the stored root CA digest the pool was built under, or the one
	// the worker's own refresh accepted since. known is false when the file
	// held no identity, in which case there is nothing for a later identity to
	// replace, because the first one a discovery records is no change.
	loaded string
	known  bool
	// warned is set once the conflict line has been logged.
	warned bool
}

// newClusterIdentityWatch returns the watch for the cache file named file,
// holding the identity of p, the payload the cache loaded. A nil payload, or
// one with no identity, leaves the watch with none.
func newClusterIdentityWatch(logger *log.Logger, file string, p *endpointcache.Payload) *clusterIdentityWatch {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	w := &clusterIdentityWatch{logger: logger, file: file}
	if p != nil && p.Cluster != nil && p.Cluster.RootCASHA256 != "" {
		w.loaded, w.known = p.Cluster.RootCASHA256, true
	}
	return w
}

// warnConflict logs the stored identity conflict at Warn the first time it is
// called and does nothing after that, so a call says it once however many
// paths find the conflict. It is safe to call concurrently.
func (w *clusterIdentityWatch) warnConflict() {
	w.mu.Lock()
	first := !w.warned
	w.warned = true
	w.mu.Unlock()
	if !first {
		return
	}
	w.logger.LogAttrs(context.Background(), log.LevelWarn,
		"endpoint cache: the cached cluster identity differs from the one the configured endpoints report and "+
			"was refused because verify_ssl is false, so no cached nodes are used; delete the cache file to "+
			"accept the new identity", log.String("file", w.file))
}

// replacedBy reports whether root, the identity another process left in the
// file, differs from the one the pool was loaded under. It returns the
// identity that was loaded. A watch that holds no identity, and an empty root,
// report no change.
func (w *clusterIdentityWatch) replacedBy(root string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.loaded, w.known && root != "" && root != w.loaded
}

// accept records root as the identity the pool now matches, which is the
// identity the worker's own refresh accepted or one it has dropped the pool's
// endpoints for, so the same identity is not found as a change a second time.
func (w *clusterIdentityWatch) accept(root string) {
	if root == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loaded, w.known = root, true
}

// logReplaced says at Debug that the worker dropped the pool's discovered
// endpoints because another process replaced the identity they were loaded
// under. The process that found the change logged it at Error, so this line
// only explains the lost endpoints.
func (w *clusterIdentityWatch) logReplaced(from, to string) {
	w.logger.LogAttrs(context.Background(), log.LevelDebug,
		fmt.Sprintf("endpoint cache: another process replaced the cluster identity %s with %s, so the discovered "+
			"endpoints and cold-start candidates loaded under the old one are dropped", shortID(from), shortID(to)),
		log.String("file", w.file))
}
