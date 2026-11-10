package endpointcache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// ErrInvalidChange means a writer handed Merge something it must not write,
// such as a discovery result with a node name that fails the pve-node format.
// Nothing is written. A caller logs it and drops the write.
var ErrInvalidChange = errors.New("endpoint cache: invalid cache change")

// ErrIdentityConflict means a discovery result would clear an identity
// conflict that the stored file holds. Under verify_ssl false a refused
// identity change stays recorded until the operator deletes the file, so no
// result replaces it. Nothing is written.
var ErrIdentityConflict = errors.New("endpoint cache: stored identity conflict stays until the file is deleted")

// DiscoveryResult is a discovery that the trust rules accepted, ready to
// replace the stored node list. It carries the identity the nodes belong to,
// so the two always change together.
type DiscoveryResult struct {
	// Cluster is the identity to store. Its root CA digest is required.
	Cluster ClusterIdentity
	// Discovery is the section to store, with its nodes in walk order.
	Discovery DiscoverySection
}

// Change is what one writer wants recorded. Every field is optional.
type Change struct {
	// Marks are endpoints that failed in the connect phase, by host:port. A
	// mark replaces a stored mark for the same endpoint only when it is not
	// older.
	Marks map[string]DownMark
	// Pool lists endpoints the writer's pool holds beyond the key's endpoints
	// and the file's nodes, so their marks survive pruning.
	Pool []string
	// Result replaces the stored identity and node list, and removes a stored
	// cold-start hint, because the next discovery through a configured
	// endpoint supersedes it.
	Result *DiscoveryResult
	// Hint replaces the stored cold-start hint. A writer that has no hint of
	// its own leaves this nil, and the stored hint stays.
	Hint *ColdStartHint
}

// Merge merges change into stored at time now and returns the payload to
// write, along with the records it dropped. stored is the file as read, or nil
// when there is none. A stored payload that fails Validate is treated as
// missing. Merge returns a nil payload, and no error, when there is nothing to
// write, which is a change that records nothing into a missing file.
//
// The merge keeps the newer mark per endpoint, drops marks that are MarkTTL
// old, and drops marks for endpoints the pool no longer holds, which are the
// key's endpoints, the nodes of the merged file, and change.Pool. It replaces
// the node list only with change.Result, and it keeps a stored hint that
// passes every other check even when the writer's own az_map does not name its
// node, because another entry that shares the key may have written it. It
// replaces the hint only with change.Hint.
func Merge(stored *Payload, key Key, change Change, now time.Time) (*Payload, []Drop, error) {
	var drops []Drop
	if stored != nil {
		valid, found, err := Validate(stored, key, now)
		if err != nil {
			valid = nil
			found = []Drop{{Record: "file", Reason: DropUnusable}}
		}
		stored, drops = valid, found
	}
	next, found, err := merge(stored, key, change, now)
	return next, append(drops, found...), err
}

// merge is Merge for a stored payload that Validate returned at the same now.
func merge(stored *Payload, key Key, change Change, now time.Time) (*Payload, []Drop, error) {
	next := stored.clone()
	if next == nil {
		next = &Payload{}
	}
	next.Key = key
	next.Key.Endpoints = append([]string(nil), key.Endpoints...)

	var drops []Drop
	if change.Result != nil {
		if err := applyResult(next, stored, change.Result, now, &drops); err != nil {
			return nil, nil, err
		}
	}
	if change.Hint != nil {
		hint, found, err := validateHint(*change.Hint, now)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: cold_start name fails the pve-node format", ErrInvalidChange)
		}
		if hint == nil {
			return nil, nil, fmt.Errorf("%w: cold_start hint is %s", ErrInvalidChange, found[0].Reason)
		}
		next.ColdStart = hint
	}
	next.Marks = mergeMarks(next.Marks, change.Marks, now, &drops)
	pruneMarks(next, change.Pool)
	if stored == nil && next.empty() {
		return nil, drops, nil
	}
	return next, drops, nil
}

// applyResult replaces next's identity and node list with result, after the
// same checks a reader applies, and removes the stored hint.
func applyResult(next, stored *Payload, result *DiscoveryResult, now time.Time, drops *[]Drop) error {
	cluster, err := validateCluster(result.Cluster)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
	section, found, err := validateDiscovery(&result.Discovery, now)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
	if section == nil {
		return fmt.Errorf("%w: discovery time is in the future", ErrInvalidChange)
	}
	if stored != nil && stored.Cluster != nil && stored.Cluster.IdentityConflict && !cluster.IdentityConflict {
		return ErrIdentityConflict
	}
	*drops = append(*drops, found...)
	next.Cluster = &cluster
	next.Discovery = section
	next.ColdStart = nil
	return nil
}

