package handlers

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// defaultOverrideStorageInfoTTL mirrors the 60s TTL cmd/cpi/main.go uses for
// the job-level StorageInfoCache, so a per-request overridden client's
// storage-classification freshness matches the job-level client's.
const defaultOverrideStorageInfoTTL = 60 * time.Second

// defaultMaxCachedBundles bounds RequestOverrideRuntime's bundle cache (see
// M3 in the A13 review). Real deployments have a small, fixed number of
// distinct cpi-config entries (a handful of clusters at most), so this
// comfortably covers every legitimate identity while still bounding a
// director/templating bug that varies a keyed override field per request.
const defaultMaxCachedBundles = 16

// RequestOverrideRuntime builds and caches everything a per-request pve_*
// config override (see config.ApplyContextOverrides) needs to actually serve
// a dispatched request against a different PVE cluster than the job-level
// config: a PVE client, boot agent, and storage backend resolver, all built
// from the request's effective CPIConfig instead of the job-level one.
//
// This closes a live defect: BOSH's cpi-config feature lets a director
// register multiple named CPI entries (e.g. two `type: pve` blocks pointing
// at distinct Proxmox clusters), all served by ONE running instance of this
// CPI binary. The director merges each entry's `properties:` hash into the
// JSON-RPC request context per dispatched request, but until this mechanism
// existed nothing in the CPI read those keys back out — every request ran
// against whichever pve.* config the process happened to be launched with,
// silently uploading stemcells / creating VMs on the wrong cluster while
// reporting success.
//
// Constructed once at process startup (cmd/cpi/main.go) and shared by every
// handler via Deps.Overrides. A nil *RequestOverrideRuntime (the zero value
// of Deps.Overrides — e.g. every handler unit test's Deps literal, which
// never sets this field) disables per-request overrides entirely:
// Deps.WithRequestOverrides then returns its receiver unchanged regardless
// of what the request context carries, matching every test's existing
// expectations and every pre-feature release's behavior.
type RequestOverrideRuntime struct {
	// ClientFactory builds a PVE client from one request's effective config.
	// main.go supplies a closure over pve.NewClientWithTracer and the
	// process's resolved tracer (nil when OTel tracing is disabled), so an
	// overridden request's client is decorated identically to the job-level
	// client built at startup.
	ClientFactory func(cfg *config.CPIConfig, logger *log.Logger) (pve.Client, error)

	// StorageInfoTTL mirrors main.go's job-level StorageInfoCache TTL. Zero
	// (the default for a Deps literal that never sets Overrides.StorageInfoTTL
	// explicitly) falls back to defaultOverrideStorageInfoTTL.
	StorageInfoTTL time.Duration

	// Logger is used only for the constructed pve.Client's own internal
	// debug logging (auth-method selection, TLS-verify warning) and the boot
	// agent's logging — never for the observability log line
	// WithRequestOverrides itself emits, which uses the REQUEST's logger
	// (deps.Log(ctx)) so it carries request_id/method/trace correlation.
	Logger *log.Logger

	// BaseHost is the job-level pve.host. An override bundle inherits the
	// job-level node_endpoints map only when its effective host equals this
	// value: the map's node names belong to the job-level cluster, and an
	// override routed at a different cluster must not dial the job-level
	// cluster's addresses for same-named nodes. Discovery (gated on the
	// effective config's verify_ssl) still applies either way, since it asks
	// the override's own cluster.
	BaseHost string

	// MaxCachedBundles caps the number of distinct override identities (see
	// requestOverrideCacheKey) whose PVE client/agent/resolver bundle stays
	// cached at once. Each bundle holds a live pve.Client with its own
	// connection pool; an unbounded cache would let a director/templating
	// bug that varies a keyed field (e.g. pve_host) per request grow this map
	// without limit, leaking connections until process exit (M3, A13
	// review). Eviction is least-recently-used. Zero/negative (the default
	// for a Deps literal that never sets this explicitly) falls back to
	// defaultMaxCachedBundles.
	MaxCachedBundles int

	mu       sync.Mutex
	entries  map[string]*list.Element // key -> element in lru; element.Value is *overrideCacheEntry
	lru      *list.List               // front = most recently used, back = least recently used
	building map[string]*buildCall    // key -> in-progress build, single-flighted (see resolve/M4)
}

