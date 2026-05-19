package http

import (
	"context"
	"time"
)

type requestOptionsKey struct{}

// RequestOptions controls per-request behavior for middleware.
type RequestOptions struct {
	Retries    *int
	RetryDelay *time.Duration
	Logging    *bool
	Fields     map[string]interface{}
}

// FromContext extracts RequestOptions from context.
func FromContext(ctx context.Context) *RequestOptions {
	if v := ctx.Value(requestOptionsKey{}); v != nil {
		if opts, ok := v.(*RequestOptions); ok {
			return opts
		}
	}

	return &RequestOptions{}
}

// with merges and returns a new context with merged options.
func with(ctx context.Context, update func(*RequestOptions)) context.Context {
	existing := FromContext(ctx)
	// copy
	opts := &RequestOptions{}

	if existing.Retries != nil {
		r := *existing.Retries
		opts.Retries = &r
	}

	if existing.RetryDelay != nil {
		d := *existing.RetryDelay
		opts.RetryDelay = &d
	}

	if existing.Logging != nil {
		b := *existing.Logging
		opts.Logging = &b
	}

	if existing.Fields != nil {
		opts.Fields = make(map[string]interface{}, len(existing.Fields))
		for k, v := range existing.Fields {
			opts.Fields[k] = v
		}
	}

	update(opts)

	return context.WithValue(ctx, requestOptionsKey{}, opts)
}

// WithRetries sets per-request retry attempts.
func WithRetries(ctx context.Context, n int) context.Context {
	return with(ctx, func(opts *RequestOptions) { opts.Retries = &n })
}

// WithRetryDelay sets per-request retry base delay.
func WithRetryDelay(ctx context.Context, d time.Duration) context.Context {
	return with(ctx, func(opts *RequestOptions) { opts.RetryDelay = &d })
}

// WithLogging toggles logging for this request.
func WithLogging(ctx context.Context, enabled bool) context.Context {
	return with(ctx, func(opts *RequestOptions) { opts.Logging = &enabled })
}

// WithLogFields attaches structured fields for logging on this request.
func WithLogFields(ctx context.Context, fields map[string]interface{}) context.Context {
	return with(ctx, func(opts *RequestOptions) {
		if opts.Fields == nil {
			opts.Fields = make(map[string]interface{}, len(fields))
		}

		for key, value := range fields {
			opts.Fields[key] = value
		}
	})
}
