package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// mustBeTheCertificateRefusal fails the test unless e names the certificate
// refusal with the class tls_verify, and nothing of the send-by refusal that
// wrapped it.
func mustBeTheCertificateRefusal(t *testing.T, e log.Entry) {
	t.Helper()
	mustAttrs(t, e, map[string]string{"class": string(PhaseTLSVerify)})
	cause, _ := e.Attrs["error"].(string)
	if cause == "" || strings.Contains(cause, ErrSendByPassed.Error()) ||
		strings.Contains(e.Message, ErrSendByPassed.Error()) {
		t.Fatalf("the line should name the certificate refusal alone, got %q with cause %q", e.Message, cause)
	}
}

// mustNoteTheCertificateRefusal fails the test unless the failure note for ep
// is the certificate refusal with the class tls_verify, without the send-by
// wrapping.
func mustNoteTheCertificateRefusal(t *testing.T, p *Pool, ep *endpoint) {
	t.Helper()
	note, ok := p.walk.noteFor(ep)
	if !ok || note.class != string(PhaseTLSVerify) || strings.Contains(note.text, ErrSendByPassed.Error()) {
		t.Fatalf("failure note = %+v, %v, want the certificate refusal with class tls_verify", note, ok)
	}
}

// An accepted cold-start candidate that fails TLS verification just as a
// tethered mutation's send-by bound passes is revoked as it would be without
// the bound. The mutation reports the send-by refusal, while the candidate
// becomes ineligible and demoted, gets no mark, and the revocation is logged
// once at Error with the certificate refusal as its cause. A later mutation
// tethered to the candidate is refused before any dial.
func TestALateVerificationFailureRevokesTheCandidate(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	cand, c := l.candidate(t, "pve2.example.com", nil)
	sink := &markRecorder{}
	cfg := l.coldConfig(&fakeProber{}, []Candidate{c}, configured(seed.Addr))
	cfg.MarkSink = sink
	p := newPool(t, cfg)
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodPost, seed.Addr, "/pools",
		"poolid=bosh-lock-x"))
	mustStatus(t, resp, err, http.StatusOK)
	ep := p.memberAt(c.Address)
	if ep == nil || ep.kind != kindCandidate {
		t.Fatalf("memberAt = %v, want the accepted candidate", ep)
	}

	bad := l.otherLeaf(t)
	l.d.set(c.Address, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, bad.Addr)
	})
	p.CloseIdleConnections()
	lateSendBy(ep.transports)
	release := func(ctx context.Context) *http.Request {
		req := tokenRequest(t, ctx, http.MethodDelete, seed.Addr, "/pools", "")
		req.URL.RawQuery = "poolid=bosh-lock-x"
		return req
	}
	dials := seed.Counts.Dials.Load()
	ctx := WithSendBy(context.Background(), time.Now().Add(time.Hour), http.MethodDelete, "/pools")
	resp, err = p.RoundTrip(release(ctx))
	closeBody(resp)
	if !errors.Is(err, ErrSendByPassed) {
		t.Fatalf("the release should report the send-by refusal, got %v", err)
	}
	if n := bad.Counts.Conns.Load(); n == 0 {
		t.Fatal("the release never reached the candidate's address")
	}
	if n := seed.Counts.Dials.Load(); n != dials {
		t.Fatalf("the release dialed the seed %d times", n-dials)
	}
	if !p.ineligibleNow(ep) || !p.demotedNow(ep) {
		t.Fatalf("candidate ineligible %v, demoted %v, want both", p.ineligibleNow(ep), p.demotedNow(ep))
	}
	prefix := "pve endpoint " + c.Address + ": the cold-start candidate"
	lines := l.entries(slog.LevelError, prefix)
	if len(lines) != 1 {
		t.Fatalf("revocation lines = %+v, want one", lines)
	}
	mustBeTheCertificateRefusal(t, lines[0])
	mustNoteTheCertificateRefusal(t, p, ep)
	wantMarks(t, sink.all(), map[string]Phase{seed.Addr: PhaseDial})

	conns := bad.Counts.Conns.Load()
	_, err = p.RoundTrip(release(context.Background()))
	if err == nil || !strings.Contains(err.Error(), "which failed TLS verification after it proved membership") {
		t.Fatalf("err = %v, want the revoked tether refusal", err)
	}
	if n := bad.Counts.Conns.Load(); n != conns {
		t.Fatalf("the refused release dialed the candidate's address %d times", n-conns)
	}
	if n := cand.Counts.Method(http.MethodDelete); n != 0 {
		t.Fatalf("candidate got %d DELETEs", n)
	}
	if lines := l.entries(slog.LevelError, prefix); len(lines) != 1 {
		t.Fatalf("revocation lines after the refused release = %+v, want one", lines)
	}
}