// buildCall tracks one in-progress bundle build for a single override
// identity, letting every concurrent resolve() call for that SAME identity
// (a cold-start race — the identity was not yet cached) wait on the ONE
// build already underway instead of each independently constructing its own
// PVE client (M4, A13 review: without this, N concurrent first requests for
// a newly-referenced cluster would open N separate client connections,
// N-1 of which are immediately discarded once the cache converges).
type buildCall struct {
	done   chan struct{}
	bundle *overrideBundle
	err    error
}

// overrideBundle is one cached (PVE client, boot agent, backend resolver)
// set for a single distinct effective-config identity (see
// requestOverrideCacheKey). Built at most once per identity while it remains
// in the cache; a subsequent request whose overrides resolve to the same
// identity reuses it, unless it was evicted (see MaxCachedBundles) in the
// meantime, in which case it is rebuilt.
type overrideBundle struct {
	client        pve.Client
	agent         agent.Agent
	resolver      pve.BackendResolver
	nodeEndpoints *pve.NodeEndpointResolver
}

// overrideCacheEntry is the value stored in each lru list element, carrying
// its own key alongside the bundle so eviction (which starts from the list,
// not the map) can find and delete the corresponding map entry.
type overrideCacheEntry struct {
	key    string
	bundle *overrideBundle
}

// acquire atomically decides, in ONE locked critical section, which of three
// things applies to key: already cached (returns the bundle), a build
// already in flight (returns that buildCall, follower), or neither (registers
// the caller as the new build leader and returns that buildCall).
//
// This MUST be a single critical section rather than two separate ones (a
// cache check, then a separate in-flight-or-register check) — an earlier
// version of this code did exactly that, and it is racy: between the cache
// check returning a miss and the second lock acquisition, a full concurrent
// build for the SAME key can complete end to end (build, cache-insert,
// clear the in-flight marker), so by the time the second check runs it also
// finds no in-flight build and wrongly registers as a SECOND leader —
// producing a spurious duplicate build despite the cache having a fresh,
// valid entry by then. Combining both checks under one lock acquisition
// closes that gap: whichever of "cached" or "in-flight" is true is observed
// atomically with respect to every other acquire() call. Safe for
// concurrent use.
func (r *RequestOverrideRuntime) acquire(key string) (bundle *overrideBundle, call *buildCall, isLeader bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if elem, ok := r.entries[key]; ok {
		r.lru.MoveToFront(elem)
		return elem.Value.(*overrideCacheEntry).bundle, nil, false //nolint:forcetypeassert // element.Value is always *overrideCacheEntry; this package is the only writer.
	}
	if r.building == nil {
		r.building = make(map[string]*buildCall)
	}
	if existing, ok := r.building[key]; ok {
		return nil, existing, false
	}
	call = &buildCall{done: make(chan struct{})}
	r.building[key] = call
	return nil, call, true
}

// cachePutOrExisting inserts bundle for key, evicting the least-recently-used
// entry first if the cache is at capacity. The only caller is
// finishBuild(), and single-flighting in resolve() (see buildCall/acquire)
// means, in the normal case, no OTHER concurrent build for the same key can
// already be racing to insert here — but the already-cached branch is kept
// as defense-in-depth (e.g. against a future caller of this method that does
// not go through the single-flight path) so cachePutOrExisting never
// overwrites an existing entry and always returns whichever bundle ends up
// cached for key. Safe for concurrent use.
func (r *RequestOverrideRuntime) cachePutOrExisting(key string, bundle *overrideBundle) *overrideBundle {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*list.Element)
		r.lru = list.New()
	}
	if elem, ok := r.entries[key]; ok {
		r.lru.MoveToFront(elem)
		return elem.Value.(*overrideCacheEntry).bundle //nolint:forcetypeassert // see acquire.
	}

	maxEntries := r.MaxCachedBundles
	if maxEntries <= 0 {
		maxEntries = defaultMaxCachedBundles
	}
	for r.lru.Len() >= maxEntries {
		oldest := r.lru.Back()
		if oldest == nil {
			break
		}
		r.lru.Remove(oldest)
		delete(r.entries, oldest.Value.(*overrideCacheEntry).key) //nolint:forcetypeassert // see acquire.
	}

	elem := r.lru.PushFront(&overrideCacheEntry{key: key, bundle: bundle})
	r.entries[key] = elem
	return bundle
}

