package pve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

const (
	// discoveryWorkerName names the worker in the drain's result.
	discoveryWorkerName = "discovery"
	// discoveryFailureWait is how long a refresh that read no result waits
	// before the next one is due. It is the interval of a refresh that found
	// nodes, so a cluster whose reads keep failing is asked once an hour, as a
	// healthy one is, and not on every call.
	discoveryFailureWait = endpointcache.RefreshInterval
	// discoveryLogNameLimit is the most node names one discovery line lists
	// before it counts the rest.
	discoveryLogNameLimit = 10
	// discoveryShortID is how many hex characters of a root CA digest a log
	// line shows, which is the length of the cluster stamp.
	discoveryShortID = 8
)

// errDiscoveryWorkerStore refuses a worker built without a cache store.
var errDiscoveryWorkerStore = errors.New("api endpoint discovery: the worker needs an open cache store")

// errDiscoveryWorkerClient refuses a binding without a client.
var errDiscoveryWorkerClient = errors.New("api endpoint discovery: the worker needs a client to send its reads")

// errDiscoveryWorkerPool refuses a binding without a pool.
var errDiscoveryWorkerPool = errors.New(
	"api endpoint discovery: the worker needs the pool it is the first-answer hook of")

// errDiscoveryWorkerBound refuses a second binding.
var errDiscoveryWorkerBound = errors.New("api endpoint discovery: the worker is already bound to a client")

// DiscoveryWorkerConfig is what a discovery worker needs for one cache key.
type DiscoveryWorkerConfig struct {
	// Store is the open cache store of the key. The worker reads it, takes its
	// discover lock, and writes through it. The caller keeps owning it and
	// closes it after the exit drain. The drain cancels a worker that is still
	// running and does not wait for it to return, so the store may be closed
	// while a cancelled worker is still unwinding. That is safe. The store
	// guards its directory against Close, a worker that finds its context
	// cancelled returns before it touches the store, and one that meets the
	// closed store after the cancel returns without a line, because the drain
	// reports the cancelled worker.
	Store *endpointcache.Store
	// Mode is the configured discovery mode of the cluster config the store
	// serves, from CPIConfig.DiscoveryModeValue, not the effective mode. It
	// sets the level of the lines the failure handling rules give Debug, which
	// become Warn under an explicit "on", and the level at which the drain
	// reports the worker if it has to cancel it.
	Mode config.DiscoveryMode
	// Logger receives the worker's lines. Nil discards them.
	Logger *log.Logger
	// Resolver checks the derived node names. Nil uses the system resolver.
	Resolver NameResolver
	// Registry runs the worker. Nil uses the process registry that the exit
	// drain waits for, which is what the client uses.
	Registry *endpointcache.Registry
	// Now is the worker's clock, which stamps the refresh. Nil uses time.Now.
	Now func() time.Time
	// Identity is the cluster identity the pool was loaded under and the
	// once-per-call conflict line, shared with the cache that loaded the
	// pool. Nil gives the worker a watch that holds no identity, so it
	// compares nothing against the file and says a conflict once itself.
	Identity *clusterIdentityWatch
}

// DiscoveryPool is what the worker needs from the endpoint pool it is the
// first-answer hook of. *endpointpool.Pool implements it.
type DiscoveryPool interface {
	// RefreshDue reports whether a discovered endpoint failed its pin in this
	// process. A pin failure in another process reaches the worker through
	// the pin_failed_at time in the cache file's discovery section, which
	// endpointcache.RefreshDue reads.
	RefreshDue() bool
	// DropDiscovered takes every discovered endpoint out of the pool for the
	// rest of the call.
	DropDiscovered() int
	// SetEndpointNode records the node a configured endpoint serves, so the
	// pool attributes a node-scoped request to it from then on. It ignores an
	// address that is no configured endpoint of the pool and an empty name.
	SetEndpointNode(addr, node string)
}

// The endpoint pool is the worker's pool.
var _ DiscoveryPool = (*endpointpool.Pool)(nil)

// DiscoveryBinding is what the worker learns once the client and its pool
// exist, which is after the pool was handed the worker as its first-answer
// hook.
type DiscoveryBinding struct {
	// Client sends the discovery reads. Its requests go through Pool, so the
	// configured-only marker the worker puts on every request keeps them on
	// the configured endpoints.
	Client Client
	// Pool is the pool the worker is the first-answer hook of. The worker
	// reads its pin failures to decide whether a refresh is due, and it drops
	// the pool's discovered endpoints when the trust rules say they belong to
	// an identity the configured endpoints no longer report.
	Pool DiscoveryPool
}

