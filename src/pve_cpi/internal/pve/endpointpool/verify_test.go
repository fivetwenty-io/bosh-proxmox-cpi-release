package endpointpool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// pinFixture is a configured server and two discovered servers with different
// certificates under one test CA.
type pinFixture struct {
	ca           *pvetest.CA
	seed         *pvetest.Server
	nodeA, nodeB *pvetest.Server
	leafA, leafB *pvetest.Leaf
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	f := &pinFixture{ca: pvetest.NewCA(t)}
	f.seed = pvetest.NewLive(t, f.ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	f.leafA = f.ca.NewLeaf(t, pvetest.LeafOptions{})
	f.leafB = f.ca.NewLeaf(t, pvetest.LeafOptions{})
	f.nodeA = pvetest.NewLive(t, f.leafA, nil)
	f.nodeB = pvetest.NewLive(t, f.leafB, nil)
	return f
}

// pool builds a pool of the seed and one discovered server with the given
// pins, under verify_ssl true or false.
func (f *pinFixture) pool(t *testing.T, verifySSL bool, node *pvetest.Server, pins ...Pin) *Pool {
	t.Helper()
	ca := f.ca
	if !verifySSL {
		ca = nil
	}
	return newPool(t, Config{
		Base:      baseFor(ca, nil),
		Endpoints: []Endpoint{configured(f.seed.Addr), discovered(node.Addr, pins...)},
	})
}

func getVersion(t *testing.T, p *Pool, addr string) error {
	t.Helper()
	resp, err := send(t, context.Background(), endpointOf(t, p, addr).transports.pick(true), http.MethodGet, addr,
		"/version", "")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// The two discovered servers present different certificates, so each is
// accepted only on its own pin under verify_ssl false.
func TestTwoServersUnderOneCAHaveDistinctPins(t *testing.T) {
	f := newPinFixture(t)
	if PinOf(f.leafA.DER) == PinOf(f.leafB.DER) {
		t.Fatal("the two leaves should differ")
	}
	pa := f.pool(t, false, f.nodeA, PinOf(f.leafA.DER))
	if err := getVersion(t, pa, f.nodeA.Addr); err != nil {
		t.Fatalf("node A on its own pin: %v", err)
	}
	pb := f.pool(t, false, f.nodeB, PinOf(f.leafA.DER))
	err := getVersion(t, pb, f.nodeB.Addr)
	mustConnectPhase(t, err, f.nodeB.Addr, PhasePin)
}

// A leaf in the pin set connects under both verify_ssl values, and makes no
// refresh due.
func TestPinnedLeafConnectsUnderBothVerifySettings(t *testing.T) {
	f := newPinFixture(t)
	for _, verifySSL := range []bool{true, false} {
		t.Run(fmt.Sprintf("verify_ssl=%v", verifySSL), func(t *testing.T) {
			p := f.pool(t, verifySSL, f.nodeA, PinOf(f.leafB.DER), PinOf(f.leafA.DER))
			if err := getVersion(t, p, f.nodeA.Addr); err != nil {
				t.Fatalf("GET: %v", err)
			}
			if p.RefreshDue() {
				t.Fatal("a pinned leaf should not make a refresh due")
			}
		})
	}
}

// Under verify_ssl true, a leaf outside the pin set that chains to the
// operator's CA and covers the endpoint's name connects, and makes a refresh
// due so the next discovery records the new pin.
func TestLeafOutsideThePinSetConnectsOnANameMatch(t *testing.T) {
	f := newPinFixture(t)
	p := f.pool(t, true, f.nodeB, PinOf(f.leafA.DER))
	if p.RefreshDue() {
		t.Fatal("no refresh should be due before any handshake")
	}
	if err := getVersion(t, p, f.nodeB.Addr); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if !p.RefreshDue() {
		t.Fatal("a name match outside the pin set should make a refresh due")
	}
	if n := f.nodeB.Counts.Method(http.MethodGet); n != 1 {
		t.Fatalf("GETs = %d, want 1", n)
	}
}

// A wrong leaf fails before the handler sees a request, with the pin phase
// and a certificate error the SDK recognizes.
func TestWrongLeafFailsBeforeTheHandler(t *testing.T) {
	f := newPinFixture(t)
	foreign := pvetest.NewCA(t)
	nameless := pvetest.NewLive(t, f.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve3.other.example"}}), nil)
	untrusted := pvetest.NewLive(t, foreign.NewLeaf(t, pvetest.LeafOptions{}), nil)
	cases := []struct {
		name      string
		verifySSL bool
		node      *pvetest.Server
		pins      []Pin
		cause     func(error) bool
	}{
		{
			name: "verify_ssl false and a leaf outside the set", node: f.nodeB, pins: []Pin{PinOf(f.leafA.DER)},
			cause: func(err error) bool { return strings.Contains(err.Error(), "is not one of the 1 pinned") },
		},
		{
			name: "verify_ssl true and a leaf for another name", verifySSL: true, node: nameless,
			pins: []Pin{PinOf(f.leafA.DER)},
			cause: func(err error) bool {
				var he x509.HostnameError
				return errors.As(err, &he)
			},
		},
		{
			name: "verify_ssl true and a pinned leaf from another CA", verifySSL: true, node: untrusted,
			pins: []Pin{PinOf(untrusted.Leaf.DER)},
			cause: func(err error) bool {
				var ua x509.UnknownAuthorityError
				return errors.As(err, &ua)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := f.pool(t, c.verifySSL, c.node, c.pins...)
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				resp, err := send(t, context.Background(), endpointOf(t, p, c.node.Addr).transports.pick(false), method,
					c.node.Addr, "/access/ticket", "username=root@pam&password=secret")
				if resp != nil {
					_ = resp.Body.Close()
				}
				mustConnectPhase(t, err, c.node.Addr, PhasePin)
				if !errors.Is(err, ErrPinMismatch) {
					t.Fatalf("want ErrPinMismatch: %v", err)
				}
				var cve *tls.CertificateVerificationError
				if !errors.As(err, &cve) {
					t.Fatalf("want a *tls.CertificateVerificationError for the SDK: %v", err)
				}
				if !c.cause(err) {
					t.Fatalf("unexpected cause: %v", err)
				}
			}
			if n := c.node.Counts.Requests.Load(); n != 0 {
				t.Fatalf("the handler saw %d requests, want none", n)
			}
			if c.node.Counts.Handshakes.Load() == 0 {
				t.Fatal("the refusal should come from the handshake")
			}
			if p.RefreshDue() {
				t.Fatal("a refusal does not make a refresh due")
			}
		})
	}
}

// Under verify_ssl false a pinned leaf is accepted even when it has expired,
// because nothing but the pin is checked, and under verify_ssl true the
// expired chain refuses it whatever the pin says.
func TestExpiredPinnedLeaf(t *testing.T) {
	f := newPinFixture(t)
	expired := f.ca.NewLeaf(t, pvetest.LeafOptions{
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-24 * time.Hour),
	})
	node := pvetest.NewLive(t, expired, nil)
	if err := getVersion(t, f.pool(t, false, node, PinOf(expired.DER)), node.Addr); err != nil {
		t.Fatalf("verify_ssl false with the pin: %v", err)
	}
	err := getVersion(t, f.pool(t, true, node, PinOf(expired.DER)), node.Addr)
	mustConnectPhase(t, err, node.Addr, PhasePin)
	var ie x509.CertificateInvalidError
	if !errors.As(err, &ie) || ie.Reason != x509.Expired {
		t.Fatalf("want an expired-certificate cause: %v", err)
	}
}

// A discovered hostname goes out as SNI and is the name the fallback checks.
func TestDiscoveredHostnameIsTheVerifiedName(t *testing.T) {
	f := newPinFixture(t)
	addr := net.JoinHostPort("localhost", fmt.Sprint(f.nodeB.Port))
	d := &dialers{}
	d.set(addr, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, f.nodeB.Addr)
	})
	p := newPool(t, Config{
		Base:      baseFor(f.ca, d),
		Endpoints: []Endpoint{configured(f.seed.Addr), discovered(addr, PinOf(f.leafA.DER))},
	})
	if err := getVersion(t, p, addr); err != nil {
		t.Fatalf("GET: %v", err)
	}
	seen := f.nodeB.Seen()
	if len(seen) != 1 || seen[0].ServerName != "localhost" {
		t.Fatalf("want one request with SNI localhost, got %+v", seen)
	}
	if !p.RefreshDue() {
		t.Fatal("the name fallback should make a refresh due")
	}
}