// resolve returns the cached bundle for cfg's override identity, building
// and caching one on first use. Concurrent resolve() calls for the SAME
// identity that race a cold cache single-flight onto ONE build (see
// buildCall/M4): the first caller becomes the "leader" and performs the
// build; every other concurrent caller for that identity blocks on the
// leader's result instead of independently building its own PVE client and
// discarding it once the cache converges. Concurrent resolve() calls for
// DIFFERENT identities never block each other — only same-identity callers
// share a buildCall, and the leader's build itself runs outside r.mu.
//
// ctx bounds the live PVE calls the build path makes (client construction
// itself issues no API call — auth is lazy — but agent.ResolveISOStorage may
// issue one /storage list call when pve.iso_storage_follow_vm_storage is
// enabled) — it is the dispatched request's own ctx, so a build blocked on a
// dead overridden cluster is bounded by whatever timeout/cancellation
// already governs that request (the operation-timeout envelope when
// enabled, process shutdown otherwise), not by an unbounded background
// timer. Only the LEADER's ctx governs the actual build; a follower that is
// itself canceled while waiting returns its own ctx error immediately — its
// cancellation is not propagated into the leader's in-flight build, since
// doing so would abort the build for every other waiting follower too.
//
// Failure modes:
//   - r is nil → returns an error rather than a nil-pointer panic. Guarded
//     defensively; Deps.WithRequestOverrides never calls this on a nil
//     receiver (it checks d.Overrides == nil first and returns early).
//   - r.ClientFactory is nil → returns an error naming the missing wiring.
//     Unreachable from cmd/cpi's production wiring (main.go always sets it
//     whenever it sets Deps.Overrides at all), but a Deps built by hand
//     (e.g. a future test harness) with a non-nil Overrides and no
//     ClientFactory fails loudly instead of nil-dereferencing.
//   - the client factory itself errors (bad host, no credentials) → wrapped
//     and returned to the leader AND every follower waiting on this same
//     buildCall; nothing is cached for this identity, so the NEXT resolve()
//     call (leader or follower alike, once this buildCall's followers have
//     all observed the shared error) retries the build from scratch —
//     transient unavailability at the overridden cluster is never cached as
//     a permanent failure.
//   - agent.NewAgent errors (e.g. cfg.AgentMode outside cloudinit/noagent) →
//     wrapped and returned, nothing cached, same retry-on-next-call
//     behavior as the client-factory failure above.
func (r *RequestOverrideRuntime) resolve(ctx context.Context, cfg *config.CPIConfig) (*overrideBundle, error) {
	if r == nil {
		return nil, fmt.Errorf("cpi: RequestOverrideRuntime.resolve called on nil receiver")
	}
	if r.ClientFactory == nil {
		return nil, fmt.Errorf("cpi: RequestOverrideRuntime: ClientFactory is not configured")
	}

	key := requestOverrideCacheKey(cfg)

	bundle, call, isLeader := r.acquire(key)
	if bundle != nil {
		return bundle, nil
	}
	if !isLeader {
		// Follower: wait for the leader's build to finish (success or
		// failure) rather than starting a redundant build of our own. The
		// ctx arm is an escape hatch, not a build abort: a cancelled
		// follower stops waiting, but the leader's build continues for the
		// other followers.
		select {
		case <-call.done:
			return call.bundle, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// finishBuild must run even when buildBundle panics: it is the only place
	// that closes call.done and clears r.building[key], so skipping it would
	// park every follower forever and turn every future request for this
	// identity into a follower of a dead buildCall — a permanent per-identity
	// wedge that outlives the dispatcher's own panic recovery. The recover
	// converts the panic into an ordinary build error (shared with followers,
	// nothing cached, next call retries) and the panic is NOT re-raised — the
	// error path already reports it.
	var built *overrideBundle
	var err error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				built, err = nil, fmt.Errorf("cpi: override bundle build panicked: %v", rec)
			}
		}()
		built, err = r.buildBundle(ctx, cfg)
	}()
	r.finishBuild(key, call, built, err)
	return built, err
}