// DiscoveryWorker refreshes the discovered node list of one cache key in the
// background. It is the pool's first-answer hook. The first answer from a
// configured endpoint starts one refresh on a goroutine that the worker
// registry tracks, so the exit drain waits for it or cancels it, and the hook
// returns at once.
//
// The refresh runs only when endpointcache.RefreshDue says it is due, and
// only in the process that holds the key's discover lock. It sends every
// request marked configured-only, builds the result, applies the trust rules
// inside the store's locked update with the store's clock, and logs the
// discovery lines. A DiscoveryWorker is safe for concurrent use.
//
// The client builds a worker with newDiscoveryWorker, hands it to the pool as
// endpointpool.Config.FirstAnswer, builds the client on that pool, and then
// calls bind with the client and the pool. A first answer that arrives before
// bind is kept, and bind starts its refresh.
type DiscoveryWorker struct {
	store    *endpointcache.Store
	mode     config.DiscoveryMode
	logger   *log.Logger
	resolver NameResolver
	start    func(name string, mode config.DiscoveryMode, fn func(ctx context.Context)) error
	now      func() time.Time
	// identity is the identity the pool was loaded under, which the worker
	// compares with the file before it checks whether a refresh is due.
	identity *clusterIdentityWatch

	mu      sync.Mutex
	binding *DiscoveryBinding
	pending string
	started bool

	// beforeWrite runs inside the locked update once the trust decision is
	// made and before the store writes. It is nil outside tests, which use it
	// to cut a write off with the drain or to make it fail.
	beforeWrite func(ctx context.Context)
}

// The worker is the pool's first-answer hook.
var _ endpointpool.FirstAnswerHook = (*DiscoveryWorker)(nil)

// newDiscoveryWorker returns a worker for the store's key. It refuses a nil
// store.
func newDiscoveryWorker(cfg DiscoveryWorkerConfig) (*DiscoveryWorker, error) {
	if cfg.Store == nil {
		return nil, errDiscoveryWorkerStore
	}
	w := &DiscoveryWorker{
		store:    cfg.Store,
		mode:     cfg.Mode,
		logger:   cfg.Logger,
		resolver: cfg.Resolver,
		start:    endpointcache.Go,
		now:      cfg.Now,
		identity: cfg.Identity,
	}
	if w.logger == nil {
		w.logger = log.NewNopLogger()
	}
	if w.identity == nil {
		w.identity = newClusterIdentityWatch(w.logger, cfg.Store.FileName(), nil)
	}
	if w.resolver == nil {
		w.resolver = lookupHostResolver(nil)
	}
	if cfg.Registry != nil {
		w.start = cfg.Registry.Go
	}
	if w.now == nil {
		w.now = time.Now
	}
	return w, nil
}

// bind gives the worker the client and the pool. It refuses a binding without
// a client, one without a pool, and a second binding. When a configured
// endpoint already answered, bind starts the refresh that answer asked for.
func (w *DiscoveryWorker) bind(b DiscoveryBinding) error {
	if b.Client == nil {
		return errDiscoveryWorkerClient
	}
	if b.Pool == nil {
		return errDiscoveryWorkerPool
	}
	w.mu.Lock()
	if w.binding != nil {
		w.mu.Unlock()
		return errDiscoveryWorkerBound
	}
	w.binding = &b
	via := ""
	if w.pending != "" && !w.started {
		w.started = true
		via = w.pending
	}
	w.mu.Unlock()
	if via != "" {
		w.launch(via, b)
	}
	return nil
}

// FirstAnswer starts the refresh on its own goroutine and returns at once. The
// pool calls it once, with the canonical host:port of the configured endpoint
// that answered, and the worker starts at most one refresh whatever the number
// of calls. Before bind it only keeps the endpoint.
func (w *DiscoveryWorker) FirstAnswer(endpoint string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	if w.binding == nil {
		if w.pending == "" {
			w.pending = endpoint
		}
		w.mu.Unlock()
		return
	}
	w.started = true
	b := *w.binding
	w.mu.Unlock()
	w.launch(endpoint, b)
}

// launch registers the refresh with the worker registry. A registry whose
// drain has started refuses it, because the call is already exiting.
func (w *DiscoveryWorker) launch(via string, b DiscoveryBinding) {
	err := w.start(discoveryWorkerName, w.mode, func(ctx context.Context) {
		w.refresh(ctx, via, b)
	})
	if err != nil {
		w.logger.LogAttrs(context.Background(), log.LevelDebug,
			"api endpoint discovery: refresh not started", log.Err(err))
	}
}

