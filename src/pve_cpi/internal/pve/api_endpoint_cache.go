package pve

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// markWorkerName names a down-mark write in the drain's result.
const markWorkerName = "down mark"

// drainStartedText is why a call that began after the exit drain started runs
// without the cache.
const drainStartedText = "the exit drain has started, so the call runs without the cache"

// drainRefusalLine is the Debug line a call logs when it gives up the cache
// because the exit drain has started.
const drainRefusalLine = "api endpoint discovery off: " + drainStartedText

// cacheDirectoryChooser chooses the cache directory for a config. It returns
// the choice and the notes the layers that declined it want logged at Debug
// whatever the discovery mode, such as a missing directory beside the journal,
// which an explicit "on" must not turn into a Warn. Tests replace it.
type cacheDirectoryChooser func(cfg *config.CPIConfig) (endpointcache.Choice, []string)

// chooseCacheDirectory is the chooser the client uses. endpointcache.Resolve
// keeps the directory out of the allocation journal directory, before a layer
// creates anything and again after the directory is open, so the chooser adds
// nothing of its own.
var chooseCacheDirectory cacheDirectoryChooser = func(cfg *config.CPIConfig) (endpointcache.Choice, []string) {
	choice := endpointcache.OptionsFor(cfg).Resolve()
	notes := make([]string, 0, len(choice.Notes))
	for _, note := range choice.Notes {
		notes = append(notes, "pve endpoints: "+note)
	}
	return choice, notes
}

// cacheClock is the clock the cache read and the mark writes use.
var cacheClock = time.Now

// endpointCache is the open endpoint cache of one client. It holds the store,
// the discovery worker that refreshes the cached nodes, and the discovered
// endpoints the cache supplied to the pool. The store is closed after the
// exit drain, through the worker registry, so a worker or a mark write that
// is still running never meets a closed directory.
type endpointCache struct {
	store  *endpointcache.Store
	worker *DiscoveryWorker
	// mode is the configured discovery mode, which sets the level of the lines
	// the failure rules keep at Debug under auto.
	mode   config.DiscoveryMode
	logger *log.Logger
	// discovered are the cached nodes handed to the pool, which the mark
	// writes name as the pool's extra endpoints so their marks survive
	// pruning.
	discovered []endpointpool.Endpoint
	sink       *cacheMarkSink
	// identity is the cluster identity the cache loaded the pool under and
	// the once-per-call conflict line, which the worker shares.
	identity *clusterIdentityWatch
	// cold is what the cold start from placement.az_map needs, which attach
	// turns into the pool's candidates when endpointpool.Gate allows them.
	cold coldStartInputs
}

