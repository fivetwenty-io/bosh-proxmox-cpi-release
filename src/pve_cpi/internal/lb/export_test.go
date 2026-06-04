package lb

import "context"

// testResolver is the interface used as the resolver test seam.
type testResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// ConfigWithResolver returns a copy of cfg with the unexported resolver field
// set to r. Used only by external (_test) packages to inject a fake DNS
// resolver so private-IP checks run without real network calls.
func ConfigWithResolver(cfg HAProxyConfig, r testResolver) HAProxyConfig {
	cfg.resolver = r
	return cfg
}
