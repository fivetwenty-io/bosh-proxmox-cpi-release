package pve

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

const (
	// poolDialTimeoutSec is the dial timeout a client with more than one
	// endpoint takes when the operator left pve.api_dial_timeout_sec at 0. A
	// dial to a live pveproxy on a LAN takes milliseconds, and the bound keeps a
	// dead endpoint from costing Linux's two minutes before the walk moves on.
	poolDialTimeoutSec = 5
	// poolTLSHandshakeTimeoutSec is the TLS handshake timeout a client with
	// more than one endpoint takes when the operator left
	// pve.api_tls_handshake_timeout_sec at 0.
	poolTLSHandshakeTimeoutSec = 10
)

// discoveredEndpointSource supplies the discovered endpoints that join a
// client's pool after the configured ones. The plan asks it once, so the pool
// size is fixed for the life of the client.
type discoveredEndpointSource interface {
	// DiscoveredEndpoints returns endpoints of kind endpointpool.KindDiscovered,
	// each with its pin set and its node's online flag, in any order. The pool
	// orders them itself, the online nodes first and each group by node name.
	// It returns none when discovery has recorded nothing for this cluster.
	DiscoveredEndpoints() []endpointpool.Endpoint
}

// noDiscoveredEndpoints is the source a plan uses before the endpoint cache has
// been read. It supplies none, so the plan holds exactly the configured
// endpoints.
type noDiscoveredEndpoints struct{}

// DiscoveredEndpoints returns no endpoints.
func (noDiscoveredEndpoints) DiscoveredEndpoints() []endpointpool.Endpoint { return nil }

// endpointDiscoveryOn reports whether the effective endpoint discovery mode is
// on, as far as the config alone can say. It reads no file, so the answer
// never changes with what a cache holds or whether its directory is usable.
func endpointDiscoveryOn(cfg *config.CPIConfig) bool {
	mode, _ := config.EffectiveDiscoveryMode(cfg)
	return mode == config.DiscoveryModeOn
}

// endpointPlan is what newClient decides about a client's endpoints from its
// config alone, before any transport exists.
type endpointPlan struct {
	// endpoints lists the configured endpoints in the operator's order, then the
	// discovered ones the source supplied. The first is the primary, whose
	// host:port the SDK puts on every request.
	endpoints []endpointpool.Endpoint
	// configured counts the leading entries of endpoints the operator listed.
	configured int
	// discovery says the effective discovery mode is on according to the
	// config alone. It is the routing input of the pinning assertion, so the
	// assertion never depends on the filesystem.
	discovery bool
	// cacheUnusable says the cache directory or file was found unusable, which
	// resolves the effective mode to off whatever the config says. The wrapper
	// then follows the pool alone, and the pinning assertion, which already
	// ran on the config, is unchanged.
	cacheUnusable bool
	// coldStartNames are the placement.az_map names the cold start may try,
	// which newClient sets from the entry and its job before the cache is
	// read. They are empty when any config condition for the cold start
	// fails, and the cache rules them out when its directory is unusable.
	coldStartNames []string
}

// discoveryEffective reports whether the effective discovery mode is on once
// the cache directory has been checked.
func (p endpointPlan) discoveryEffective() bool {
	return p.discovery && !p.cacheUnusable
}

// install reports whether the failover transport wraps the SDK's. A pool of
// more than one endpoint needs it, and so does a client whose effective
// discovery mode is on, so the wrapper sees the first answered request even
// when the pool starts at one. Every other client keeps the SDK's own
// transport and builds the same Options as before the pool existed.
func (p endpointPlan) install() bool {
	return len(p.endpoints) > 1 || p.discoveryEffective()
}

// failover reports whether this client can fail over between endpoints, which
// the pinning assertion needs. It reads only the config, never what discovery
// cached and never whether the cache directory is usable, so a config that
// builds today cannot fail to build tomorrow because the cache filled or its
// directory went bad.
func (p endpointPlan) failover() bool {
	return p.configured > 1 || p.discovery
}