// openEndpointCache resolves the effective discovery mode, chooses and opens
// the cache directory and file, and adds the cached nodes and marks to plan.
// It returns nil when the call runs without a cache, which is the case for a
// mode that resolves to off, a directory or file that is unusable, and an exit
// drain that has already started. When the config says discovery is on and
// the directory turns out unusable, it sets plan.cacheUnusable, so the
// effective mode is off, and logs why at Debug under auto and at Warn under an
// explicit on.
//
// It runs after the pinning assertion, so a config that is refused for
// pinning never touches the filesystem. It never returns an error, because a
// cache that fails costs the call its discovery and never the call.
func openEndpointCache(cfg *config.CPIConfig, plan *endpointPlan, logger *log.Logger) *endpointCache {
	ctx := context.Background()
	configured := cfg.DiscoveryModeValue()
	level := endpointcache.Level(configured)
	mode, reason := config.EffectiveDiscoveryMode(cfg)
	if reason != "" {
		logger.LogAttrs(ctx, level, reason)
	}
	if mode != config.DiscoveryModeOn {
		return nil
	}
	off := func(why string) *endpointCache {
		plan.cacheUnusable = true
		logger.LogAttrs(ctx, level, "api endpoint discovery off: "+why)
		return nil
	}
	// A call that starts after the exit drain began would leave a store that
	// nothing closes. The drain is a state of the process and not a fault in
	// the directory, so the line stays at Debug whatever the mode. The check
	// comes before the directory is chosen or opened, so the call touches
	// nothing.
	drainRefused := func() *endpointCache {
		plan.cacheUnusable = true
		logger.LogAttrs(ctx, log.LevelDebug, drainRefusalLine)
		return nil
	}
	if endpointcache.Draining() {
		return drainRefused()
	}

	choice, notes := chooseCacheDirectory(cfg)
	for _, note := range notes {
		logger.LogAttrs(ctx, log.LevelDebug, note)
	}
	if !choice.Usable() {
		return off(refusalText(choice))
	}
	logger.LogAttrs(ctx, log.LevelDebug, "pve endpoints: cache directory "+choice.Path+", "+layerText(choice.Layer))

	user, credential := cacheCredential(cfg)
	key := endpointcache.KeyConfig{
		Endpoints: configuredAddresses(plan),
		VerifySSL: cfg.VerifySSLValue(),
		CACert:    cfg.PVECACertPEM,
		User:      user,
	}
	// OpenStore owns the directory from here on, whether it succeeds or fails.
	store, err := endpointcache.OpenStore(ctx, choice.Root, key, credential,
		endpointcache.StoreOptions{Logger: logger, ConfiguredMode: configured})
	if err != nil {
		return off(fmt.Sprintf("cache directory %s could not be opened: %v", choice.Path, err))
	}
	cache := &endpointCache{store: store, mode: configured, logger: logger, cold: coldStartInputs{
		names:      plan.coldStartNames,
		seed:       plan.endpoints[0].Address,
		configured: configuredAddresses(plan),
		userAgent:  buildUserAgent(cfg),
	}}
	// The release goes first, so every return below leaves the store closed
	// by the drain or by the code that gave up on it.
	if !endpointcache.AfterDrain(cache.release) {
		// The drain began after the check above, while the store was opening.
		cache.release()
		return drainRefused()
	}
	if path, replaced := store.SecretReplaced(); replaced {
		logger.LogAttrs(ctx, log.LevelWarn, "endpoint cache: replaced the directory secret, which had the wrong "+
			"length; every cache file written under the old one is rebuilt", log.String("path", path))
	}

	payload, ok := cache.read()
	if !ok {
		cache.release()
		plan.cacheUnusable = true
		return nil
	}
	cache.identity = newClusterIdentityWatch(logger, store.FileName(), payload)
	cache.load(plan, payload)
	if payload != nil {
		if payload.ColdStart != nil {
			cache.cold.hint = payload.ColdStart.Name
		}
		if payload.Discovery != nil {
			cache.cold.outcome = string(payload.Discovery.Outcome)
		}
	}

	worker, err := newDiscoveryWorker(DiscoveryWorkerConfig{
		Store: store, Mode: configured, Logger: logger, Identity: cache.identity,
	})
	if err != nil {
		cache.release()
		return off(fmt.Sprintf("the discovery worker could not be built: %v", err))
	}
	cache.worker = worker
	cache.sink = &cacheMarkSink{
		store: store, mode: configured, logger: logger, now: cacheClock, start: endpointcache.Go,
		pool: cache.poolAddresses(),
	}
	return cache
}

// refusalText describes why a layer gave no directory.
func refusalText(choice endpointcache.Choice) string {
	if choice.Err == nil {
		return "no cache directory is available"
	}
	return choice.Err.Error()
}

// layerText says where the cache directory came from, for the Debug line that
// names it.
func layerText(layer endpointcache.Layer) string {
	switch layer {
	case endpointcache.LayerOverride:
		return "from pve.endpoint_cache_dir"
	case endpointcache.LayerJournal:
		return "beside the allocation journal directory"
	case endpointcache.LayerTemporary:
		return "in the temporary directory"
	default:
		return "from the " + string(layer) + " layer"
	}
}

// release closes the store. It is safe to call more than once and from the
// drain, and a close error is logged at Debug because the call is ending.
func (c *endpointCache) release() {
	if c == nil {
		return
	}
	if err := c.store.Close(); err != nil {
		c.logger.LogAttrs(context.Background(), log.LevelDebug, "endpoint cache: close the cache directory",
			log.Err(err))
	}
}

