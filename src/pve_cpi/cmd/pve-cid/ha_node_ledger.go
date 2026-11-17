package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A read addressed to one node goes through the PVE API node that answers,
// which proxies it to the named node. When that node is gone, every attempt
// waits for the proxy to give up, and the retry budget turns one read into
// minutes. The doctor makes such reads for each replication job, each storage,
// and each VM config, and the cpi-config form repeats them for every entry
// that reaches the same cluster. So the doctor skips a per-node read when it
// already knows the node cannot answer, and it reports the read as unchecked
// with the reason instead.
//
// It knows this in two ways. The profile it reads first says which members
// cluster status lists offline, and whether the cluster has quorum. And a
// read that failed on a node with a transport fault earlier in the same run
// marks that node for the rest of the run, across every entry that reaches the
// same cluster. Only reads go through here, so the doctor stays read-only.

// haNodeLedger holds what one doctor run learned about nodes that cannot
// answer. It is shared by every entry of the run, and it is keyed by the
// cluster's identity, which is the sorted list of its member names, so two
// entries that reach the same cluster share what each learned.
type haNodeLedger struct {
	mu sync.Mutex
	// failed maps a cluster key to its nodes and the read that failed on each.
	failed map[string]map[string]string
	// guests maps a cluster key to a guest listing that failed because the
	// cluster has lost quorum, which every entry on that cluster would repeat.
	guests map[string]error
}

func newHANodeLedger() *haNodeLedger {
	return &haNodeLedger{failed: map[string]map[string]string{}, guests: map[string]error{}}
}

// failure returns the reason a read of node on cluster key failed earlier in
// this run, and false when none did.
func (l *haNodeLedger) failure(key, node string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	reason, ok := l.failed[key][node]
	return reason, ok
}

// recordFailure marks node on cluster key as unreachable for the rest of the
// run. The first failure is kept, since it is the one the operator should see.
func (l *haNodeLedger) recordFailure(key, node, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed[key] == nil {
		l.failed[key] = map[string]string{}
	}
	if _, ok := l.failed[key][node]; !ok {
		l.failed[key][node] = reason
	}
}

func (l *haNodeLedger) guestFailure(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.guests[key]
}

func (l *haNodeLedger) recordGuestFailure(key string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.guests[key]; !ok {
		l.guests[key] = err
	}
}

// haClusterKey identifies a cluster by the sorted names of its members.
func haClusterKey(nodes []ha.NodeFacts) string {
	names := make([]string, 0, len(nodes))
	for i := range nodes {
		names = append(names, nodes[i].Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// haSkippedReadError is what a per-node read returns when the doctor did not
// send it. Its text names the node and the reason, and the collector puts it
// in the unchecked finding for that read.
type haSkippedReadError struct {
	node   string
	reason string
}

func (e *haSkippedReadError) Error() string {
	return fmt.Sprintf("node %s was not asked, because %s", e.node, e.reason)
}

// ledgerHAReader wraps an HAReader and skips per-node reads on nodes the run
// already knows cannot answer.
type ledgerHAReader struct {
	HAReader
	ledger *haNodeLedger

	// key, offline, and quorumLost come from this entry's profile.
	key        string
	offline    map[string]bool
	quorumLost bool
}

// newLedgerHAReader wraps r so its per-node reads consult and feed ledger.
func newLedgerHAReader(r HAReader, ledger *haNodeLedger) HAReader {
	return &ledgerHAReader{HAReader: r, ledger: ledger}
}

// Profile reads the profile and learns from it which members cluster status
// lists offline and whether the cluster has lost quorum.
func (r *ledgerHAReader) Profile(ctx context.Context) (*ha.Profile, error) {
	p, err := r.HAReader.Profile(ctx)
	if err != nil || p == nil {
		return p, err
	}
	r.key = haClusterKey(p.Nodes)
	r.quorumLost = p.Clustered && !p.Quorate
	r.offline = map[string]bool{}
	for i := range p.Nodes {
		if !p.Nodes[i].Online {
			r.offline[p.Nodes[i].Name] = true
		}
	}
	return p, nil
}

// skip returns why a read of node should not be sent, or an empty string.
func (r *ledgerHAReader) skip(node string) string {
	if r.offline[node] {
		if r.quorumLost {
			return pve.ReasonNotQuorate + " and lists it offline"
		}
		return "cluster status lists it offline"
	}
	if r.key == "" {
		return ""
	}
	if reason, ok := r.ledger.failure(r.key, node); ok {
		return "an earlier read of it in this run failed: " + reason
	}
	return ""
}

// learn records a per-node read that failed with a transport fault, which says
// the node did not answer. An answer from PVE, such as a missing grant on one
// object, says nothing about the node, so it is not recorded.
func (r *ledgerHAReader) learn(node string, err error) {
	if err == nil || r.key == "" || !pve.IsTransientTransport(err) {
		return
	}
	r.ledger.recordFailure(r.key, node, err.Error())
}

// Guests lists the cluster's guests. When the cluster has lost quorum and an
// earlier entry of this run on the same cluster already failed to list them
// for that reason, the earlier failure is returned instead of a new listing.
func (r *ledgerHAReader) Guests(ctx context.Context) ([]pve.GuestRef, []string, error) {
	if r.quorumLost && r.key != "" {
		if err := r.ledger.guestFailure(r.key); err != nil {
			return nil, nil, fmt.Errorf("an earlier entry in this run already failed to list them: %w", err)
		}
	}
	guests, excluded, err := r.HAReader.Guests(ctx)
	if err != nil && r.key != "" {
		var enumeration *pve.GuestEnumerationError
		if errors.As(err, &enumeration) {
			for _, node := range enumeration.NoQuorum {
				r.ledger.recordFailure(r.key, node, pve.ReasonNotQuorate+" and its guest listing failed")
			}
			if r.quorumLost && len(enumeration.NoQuorum) > 0 {
				r.ledger.recordGuestFailure(r.key, err)
			}
		}
	}
	return guests, excluded, err
}

func (r *ledgerHAReader) GuestConfig(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if reason := r.skip(node); reason != "" {
		return nil, &haSkippedReadError{node: node, reason: reason}
	}
	cfg, err := r.HAReader.GuestConfig(ctx, node, vmid)
	r.learn(node, err)
	return cfg, err
}

func (r *ledgerHAReader) ReplicaStatus(ctx context.Context, node, id string) (ha.ReplicaSyncStatus, error) {
	if reason := r.skip(node); reason != "" {
		return ha.ReplicaSyncStatus{}, &haSkippedReadError{node: node, reason: reason}
	}
	st, err := r.HAReader.ReplicaStatus(ctx, node, id)
	r.learn(node, err)
	return st, err
}

func (r *ledgerHAReader) StorageVolumes(ctx context.Context, node, storage string) (map[string]bool, error) {
	if reason := r.skip(node); reason != "" {
		return nil, &haSkippedReadError{node: node, reason: reason}
	}
	volumes, err := r.HAReader.StorageVolumes(ctx, node, storage)
	r.learn(node, err)
	return volumes, err
}
