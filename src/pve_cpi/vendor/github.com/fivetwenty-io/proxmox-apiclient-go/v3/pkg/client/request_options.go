package client

import (
	"context"
	"time"

	pvehttp "github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/http"
)

// WithRetries sets per-request retry attempts in the context for this client.
func WithRetries(ctx context.Context, n int) context.Context { return pvehttp.WithRetries(ctx, n) }

// WithRetryDelay sets per-request retry base delay in the context for this client.
func WithRetryDelay(ctx context.Context, d time.Duration) context.Context {
	return pvehttp.WithRetryDelay(ctx, d)
}

// WithHost overrides the host[:port] this request is sent to, keeping the
// client's protocol and path — for endpoints that must be reached on a
// specific cluster node (e.g. uploading to the node that owns a local
// storage). When the override carries no port the client's configured port is
// kept. Authentication is unaffected: PVE tokens and tickets are cluster-wide,
// and any re-authentication still goes to the configured base host.
//
// TLS: standard CA verification follows the request URL, so the target node's
// certificate must carry a SAN for the dialed host. TLS fingerprint pinning
// verifies only the configured base host; to avoid silently accepting the
// wrong pin, a request with a host override fails fast with
// ErrHostOverrideFingerprint when fingerprint pinning is enabled.
func WithHost(ctx context.Context, host string) context.Context { return pvehttp.WithHost(ctx, host) }

// WithLogging toggles request logging on or off for this request via the context.
func WithLogging(ctx context.Context, enabled bool) context.Context {
	return pvehttp.WithLogging(ctx, enabled)
}

// WithLogFields attaches structured fields for logging on this request.
func WithLogFields(ctx context.Context, fields map[string]interface{}) context.Context {
	return pvehttp.WithLogFields(ctx, fields)
}

// WithTimeout is a convenience helper that wraps context with a timeout.
func WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