// read loads and validates the cache file. ok is false when the file cannot
// be read for a reason that makes the directory unusable, after logging why,
// and when the exit drain has closed the store, which logs the drain's line.
// A file that is absent, or one that fails its integrity or key checks, is no
// cache yet and gives a nil payload. The read keeps a cold-start hint only
// when it names a node the cold start may try, so a client with no cold start
// keeps none.
func (c *endpointCache) read() (*endpointcache.Payload, bool) {
	ctx := context.Background()
	payload, drops, err := c.store.ReadPayload(cacheClock(), c.cold.names)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return nil, true
	case errors.Is(err, endpointcache.ErrFileInvalid):
		c.logger.LogAttrs(ctx, endpointcache.Level(c.mode),
			"endpoint cache: the cache file is not usable and is ignored", log.String("file", c.store.FileName()),
			log.Err(err))
		return nil, true
	case errors.Is(err, endpointcache.ErrStoreClosed):
		// The exit drain closed the store between the open and this read. The
		// directory is not at fault, so the call gives the cache up the way a
		// call that meets the drain at the open does, at Debug whatever the mode.
		c.logger.LogAttrs(ctx, log.LevelDebug, drainRefusalLine)
		return nil, false
	default:
		c.logger.LogAttrs(ctx, endpointcache.Level(c.mode),
			"api endpoint discovery off: the cache file could not be read: "+err.Error(),
			log.String("file", c.store.FileName()))
		return nil, false
	}
	if len(drops) > 0 {
		// A record the validation left out is routine, such as an expired
		// node or mark, the seed's own node, or the nodes a stored identity
		// conflict already warned about, so these stay at Debug in every mode.
		texts := make([]string, len(drops))
		for i, d := range drops {
			texts[i] = d.String()
		}
		c.logger.LogAttrs(ctx, log.LevelDebug, "endpoint cache: "+strings.Join(texts, "; "),
			log.String("file", c.store.FileName()))
	}
	return payload, true
}

// watch returns the cache's identity watch, making one that holds no identity
// for a cache that was not opened through openEndpointCache.
func (c *endpointCache) watch() *clusterIdentityWatch {
	if c.identity == nil {
		c.identity = newClusterIdentityWatch(c.logger, c.store.FileName(), nil)
	}
	return c.identity
}

// load adds the payload's usable nodes to plan as discovered endpoints and its
// marks to the endpoints they name. A node whose address is a configured
// endpoint, a repeat of an earlier node, or whose pins do not parse is left
// out, because the pool refuses each of those and a bad cache record must not
// keep the client from building. A stored identity conflict logs one Warn
// line, because the validated payload then holds no nodes.
func (c *endpointCache) load(plan *endpointPlan, p *endpointcache.Payload) {
	if p == nil {
		return
	}
	if standingConflict(p) {
		c.watch().warnConflict()
	}
	for i := range plan.endpoints[:plan.configured] {
		plan.endpoints[i].DownMarkedAt = markTime(p, plan.endpoints[i].Address)
	}
	nameConfiguredEndpoints(plan, p)
	if p.Discovery == nil {
		return
	}
	seen := normalizedEndpointSet(configuredAddresses(plan))
	for _, node := range p.Discovery.Nodes {
		endpoint, ok := c.endpointOf(node, p, seen)
		if !ok {
			continue
		}
		plan.endpoints = append(plan.endpoints, endpoint)
		c.discovered = append(c.discovered, endpoint)
	}
}

// nameConfiguredEndpoints gives each configured endpoint of plan the node name
// an earlier discovery recorded for it, so the pool attributes a node-scoped
// request to the right entry in a call that runs no discovery of its own. A
// name already on the endpoint stays, which is the name a test gave it. A
// payload that holds an identity conflict names nothing, because the
// configured endpoints no longer report the cluster the names were read from.
func nameConfiguredEndpoints(plan *endpointPlan, p *endpointcache.Payload) {
	if p.Cluster == nil || p.Cluster.IdentityConflict {
		return
	}
	for i := range plan.endpoints[:plan.configured] {
		endpoint := &plan.endpoints[i]
		if node := p.Cluster.EndpointNodes[normalizedEndpoint(endpoint.Address)]; endpoint.Node == "" && node != "" {
			endpoint.Node = node
		}
	}
}

// endpointOf turns one cached node into a pool endpoint. ok is false, after a
// Debug line, for a node the pool would refuse. seen holds the normalized
// addresses already in the pool, and the node's joins it.
func (c *endpointCache) endpointOf(
	node endpointcache.DiscoveredNode, p *endpointcache.Payload, seen map[string]bool,
) (endpointpool.Endpoint, bool) {
	ctx := context.Background()
	address := net.JoinHostPort(node.Address, strconv.Itoa(node.Port))
	normalized := normalizedEndpoint(address)
	if normalized == "" || seen[normalized] {
		c.logger.LogAttrs(ctx, log.LevelDebug,
			"endpoint cache: a cached node is a repeat of an endpoint in the pool, or is not an endpoint; skipping it",
			log.String("node", node.Name))
		return endpointpool.Endpoint{}, false
	}
	pins := make([]endpointpool.Pin, 0, len(node.Pins))
	for _, text := range node.Pins {
		pin, err := endpointpool.ParsePin(text)
		if err != nil {
			c.logger.LogAttrs(ctx, log.LevelDebug, "endpoint cache: a cached pin does not parse; skipping its node",
				log.String("node", node.Name))
			return endpointpool.Endpoint{}, false
		}
		pins = append(pins, pin)
	}
	if len(pins) == 0 {
		c.logger.LogAttrs(ctx, log.LevelDebug, "endpoint cache: a cached node has no pins; skipping it",
			log.String("node", node.Name))
		return endpointpool.Endpoint{}, false
	}
	seen[normalized] = true
	return endpointpool.Endpoint{
		Address:      address,
		Kind:         endpointpool.KindDiscovered,
		Node:         node.Name,
		Pins:         pins,
		Online:       node.Online,
		DownMarkedAt: markTime(p, address),
	}, true
}

