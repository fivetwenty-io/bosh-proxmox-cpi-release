package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A request refused because DropDiscovered took its endpoint out says so. The
// endpoint did not fail its pin, so the refusal is not a pin mismatch, and it
// keeps the connect-phase tag and the certificate error that make the walk hop
// and the SDK stop at once.
func TestDroppedEndpointRefusalNamesTheDropNotAPinFailure(t *testing.T) {
	l := newLab(t)
	p := l.poolWith(t, nil, nil, nil, configured("c1.example.invalid:8006"),
		node("d1.example.invalid:8006", "pve2", true, Pin{1}), node("d2.example.invalid:8006", "pve3", true, Pin{2}))
	d1, d2 := endpointOf(t, p, "d1.example.invalid:8006"), endpointOf(t, p, "d2.example.invalid:8006")
	if _, ok := p.makeIneligible(d1, &ConnectPhaseError{Phase: PhasePin, Endpoint: d1.addr, Err: ErrPinMismatch}); !ok {
		t.Fatal("fail the first endpoint's pin")
	}
	if n := p.DropDiscovered(); n != 1 {
		t.Fatalf("DropDiscovered took out %d endpoints, want only the second", n)
	}

	req := apiRequest(t, context.Background(), http.MethodGet, "c1.example.invalid:8006", "/version", "")
	hopOf := func(ep *endpoint) hopResult {
		w := p.newWalker(req, false, defaultBounds())
		w.order = []*endpoint{ep, endpointOf(t, p, "c1.example.invalid:8006")}
		return w.try(0, ep, nil)
	}

	dropped := hopOf(d2).err
	var certErr *tls.CertificateVerificationError
	switch {
	case !errors.Is(dropped, errDropped), errors.Is(dropped, errIneligible), errors.Is(dropped, ErrPinMismatch):
		t.Errorf("a dropped endpoint's refusal is %v, want the drop's cause alone", dropped)
	case !IsConnectPhase(dropped), IsMaybeSent(dropped), !errors.As(dropped, &certErr):
		t.Errorf("a dropped endpoint's refusal %v lost its connect-phase tag or its certificate error", dropped)
	case !strings.Contains(dropped.Error(), "discovery found that the cluster identity changed, so the pool "+
		"stopped using its discovered endpoints"):
		t.Errorf("a dropped endpoint's refusal does not say why: %v", dropped)
	case blamesEndpoint(dropped):
		t.Error("a dropped endpoint's refusal counts against the endpoint")
	}

	failed := hopOf(d1).err
	if !errors.Is(failed, errIneligible) || errors.Is(failed, errDropped) || !errors.Is(failed, ErrPinMismatch) {
		t.Errorf("an endpoint that failed its pin before the drop is refused with %v, want its pin refusal", failed)
	}
	if blamesEndpoint(failed) {
		t.Error("a pin refusal counts against the endpoint again")
	}
	mustDemoted(t, p, "d2.example.invalid:8006", false)
}