// mergeMarks returns stored with incoming merged in. incoming is checked like
// a stored mark, so a mark that is ahead, old, or malformed is dropped.
func mergeMarks(stored, incoming map[string]DownMark, now time.Time, drops *[]Drop) map[string]DownMark {
	fresh, found := validateMarks(incoming, now, nil)
	*drops = append(*drops, found...)
	if len(stored) == 0 && len(fresh) == 0 {
		return nil
	}
	out := make(map[string]DownMark, len(stored)+len(fresh))
	for endpoint, mark := range stored {
		out[endpoint] = mark
	}
	for endpoint, mark := range fresh {
		if prior, seen := out[endpoint]; !seen || !mark.DownAt.Before(prior.DownAt) {
			out[endpoint] = mark
		}
	}
	return out
}

// pruneMarks removes the marks of endpoints that are no longer in the pool.
func pruneMarks(p *Payload, extra []string) {
	if len(p.Marks) == 0 {
		return
	}
	pool := make(map[string]struct{}, len(p.Key.Endpoints)+len(extra))
	for _, endpoint := range append(append([]string(nil), p.Key.Endpoints...), extra...) {
		if normalized, ok := normalizeMarkEndpoint(endpoint); ok {
			pool[normalized] = struct{}{}
		}
	}
	if p.Discovery != nil {
		for _, node := range p.Discovery.Nodes {
			pool[net.JoinHostPort(node.Address, strconv.Itoa(node.Port))] = struct{}{}
		}
	}
	for endpoint := range p.Marks {
		if _, held := pool[endpoint]; !held {
			delete(p.Marks, endpoint)
		}
	}
	if len(p.Marks) == 0 {
		p.Marks = nil
	}
}

// ReadPayload reads the store's file and returns the payload a reader may use.
// It validates the payload against the store's key and drops the hint when the
// reader's az_map, whose node names are azNodes, does not name its node.
// Readers take no lock.
//
// An absent file returns an error matching fs.ErrNotExist. A file that fails
// the store's checks or Validate returns an error matching ErrFileInvalid,
// and the caller treats it as missing, logs at Debug, and lets the next
// successful write replace it. A file that fails the private-file checks
// returns that refusal, and the caller runs without a cache. The drops are the
// records that were left out and the file stood.
func (s *Store) ReadPayload(now time.Time, azNodes []string) (*Payload, []Drop, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("%s: store is closed", cacheLabel)
	}
	var stored Payload
	if err := s.Read(&stored); err != nil {
		return nil, nil, err
	}
	valid, drops, err := Validate(&stored, s.key, now)
	if err != nil {
		return nil, nil, err
	}
	valid, found := ForReader(valid, azNodes)
	return valid, append(drops, found...), nil
}

// UpdateResult reports what UpdatePayload did.
type UpdateResult struct {
	// Locked is whether the key's lock was taken. It is false when the wait
	// ran out and the write was skipped, and then nothing else is set.
	Locked bool
	// Written is whether the file was replaced.
	Written bool
	// Dropped are the records validation or the merge left out.
	Dropped []Drop
	// Swept is the sweep's outcome.
	Swept SweepResult
}

// UpdatePayload reads, merges, and writes the store's file under the key's
// lock, and sweeps the cache directory under the same lock. build receives
// the stored payload as Validate returns it, or nil when the file is absent,
// invalid, or fails validation, and returns the change to record. A trust
// decision for a discovery result belongs in build, because it needs the
// stored state and the lock is what keeps that state from changing under it.
// build may return a change that records nothing. An error from build or from
// the merge writes nothing and is returned.
//
// The lock wait, the skipped write and its log line, and the write itself are
// Update's. A sweep failure does not fail the update, and it is reported in
// the result. The caller logs the errors and the drops.
func (s *Store) UpdatePayload(
	ctx context.Context, now time.Time, build func(stored *Payload) (Change, error),
) (UpdateResult, error) {
	var result UpdateResult
	if build == nil {
		return result, fmt.Errorf("%s: update needs a build function", cacheLabel)
	}
	written, err := Update(ctx, s, func(current *Payload) (*Payload, error) {
		result.Locked = true
		result.Swept = Sweep(s.root, now, s.key.LockName(), s.key.FileName())
		var stored *Payload
		if current != nil {
			valid, drops, err := Validate(current, s.key, now)
			if err != nil {
				drops = []Drop{{Record: "file", Reason: DropUnusable}}
			} else {
				stored = valid
			}
			result.Dropped = append(result.Dropped, drops...)
		}
		change, err := build(stored)
		if err != nil {
			return nil, err
		}
		next, drops, err := merge(stored, s.key, change, now)
		result.Dropped = append(result.Dropped, drops...)
		return next, err
	})
	result.Written = written
	return result, err
}