// refresh is one background refresh. It checks that the refresh is due, takes
// the discover lock without waiting, checks again under it, because another
// process may have finished a refresh in between, and then discovers.
func (w *DiscoveryWorker) refresh(ctx context.Context, via string, b DiscoveryBinding) {
	// A worker the drain cancelled before it started has nothing to do, and
	// the caller may be closing the store.
	if ctx.Err() != nil {
		return
	}
	source, ok := keyEndpoint(w.store.Key(), via)
	if !ok {
		w.logger.LogAttrs(ctx, endpointcache.Level(w.mode),
			"api endpoint discovery: the answering endpoint is not one of the configured endpoints; "+
				"skipping the refresh",
			log.String("endpoint", via))
		// The identity rules do not depend on which endpoint answered, so a
		// pool that holds discovered endpoints still drops them.
		w.dropOnStoredIdentity(b)
		return
	}
	if !w.due(ctx, b) {
		return
	}
	lock, held, err := w.store.TryLockDiscovery(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.LogAttrs(ctx, endpointcache.Level(w.mode),
				"api endpoint discovery: could not take the discover lock; skipping the refresh", log.Err(err))
		}
		return
	}
	if !held {
		w.logger.LogAttrs(ctx, log.LevelDebug,
			"api endpoint discovery: another process holds the discover lock; skipping the refresh",
			log.String("file", w.store.FileName()))
		return
	}
	defer func() {
		if err := lock.Close(); err != nil {
			w.logger.LogAttrs(ctx, log.LevelDebug, "api endpoint discovery: release the discover lock", log.Err(err))
		}
	}()
	if !w.due(ctx, b) {
		return
	}
	w.discover(ctx, source, b)
}

// due reads the stored file without a lock and reports whether a refresh is
// due. A missing file, or one that fails validation, makes it due. A file it
// cannot read for another reason, such as a failed ownership check, makes no
// refresh due, because the write would fail the same way. A read that fails
// after the context ended, such as one that meets a store the caller closed
// behind a drain, says nothing, because the drain reports the cancelled
// worker. A pin failure in another process counts through the file's
// pin_failed_at, which endpointcache.RefreshDue reads, and through a pin mark
// the reader still loads, which lives two minutes and so is always younger
// than the attempt the refresh floor waits for.
func (w *DiscoveryWorker) due(ctx context.Context, b DiscoveryBinding) bool {
	now := w.now()
	stored, _, err := w.store.ReadPayload(now, nil)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, endpointcache.ErrFileInvalid):
		stored = nil
	default:
		if ctx.Err() == nil {
			w.logger.LogAttrs(ctx, endpointcache.Level(w.mode),
				"api endpoint discovery: could not read the cache file; skipping the refresh",
				log.String("file", w.store.FileName()), log.Err(err))
		}
		return false
	}
	// The stored identity is checked before the refresh rule, because no
	// refresh is due while a conflict stands, and a fresh file that another
	// process replaced makes none due either, while the pool this process
	// loaded before that still holds discovered endpoints. DropDiscovered is
	// idempotent, so the second check under the lock drops nothing more.
	if w.reconcileIdentity(stored, b) {
		return false
	}
	if !endpointcache.RefreshDue(stored, now, pinMarked(stored) || b.Pool.RefreshDue()) {
		w.logger.LogAttrs(ctx, log.LevelDebug, "api endpoint discovery: no refresh due",
			log.String("file", w.store.FileName()))
		return false
	}
	return true
}

// dropOnStoredIdentity reads the stored file and takes the discovered
// endpoints out of the pool when it holds a standing identity conflict or an
// identity other than the one the pool was loaded under. It is the check due
// makes, for a refresh that ends before due runs. A file it cannot read says
// nothing about the identity, and the drain reports a read cut off by the end
// of the call.
func (w *DiscoveryWorker) dropOnStoredIdentity(b DiscoveryBinding) {
	stored, _, err := w.store.ReadPayload(w.now(), nil)
	if err == nil {
		w.reconcileIdentity(stored, b)
	}
}

// reconcileIdentity compares the stored file with the identity the pool was
// loaded under, and takes the pool's discovered endpoints and cold-start
// candidates out when the file no longer matches it. A standing conflict is
// how another process tells this one that the cluster the endpoints came from
// is no longer the one it trusts, and it is said once per call at Warn. An
// identity that another process replaced under verify_ssl true is the same
// news, and the process that replaced it already logged it at Error. It
// reports whether a conflict stands, which is the case in which no refresh
// is due.
func (w *DiscoveryWorker) reconcileIdentity(stored *endpointcache.Payload, b DiscoveryBinding) bool {
	if standingConflict(stored) {
		w.identity.warnConflict()
		b.Pool.DropDiscovered()
		return true
	}
	if stored == nil || stored.Cluster == nil {
		return false
	}
	root := stored.Cluster.RootCASHA256
	if from, replaced := w.identity.replacedBy(root); replaced {
		b.Pool.DropDiscovered()
		w.identity.logReplaced(from, root)
		w.identity.accept(root)
	}
	return false
}

