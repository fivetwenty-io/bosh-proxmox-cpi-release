package client

import (
	"context"
	"time"

	pvehttp "github.com/fivetwenty-io/pve-apiclient-go/v3/internal/http"
)

// WithRetries sets per-request retry attempts in the context for this client.
func WithRetries(ctx context.Context, n int) context.Context { return pvehttp.WithRetries(ctx, n) }

// WithRetryDelay sets per-request retry base delay in the context for this client.
func WithRetryDelay(ctx context.Context, d time.Duration) context.Context {
	return pvehttp.WithRetryDelay(ctx, d)
}

// WithLogging toggles request logging for this client (no-op logger currently) in the context.
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
