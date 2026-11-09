package endpointpool

import (
	"context"
	"strings"
	"time"
)

type sendByKey struct{}

// sendByMarker is one send-by deadline and the request it guards. Markers
// chain, so a second WithSendBy on the same context for another request does
// not hide the first.
type sendByMarker struct {
	deadline time.Time
	method   string
	path     string
	parent   *sendByMarker
}

func (m *sendByMarker) matches(method, path string) bool {
	return strings.EqualFold(m.method, method) && m.path == path
}

// WithSendBy returns a context that carries a send-by deadline for the request
// with the given method and URL path. The deadline bounds the moment the
// request's bytes are written. The failover transport checks it before every
// hop, uses it to bound connection setup, and refuses to send with
// ErrSendByPassed once it has passed.
//
// The marker names the one request it guards, so a read on the same context,
// such as a readback after the guarded call, ignores it. The path is the URL
// path without the query string. A marker with an empty method or path never
// matches anything. When the context already carries markers they stay, and a
// request that matches several is held to the earliest deadline.
func WithSendBy(ctx context.Context, deadline time.Time, method, path string) context.Context {
	if method == "" || path == "" {
		return ctx
	}
	parent, _ := ctx.Value(sendByKey{}).(*sendByMarker)
	return context.WithValue(ctx, sendByKey{}, &sendByMarker{
		deadline: deadline,
		method:   strings.ToUpper(method),
		path:     path,
		parent:   parent,
	})
}

// SendBy returns the send-by deadline that applies to the request with the
// given method and URL path. It reports false when the context carries no
// marker that matches the request. The method comparison ignores case.
func SendBy(ctx context.Context, method, path string) (time.Time, bool) {
	var (
		found    bool
		earliest time.Time
	)
	m, _ := ctx.Value(sendByKey{}).(*sendByMarker)
	for ; m != nil; m = m.parent {
		if !m.matches(method, path) {
			continue
		}
		if !found || m.deadline.Before(earliest) {
			earliest = m.deadline
			found = true
		}
	}
	return earliest, found
}

// CheckSendBy returns ErrSendByPassed when the context carries a send-by
// deadline that matches the request and now is at or after it. A deadline that
// has only just been reached counts as passed, so the check fails closed. It
// returns nil for a request no marker guards.
func CheckSendBy(ctx context.Context, now time.Time, method, path string) error {
	deadline, ok := SendBy(ctx, method, path)
	if !ok {
		return nil
	}
	if !now.Before(deadline) {
		return ErrSendByPassed
	}
	return nil
}