// standingConflict reports whether the file records a cluster identity
// conflict, which is how another process tells this one that the cluster the
// pool's discovered endpoints came from is no longer the one it trusts.
func standingConflict(p *endpointcache.Payload) bool {
	return p != nil && p.Cluster != nil && p.Cluster.IdentityConflict
}

// pinMarked reports whether the file holds a pin mark a reader still loads,
// which is how a pin failure in another process makes a refresh due while its
// mark lives. Only a discovered endpoint is ever marked for its pin.
func pinMarked(p *endpointcache.Payload) bool {
	if p == nil {
		return false
	}
	for _, mark := range p.Marks {
		if mark.Class == endpointcache.MarkPin {
			return true
		}
	}
	return false
}

// keyEndpoint returns the key's spelling of the configured endpoint addr
// normalizes to, and false when addr is none of the key's endpoints.
func keyEndpoint(key endpointcache.Key, addr string) (string, bool) {
	want := normalizedEndpoint(addr)
	if want == "" {
		return "", false
	}
	for _, endpoint := range key.Endpoints {
		if normalizedEndpoint(endpoint) == want {
			return endpoint, true
		}
	}
	return "", false
}

// discover sends the reads, builds the result, and records it. The log lines
// compare the view the update itself reads under the file lock, which is the
// only view that cannot change between the comparison and the write, so
// discover takes nothing from the due check.
func (w *DiscoveryWorker) discover(ctx context.Context, via string, b DiscoveryBinding) {
	key := w.store.Key()
	requests := endpointpool.WithConfiguredOnly(ctx)
	reads, err := ReadDiscovery(requests, b.Client)
	if err != nil {
		w.recordFailure(ctx, via, err)
		return
	}
	endpointNodes := w.learnEndpointNodes(ctx, b, reads)
	result, err := buildDiscoveryResult(requests, reads, DiscoveryInput{
		Seed:       key.Endpoints[0],
		Configured: key.Endpoints,
		Resolver:   w.resolver,
	})
	if err != nil {
		w.recordFailure(ctx, via, err)
		return
	}
	w.logBuildDrops(ctx, result.Dropped)
	via = w.answeringEndpoint(ctx, via, reads)
	obs, err := discoveryObservation(via, result)
	if err != nil {
		w.logDropped(ctx, err)
		return
	}
	obs.EndpointNodes = endpointNodes
	w.recordResult(ctx, b, reads, result, obs)
}

// answeringEndpoint returns the key's spelling of the configured endpoint that
// answered the identity read, which is the evidence's source, and not the
// endpoint whose answer began the refresh, because the reads walk the
// configured endpoints and can hop when one goes down. A client that reports
// no answering endpoint, or one that is none of the key's endpoints, leaves
// triggered, the endpoint that began the refresh. A run whose reads were
// answered by more than one configured endpoint is logged at Debug.
func (w *DiscoveryWorker) answeringEndpoint(ctx context.Context, triggered string, reads DiscoveryReads) string {
	via, ok := keyEndpoint(w.store.Key(), reads.RootVia)
	if !ok {
		return triggered
	}
	if len(reads.AnsweredBy) > 1 || (len(reads.AnsweredBy) == 1 && normalizedEndpoint(reads.AnsweredBy[0]) !=
		normalizedEndpoint(via)) {
		w.logger.LogAttrs(ctx, log.LevelDebug,
			"api endpoint discovery: the reads were answered by more than one configured endpoint; the "+
				"cluster identity came from "+via,
			log.String("endpoint", via),
			log.String("answered_by", strings.Join(reads.AnsweredBy, ",")),
			log.String("triggered_by", triggered))
	}
	return via
}

