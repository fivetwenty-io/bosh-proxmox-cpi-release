package ha

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const (
	// ProfileCacheTTL is how long stored slow-changing facts answer a call.
	// The age counts from the moment the probe that produced them began.
	ProfileCacheTTL = 60 * time.Second

	// profileCacheLabel is the HMAC label of the profile cache file. It
	// differs from the endpoint cache's, so one kind of file never verifies as
	// the other.
	profileCacheLabel = "pve-cpi ha-profile"

	// profileCacheVersion is the profile cache file's envelope schema version.
	// The stored facts have explicit JSON tags on SlowFacts and the types it
	// holds, and a golden test pins the field set, so any change to those tags
	// or fields bumps this version.
	profileCacheVersion = 1
)

// ProfileCacheOptions says where the profile cache lives and whose facts it
// holds.
type ProfileCacheOptions struct {
	// Dir says where to look for the cache directory, and comes from
	// endpointcache.OptionsFor. The cache follows the endpoint cache's
	// directory, which is pve.endpoint_cache_dir when it is set and the
	// per-user temporary directory otherwise. A missing override directory
	// gives no cache, because the override is the only directory the CPI uses
	// when it is set.
	Dir endpointcache.DirOptions

	// Key names the cluster entry. It is the endpoint cache's key input, so one
	// entry maps to one endpoint file and one profile file.
	Key endpointcache.KeyConfig

	// Credential is the entry's token secret or password. It keys the file's
	// HMAC, so rotating it invalidates every stored file. It is never logged.
	Credential string

	// Log receives the cache's debug lines. When it is nil, the cache uses the
	// logger in the context.
	Log *log.Logger

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// TTL overrides ProfileCacheTTL. Zero means ProfileCacheTTL.
	TTL time.Duration
}

// profilePayload is the cache file's payload. Key is required by the store.
type profilePayload struct {
	Key endpointcache.Key `json:"key"`

	// StoredAtMillis is when the probe that produced Facts began, in
	// milliseconds since the Unix epoch.
	StoredAtMillis int64 `json:"stored_at_ms"`

	Facts SlowFacts `json:"facts"`
}

// ProfileCache is a SlowSource that answers from a stored copy of the
// slow-changing facts while it is fresh, and probes through the live source
// otherwise. A cache that cannot be used for any reason, such as a refused
// directory, an unreadable or damaged file, or a busy lock, only makes the
// call probe, which is correct and slower. The cache never holds node health,
// watchdog answers, or the QDevice's connection status.
type ProfileCache struct {
	live SlowSource
	opts ProfileCacheOptions
}

// NewProfileCache wraps live with the cache described by opts.
func NewProfileCache(live SlowSource, opts ProfileCacheOptions) *ProfileCache {
	return &ProfileCache{live: live, opts: opts}
}

// WithProfileCache returns d with its slow-changing facts coming from the
// profile cache described by opts, over the live probes d already holds.
func (d Deps) WithProfileCache(opts ProfileCacheOptions) Deps {
	d.Slow = NewProfileCache(liveSlow{deps: d}, opts)
	return d
}

// ProfileCacheOptionsFromConfig builds the cache options for the cluster entry
// cfg describes. It returns false when the entry cannot form a cache key, such
// as one with no host or no credential, in which case the profile runs without
// a cache.
func ProfileCacheOptionsFromConfig(cfg *config.CPIConfig, logger *log.Logger) (ProfileCacheOptions, bool) {
	if cfg == nil {
		return ProfileCacheOptions{}, false
	}
	endpoints, ok := cacheEndpoints(cfg)
	if !ok {
		return ProfileCacheOptions{}, false
	}
	user, credential := cacheCredential(cfg)
	if user == "" || credential == "" {
		return ProfileCacheOptions{}, false
	}
	return ProfileCacheOptions{
		Dir: endpointcache.OptionsFor(cfg),
		Key: endpointcache.KeyConfig{
			Endpoints: endpoints,
			VerifySSL: cfg.VerifySSLValue(),
			CACert:    cfg.PVECACertPEM,
			User:      user,
		},
		Credential: credential,
		Log:        logger,
	}, true
}