// finishBuild records the leader's build result onto call, caching a
// successful bundle (evicting the least-recently-used entry first if the
// cache is at capacity — see MaxCachedBundles), clears the in-flight marker
// for key so a subsequent resolve() call starts a fresh build attempt
// (whether this one succeeded — a later call is now a cache hit via
// acquire() — or failed — a later call retries from scratch), and wakes every
// follower waiting on call.done. Safe for concurrent use.
func (r *RequestOverrideRuntime) finishBuild(key string, call *buildCall, bundle *overrideBundle, err error) {
	if err == nil {
		bundle = r.cachePutOrExisting(key, bundle)
	}
	r.mu.Lock()
	delete(r.building, key)
	r.mu.Unlock()
	call.bundle, call.err = bundle, err
	close(call.done)
}

// buildBundle performs the actual (network-bound) construction of a PVE
// client, boot agent, and backend resolver for cfg. Called only by the
// single build leader for a given identity (see resolve/acquire); never
// holds r.mu for the duration of this call, so a slow or unreachable
// overridden cluster never blocks resolve() calls for OTHER identities —
// only same-identity followers, who are waiting on this exact result anyway.
func (r *RequestOverrideRuntime) buildBundle(ctx context.Context, cfg *config.CPIConfig) (*overrideBundle, error) {
	logger := r.Logger
	if logger == nil {
		logger = log.NewNopLogger()
	}

	client, err := r.ClientFactory(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("build PVE client: %w", err)
	}

	// Mirror main.go's boot-agent construction (see cmd/cpi/main.go's
	// cfgForBoot) and its ISOStorage resolution (agent.ResolveISOStorage), so
	// an overridden request's agent behaves identically to the job-level one,
	// modulo the overridden connection/storage/node identity itself. Both
	// paths take the normalized mode from config.BootAgentMode.
	cfgForBoot := *cfg
	cfgForBoot.AgentMode = cfg.BootAgentMode()
	cfgForBoot.ISOStorage = agent.ResolveISOStorage(ctx, &cfgForBoot, client, logger)

	// See RequestOverrideRuntime.BaseHost: the explicit node_endpoints map
	// names nodes of the job-level cluster, so it applies only when this
	// bundle targets that same host.
	explicitEndpoints := cfg.NodeEndpoints
	if cfg.Host != r.BaseHost {
		explicitEndpoints = nil
	}
	nodeEndpoints := pve.NewNodeEndpointResolver(client, explicitEndpoints, cfg.Host, !cfg.VerifySSLValue(), logger)

	bootAgent, err := agent.NewAgent(&cfgForBoot, client, nodeEndpoints, logger)
	if err != nil {
		return nil, fmt.Errorf("build boot agent: %w", err)
	}

	ttl := r.StorageInfoTTL
	if ttl <= 0 {
		ttl = defaultOverrideStorageInfoTTL
	}
	storageInfoCache := pve.NewStorageInfoCache(client.ClusterStorage(), ttl)
	backendResolver := pve.NewBackendResolver(client, storageInfoCache, cfg.Node)

	return &overrideBundle{client: client, agent: bootAgent, resolver: backendResolver, nodeEndpoints: nodeEndpoints}, nil
}