// recordFailure handles a refresh that ended without a result. A refresh the
// drain cut off writes and logs nothing more, because the drain reports it.
// Any other failure records an attempt that stores the failure's reason as
// the section's last_failure and moves the next refresh out by
// discoveryFailureWait, even on a file with no discovery yet, so a cluster
// whose discovery keeps failing is asked once an hour and not on every call.
// The stored outcome, nodes, and pins stay as they were. The line is Info when
// the reason differs from the last_failure the stored section records, which
// is the outcome changing for these failures, and otherwise at the failure
// level. The stored failure is the raw section's, as the update reads it under
// the file lock, so a section a reader hides because the clock stepped back
// still counts as the failure it records.
//
// An attempt that never reached the file, because the write failed or the
// lock wait ran out, promises no wait, since the next call asks again. Its
// line is at the failure level, because the update read no stored section to
// tell whether the outcome changed, and the store or logDropped has already
// said why nothing was written.
func (w *DiscoveryWorker) recordFailure(ctx context.Context, via string, cause error) {
	failure := discoveryFailure(cause)
	var prior endpointcache.DiscoveryFailure
	res, err := w.store.UpdatePayload(ctx, w.now(), func(view endpointcache.Stored) (endpointcache.Change, error) {
		prior = view.LastFailure
		at := view.Now.UTC()
		return endpointcache.Change{Attempt: &endpointcache.DiscoveryAttempt{
			At: at, NextRefreshAfter: at.Add(discoveryFailureWait), Failure: failure,
		}}, nil
	})
	if err != nil && ctx.Err() != nil {
		return
	}
	if err != nil {
		w.logDropped(ctx, err)
	}
	if res.Locked {
		w.logUpdate(ctx, res)
	}
	attrs := []slog.Attr{
		log.String("file", w.store.FileName()), log.String("failure", string(failure)), log.Err(cause),
	}
	if err != nil || !res.Locked {
		w.logger.LogAttrs(ctx, endpointcache.Level(w.mode),
			fmt.Sprintf("api endpoint discovery: no result through %s, because %s; the attempt was not recorded, "+
				"so the next call tries again", via, discoveryFailureReason(cause)),
			attrs...)
		return
	}
	level := endpointcache.Level(w.mode)
	if failure != prior {
		level = log.LevelInfo
	}
	w.logger.LogAttrs(ctx, level,
		fmt.Sprintf("api endpoint discovery: no result through %s, because %s; next attempt in %s",
			via, discoveryFailureReason(cause), waitText(discoveryFailureWait)),
		attrs...)
}

// discoveryFailure names the failure the cache records for a refresh that
// ended with err before it had a result. A failed GET /nodes is told from one
// that listed no nodes by the read error that wraps the failed request.
func discoveryFailure(err error) endpointcache.DiscoveryFailure {
	var readErr *discoveryReadError
	switch {
	case errors.Is(err, ErrDiscoveryNodeName):
		return endpointcache.FailureNodeNameInvalid
	case errors.Is(err, ErrDiscoveryNodes) && errors.As(err, &readErr):
		return endpointcache.FailureNodesUnreadable
	case errors.Is(err, ErrDiscoveryNodes):
		return endpointcache.FailureNodesEmpty
	case errors.Is(err, ErrDiscoveryRoot):
		return endpointcache.FailureRootCAUnreadable
	case errors.Is(err, context.DeadlineExceeded):
		return endpointcache.FailureReadsTimedOut
	}
	return endpointcache.FailureReadsFailed
}

// discoveryFailureReason names the read that failed, in fixed text that never
// echoes an API answer.
func discoveryFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrDiscoveryNodeName):
		return "a node name from GET /nodes is not a valid pve-node label"
	case errors.Is(err, ErrDiscoveryNodes):
		return "GET /nodes gave no usable node list"
	case errors.Is(err, ErrDiscoveryRoot):
		return "the cluster root CA could not be read"
	case errors.Is(err, context.DeadlineExceeded):
		return "the discovery reads ran out of time"
	case errors.Is(err, ErrDiscoveryInput):
		return "the seed endpoint cannot derive node names"
	}
	return "the discovery reads failed"
}

// discoverySummary is what the summary line compares, taken from the one view
// the update reads under the file lock. before is the set of nodes the last
// refresh left a reader with, which is the stored nodes a reader would load at
// the stored section's own time, and after is the set a reader loads from the
// result at the update's clock. Both leave out a node the file only keeps, so
// a node never counts on a side where a reader hides it, and a node that ages
// out between two refreshes shows as a change in the refresh that notices it.
// outcome is the outcome of the section a reader would load before the write,
// which is empty when there is none, and priorFailure is the stored section's
// last_failure as written.
type discoverySummary struct {
	before, after []endpointcache.DiscoveredNode
	outcome       endpointcache.DiscoveryOutcome
	priorFailure  endpointcache.DiscoveryFailure
}

// summarize reads the summary's two node lists and the prior failure from view,
// the stored file as the update sees it, and from d, the change the decision
// builds on it.
func summarize(view endpointcache.Stored, key endpointcache.Key, d endpointcache.TrustDecision) discoverySummary {
	sum := discoverySummary{priorFailure: view.LastFailure}
	if stored := view.Usable; stored != nil && stored.Discovery != nil {
		sum.outcome = stored.Discovery.Outcome
		if stored.Cluster != nil {
			section := *stored.Discovery
			section.Nodes = view.Known
			sum.before = readerNodes(key, *stored.Cluster, section, section.DiscoveredAt)
		}
	}
	if d.Change.Result != nil {
		sum.after = readerNodes(key, d.Change.Result.Cluster, d.Change.Result.Discovery, view.Now)
	}
	return sum
}

