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
// client's pool after the configured ones. newClient asks it once, while it
// builds the pool, so the pool size is fixed for the life of the client.
type discoveredEndpointSource interface {
	// DiscoveredEndpoints returns endpoints of kind endpointpool.KindDiscovered,
	// each with its pin set and its node's online flag, in any order. The pool
	// orders them itself, the online nodes first and each group by node name.
	// It returns none when discovery has recorded nothing for this cluster.
	DiscoveredEndpoints() []endpointpool.Endpoint
}

// noDiscoveredEndpoints is the source a client uses while nothing discovers
// endpoints. It supplies none, so a pool holds exactly the configured ones.
type noDiscoveredEndpoints struct{}

// DiscoveredEndpoints returns no endpoints.
func (noDiscoveredEndpoints) DiscoveredEndpoints() []endpointpool.Endpoint { return nil }

// endpointDiscoveryOn reports whether the effective endpoint discovery mode is
// on. No discovery exists yet, so the mode is a fixed off for every config.
func endpointDiscoveryOn(_ *config.CPIConfig) bool { return false }

// endpointPlan is what newClient decides about a client's endpoints from its
// config alone, before any transport exists.
type endpointPlan struct {
	// endpoints lists the configured endpoints in the operator's order, then the
	// discovered ones the source supplied. The first is the primary, whose
	// host:port the SDK puts on every request.
	endpoints []endpointpool.Endpoint
	// configured counts the leading entries of endpoints the operator listed.
	configured int
	// discovery says the effective discovery mode is on.
	discovery bool
}

// install reports whether the failover transport wraps the SDK's. A pool of
// more than one endpoint needs it, and so does a client with discovery on, so
// the wrapper sees the first answered request even when the pool starts at
// one. Every other client keeps the SDK's own transport and builds the same
// Options as before the pool existed.
func (p endpointPlan) install() bool {
	return len(p.endpoints) > 1 || p.discovery
}

// failover reports whether this client can fail over between endpoints, which
// the pinning assertion needs. It reads only the config, never what discovery
// cached, so a config that builds today cannot fail to build tomorrow because
// the cache filled.
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
func planEndpoints(cfg *config.CPIConfig, opts sdkclient.Options, source discoveredEndpointSource) (endpointPlan, error) {
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
				"pve.endpoints[%d] has no port; the config layer must give every entry its port before the client is built", i)
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

// applyPoolTimeoutDefaults gives a client whose pool holds more than one
// endpoint a 5-second dial timeout and a 10-second TLS handshake timeout, for
// each one the operator left at 0. An operator's value wins, and a pool of one
// endpoint keeps exactly the options it was given. buildTransportOpts stays
// untouched, so its byte-identical test keeps its meaning.
func applyPoolTimeoutDefaults(opts sdkclient.Options, poolSize int) sdkclient.Options {
	if poolSize <= 1 {
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
}

// wireEndpoints plans cfg's endpoints and returns the SDK options they decide.
// Those are the transport options with the pool timeout defaults, and the
// transport wrapper when the plan installs one. newClient adds the auth and
// TLS settings to the result.
func wireEndpoints(cfg *config.CPIConfig, logger *log.Logger) (sdkclient.Options, *endpointWiring, error) {
	opts := buildTransportOpts(cfg)
	plan, err := planEndpoints(cfg, opts, noDiscoveredEndpoints{})
	if err != nil {
		return sdkclient.Options{}, nil, err
	}
	opts = applyPoolTimeoutDefaults(opts, len(plan.endpoints))
	wiring := &endpointWiring{plan: plan}
	if plan.install() {
		opts.WrapTransport = func(base *http.Transport) http.RoundTripper {
			pool, poolErr := endpointpool.New(endpointpool.Config{Base: base, Logger: logger, Endpoints: plan.endpoints})
			if poolErr != nil {
				wiring.poolErr = poolErr
				return nil
			}
			return pool
		}
	}
	return opts, wiring, nil
}
