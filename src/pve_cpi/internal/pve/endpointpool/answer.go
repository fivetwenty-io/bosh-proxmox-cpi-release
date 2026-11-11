package endpointpool

import (
	"context"
	"net/http"
	"slices"
	"sync"
)

type answerRecorderKey struct{}

// AnswerRecorder keeps which configured endpoints answered the requests made
// on a context that carries it. A walk can hop between configured endpoints
// while one logical read goes on, so the endpoint that answered the request
// that began a piece of work is not always the one that answered its later
// requests. A caller that records evidence about where an answer came from
// reads it here and not from the first answer the pool reports. The zero value
// is ready to use, and it is safe for concurrent use.
type AnswerRecorder struct {
	mu   sync.Mutex
	last string
	seen []string
}

// WithAnswerRecorder returns a context whose requests record, in r, the
// configured endpoint that answered them. A context may carry several
// recorders, and each one records every answer, so a caller can scope one to a
// single read inside a run that another recorder covers. A nil r adds none.
func WithAnswerRecorder(ctx context.Context, r *AnswerRecorder) context.Context {
	if r == nil {
		return ctx
	}
	prior, _ := ctx.Value(answerRecorderKey{}).([]*AnswerRecorder)
	return context.WithValue(ctx, answerRecorderKey{}, append(slices.Clone(prior), r))
}

// Last returns the canonical host:port of the configured endpoint that
// answered the most recent request recorded, or the empty string when none
// has.
func (r *AnswerRecorder) Last() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Endpoints returns every configured endpoint that answered a recorded request,
// in the order each first answered.
func (r *AnswerRecorder) Endpoints() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

func (r *AnswerRecorder) record(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = addr
	if !slices.Contains(r.seen, addr) {
		r.seen = append(r.seen, addr)
	}
}

// recordAnswer tells the recorders on the request's context which configured
// endpoint answered it. oc is the outcome of the request that got resp, and
// its addr names the endpoint the response came from. A response from a
// discovered endpoint, a candidate, or a routed host is not recorded, and
// neither is one with a status that shows the endpoint could not serve the
// request, as noteAnswer counts them.
func (p *Pool) recordAnswer(ctx context.Context, resp *http.Response, oc outcome) {
	recorders, _ := ctx.Value(answerRecorderKey{}).([]*AnswerRecorder)
	if len(recorders) == 0 || resp == nil {
		return
	}
	ep := p.byAddr[oc.addr]
	if ep == nil || ep.kind != KindConfigured || !isAnswerStatus(resp.StatusCode) {
		return
	}
	for _, r := range recorders {
		r.record(ep.addr)
	}
}