// readerNodes returns the nodes of section that a reader would load at the
// time at, which are the ones that pass the checks a read applies. A node that
// a verify_ssl false refusal carries with its stored confirmation stays in the
// file after 24 hours, and a reader hides it, so it is left out here. A
// section that Validate refuses outright, which the merge never writes, counts
// as holding the nodes it lists, because that is the best account of what the
// file would hold.
func readerNodes(
	key endpointcache.Key, cluster endpointcache.ClusterIdentity, section endpointcache.DiscoverySection, at time.Time,
) []endpointcache.DiscoveredNode {
	usable, _, err := endpointcache.Validate(
		&endpointcache.Payload{Key: key, Cluster: &cluster, Discovery: &section}, key, at)
	if err != nil || usable == nil || usable.Discovery == nil {
		return section.Nodes
	}
	return usable.Discovery.Nodes
}

// recordResult applies the trust rules to obs inside the store's locked update
// and logs what the decision and the write call for.
func (w *DiscoveryWorker) recordResult(
	ctx context.Context, b DiscoveryBinding, reads DiscoveryReads, result DiscoveryResult,
	obs endpointcache.Observation,
) {
	key := w.store.Key()
	var (
		decision endpointcache.TrustDecision
		decided  bool
		sum      discoverySummary
	)
	res, err := w.store.UpdatePayload(ctx, w.now(), func(view endpointcache.Stored) (endpointcache.Change, error) {
		d, err := endpointcache.DecideTrust(view, key, obs, view.Now)
		if err != nil {
			return endpointcache.Change{}, err
		}
		decision, decided = d, true
		sum = summarize(view, key, d)
		if w.beforeWrite != nil {
			w.beforeWrite(ctx)
		}
		return d.Change, nil
	})
	if decided {
		w.settleIdentity(b, decision, err == nil)
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.logDropped(ctx, err)
	}
	if !res.Locked {
		return
	}
	w.logUpdate(ctx, res)
	if !decided {
		return
	}
	w.logDecision(ctx, decision, key.VerifySSL)
	if res.Written && decision.Change.Result != nil {
		w.logDiscovery(ctx, sum, decision, reads, result, key.Endpoints[0])
	}
}

// settleIdentity drops the pool's discovered endpoints when the decision says
// to, and also when the identity the decision accepted is not the one the pool
// was loaded under, which is the case when another process replaced the
// identity between the worker's last check and its locked update. The update
// then sees the other process's identity as stored and decides nothing
// changed. An identity the decision refused is not accepted, so the pool keeps
// the identity it was loaded under. written is whether the update succeeded,
// because the file holds an identity the pool matches only then.
func (w *DiscoveryWorker) settleIdentity(b DiscoveryBinding, d endpointcache.TrustDecision, written bool) {
	accepted := d.Outcome != endpointcache.TrustIdentityRefused && d.Outcome != endpointcache.TrustConflictStands
	if d.DropDiscovered {
		b.Pool.DropDiscovered()
	} else if from, replaced := w.identity.replacedBy(d.ObservedRootCA); replaced && accepted {
		b.Pool.DropDiscovered()
		w.identity.logReplaced(from, d.ObservedRootCA)
	}
	if accepted && written {
		w.identity.accept(d.ObservedRootCA)
	}
}

// logDecision logs the trust decision's own lines. The line of a standing
// conflict is the watch's, which says it once per call whichever path finds
// the conflict first.
func (w *DiscoveryWorker) logDecision(ctx context.Context, d endpointcache.TrustDecision, verifySSL bool) {
	switch d.Outcome {
	case endpointcache.TrustConflictStands:
		w.identity.warnConflict()
		return
	case endpointcache.TrustIdentityRefused, endpointcache.TrustIdentityReplaced:
		what := "with verify_ssl true the cached nodes and pins are replaced"
		if !verifySSL {
			what = "with verify_ssl false this is refused, delete the cache file to accept it"
		}
		w.logger.LogAttrs(ctx, log.LevelError,
			fmt.Sprintf("endpoint cache: cluster identity changed from %s to %s through configured endpoint %s; %s",
				shortID(d.StoredRootCA), shortID(d.ObservedRootCA), d.Via, what),
			log.String("file", w.store.FileName()), log.String("outcome", string(d.Outcome)))
		return
	}
	if d.Outcome == endpointcache.TrustPinsRefused && len(d.PinsChanged) > 0 {
		w.logger.LogAttrs(ctx, log.LevelWarn,
			fmt.Sprintf("endpoint cache: the pin set of known %s %s changed through configured endpoint %s; "+
				"with verify_ssl false the stored pins stay, delete the cache file to accept the new ones",
				nodeNoun(len(d.PinsChanged)), nameList(d.PinsChanged), d.Via),
			log.String("file", w.store.FileName()), log.String("outcome", string(d.Outcome)))
	}
	if len(d.PinsShrunk) > 0 {
		w.logger.LogAttrs(ctx, log.LevelDebug,
			fmt.Sprintf("endpoint cache: known %s %s reported fewer pins than stored through configured endpoint %s; "+
				"the stored pins stay", nodeNoun(len(d.PinsShrunk)), nameList(d.PinsShrunk), d.Via),
			log.String("file", w.store.FileName()))
	}
}