// A cold-start candidate has no pin set, so only a verified chain and a name
// match accept it. Its refusal is a verification failure, not a pin failure,
// and its acceptance makes no refresh due.
func TestCandidateVerifier(t *testing.T) {
	f := newPinFixture(t)
	if _, err := newCandidateVerifier("pve2", false, nil, nil); err == nil {
		t.Fatal("a candidate under verify_ssl false must be refused")
	}
	nameless := pvetest.NewLive(t, f.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve3.other.example"}}), nil)
	base := baseFor(f.ca, nil)
	for _, c := range []struct {
		node  *pvetest.Server
		phase Phase
	}{{f.nodeA, ""}, {nameless, PhaseTLSVerify}} {
		v, err := newCandidateVerifier(c.node.Host, true, f.ca.Pool(), nil)
		if err != nil {
			t.Fatal(err)
		}
		refreshed := false
		v.refreshDue = func() { refreshed = true }
		o, err := newOwnedTransports(base, transportSpec{
			addr: c.node.Addr, verify: v.verifyConnection,
			dialCap: candidateDialCap, handshakeCap: candidateHandshakeCap,
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := send(t, context.Background(), o.pick(false), http.MethodGet, c.node.Addr, "/cluster/status", "")
		if c.phase == "" {
			if err != nil {
				t.Fatalf("candidate with a matching name: %v", err)
			}
			_ = resp.Body.Close()
		} else {
			if resp != nil {
				_ = resp.Body.Close()
			}
			mustConnectPhase(t, err, c.node.Addr, c.phase)
			if errors.Is(err, ErrPinMismatch) {
				t.Fatalf("a candidate's refusal is not a pin failure: %v", err)
			}
		}
		if refreshed {
			t.Fatal("a candidate never makes a refresh due")
		}
		o.closeIdle()
	}
}

// The callback refuses a handshake that carried no certificate, and a
// discovered endpoint must have pins.
func TestVerifierEdgeCases(t *testing.T) {
	if _, err := newDiscoveredVerifier("pve2", true, nil, nil, nil, nil); err == nil {
		t.Fatal("a discovered verifier without pins must be refused")
	}
	v, err := newDiscoveredVerifier("pve2", false, nil, nil, []Pin{{1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = v.verifyConnection(tls.ConnectionState{})
	if !errors.Is(err, errNoPeerCertificate) || !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("want a pin refusal for a missing certificate: %v", err)
	}
	c, err := newCandidateVerifier("pve2", true, nil, func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatal(err)
	}
	if err := c.verifyConnection(tls.ConnectionState{}); !errors.Is(err, errNoPeerCertificate) ||
		errors.Is(err, ErrPinMismatch) {
		t.Fatalf("want a plain verification refusal for a missing certificate: %v", err)
	}
}

// The callback checks the chain at the TLS config's clock when one is set.
func TestVerifierUsesTheConfiguredClock(t *testing.T) {
	f := newPinFixture(t)
	v, err := newDiscoveredVerifier(f.nodeA.Host, true, f.ca.Pool(), func() time.Time {
		return time.Now().Add(72 * time.Hour)
	}, []Pin{PinOf(f.leafA.DER)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.leafA.X509}}
	var ie x509.CertificateInvalidError
	if err := v.verifyConnection(cs); !errors.As(err, &ie) || ie.Reason != x509.Expired {
		t.Fatalf("want the leaf to read as expired three days from now: %v", err)
	}
	v.clock = nil
	if err := v.verifyConnection(cs); err != nil {
		t.Fatalf("today the leaf is valid and pinned: %v", err)
	}
}

func TestParsePin(t *testing.T) {
	der := []byte("certificate")
	want := PinOf(der)
	hex := want.String()
	var colon []string
	for i := 0; i < len(hex); i += 2 {
		colon = append(colon, strings.ToUpper(hex[i:i+2]))
	}
	for _, in := range []string{hex, strings.ToUpper(hex), strings.Join(colon, ":")} {
		got, err := ParsePin(in)
		if err != nil || got != want {
			t.Errorf("ParsePin(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	bad := []string{
		"", hex[:62], hex + "00", strings.Replace(hex, hex[:1], "g", 1),
		strings.Join(colon[:31], ":"), strings.Join(colon, ":") + ":",
		strings.Join(append(colon[:30:30], "A", "BCD"), ":"), strings.Join(colon, "-"),
	}
	for _, in := range bad {
		if _, err := ParsePin(in); err == nil {
			t.Errorf("ParsePin(%q) should fail", in)
		}
	}
}