// markTime returns when the payload marked address down, or the zero time.
func markTime(p *endpointcache.Payload, address string) time.Time {
	key := normalizedEndpoint(address)
	if key == "" {
		return time.Time{}
	}
	return p.Marks[key].DownAt
}

// poolAddresses lists the discovered endpoints' addresses, which a mark write
// names as the pool's extra endpoints.
func (c *endpointCache) poolAddresses() []string {
	out := make([]string, len(c.discovered))
	for i, ep := range c.discovered {
		out[i] = ep.Address
	}
	return out
}

// dropDiscovered forgets the discovered endpoints, which the pool refused, so
// the mark writes no longer name them as endpoints the pool holds. A mark for
// one of them is then pruned like any other mark for an endpoint nobody holds.
func (c *endpointCache) dropDiscovered() {
	if c == nil {
		return
	}
	c.discovered = nil
	if c.sink != nil {
		c.sink.pool = nil
	}
}

// attach gives the pool the cache's mark sink and first-answer hook, and the
// cold-start candidates and prober when the cold start applies. A nil cache
// gives it none of them, so a call without a usable cache directory has no
// cold start.
func (c *endpointCache) attach(cfg *endpointpool.Config) {
	if c == nil {
		return
	}
	cfg.MarkSink = c.sink
	cfg.FirstAnswer = c.worker
	c.attachColdStart(cfg)
}

// poolRefused logs that the pool would not take the cached nodes, which leaves
// it with the configured endpoints.
func (c *endpointCache) poolRefused(err error) {
	if c == nil {
		return
	}
	c.logger.LogAttrs(context.Background(), endpointcache.Level(c.mode),
		"api endpoint discovery: the pool refused the cached nodes, so it starts from the configured endpoints",
		log.Err(err))
}

// bind hands the worker the client that sends its reads and the pool it is the
// first-answer hook of. The reads go through the pool, and each is marked
// configured only, so they never reach a discovered endpoint. A failed bind
// costs the call its discovery and nothing else.
func (c *endpointCache) bind(client Client, pool *endpointpool.Pool) {
	if c == nil {
		return
	}
	if pool == nil {
		c.logger.LogAttrs(context.Background(), log.LevelDebug,
			"api endpoint discovery: the client has no endpoint pool, so discovery does not run")
		return
	}
	if err := c.worker.bind(DiscoveryBinding{Client: client, Pool: pool}); err != nil {
		c.logger.LogAttrs(context.Background(), log.LevelDebug, "api endpoint discovery: bind the worker",
			log.Err(err))
	}
}

// startedAwayLog remembers which starts away from the first endpoint this
// process has said so about. A CPI call is one process, so saying each start
// once per process is saying it once per call, whatever number of clients the
// process builds for the same cluster.
var startedAwayLog struct {
	mu   sync.Mutex
	said map[string]bool
}

// firstStartedAway reports whether this process has not yet said that it
// started on start because first was marked down, and records that it has.
func firstStartedAway(first, start string) bool {
	startedAwayLog.mu.Lock()
	defer startedAwayLog.mu.Unlock()
	key := first + " -> " + start
	if startedAwayLog.said[key] {
		return false
	}
	if startedAwayLog.said == nil {
		startedAwayLog.said = map[string]bool{}
	}
	startedAwayLog.said[key] = true
	return true
}

// logStartedAway logs the Info line for a pool that started on an endpoint
// other than the first one, because the first carried a down mark that is
// still in effect. The pool reports a start away only while a mark is in
// effect, so the line is rare, and the process says it once however many
// clients it builds.
func logStartedAway(pool *endpointpool.Pool, logger *log.Logger) {
	if pool == nil {
		return
	}
	away, started := pool.StartedAway()
	if !started || !firstStartedAway(away.First, away.Start) {
		return
	}
	age := max(cacheClock().Sub(away.MarkedAt), 0).Round(time.Second)
	logger.LogAttrs(context.Background(), log.LevelInfo, fmt.Sprintf(
		"pve endpoints: starting on %s because %s was marked down %s ago", away.Start, away.First, age))
}

