package endpointcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidChange means a writer handed Merge something it must not write,
// such as a discovery result with a node name that fails the pve-node format.
// Nothing is written. A caller logs it and drops the write.
var ErrInvalidChange = errors.New("endpoint cache: invalid cache change")

// ErrIdentityConflict means the stored file holds an identity conflict, so
// Merge refuses every discovery result. Under verify_ssl false a refused
// identity change stays recorded until the operator deletes the file, and
// discovery stops until then. Nothing is written.
var ErrIdentityConflict = errors.New("endpoint cache: stored identity conflict stays until the file is deleted")

// Trust refusals under verify_ssl false. Merge returns them as a
// *TrustRefusalError, which matches ErrTrustRefused and the one of the other
// two that applies. ErrIdentityChanged means a result's root CA differs from
// the stored one. ErrPinsChanged means a result leaves out a stored pin of a
// node the stored file knows, including a node past NodeTTL. A result that
// keeps every stored pin, and may add more, is not a refused change. Nothing
// is written.
var (
	ErrTrustRefused    = errors.New("endpoint cache: discovery result refused under verify_ssl false")
	ErrIdentityChanged = errors.New("endpoint cache: cluster identity differs from the stored one")
	ErrPinsChanged     = errors.New("endpoint cache: pin set leaves out a known node's stored pins")
)

// TrustRefusalError is a discovery result that a verify_ssl false trust rule
// refused. The caller turns an identity change into a recorded conflict with
// a Change whose Conflict is set, which stops discovery until the operator
// deletes the file. A refused pin change is written instead as a result that
// carries the node's stored pins, as DecideTrust builds it, and the caller
// logs it. The text never echoes a node name, and Nodes is for the caller's
// own log line.
type TrustRefusalError struct {
	// IdentityChanged is set when the result's root CA differs from the
	// stored one. The pins are then not compared.
	IdentityChanged bool
	// Nodes names the known nodes whose stored pins the result leaves out, in
	// the result's order.
	Nodes []string
}

// Error describes the refusal.
func (e *TrustRefusalError) Error() string {
	if e.IdentityChanged {
		return ErrTrustRefused.Error() + ": " + ErrIdentityChanged.Error()
	}
	return fmt.Sprintf("%s: %s for %d nodes", ErrTrustRefused, ErrPinsChanged, len(e.Nodes))
}

// Is matches ErrTrustRefused, and ErrIdentityChanged or ErrPinsChanged by the
// refusal's kind.
func (e *TrustRefusalError) Is(target error) bool {
	switch target {
	case ErrTrustRefused:
		return true
	case ErrIdentityChanged:
		return e.IdentityChanged
	case ErrPinsChanged:
		return !e.IdentityChanged && len(e.Nodes) > 0
	}
	return false
}

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
	// endpoint supersedes it. A change without one writes the stored
	// discovery section back as it was read, apart from the two times an
	// Attempt moves.
	Result *DiscoveryResult
	// Hint replaces the stored cold-start hint. A writer that has no hint of
	// its own leaves this nil, and the stored hint stays. A change that
	// carries both a Result and a Hint is refused, because a real file holds
	// one or the other.
	Hint *ColdStartHint
	// Conflict records a refused identity change under verify_ssl false. It
	// sets the flag on the stored identity and changes nothing else, and it
	// needs a stored identity and no Result.
	Conflict bool
	// Attempt records a discovery that ran and records no result, so the
	// stored refresh time moves and the refresh is not due again on every
	// call. It sets the stored section's discovered_at and next_refresh_after
	// and changes nothing else, so every node, pin, confirmation, the outcome,
	// and the identity stay as stored. It is accepted while a conflict stands
	// and alongside Conflict. A change that carries it with a Result or a Hint
	// is refused. When the file holds no discovery section, there is no
	// refresh time to move, and the attempt records nothing.
	Attempt *DiscoveryAttempt
}