// requestOverrideCacheKey derives a stable identity string from exactly the
// CPIConfig fields config.ApplyContextOverrides can change (see the
// unexported contextOverrideFieldOrder slice in
// internal/config/context_overrides.go), so two requests
// whose overrides resolve to the same effective connection/routing config
// share one cached bundle, regardless of how many distinct *CPIConfig copies
// ApplyContextOverrides allocated to get there. Password and APIToken are
// hashed (sha256) rather than embedded verbatim — the key is never logged
// today, but hashing costs nothing and keeps that true even if a future
// caller logs cache diagnostics.
func requestOverrideCacheKey(cfg *config.CPIConfig) string {
	h := sha256.New()
	// hash.Hash.Write never returns an error, so the Fprintf results are discarded.
	_, _ = fmt.Fprintf(h, "host=%s\x00port=%d\x00user=%s\x00realm=%s\x00node=%s\x00",
		cfg.Host, cfg.Port, cfg.User, cfg.Realm, cfg.Node)
	_, _ = fmt.Fprintf(h, "vm_storage=%s\x00disk_storage=%s\x00stemcell_storage=%s\x00iso_storage=%s\x00",
		cfg.VMStorage, cfg.DiskStorage, cfg.StemcellStorage, cfg.ISOStorage)
	_, _ = fmt.Fprintf(h, "bridge=%s\x00verify_ssl=%t\x00vmid_start=%d\x00vmid_end=%d\x00",
		cfg.NetworkBridge, cfg.VerifySSLValue(), cfg.VMIDRangeStart, cfg.VMIDRangeEnd)
	_, _ = fmt.Fprintf(h, "disk_vmid_start=%d\x00disk_vmid_end=%d\x00tmpl_vmid_start=%d\x00tmpl_vmid_end=%d\x00",
		cfg.DiskVMIDRangeStart, cfg.DiskVMIDRangeEnd,
		cfg.StemcellTemplateVMIDRangeStart, cfg.StemcellTemplateVMIDRangeEnd)
	_, _ = fmt.Fprintf(h, "parked_vmid_start=%d\x00parked_vmid_end=%d\x00detached_disk_strategy=%s\x00",
		cfg.ParkedDiskVMIDRangeStart, cfg.ParkedDiskVMIDRangeEnd,
		cfg.DetachedDiskStrategyValue())
	_, _ = fmt.Fprintf(h, "disk_migration=%s\x00", cfg.DiskMigrationValue())
	_, _ = fmt.Fprintf(h, "replicate_local=%t\x00vm_prefix=%s\x00",
		cfg.StemcellReplicateLocal, cfg.VMPrefix)
	_, _ = fmt.Fprintf(h, "agent_mode=%s\x00vm_disk_format=%s\x00agent_mbus=%s\x00",
		cfg.AgentMode, cfg.VMDiskFormat, cfg.AgentMBus)
	// Placement is a nested block rather than a scalar, so it is folded in as
	// its canonical JSON. encoding/json sorts map keys, which makes this
	// deterministic across runs for the az_map. A marshal error is impossible
	// for this struct (no channels, funcs, or cyclic values) but is folded in
	// rather than ignored so an unexpected one can never collapse two distinct
	// placements onto one key.
	if placementJSON, perr := json.Marshal(cfg.Placement); perr == nil {
		_, _ = fmt.Fprintf(h, "placement=%s\x00", placementJSON)
	} else {
		_, _ = fmt.Fprintf(h, "placement_marshal_error=%s\x00", perr.Error())
	}
	pwHash := sha256.Sum256([]byte(cfg.Password))
	tokHash := sha256.Sum256([]byte(cfg.APIToken))
	_, _ = fmt.Fprintf(h, "password_sha=%s\x00api_token_sha=%s",
		hex.EncodeToString(pwHash[:]), hex.EncodeToString(tokHash[:]))
	return hex.EncodeToString(h.Sum(nil))
}

