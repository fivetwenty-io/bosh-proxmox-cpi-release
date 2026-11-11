package endpointpool

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

// The recorder holds the endpoint that answered the latest request, which is
// not the endpoint that answered the first one once a walk hops. The
// first-answer hook is told once, about the first.
func TestAnswerRecorderHoldsTheEndpointThatAnsweredAfterAHop(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	var told []string
	p := newPool(t, Config{
		Base: baseFor(l.ca, l.d), Logger: l.logger, Endpoints: []Endpoint{configured(a.Addr), configured(b.Addr)},
		FirstAnswer: answerFunc(func(addr string) { told = append(told, addr) }),
	})
	rec, scoped := &AnswerRecorder{}, &AnswerRecorder{}
	ctx := WithAnswerRecorder(WithConfiguredOnly(context.Background()), rec)

	resp, err := send(t, ctx, p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if got := rec.Last(); got != a.Addr {
		t.Fatalf("Last = %q after the first answer, want %s", got, a.Addr)
	}

	l.d.set(a.Addr, l.refusedDial(t))
	p.CloseIdleConnections()
	resp, err = send(t, WithAnswerRecorder(ctx, scoped), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if got := rec.Last(); got != b.Addr {
		t.Fatalf("Last = %q after the hop, want %s", got, b.Addr)
	}
	if got := rec.Endpoints(); !slices.Equal(got, []string{a.Addr, b.Addr}) {
		t.Fatalf("Endpoints = %v, want %s then %s", got, a.Addr, b.Addr)
	}
	if got := scoped.Endpoints(); !slices.Equal(got, []string{b.Addr}) {
		t.Fatalf("a recorder added for the second read holds %v, want only %s", got, b.Addr)
	}
	if !slices.Equal(told, []string{a.Addr}) {
		t.Fatalf("the first-answer hook was told %v, want %s once", told, a.Addr)
	}
}

// Only a configured endpoint's answer is recorded: a discovered endpoint that
// serves the request after every configured endpoint failed leaves the
// recorder as it was, and so does a request that carries no recorder.
func TestAnswerRecorderIgnoresDiscoveredEndpointsAndNilRecorders(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	d := l.live(t, nil)
	p := l.poolWith(t, nil, nil, nil, configured(seed.Addr), discovered(d.Addr, PinOf(l.leaf.DER)))
	rec := &AnswerRecorder{}
	ctx := WithAnswerRecorder(context.Background(), rec)
	ctx = WithAnswerRecorder(ctx, nil)

	resp, err := send(t, ctx, p, http.MethodGet, seed.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := d.Counts.Requests.Load(); n != 1 {
		t.Fatalf("the discovered endpoint got %d requests, want the one the walk hopped to", n)
	}
	if rec.Last() != "" || len(rec.Endpoints()) != 0 {
		t.Fatalf("the recorder holds %q and %v after an answer from a discovered endpoint", rec.Last(), rec.Endpoints())
	}

	var none *AnswerRecorder
	if none.Last() != "" || none.Endpoints() != nil {
		t.Fatal("a nil recorder is not empty")
	}
	resp, err = send(t, context.Background(), p, http.MethodGet, seed.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
}

type answerFunc func(addr string)

func (f answerFunc) FirstAnswer(addr string) { f(addr) }