// DiscoveryAttempt is a discovery that ran without a result to record.
type DiscoveryAttempt struct {
	// At is when the discovery ran. It becomes discovered_at, which is the
	// last attempt that RefreshDue measures the pin-failure floor from. A
	// zero time, or one more than FutureSkew ahead of the merge's clock, is
	// refused.
	At time.Time
	// NextRefreshAfter is when the next discovery is due. A time further
	// ahead than MaxRefreshWait is dropped, as validation drops it on read, so
	// the refresh is due at once.
	NextRefreshAfter time.Time
}

// Stored is the stored file as a writer's build function sees it under the
// key's lock. Both views are empty when the file is absent, invalid, or fails
// Validate, and build may change them freely.
type Stored struct {
	// Usable is what a reader may use now, the payload as Validate returns it
	// at the writer's clock. A record dropped for its time or its address is
	// left out of it, and so is every node of a conflicted identity.
	Usable *Payload
	// Known is every node the file knows, in its normalized spelling, with
	// its pins. It holds each stored node that passes the format checks and
	// is not an ambiguous name, whatever its timestamps and whether its
	// address still derives, so a node past NodeTTL is here although Usable
	// leaves it out. The trust rules read this view, because a known node's
	// pins still bind under verify_ssl false after the pool stops loading it.
	Known []DiscoveredNode
	// Now is the clock UpdatePayload was given, which the merge of the
	// returned change uses and both views were read at. A decision made in
	// build, such as DecideTrust, takes this time, so the decision and the
	// merge stamp and check one clock. It is zero in a view that
	// UpdatePayload did not build.
	Now time.Time
}

// Merge merges change into stored at time now and returns the payload to
// write, along with the records it dropped. stored is the file as read, or nil
// when there is none. A stored payload that fails Validate is treated as
// missing. Merge returns a nil payload, and no error, when there is nothing to
// write, which is a change that records nothing into a missing file.
//
// The merge keeps the newer mark per endpoint, drops marks that are MarkTTL
// old, and drops marks for endpoints the pool no longer holds, which are the
// key's endpoints, the nodes of the merged file, and change.Pool. It keeps the
// mark of every configured endpoint and at most MaxOtherMarks others, newest
// first, and when the payload would still marshal past the file's size cap,
// the oldest marks go next, configured endpoints' included, so no write fails
// for its size. It replaces the node list only with change.Result, and a change
// without one writes the stored discovery section back as it was read, so a
// record a reader drops for its time or its address is hidden and never
// erased. It keeps a stored hint that passes every other check even when the
// writer's own az_map does not name its node, because another entry that
// shares the key may have written it, and it replaces the hint only with
// change.Hint.
//
// The merge refuses every result while the stored identity holds a conflict,
// with ErrIdentityConflict. Under verify_ssl false it refuses, with a
// *TrustRefusalError, a result whose root CA differs from the stored one and
// a result that leaves out a stored pin of a node the stored file knows,
// which includes a node past NodeTTL. A known node's result may keep its
// stored pins or add to them. A known node that the result carries with its
// stored pins and its stored confirmation stays in the file even when that
// confirmation is past NodeTTL or ahead of now, so the node stays known while
// the pool no longer loads it. A node the file does not know is added on
// first use. An attempt moves the stored section's two times and nothing
// else. A change that breaks the change's own rules is refused with
// ErrInvalidChange.
func Merge(stored *Payload, key Key, change Change, now time.Time) (*Payload, []Drop, error) {
	state, drops := readState(stored, key, now)
	next, found, err := merge(state, key, change, now)
	return next, append(drops, found...), err
}

// readState reads stored as a writer does, treating a payload that fails
// validation as missing with a drop for the file. A nil stored is a missing
// file.
func readState(stored *Payload, key Key, now time.Time) (storedState, []Drop) {
	if stored == nil {
		return storedState{}, nil
	}
	state, drops, err := readStored(stored, key, now)
	if err != nil {
		return storedState{}, []Drop{{Record: "file", Reason: DropUnusable}}
	}
	return state, drops
}