// A configured endpoint that fails TLS verification just as the request's
// send-by bound passes counts as it would without the bound, on the walk and
// on a routed request. The request reports the send-by refusal, and the
// endpoint is demoted, gets a tls_verify mark, and is logged once at Error
// with the certificate refusal as its cause.
func TestALateVerificationFailureOfAConfiguredEndpointCounts(t *testing.T) {
	sendByCtx := func() context.Context {
		return WithSendBy(context.Background(), time.Now().Add(time.Hour), http.MethodGet, "/version")
	}
	check := func(t *testing.T, l *lab, p *Pool, sink *markRecorder, err error, bad string) {
		t.Helper()
		if !errors.Is(err, ErrSendByPassed) {
			t.Fatalf("the request should report the send-by refusal, got %v", err)
		}
		mustDemoted(t, p, bad, true)
		wantMarks(t, sink.all(), map[string]Phase{bad: PhaseTLSVerify})
		lines := l.entries(slog.LevelError, "pve endpoint "+bad+": TLS verification failed (")
		if len(lines) != 1 || !strings.HasSuffix(lines[0].Message, "; no other endpoint is tried for this request") {
			t.Fatalf("verification lines = %+v, want one that tries no other endpoint", lines)
		}
		mustBeTheCertificateRefusal(t, lines[0])
		mustNoteTheCertificateRefusal(t, p, endpointOf(t, p, bad))
	}
	t.Run("on the walk", func(t *testing.T) {
		l := newLab(t)
		a := l.otherLeaf(t)
		b := l.live(t, nil)
		sink := &markRecorder{}
		p := l.poolWith(t, sink, nil, nil, configured(a.Addr), configured(b.Addr))
		lateSendBy(endpointOf(t, p, a.Addr).transports)
		resp, err := send(t, sendByCtx(), p, http.MethodGet, a.Addr, "/version", "")
		closeBody(resp)
		check(t, l, p, sink, err, a.Addr)
		if n := b.Counts.Requests.Load(); n != 0 {
			t.Fatalf("the next endpoint got %d requests after the send-by refusal", n)
		}
	})
	t.Run("on a routed request", func(t *testing.T) {
		l := newLab(t)
		a := l.live(t, nil)
		b := l.otherLeaf(t)
		sink := &markRecorder{}
		p := l.poolWith(t, sink, nil, nil, configured(a.Addr), configured(b.Addr))
		o, err := p.routedTransportsAt(b.Addr, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		lateSendBy(o)
		resp, err := send(t, sendByCtx(), p, http.MethodGet, b.Addr, "/version", "")
		closeBody(resp)
		check(t, l, p, sink, err, b.Addr)
		mustActive(t, p, a.Addr)
		if n := a.Counts.Requests.Load(); n != 0 {
			t.Fatalf("the active endpoint got %d requests", n)
		}
	})
}

// A late certificate refusal counts only in the refusal phase of its
// endpoint's kind, which is the pin for a discovered endpoint and TLS
// verification for a configured endpoint and a cold-start candidate, and its
// cause loses the send-by wrapping and keeps the certificate refusal.
func TestLateCertFailureCountsTheKindsOwnRefusal(t *testing.T) {
	refused := &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}
	pinRefused := &tls.CertificateVerificationError{Err: fmt.Errorf("%w: not pinned", ErrPinMismatch)}
	late := func(phase Phase, cause error) error {
		return &ConnectPhaseError{Phase: phase, Endpoint: "pve2.example.com:8006",
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, cause)}
	}
	cases := []struct {
		name  string
		kind  Kind
		err   error
		phase Phase
	}{
		{"a candidate's verification failure", kindCandidate, late(PhaseTLSVerify, refused), PhaseTLSVerify},
		{"a configured endpoint's verification failure", KindConfigured, late(PhaseTLSVerify, refused),
			PhaseTLSVerify},
		{"a discovered endpoint's pin failure", KindDiscovered, late(PhasePin, pinRefused), PhasePin},
		{"a refusal joined to more than the send-by refusal", kindCandidate,
			&ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: "pve2.example.com:8006",
				Err: errors.Join(ErrSendByPassed, errors.New("handshake ended late"), refused)}, PhaseTLSVerify},
		{"a candidate's pin failure", kindCandidate, late(PhasePin, pinRefused), ""},
		{"a configured endpoint's pin failure", KindConfigured, late(PhasePin, pinRefused), ""},
		{"a discovered endpoint's verification failure", KindDiscovered, late(PhaseTLSVerify, refused), ""},
		{"a candidate's late handshake failure", kindCandidate, late(PhaseTLSHandshake, refused), ""},
		{"a candidate's late dial failure", kindCandidate, late(PhaseDial, errors.New("connection refused")), ""},
		{"a verification failure inside the bound", kindCandidate,
			&ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: "pve2.example.com:8006", Err: refused}, ""},
		{"a bare send-by refusal", kindCandidate,
			&ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: "pve2.example.com:8006", Err: ErrSendByPassed}, ""},
		{"a pool refusal", KindConfigured, late(PhaseTLSVerify, errPlaintextRefused), ""},
		{"an untagged error", kindCandidate, fmt.Errorf("%w: %w", ErrSendByPassed, refused), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lateCertFailure(tc.kind, tc.err)
			if tc.phase == "" {
				if got != nil {
					t.Fatalf("lateCertFailure = %v, want nil", got)
				}
				return
			}
			if got == nil || got.Phase != tc.phase || got.Endpoint != "pve2.example.com:8006" {
				t.Fatalf("lateCertFailure = %v, want a %s refusal for the endpoint", got, tc.phase)
			}
			if errors.Is(got, ErrSendByPassed) || !blamesEndpoint(got) {
				t.Fatalf("lateCertFailure = %v, want the refusal without the send-by wrapping", got)
			}
			var cert *tls.CertificateVerificationError
			if !errors.As(got, &cert) {
				t.Fatalf("lateCertFailure = %v, want it to keep the certificate refusal", got)
			}
		})
	}
}