// logDiscovery logs the line of a written result. A result that holds nodes
// logs at Info when the node set, the identity, or the outcome changed, and a
// degraded result logs at Info when the outcome changed. A result that
// follows a failed refresh, which the stored last_failure records, changes
// the outcome too, so the recovery is logged at Info as the failure was.
// Otherwise a result with a status logs at Debug and every degraded one at the
// failure level. The node set and the count are the nodes a reader loads, from
// sum, so a node the file only keeps never counts or changes the set.
func (w *DiscoveryWorker) logDiscovery(
	ctx context.Context, sum discoverySummary, d endpointcache.TrustDecision,
	reads DiscoveryReads, result DiscoveryResult, seed string,
) {
	written := d.Change.Result.Discovery
	changed := sum.outcome != written.Outcome || sum.priorFailure != ""
	attrs := []slog.Attr{log.String("file", w.store.FileName()), log.String("outcome", string(written.Outcome))}
	switch written.Outcome {
	case endpointcache.OutcomeOK, endpointcache.OutcomeNoStatus:
		level := log.LevelDebug
		if written.Outcome == endpointcache.OutcomeNoStatus {
			level = endpointcache.Level(w.mode)
		}
		if changed || d.StoredRootCA != d.ObservedRootCA || nodeSetChanged(sum.before, sum.after) {
			level = log.LevelInfo
		}
		w.logger.LogAttrs(ctx, level,
			fmt.Sprintf("api endpoint discovery: cached %d of %d nodes of cluster %s via %s (%s)",
				len(sum.after), len(reads.Nodes), shortID(d.ObservedRootCA), d.Via, statusText(reads.Status)),
			attrs...)
	default:
		level := endpointcache.Level(w.mode)
		if changed {
			level = log.LevelInfo
		}
		why := degradedText(written.Outcome, result, reads, d, seed)
		wait := waitText(written.NextRefreshAfter.Sub(written.DiscoveredAt))
		w.logger.LogAttrs(ctx, level,
			fmt.Sprintf("api endpoint discovery: %s; next attempt in %s", why, wait), attrs...)
	}
}

// degradedText says why a result holds no nodes.
func degradedText(
	outcome endpointcache.DiscoveryOutcome, result DiscoveryResult, reads DiscoveryReads,
	d endpointcache.TrustDecision, seed string,
) string {
	switch outcome {
	case endpointcache.OutcomeStandalone:
		return fmt.Sprintf("%s is a standalone host with no peers", d.Via)
	case endpointcache.OutcomePublicSuffix:
		return fmt.Sprintf("the domain of %s is a public suffix, so no node names are derived under it", seed)
	}
	var unresolved []string
	for _, drop := range result.Dropped {
		if drop.Reason != DropUnresolved {
			continue
		}
		name := drop.Name
		if address, err := endpointcache.DeriveAddress(seed, drop.Name); err == nil {
			if host, _, err := net.SplitHostPort(address); err == nil {
				name = host
			}
		}
		unresolved = append(unresolved, name)
	}
	if len(unresolved) > 0 {
		return fmt.Sprintf("%d of %d derived names did not resolve (%s)",
			len(unresolved), len(unresolved)+len(result.Nodes), nameList(unresolved))
	}
	return fmt.Sprintf("no node of the %d that cluster %s lists via %s is a usable candidate",
		len(reads.Nodes), shortID(d.ObservedRootCA), d.Via)
}

// nodeSetChanged reports whether the nodes a reader loads after the write
// differ, by name, from those it loaded before. A node's address is a function
// of its name under the key's seed, which the file's key fixes, so the name
// says everything the address would.
func nodeSetChanged(before, after []endpointcache.DiscoveredNode) bool {
	return !slices.Equal(cachedNodeNameSet(before), cachedNodeNameSet(after))
}

// cachedNodeNameSet returns the lowercase names of nodes, sorted.
func cachedNodeNameSet(nodes []endpointcache.DiscoveredNode) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, strings.ToLower(node.Name))
	}
	slices.Sort(names)
	return names
}

// statusText describes the cluster status a result was ordered by.
func statusText(status DiscoveryStatus) string {
	if !status.Available {
		return "no cluster status"
	}
	if !status.Quorate {
		return "not quorate"
	}
	online := 0
	for _, up := range status.Online {
		if up {
			online++
		}
	}
	return fmt.Sprintf("quorate, %d online", online)
}