// cacheEndpoints returns the entry's configured endpoint list in order, each
// with an explicit port. An entry with no endpoints list has the one-entry
// list made from its host and port.
func cacheEndpoints(cfg *config.CPIConfig) ([]string, bool) {
	list := cfg.Endpoints
	if len(list) == 0 {
		if cfg.Host == "" {
			return nil, false
		}
		addr := cfg.Host
		if cfg.Port != 0 {
			addr = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
		}
		list = []string{addr}
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		host, port, ok := config.NormalizeEndpoint(entry)
		if !ok {
			return nil, false
		}
		out = append(out, net.JoinHostPort(host, strconv.Itoa(port)))
	}
	return out, true
}

// cacheCredential returns the identity the entry authenticates as and its
// secret. A token gives its ID and its secret, and a password gives the user
// with its realm.
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

func (c *ProfileCache) now() time.Time {
	if c.opts.Now != nil {
		return c.opts.Now()
	}
	return time.Now()
}

func (c *ProfileCache) ttl() time.Duration {
	if c.opts.TTL > 0 {
		return c.opts.TTL
	}
	return ProfileCacheTTL
}

// Slow implements SlowSource. It returns the stored facts when they are fresh
// and match the nodes asked about, marked Cached, and otherwise the live
// source's facts, which it then stores when they are fully clean.
func (c *ProfileCache) Slow(ctx context.Context, nodes []NodeRef) (*SlowFacts, error) {
	if c == nil || c.live == nil {
		return nil, errors.New("ha profile cache: no live source")
	}
	logger := c.opts.Log
	if logger == nil {
		logger = log.FromContext(ctx)
	}
	store := c.openStore(ctx, logger)
	if store == nil {
		return c.live.Slow(ctx, nodes)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Debug("ha profile cache: close the cache directory", log.Err(err))
		}
	}()

	if facts, ok := c.read(store, nodes, logger); ok {
		return facts, nil
	}
	started := c.now()
	facts, err := c.live.Slow(ctx, nodes)
	if err != nil || facts == nil {
		return facts, err
	}
	c.write(ctx, store, facts, nodes, started, logger)
	return facts, nil
}

// openStore opens the profile file's store, or returns nil after logging why
// there is no cache for this call.
func (c *ProfileCache) openStore(ctx context.Context, logger *log.Logger) *endpointcache.Store {
	choice := c.opts.Dir.Resolve()
	if !choice.Usable() {
		logger.Debug("ha profile cache: no usable cache directory; probing",
			log.String("layer", string(choice.Layer)), log.String("path", choice.Path), log.Err(choice.Err))
		return nil
	}
	store, err := endpointcache.OpenStore(ctx, choice.Root, c.opts.Key, c.opts.Credential, endpointcache.StoreOptions{
		Logger:     logger,
		FilePrefix: endpointcache.HAProfilePrefix,
		Label:      profileCacheLabel,
		Version:    profileCacheVersion,
	})
	if err != nil {
		logger.Debug("ha profile cache: cannot open the cache; probing",
			log.String("path", choice.Path), log.Err(err))
		return nil
	}
	if path, replaced := store.SecretReplaced(); replaced {
		// OpenStore reports a replacement only to the call that made it, so this
		// line appears once per replacement.
		logger.Warn("ha profile cache: replaced the cache directory secret, which had the wrong length; "+
			"every cache file written under the old one is rebuilt", log.String("path", path))
	}
	return store
}

// read returns the stored facts when the file holds a fresh, clean copy that
// matches nodes.
func (c *ProfileCache) read(store *endpointcache.Store, nodes []NodeRef, logger *log.Logger) (*SlowFacts, bool) {
	var stored profilePayload
	if err := store.Read(&stored); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Debug("ha profile cache: stored file not usable; probing",
				log.String("file", store.FileName()), log.Err(err))
		}
		return nil, false
	}
	if reason := c.staleReason(&stored, nodes); reason != "" {
		logger.Debug("ha profile cache: stored facts not used; probing",
			log.String("file", store.FileName()), log.String("reason", reason))
		return nil, false
	}
	facts := stored.Facts
	facts.Cached = true
	return &facts, true
}

