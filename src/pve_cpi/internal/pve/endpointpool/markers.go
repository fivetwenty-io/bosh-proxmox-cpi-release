package endpointpool

import "context"

type configuredOnlyKey struct{}

// WithConfiguredOnly returns a context that marks a request as configured only.
// The failover transport walks only the configured endpoints for such a
// request, so the discovery worker never sends a credential to a discovered
// address and never learns anything through a node it discovered. The marker
// reaches every request made on the returned context.
func WithConfiguredOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, configuredOnlyKey{}, true)
}

// IsConfiguredOnly reports whether the context carries the configured-only
// marker.
func IsConfiguredOnly(ctx context.Context) bool {
	v, _ := ctx.Value(configuredOnlyKey{}).(bool)
	return v
}