// discoveryObservation turns a built result into the observation the trust
// rules judge. via is the configured endpoint the reads went through.
func discoveryObservation(via string, result DiscoveryResult) (endpointcache.Observation, error) {
	outcome, err := cacheOutcome(result.Outcome)
	if err != nil {
		return endpointcache.Observation{}, err
	}
	nodes := make([]endpointcache.ObservedNode, 0, len(result.Nodes))
	for _, node := range result.Nodes {
		pins := make([][sha256.Size]byte, 0, len(node.Pins))
		for _, pin := range node.Pins {
			pins = append(pins, [sha256.Size]byte(pin))
		}
		nodes = append(nodes, endpointcache.ObservedNode{
			Name: node.Name, Address: node.Address, Pins: pins, Online: node.Online,
		})
	}
	return endpointcache.Observation{
		Via:     via,
		RootCA:  result.RootCA,
		Outcome: outcome,
		Quorate: result.Quorate,
		Nodes:   nodes,
	}, nil
}

// cacheOutcome maps a built result's outcome to the file's enumeration.
func cacheOutcome(outcome DiscoveryOutcome) (endpointcache.DiscoveryOutcome, error) {
	switch outcome {
	case DiscoveryOK:
		return endpointcache.OutcomeOK, nil
	case DiscoveryNoStatus:
		return endpointcache.OutcomeNoStatus, nil
	case DiscoveryNoCandidates:
		return endpointcache.OutcomeNoCandidates, nil
	case DiscoveryStandalone:
		return endpointcache.OutcomeStandalone, nil
	case DiscoveryPublicSuffix:
		return endpointcache.OutcomePublicSuffix, nil
	}
	return "", fmt.Errorf("%w: discovery outcome %q has no cache value",
		endpointcache.ErrInvalidChange, string(outcome))
}

// logBuildDrops logs one Debug line for each node the builder left out.
func (w *DiscoveryWorker) logBuildDrops(ctx context.Context, dropped []DroppedNode) {
	for _, drop := range dropped {
		w.logger.LogAttrs(ctx, log.LevelDebug,
			fmt.Sprintf("api endpoint discovery: left out node %s, because %s", drop.Name, dropReasonText(drop.Reason)),
			log.String("reason", string(drop.Reason)))
	}
}

// dropReasonText says why the builder left a node out.
func dropReasonText(reason DropReason) string {
	switch reason {
	case DropAmbiguousName:
		return "a resolver could read its name as a loopback or numeric address"
	case DropDuplicateName:
		return "an earlier row used its name"
	case DropSeedNode:
		return "it is the node that answered"
	case DropNoFingerprint:
		return "GET /nodes gave no usable ssl_fingerprint for it"
	case DropPublicSuffix:
		return "its derived name is outside the seed's registrable domain"
	case DropUnaddressable:
		return "its derived address is not a legal endpoint"
	case DropConfigured:
		return "its address is a configured endpoint"
	case DropUnresolved:
		return "its derived name did not resolve"
	case DropOverCap:
		return "the result already holds the most nodes it can"
	}
	return string(reason)
}

// logDropped logs a refresh whose write was dropped, at the failure level.
func (w *DiscoveryWorker) logDropped(ctx context.Context, err error) {
	w.logger.LogAttrs(ctx, endpointcache.Level(w.mode),
		"api endpoint discovery: the cache write failed; the discovery result is dropped",
		log.String("file", w.store.FileName()), log.Err(err))
}

// logUpdate logs the records the update left out and what its sweep could
// not remove, at Debug.
func (w *DiscoveryWorker) logUpdate(ctx context.Context, res endpointcache.UpdateResult) {
	if len(res.Dropped) > 0 {
		drops := make([]string, 0, len(res.Dropped))
		for _, drop := range res.Dropped {
			drops = append(drops, drop.String())
		}
		w.logger.LogAttrs(ctx, log.LevelDebug, "endpoint cache: records left out: "+strings.Join(drops, "; "),
			log.String("file", w.store.FileName()))
	}
	if res.Swept.Err != nil {
		w.logger.LogAttrs(ctx, log.LevelDebug, "endpoint cache: sweep could not remove every stale file",
			log.Err(res.Swept.Err))
	}
}

// shortID returns the first eight hex characters of a root CA digest, or
// "none" when there is no digest.
func shortID(digest string) string {
	if digest == "" {
		return "none"
	}
	return digest[:min(len(digest), discoveryShortID)]
}

// nodeNoun returns "node" or "nodes" for n.
func nodeNoun(n int) string {
	if n == 1 {
		return "node"
	}
	return "nodes"
}

// nameList joins names with commas, listing at most discoveryLogNameLimit and
// counting the rest.
func nameList(names []string) string {
	if len(names) <= discoveryLogNameLimit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:discoveryLogNameLimit], ", "),
		len(names)-discoveryLogNameLimit)
}

// waitText formats a refresh wait in whole hours or minutes when it has no
// remainder, such as 24h or 10m.
func waitText(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d > 0 && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}