// view returns the build function's copy of the state, read at now.
func (s storedState) view(now time.Time) Stored {
	return Stored{Usable: s.usable.clone(), Known: cloneNodes(s.known), Now: now}
}

// merge is Merge for a stored state that readStored returned at the same now.
func merge(state storedState, key Key, change Change, now time.Time) (*Payload, []Drop, error) {
	stored := state.usable
	next := stored.clone()
	if next == nil {
		next = &Payload{}
	}
	next.Key = key
	next.Key.Endpoints = append([]string(nil), key.Endpoints...)
	// A drop on read hides a record from use and never deletes it.
	next.Discovery = state.discovery.clone()

	var drops []Drop
	if change.Hint != nil {
		hint, found, err := validateHint(*change.Hint, now)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: cold_start name fails the pve-node format", ErrInvalidChange)
		}
		if hint == nil {
			return nil, nil, fmt.Errorf("%w: cold_start hint is %s", ErrInvalidChange, found[0].Reason)
		}
		if change.Result != nil {
			return nil, nil, fmt.Errorf("%w: change carries both a discovery result and a cold_start hint", ErrInvalidChange)
		}
		next.ColdStart = hint
	}
	if change.Conflict {
		if err := applyConflict(next, key, change); err != nil {
			return nil, nil, err
		}
	}
	if change.Attempt != nil {
		if err := applyAttempt(next, change, now, &drops); err != nil {
			return nil, nil, err
		}
	}
	if change.Result != nil {
		if err := applyResult(next, state, key, change.Result, now, &drops); err != nil {
			return nil, nil, err
		}
	}
	next.Marks = mergeMarks(next.Marks, change.Marks, now, &drops)
	pruneMarks(next, change.Pool)
	capMarks(next)
	if err := trimMarksToSize(next); err != nil {
		return nil, nil, err
	}
	if stored == nil && next.empty() {
		return nil, drops, nil
	}
	return next, drops, nil
}

// applyConflict sets the conflict flag on next's stored identity.
func applyConflict(next *Payload, key Key, change Change) error {
	switch {
	case change.Result != nil:
		return fmt.Errorf("%w: change carries both a discovery result and an identity conflict", ErrInvalidChange)
	case key.VerifySSL:
		return fmt.Errorf("%w: an identity conflict is recorded only under verify_ssl false", ErrInvalidChange)
	case next.Cluster == nil:
		return fmt.Errorf("%w: no stored identity to record a conflict on", ErrInvalidChange)
	}
	next.Cluster.IdentityConflict = true
	return nil
}

// applyAttempt moves next's stored discovery section to change.Attempt's two
// times. next.Discovery is the raw stored section, so every node, pin, and
// confirmation, and the outcome, stay as stored.
func applyAttempt(next *Payload, change Change, now time.Time, drops *[]Drop) error {
	attempt := change.Attempt
	switch {
	case change.Result != nil:
		return fmt.Errorf("%w: change carries both a discovery result and a discovery attempt", ErrInvalidChange)
	case change.Hint != nil:
		return fmt.Errorf("%w: change carries both a cold_start hint and a discovery attempt", ErrInvalidChange)
	case attempt.At.IsZero():
		return fmt.Errorf("%w: discovery attempt carries no time", ErrInvalidChange)
	case attempt.At.After(now.Add(FutureSkew)):
		return fmt.Errorf("%w: discovery attempt time is in the future", ErrInvalidChange)
	}
	if next.Discovery == nil {
		return nil
	}
	next.Discovery.DiscoveredAt = attempt.At.UTC()
	next.Discovery.NextRefreshAfter = attempt.NextRefreshAfter.UTC()
	if attempt.NextRefreshAfter.After(now.Add(MaxRefreshWait + FutureSkew)) {
		next.Discovery.NextRefreshAfter = time.Time{}
		*drops = append(*drops, Drop{Record: "next_refresh_after", Reason: DropFuture})
	}
	return nil
}