// planEndpoints builds the endpoint plan for cfg. opts carries the host and
// port the SDK will put on every request, so the plan can refuse a config
// whose first endpoint is somewhere else. Validation already makes the two
// agree, and the check keeps a hand-built config from sending every request
// down the routed-host path. The config layer resolves every list entry to a
// host and port, so an entry without a port means the list never went through
// it, and the plan refuses to guess one.
func planEndpoints(
	cfg *config.CPIConfig,
	opts sdkclient.Options,
	source discoveredEndpointSource,
) (endpointPlan, error) {
	plan := endpointPlan{discovery: endpointDiscoveryOn(cfg)}
	if len(cfg.Endpoints) == 0 {
		plan.endpoints = []endpointpool.Endpoint{{
			Address: net.JoinHostPort(strings.Trim(opts.Host, "[]"), strconv.Itoa(opts.Port)),
			Kind:    endpointpool.KindConfigured,
		}}
	}
	for i, entry := range cfg.Endpoints {
		host, port, ok := config.NormalizeEndpoint(entry)
		if !ok {
			return endpointPlan{}, cpierrors.Cloud("pve client init: pve.endpoints[%d] is not a host or host:port", i)
		}
		if _, _, splitErr := net.SplitHostPort(entry); splitErr != nil {
			return endpointPlan{}, cpierrors.Cloud("pve client init: the endpoint list was not resolved: "+
				"pve.endpoints[%d] has no port; "+
				"the config layer must give every entry its port before the client is built", i)
		}
		address := net.JoinHostPort(host, strconv.Itoa(port))
		plan.endpoints = append(plan.endpoints, endpointpool.Endpoint{
			Address: address,
			Kind:    endpointpool.KindConfigured,
			Node:    configuredEndpointNode(address),
		})
	}
	plan.configured = len(plan.endpoints)

	if len(cfg.Endpoints) > 0 {
		if err := checkPrimaryEndpoint(plan.endpoints[0].Address, opts); err != nil {
			return endpointPlan{}, err
		}
	}
	if source != nil {
		plan.endpoints = append(plan.endpoints, source.DiscoveredEndpoints()...)
	}
	return plan, nil
}

// checkPrimaryEndpoint refuses a config whose first pve.endpoints entry is not
// the host and port the SDK will send to.
func checkPrimaryEndpoint(first string, opts sdkclient.Options) error {
	host, _, ok := config.NormalizeEndpoint(strings.Trim(opts.Host, "[]"))
	if ok && net.JoinHostPort(host, strconv.Itoa(opts.Port)) == first {
		return nil
	}
	return cpierrors.Cloud("pve client init: the first pve.endpoints entry (%s) is not pve.host with pve.port; "+
		"set pve.host to the first endpoint, or drop pve.host", first)
}

// applyPoolTimeoutDefaults gives a client whose pool is meant to hold more than
// one endpoint a 5-second dial timeout and a 10-second TLS handshake timeout,
// for each one the operator left at 0. A pool is meant to hold more than one
// endpoint when it already does, or when discovery is effectively on, because
// then a lone name seed grows once discovery answers and must not wait on the
// kernel's connect timeout before that happens. An operator's value wins, and a
// pool of one endpoint with discovery effectively off keeps exactly the options
// it was given. buildTransportOpts stays untouched, so its byte-identical test
// keeps its meaning.
func applyPoolTimeoutDefaults(opts sdkclient.Options, poolSize int, discovery bool) sdkclient.Options {
	if poolSize <= 1 && !discovery {
		return opts
	}
	if opts.DialTimeoutSec == 0 {
		opts.DialTimeoutSec = poolDialTimeoutSec
	}
	if opts.TLSHandshakeTimeoutSec == 0 {
		opts.TLSHandshakeTimeoutSec = poolTLSHandshakeTimeoutSec
	}
	return opts
}