// payloadUpdater is the part of the cache store a mark write uses, which lets a
// test see whether a write reached the store at all.
type payloadUpdater interface {
	UpdatePayload(
		ctx context.Context, now time.Time, build func(stored endpointcache.Stored) (endpointcache.Change, error),
	) (endpointcache.UpdateResult, error)
}

// cacheMarkSink records the pool's down marks in the cache file. The pool
// calls MarkDown on the failed request's goroutine and waits for it, so the
// sink hands each write to a goroutine of the process registry and returns at
// once. The exit drain then waits for the write or cancels it.
type cacheMarkSink struct {
	store  payloadUpdater
	mode   config.DiscoveryMode
	logger *log.Logger
	now    func() time.Time
	// start runs a write as a registered worker. It is the process registry's
	// Go, and tests replace it.
	start func(name string, mode config.DiscoveryMode, fn func(ctx context.Context)) error
	// pool lists the pool's discovered endpoints, which the merge keeps marks
	// for.
	pool []string
}

// The sink is the pool's mark sink.
var _ endpointpool.MarkSink = (*cacheMarkSink)(nil)

// MarkDown starts the write of m and returns at once. A registry that has
// started its drain refuses the write, and the cache then misses one mark.
func (s *cacheMarkSink) MarkDown(m endpointpool.Mark) {
	ctx := context.Background()
	class := endpointcache.MarkClass(m.Class)
	if !class.Valid() {
		s.logger.LogAttrs(ctx, log.LevelDebug, "endpoint cache: a down mark with an unknown class is not recorded",
			log.String("endpoint", m.Endpoint))
		return
	}
	err := s.start(markWorkerName, s.mode, func(workCtx context.Context) { s.write(workCtx, m, class) })
	if err != nil {
		s.logger.LogAttrs(ctx, log.LevelDebug, "endpoint cache: the down mark is not recorded",
			log.String("endpoint", m.Endpoint), log.Err(err))
	}
}

// write records one mark. A pin mark also sets the discovery section's
// pin_failed_at, which the merge keeps at the newest time, so the pin failure
// outlives the two minutes the mark lasts and a refresh stays due in other
// processes until a discovery writes a result.
func (s *cacheMarkSink) write(ctx context.Context, m endpointpool.Mark, class endpointcache.MarkClass) {
	// A drain that gave up cancels the context, and the store would refuse the
	// write anyway, so the write is not started.
	if ctx.Err() != nil {
		return
	}
	change := endpointcache.Change{
		Marks: map[string]endpointcache.DownMark{m.Endpoint: {DownAt: m.At, Class: class}},
		Pool:  s.pool,
	}
	if class == endpointcache.MarkPin {
		change.PinFailedAt = m.At
	}
	_, err := s.store.UpdatePayload(ctx, s.now(), func(endpointcache.Stored) (endpointcache.Change, error) {
		return change, nil
	})
	if err != nil && ctx.Err() == nil {
		s.logger.LogAttrs(ctx, endpointcache.Level(s.mode), "endpoint cache: the down mark could not be recorded",
			log.String("endpoint", m.Endpoint), log.Err(err))
	}
}

// configuredAddresses lists the plan's configured endpoints in the operator's
// order. The cache key comes from these and not from cfg.Endpoints, which
// stays empty for a config that sets only pve.host.
func configuredAddresses(plan *endpointPlan) []string {
	out := make([]string, plan.configured)
	for i, ep := range plan.endpoints[:plan.configured] {
		out[i] = ep.Address
	}
	return out
}

// cacheCredential returns the identity the entry authenticates as and its
// secret. A token gives its ID and its secret, and a password gives the user
// with its realm. The pair is the one newClient authenticates with.
func cacheCredential(cfg *config.CPIConfig) (user, credential string) {
	if cfg.APIToken != "" {
		id, secret, _ := strings.Cut(cfg.APIToken, "=")
		return id, secret
	}
	user = cfg.User
	if user != "" && !strings.Contains(user, "@") {
		realm := cfg.Realm
		if realm == "" {
			realm = "pam"
		}
		user += "@" + realm
	}
	return user, cfg.Password
}