// applyResult replaces next's identity and node list with result, after the
// same checks a reader applies and the trust rules the stored state calls
// for, and removes the stored hint.
func applyResult(
	next *Payload, state storedState, key Key, result *DiscoveryResult, now time.Time, drops *[]Drop,
) error {
	if stored := state.usable; stored != nil && stored.Cluster != nil && stored.Cluster.IdentityConflict {
		return ErrIdentityConflict
	}
	if result.Cluster.IdentityConflict {
		return fmt.Errorf("%w: a discovery result carries an identity conflict", ErrInvalidChange)
	}
	cluster, found, err := validateCluster(result.Cluster, key)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
	if len(found) > 0 {
		return fmt.Errorf("%w: cluster observed_via is not a configured endpoint", ErrInvalidChange)
	}
	var held func(DiscoveredNode) bool
	if !key.VerifySSL {
		held = state.holds
	}
	section, _, found, err := validateDiscovery(&result.Discovery, keyDerivation(key), now, held)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
	if section == nil {
		return fmt.Errorf("%w: discovery time is in the future", ErrInvalidChange)
	}
	if !key.VerifySSL {
		if err := trustOnFirstUse(state, cluster, section); err != nil {
			return err
		}
	}
	*drops = append(*drops, found...)
	next.Cluster = &cluster
	next.Discovery = section
	next.ColdStart = nil
	return nil
}

// holds reports whether n is a node the stored file knows, carried with its
// stored pins and its stored confirmation, which is how DecideTrust writes a
// known node whose observed pins share none of the stored ones.
func (s storedState) holds(n DiscoveredNode) bool {
	for _, node := range s.known {
		if strings.EqualFold(node.Name, n.Name) {
			return samePins(node.Pins, n.Pins) && node.ConfirmedAt.Equal(n.ConfirmedAt)
		}
	}
	return false
}

// trustOnFirstUse applies the verify_ssl false rules to a result that would
// replace state. The first discovery records the identity and the pins. Once
// an identity is stored, a result with another root CA is refused, and so is
// a result that leaves out any stored pin of a node the file knows. A known
// node's result may keep its stored pins or add to them, which is the trust
// a node the file does not know gets when it is added on first use.
func trustOnFirstUse(state storedState, cluster ClusterIdentity, section *DiscoverySection) error {
	if state.usable == nil || state.usable.Cluster == nil {
		return nil
	}
	if cluster.RootCASHA256 != state.usable.Cluster.RootCASHA256 {
		return &TrustRefusalError{IdentityChanged: true}
	}
	known := make(map[string][]string, len(state.known))
	for _, node := range state.known {
		known[strings.ToLower(node.Name)] = node.Pins
	}
	var changed []string
	for _, node := range section.Nodes {
		if pins, ok := known[strings.ToLower(node.Name)]; ok && !holdsPins(node.Pins, pins) {
			changed = append(changed, node.Name)
		}
	}
	if len(changed) > 0 {
		return &TrustRefusalError{Nodes: changed}
	}
	return nil
}

// samePins reports whether a and b hold the same pins in any order. Both are
// in validatePins' form, lowercase and without repeats.
func samePins(a, b []string) bool {
	return len(a) == len(b) && holdsPins(a, b)
}