// endpointWiring is what newClient keeps from planning a client's endpoints.
type endpointWiring struct {
	plan endpointPlan
	// poolErr is the error the transport wrapper met while building the pool.
	// The SDK gives the wrapper no way to return one, so a pool that fails to
	// build comes back as a nil round tripper, which makes sdkclient.NewClient
	// fail, and newClient reads the cause here.
	poolErr error
	// pool is the pool the transport wrapper built, or nil when no wrapper was
	// installed or the build failed.
	pool *endpointpool.Pool
}

// wireEndpoints plans cfg's endpoints from the config alone and returns the
// transport options they start from. It touches no file. newClient adds the
// auth and TLS settings, reads the endpoint cache into the plan, and then
// calls apply for the pool settings.
func wireEndpoints(cfg *config.CPIConfig) (sdkclient.Options, *endpointWiring, error) {
	opts := buildTransportOpts(cfg)
	plan, err := planEndpoints(cfg, opts, noDiscoveredEndpoints{})
	if err != nil {
		return sdkclient.Options{}, nil, err
	}
	return opts, &endpointWiring{plan: plan}, nil
}

// apply gives opts the pool timeout defaults and, when the plan installs one,
// the transport wrapper. It runs after the cache was read, because the cache
// decides the pool's size. cache may be nil, and then the pool gets no mark
// sink and no first-answer hook.
//
// The endpoints are settled first. If the pool would refuse the cached
// endpoints, the plan keeps the configured endpoints alone, before the timeouts
// are sized from the plan, so a client that falls back to one endpoint with
// discovery effectively off keeps the timeouts of a pool of one. A cache file can cost the client its
// discovered nodes and never the client itself.
func (w *endpointWiring) apply(opts sdkclient.Options, logger *log.Logger, cache *endpointCache) sdkclient.Options {
	if w.plan.configured < len(w.plan.endpoints) {
		if err := endpointpool.CheckEndpoints(w.plan.endpoints); err != nil {
			cache.poolRefused(err)
			w.dropDiscovered(cache)
		}
	}
	opts = applyPoolTimeoutDefaults(opts, len(w.plan.endpoints), w.plan.discoveryEffective())
	if !w.plan.install() {
		return opts
	}
	opts.WrapTransport = func(base *http.Transport) http.RoundTripper {
		cfg := endpointpool.Config{Base: base, Logger: logger, Endpoints: w.plan.endpoints}
		cache.attach(&cfg)
		pool, poolErr := endpointpool.New(cfg)
		if poolErr != nil {
			w.poolErr = poolErr
			return nil
		}
		w.pool = pool
		return pool
	}
	return opts
}

// refuseRedirectsWithoutPool gives a client the plan builds no pool for a
// transport wrapper that refuses a redirect follow-up, so no client the CPI
// builds follows a redirect, whether or not the pool is installed. A client
// with a pool already refuses one inside the pool. The SDK's client refuses a
// redirect answer first, in a transport it installs above this wrapper and
// above the pool, unless its options set a CheckRedirect policy, which the CPI
// never does. Both refusals carry the SDK's permanent marker and match
// sdkclient.ErrRedirectRefused, so either one ends the request after one
// attempt. The CPI's refusal stays for a client whose policy follows
// redirects, where it is the only one that fires. The wrapper replaces the
// SDK's swappable transport, which only SetKeepAlive uses, and the CPI never
// calls it. The SDK's Close still reaches the base transport's idle
// connections.
func refuseRedirectsWithoutPool(opts sdkclient.Options) sdkclient.Options {
	if opts.WrapTransport == nil {
		opts.WrapTransport = func(base *http.Transport) http.RoundTripper {
			return endpointpool.RefuseRedirects(base)
		}
	}
	return opts
}

// dropDiscovered takes the discovered endpoints out of the plan, and out of the
// cache's mark writes, so the pool is built from the configured endpoints
// alone.
func (w *endpointWiring) dropDiscovered(cache *endpointCache) {
	w.plan.endpoints = w.plan.endpoints[:w.plan.configured]
	cache.dropDiscovered()
}