// staleReason says why stored cannot answer for nodes, or returns "" when it
// can.
func (c *ProfileCache) staleReason(stored *profilePayload, nodes []NodeRef) string {
	age := c.now().Sub(time.UnixMilli(stored.StoredAtMillis))
	switch {
	case age < 0:
		return "stored in the future"
	case age >= c.ttl():
		return "expired"
	}
	if reason := uncleanReason(&stored.Facts); reason != "" {
		return "stored facts are not clean: " + reason
	}
	if reason := nodeMismatch(&stored.Facts, nodes); reason != "" {
		return reason
	}
	return ""
}

// write stores facts when they are fully clean. A newer file that another
// process wrote while this one probed is left alone.
func (c *ProfileCache) write(
	ctx context.Context, store *endpointcache.Store, facts *SlowFacts, nodes []NodeRef, started time.Time,
	logger *log.Logger,
) {
	if reason := uncleanReason(facts); reason != "" {
		logger.Debug("ha profile cache: facts not stored", log.String("reason", reason))
		return
	}
	if reason := nodeMismatch(facts, nodes); reason != "" {
		logger.Debug("ha profile cache: facts not stored", log.String("reason", reason))
		return
	}
	next := &profilePayload{Key: store.Key(), StoredAtMillis: started.UnixMilli(), Facts: storable(facts)}
	_, err := endpointcache.Update(ctx, store, func(current *profilePayload) (*profilePayload, error) {
		// The writer holds the key's lock here, and sweeps the directory at the
		// cadence the endpoint cache does, so profile files that a credential
		// rotation or an endpoint list change orphaned, and temporary files that
		// a crash left, do not stay forever.
		if swept := store.Sweep(c.now()); swept.Err != nil {
			logger.Debug("ha profile cache: the sweep of the cache directory failed", log.Err(swept.Err))
		}
		if current != nil && current.StoredAtMillis > next.StoredAtMillis &&
			c.staleReason(current, nodes) == "" {
			return nil, nil
		}
		return next, nil
	})
	if err != nil {
		logger.Debug("ha profile cache: cannot store the facts", log.String("file", store.FileName()), log.Err(err))
	}
}

// storable returns a copy of facts for the file. A configured QDevice loses
// its connection status, which a cached call reads again, and Cached is never
// stored.
func storable(facts *SlowFacts) SlowFacts {
	out := *facts
	out.Cached = false
	if out.QDevice.Configured {
		out.QDevice.Status = QDeviceUnknown
	}
	return out
}

// uncleanReason returns why facts must not be cached, or "" when every source
// answered. A fact that carries any error is never kept, so a transient failure
// cannot outlive its call.
func uncleanReason(f *SlowFacts) string {
	switch {
	case f.RulesErr != "":
		return "the HA rules endpoint did not answer: " + f.RulesErr
	case f.ConfigVotesErr != "" || f.ConfigVotes == nil:
		return "configured votes unreadable"
	case f.PermissionsErr != "":
		return "token permissions unreadable"
	case f.StoragesErr != "":
		return "storage definitions unreadable"
	case !f.QDevice.Read || f.QDevice.ReadErr != "":
		return "QDevice unreadable"
	}
	names := make([]string, 0, len(f.Nodes))
	for name := range f.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		n := f.Nodes[name]
		switch {
		case !n.Probed:
			continue
		case n.VersionErr != "" || n.Version == "":
			return fmt.Sprintf("version of node %s unreadable", name)
		case n.CPUErr != "" || !n.CPU.Known:
			return fmt.Sprintf("CPU of node %s unreadable", name)
		}
	}
	return ""
}

// nodeMismatch returns why facts do not describe exactly the nodes asked about,
// or "" when they do. A node that joined or left, or whose online state
// changed, needs a fresh probe.
func nodeMismatch(f *SlowFacts, nodes []NodeRef) string {
	if len(f.Nodes) != len(nodes) {
		return "the cluster's node set changed"
	}
	for _, n := range nodes {
		stored, ok := f.Nodes[n.Name]
		switch {
		case !ok:
			return "the cluster's node set changed"
		case stored.Probed != n.Online:
			return fmt.Sprintf("node %s changed its online state", n.Name)
		}
	}
	return ""
}