// holdsPins reports whether set holds every pin in pins. Both are in
// validatePins' form, lowercase and without repeats.
func holdsPins(set, pins []string) bool {
	for _, pin := range pins {
		if !slices.Contains(set, pin) {
			return false
		}
	}
	return true
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
			if normalized, ok := normalizeMarkEndpoint(net.JoinHostPort(node.Address, strconv.Itoa(node.Port))); ok {
				pool[normalized] = struct{}{}
			}
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

// capMarks keeps the mark of every configured endpoint and the MaxOtherMarks
// newest marks of the others, so a pool with many candidates cannot push the
// file past its size cap and lose the whole write. Two marks with one time
// keep the endpoint that sorts first, so the same input keeps the same marks.
func capMarks(p *Payload) {
	others := make([]string, 0, len(p.Marks))
	for endpoint := range p.Marks {
		if !slices.Contains(p.Key.Endpoints, endpoint) {
			others = append(others, endpoint)
		}
	}
	if len(others) <= MaxOtherMarks {
		return
	}
	slices.SortFunc(others, markOrder(p.Marks))
	for _, endpoint := range others[MaxOtherMarks:] {
		delete(p.Marks, endpoint)
	}
}

// markOrder sorts endpoints by the time of their mark, newest first. Two marks
// with one time sort by endpoint name.
func markOrder(marks map[string]DownMark) func(a, b string) int {
	return func(a, b string) int {
		if order := marks[b].DownAt.Compare(marks[a].DownAt); order != 0 {
			return order
		}
		return strings.Compare(a, b)
	}
}

// envelopeMargin is what the file's envelope adds to the marshaled payload,
// rounded up. The envelope is {"version":1,"hmac_sha256":"<64 hex>","payload":
// ...}, which is 105 bytes around the payload, and a test checks the margin
// against a written file.
const envelopeMargin = 128

// trimMarksToSize drops marks, oldest first, until the file the payload
// marshals into fits maxFileBytes, so a write never fails for its size. The
// mark cap bounds the other marks, but the config allows 16 endpoints of up to
// 253 characters each, and their place in the key and their marks alone can
// outgrow the file. A configured endpoint's mark is dropped like any other,
// and it comes back with the next failure. Marks are the only part the merge
// can shed, and the bounds on nodes, pins, and the key keep the rest under the
// limit. The size is the marshaled payload, which is the bytes the store
// writes, plus envelopeMargin.
func trimMarksToSize(p *Payload) error {
	size, err := payloadSize(p)
	if err != nil {
		return err
	}
	if size+envelopeMargin <= maxFileBytes || len(p.Marks) == 0 {
		return nil
	}
	oldestLast := make([]string, 0, len(p.Marks))
	for endpoint := range p.Marks {
		oldestLast = append(oldestLast, endpoint)
	}
	slices.SortFunc(oldestLast, markOrder(p.Marks))
	for i := len(oldestLast) - 1; i >= 0 && size+envelopeMargin > maxFileBytes; i-- {
		delete(p.Marks, oldestLast[i])
		if size, err = payloadSize(p); err != nil {
			return err
		}
	}
	if len(p.Marks) == 0 {
		p.Marks = nil
	}
	return nil
}

// payloadSize returns the length of p as the store marshals it.
func payloadSize(p *Payload) (int, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("%s: marshal payload: %w", cacheLabel, err)
	}
	return len(data), nil
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
// the stored file's two views, which are empty when the file is absent,
// invalid, or fails validation, and now as Stored.Now, and returns the change
// to record. A trust decision for a discovery result belongs in build, because
// it needs the stored state and the lock is what keeps that state from
// changing under it. It takes Stored.Now as its clock, and the merge applies
// the verify_ssl false rules again as Merge describes.
// build may return a change that records nothing. An error from build or from
// the merge writes nothing and is returned.
//
// The lock wait, the skipped write and its log line, and the write itself are
// Update's. A sweep failure does not fail the update, and it is reported in
// the result. The caller logs the errors and the drops.
func (s *Store) UpdatePayload(
	ctx context.Context, now time.Time, build func(stored Stored) (Change, error),
) (UpdateResult, error) {
	var result UpdateResult
	if build == nil {
		return result, fmt.Errorf("%s: update needs a build function", cacheLabel)
	}
	written, err := Update(ctx, s, func(current *Payload) (*Payload, error) {
		result.Locked = true
		result.Swept = Sweep(s.root, now, s.key.LockName(), s.key.FileName())
		state, drops := readState(current, s.key, now)
		result.Dropped = append(result.Dropped, drops...)
		change, err := build(state.view(now))
		if err != nil {
			return nil, err
		}
		next, drops, err := merge(state, s.key, change, now)
		result.Dropped = append(result.Dropped, drops...)
		return next, err
	})
	result.Written = written
	return result, err
}