// WithRequestOverrides returns a per-request copy of d with Config, PVE,
// Agent, and Resolver rebound to a request-scoped effective CPIConfig (see
// config.ApplyContextOverrides) whenever reqCtx.Extra carries pve_* context
// properties — the mechanism BOSH's cpi-config feature uses to route one
// dispatched request at a PVE cluster other than this process's job-level
// pve.* config (see the RequestOverrideRuntime doc for the full defect this
// closes).
//
// The common case — no pve_* keys in reqCtx.Extra, or d.Overrides is nil
// (disabled) — returns d completely UNCHANGED: same Config/PVE/Agent/
// Resolver values, zero extra work beyond the initial nil/empty checks. This
// is what makes a single-CPI deployment (or any handler unit test's Deps
// literal, which never sets Overrides) byte-identical to every release
// before this method existed.
//
// Every handler's top-level Handle wrapper calls this exactly once, as the
// first statement, and uses ITS RETURN VALUE (not the outer captured deps)
// for the remainder of that one request — see e.g. HandleCreateVM. Because
// each HandlerFunc closure is registered once at startup and reused for
// every subsequent request, shadowing the local `deps` binding inside each
// invocation (via `deps, err := deps.WithRequestOverrides(...)`, rather than
// mutating the captured outer variable) is what keeps concurrent requests
// carrying different overrides from racing each other.
//
// Failure modes:
//   - reqCtx.Extra contains a supported pve_* key with a malformed value
//     (bad type, out-of-range number, inverted vmid range) →
//     config.ApplyContextOverrides' error, wrapped as a non-retriable
//     CloudError: a malformed override is a manifest/cpi-config authoring
//     bug, not a transient condition, and retrying identical input changes
//     nothing.
//   - reqCtx.Extra contains an override AND building/caching its PVE
//     client/agent/resolver fails (unreachable host, bad credentials) →
//     wrapped as a retriable CloudError: this may be a transient network or
//     auth-propagation issue at the overridden cluster, distinct from the
//     job-level cluster's own health, so the Director should retry rather
//     than treat it as permanent.
//   - reqCtx.Extra contains pve_* keys outside the supported override set
//     ALONGSIDE at least one supported key that DID apply (mixed case) →
//     never an error; logged at Warn (once, this call) via d.Log(ctx) so the
//     condition is visible without failing the request, since a director's
//     cpi-config properties block commonly carries the FULL pve.* property
//     set for that entry — most of which (hooks, otel, retry curves, ...) is
//     intentionally NOT overridable per-request; see
//     config.ApplyContextOverrides' doc comment for the full list in scope.
//   - reqCtx.Extra contains ONLY unsupported pve_* keys (zero applied) →
//     non-retriable CloudError, logged at Error. This is deliberately NOT
//     the same fail-open behavior as the mixed case above: a request whose
//     every pve_* key is unrecognized (systematic misspelling, a dropped or
//     renamed key prefix, a cpi-config templating bug) is indistinguishable
//     from LF16's "succeeds while silently targeting the wrong cluster"
//     defect class if allowed to fall through to the job-level cluster —
//     see M2 in the A13 review.
func (d Deps) WithRequestOverrides(ctx context.Context, reqCtx jsonrpc.Context) (Deps, error) {
	// Stamp the caller's director UUID before any early return so every
	// handler path (override or not) carries it — see Deps.RequestDirectorUUID.
	d.RequestDirectorUUID = reqCtx.DirectorUUID
	if d.Overrides == nil || len(reqCtx.Extra) == 0 {
		return d, nil
	}

	effCfg, applied, unknown, err := config.ApplyContextOverrides(d.Config, reqCtx.Extra)
	if err != nil {
		return Deps{}, cpierrors.Cloud("cpi: context override rejected: %s", log.ScrubMessage(err.Error()))
	}

	if len(applied) == 0 {
		if len(unknown) > 0 {
			// M2 (A13 review): every pve_* key present in this request's
			// context is unrecognized — a systematic misspelling, a
			// dropped/renamed key prefix, or a cpi-config templating bug all
			// produce exactly this shape. Silently falling through to the
			// job-level cluster here is indistinguishable from LF16's defect
			// class ("succeeds while silently targeting the wrong cluster"):
			// an operator staring at a director whose properties block
			// clearly names an overridden cluster would see the request
			// quietly land on this process's own job-level cluster instead,
			// with no signal beyond a log line nobody is watching. Fail hard
			// instead. This is distinct from the MIXED case below (at least
			// one override applied, some others did not) — there, the
			// request demonstrably reached the cluster its recognized
			// overrides named, and a cpi-config properties block routinely
			// carries pve.* properties this mechanism intentionally does not
			// override per-request (hooks, otel, retry curves, ...), so that
			// case stays Warn-only by design.
			d.Log(ctx).Error(
				"cpi: request context carried pve_* properties but none are supported per-request overrides; refusing to silently fall back to the job-level cluster",
				log.Any("unsupported_keys", unknown),
			)
			return Deps{}, cpierrors.Cloud(
				"cpi: context carried pve_* properties but none are supported per-request overrides (got: %v) — refusing to silently target the job-level cluster; see config.ApplyContextOverrides for the supported key list",
				unknown,
			)
		}
		// extra held no pve_-prefixed keys at all (e.g. only non-CPI context
		// data) — nothing to rebind, job-level Deps unchanged.
		return d, nil
	}

	if len(unknown) > 0 {
		// Mixed case: some overrides applied, some pve_* keys are
		// unsupported. Warn-only by design — see the comment above.
		d.Log(ctx).Warn(
			"cpi: request context carried pve_* properties this CPI does not support overriding per-request; job-level config is used for them",
			log.Any("unsupported_keys", unknown),
		)
	}

	// The per-entry bands this request just rewrote can put another VMID range
	// on top of the built-in parker band. config.ApplyContextOverrides stands
	// the defaulted parked strategy down rather than failing every request
	// routed to this entry; say so, once per call, so the operator learns that
	// this cluster's detached disks stay free-floating.
	if stoodDown := effCfg.ParkedDefaultStoodDown(); stoodDown != "" && d.Config.ParkedDefaultStoodDown() == "" {
		d.Log(ctx).Warn(
			"cpi: the default detached_disk_strategy \"parked\" is standing down for this request: the overridden VMID bands overlap the built-in parker band [90000,90999]; "+
				"set parked_disk_vmid_range_start/end on this cpi-config entry to park disks on this cluster",
			log.String("colliding_range", stoodDown),
		)
	}

	verifySSLOverrideApplied := false
	for _, k := range applied {
		if k == "pve_verify_ssl" {
			verifySSLOverrideApplied = true
			break
		}
	}

	// S8 follow-up: pve.reject_tls_downgrade_overrides hardens the warn-only
	// TLS-downgrade path below into a hard failure. A genuine downgrade is a
	// job-level config that itself verifies (d.Config.VerifySSLValue() true)
	// whose applied overrides include pve_verify_ssl AND whose effective
	// config no longer verifies. A job-level config that already has
	// verify_ssl=false is NOT a downgrade and is never rejected here — it
	// falls through to the existing warn-only path below unchanged. This
	// check MUST run before d.Overrides.resolve() below: resolve() builds
	// (and caches) the PVE client/agent/resolver bundle for effCfg, so
	// rejecting after that call would still have constructed and cached a
	// client that talks to the overridden host without certificate
	// validation before the request was ever refused.
	if verifySSLOverrideApplied && d.Config.VerifySSLValue() && !effCfg.VerifySSLValue() &&
		d.Config.RejectTLSDowngradeOverridesEnabled() {
		return Deps{}, cpierrors.Cloud(
			"cpi: reject_tls_downgrade_overrides is enabled; request context override %q would disable TLS certificate verification (job-level pve.verify_ssl=true) for overridden host %q — rejecting",
			"pve_verify_ssl", effCfg.Host,
		)
	}

	bundle, resolveErr := d.Overrides.resolve(ctx, effCfg)
	if resolveErr != nil {
		return Deps{}, cpierrors.Retriable(
			"cpi: context override: build PVE connection for overridden host %q: %s",
			effCfg.Host, log.ScrubMessage(resolveErr.Error()),
		)
	}

	// Sole observability line for an otherwise invisible failure mode: prior
	// to this mechanism existing, an operator staring at PVE-side symptoms
	// (wrong cluster's storage filling up, wrong cluster's template
	// content-hash-matched) had no way to see which cluster a given
	// dispatched request actually targeted. Key NAMES only, never values:
	// `applied` holds the literal strings "pve_password"/"pve_api_token"
	// when those keys were present in the request, never their values, so
	// this line cannot leak a credential. effCfg.Host is logged by value
	// because a PVE hostname is not a secret and is the one piece of
	// information that makes this line actionable.
	d.Log(ctx).Info("cpi: dispatching request with per-request context config overrides",
		log.Any("overridden_keys", applied),
		log.String("effective_pve_host", effCfg.Host),
	)

	// A per-request TLS-verification downgrade is a security-relevant event
	// that must be attributable to the request that carried it: the client
	// constructor's own warning goes to the runtime logger (no request_id or
	// method) and fires once per cached bundle, not once per affected
	// request. The inherited job-level credentials are sent to the overridden
	// host over the unverified connection, so name the host explicitly. This
	// only fires when reject_tls_downgrade_overrides did NOT already reject
	// the request above (knob off, or base config already had verify_ssl=false).
	if !effCfg.VerifySSLValue() && verifySSLOverrideApplied {
		d.Log(ctx).Warn(
			"cpi: request context override disables TLS verification for this request; job-level PVE credentials will be sent to the overridden host without certificate validation",
			log.String("effective_pve_host", effCfg.Host),
		)
	}

	d.Config = effCfg
	d.PVE = bundle.client
	d.Agent = bundle.agent
	d.Resolver = bundle.resolver
	d.NodeEndpoints = bundle.nodeEndpoints
	return d, nil
}
