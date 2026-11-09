package endpointpool

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestPhaseValid(t *testing.T) {
	for _, p := range []Phase{PhaseDial, PhaseDNS, PhaseTLSHandshake, PhaseTLSVerify, PhasePin} {
		if !p.Valid() {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []Phase{"", "dialing", "TLS_VERIFY"} {
		if p.Valid() {
			t.Errorf("%q should not be valid", p)
		}
	}
}

func TestPhaseValues(t *testing.T) {
	want := map[Phase]string{
		PhaseDial: "dial", PhaseDNS: "dns", PhaseTLSHandshake: "tls_handshake",
		PhaseTLSVerify: "tls_verify", PhasePin: "pin",
	}
	for p, s := range want {
		if string(p) != s {
			t.Errorf("phase %q should spell %q", p, s)
		}
	}
}

func TestConnectPhaseError(t *testing.T) {
	cause := errors.New("connection refused")
	err := &ConnectPhaseError{Phase: PhaseDial, Endpoint: "pve1:8006", Err: cause}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is should reach the cause")
	}
	msg := err.Error()
	for _, part := range []string{"dial", "pve1:8006", "connection refused"} {
		if !strings.Contains(msg, part) {
			t.Errorf("message %q lacks %q", msg, part)
		}
	}
	if got := (&ConnectPhaseError{Phase: PhaseDNS}).Error(); got != "connect phase dns failed" {
		t.Errorf("bare message = %q", got)
	}
}

func TestConnectPhaseThroughWrapping(t *testing.T) {
	inner := &ConnectPhaseError{Phase: PhaseTLSHandshake, Endpoint: "pve2:8006", Err: errors.New("EOF")}
	wrapped := fmt.Errorf("get version: %w", fmt.Errorf("send: %w", inner))
	cpe, ok := AsConnectPhase(wrapped)
	if !ok || cpe != inner {
		t.Fatalf("AsConnectPhase = %v, %v", cpe, ok)
	}
	if !IsConnectPhase(wrapped) {
		t.Error("IsConnectPhase should see the tag through %w wrapping")
	}
	if IsConnectPhase(errors.New("plain")) || IsConnectPhase(nil) {
		t.Error("IsConnectPhase should be false without the tag")
	}
	if _, ok := AsConnectPhase(nil); ok {
		t.Error("AsConnectPhase(nil) should be false")
	}
}

func TestMaybeSentError(t *testing.T) {
	cause := errors.New("read tcp 10.0.0.1:1->10.0.0.2:8006: connection reset by peer")
	err := &MaybeSentError{Err: cause}
	if err.Error() != cause.Error() {
		t.Errorf("message should stay unchanged, got %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is should reach the cause")
	}
	wrapped := fmt.Errorf("upload: %w", err)
	if !IsMaybeSent(wrapped) {
		t.Error("IsMaybeSent should see the marker through %w wrapping")
	}
	if IsMaybeSent(cause) || IsMaybeSent(nil) {
		t.Error("IsMaybeSent should be false without the marker")
	}
	if got := (&MaybeSentError{}).Error(); got == "" {
		t.Error("empty marker should still describe itself")
	}
}

func TestMaybeSentAndConnectPhaseAreDistinct(t *testing.T) {
	if IsConnectPhase(&MaybeSentError{Err: errors.New("x")}) {
		t.Error("maybe-sent must not read as connect phase")
	}
	if IsMaybeSent(&ConnectPhaseError{Phase: PhaseDial, Err: errors.New("x")}) {
		t.Error("connect phase must not read as maybe-sent")
	}
}

func TestSentinelsThroughWrapping(t *testing.T) {
	for _, sentinel := range []error{ErrSendByPassed, ErrTetherConflict} {
		wrapped := fmt.Errorf("delete pool: %w", fmt.Errorf("sdk: %w", sentinel))
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("errors.Is lost %v through %%w", sentinel)
		}
	}
	if errors.Is(ErrSendByPassed, ErrTetherConflict) || errors.Is(ErrTetherConflict, ErrSendByPassed) {
		t.Error("the two sentinels must be distinct")
	}
}

func dialFailure(endpoint string) EndpointFailure {
	return EndpointFailure{
		Endpoint: endpoint,
		Err:      &ConnectPhaseError{Phase: PhaseDial, Endpoint: endpoint, Err: errors.New("refused")},
	}
}

// certVerifyError wraps cause the way a real handshake reports a refused
// certificate.
func certVerifyError(cause error) error {
	return &tls.CertificateVerificationError{Err: cause}
}

func verifyFailure(endpoint string, cause error) EndpointFailure {
	return EndpointFailure{
		Endpoint: endpoint,
		Err:      &ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: endpoint, Err: cause},
	}
}

func TestEntryUnreachableUnwrapsToDialError(t *testing.T) {
	headerTimeout := EndpointFailure{Endpoint: "pve3:8006", Err: errors.New("response header timeout")}
	verify := verifyFailure("pve2:8006", certVerifyError(x509.UnknownAuthorityError{}))
	orders := [][]EndpointFailure{
		{dialFailure("pve1:8006"), verify, headerTimeout},
		{headerTimeout, verify, dialFailure("pve1:8006")},
		{verify, headerTimeout, dialFailure("pve1:8006")},
	}
	for i, attempts := range orders {
		err := error(NewEntryUnreachableError(attempts))
		var op *net.OpError
		if !errors.As(err, &op) {
			t.Fatalf("order %d: no *net.OpError", i)
		}
		if op.Op != "dial" {
			t.Errorf("order %d: Op = %q, want dial", i, op.Op)
		}
		var netErr net.Error
		if !errors.As(err, &netErr) {
			t.Errorf("order %d: should read as a net.Error", i)
		}
		// The synthesized error joins the causes, so each stays reachable.
		var cpe *ConnectPhaseError
		if !errors.As(err, &cpe) {
			t.Errorf("order %d: per-endpoint cause lost", i)
		}
		for _, a := range attempts {
			if !errors.Is(err, a.Err) {
				t.Errorf("order %d: cause for %s not reachable", i, a.Endpoint)
			}
		}
	}
}

func TestEntryUnreachableAllVerifyUnwrapsToCertificateError(t *testing.T) {
	unknown := certVerifyError(x509.UnknownAuthorityError{})
	host := x509.HostnameError{Host: "pve2"}
	err := error(NewEntryUnreachableError([]EndpointFailure{
		verifyFailure("pve1:8006", unknown),
		verifyFailure("pve2:8006", certVerifyError(host)),
	}))
	var op *net.OpError
	if errors.As(err, &op) {
		t.Fatal("an all-verification walk must not read as a dial error")
	}
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) {
		t.Error("the unknown-authority error should be reachable")
	}
	var he x509.HostnameError
	if !errors.As(err, &he) {
		t.Error("the hostname error should be reachable")
	}
}

func TestEntryUnreachableMixedWithOneVerifyIsDial(t *testing.T) {
	err := error(NewEntryUnreachableError([]EndpointFailure{
		verifyFailure("pve1:8006", certVerifyError(x509.UnknownAuthorityError{})),
		dialFailure("pve2:8006"),
	}))
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		t.Fatalf("a walk with a non-verification cause should unwrap to a dial error, got %v", err)
	}
}

func TestEntryUnreachableEmptyAndNilCauses(t *testing.T) {
	empty := NewEntryUnreachableError(nil)
	if empty.Error() == "" {
		t.Error("empty error should describe itself")
	}
	var op *net.OpError
	if !errors.As(empty, &op) || op.Err == nil || op.Op != "dial" {
		t.Errorf("empty walk should unwrap to a dial error with a cause, got %#v", op)
	}
	nilCause := NewEntryUnreachableError([]EndpointFailure{{Endpoint: "pve1:8006"}})
	if !strings.Contains(nilCause.Error(), "pve1:8006") {
		t.Errorf("message %q should name the endpoint", nilCause.Error())
	}
	if !errors.As(nilCause, &op) || op.Err == nil {
		t.Error("a nil cause should still unwrap to a dial error with a cause")
	}
}

func TestEntryUnreachableMessageListsEveryEndpoint(t *testing.T) {
	err := NewEntryUnreachableError([]EndpointFailure{dialFailure("pve1:8006"), dialFailure("pve2:8006")})
	msg := err.Error()
	for _, ep := range []string{"pve1:8006", "pve2:8006"} {
		if !strings.Contains(msg, ep) {
			t.Errorf("message %q lacks %s", msg, ep)
		}
	}
	got := err.Endpoints()
	if len(got) != 2 || got[0] != "pve1:8006" || got[1] != "pve2:8006" {
		t.Errorf("Endpoints = %v", got)
	}
}

func TestEntryUnreachableCopiesAttempts(t *testing.T) {
	attempts := []EndpointFailure{dialFailure("pve1:8006")}
	err := NewEntryUnreachableError(attempts)
	attempts[0].Endpoint = "changed"
	if err.Attempts[0].Endpoint != "pve1:8006" {
		t.Error("constructor should copy the attempts")
	}
}

func TestEntryUnreachableThroughWrapping(t *testing.T) {
	inner := NewEntryUnreachableError([]EndpointFailure{dialFailure("pve1:8006")})
	wrapped := fmt.Errorf("list nodes: %w", inner)
	var got *EntryUnreachableError
	if !errors.As(wrapped, &got) || got != inner {
		t.Fatal("errors.As should find the EntryUnreachableError through %w wrapping")
	}
	var op *net.OpError
	if !errors.As(wrapped, &op) || op.Op != "dial" {
		t.Error("the dial classification should survive wrapping")
	}
}

// A pin failure counts as a verification failure. A walk in which every hop
// failed verification or pin unwraps to the certificate errors, and one that
// mixes in any other failure reads as a dial error.
func TestEntryUnreachablePinFailureCountsAsVerification(t *testing.T) {
	pinCause := errors.New("fingerprint mismatch")
	pin := EndpointFailure{
		Endpoint: "pve2:8006",
		Err:      &ConnectPhaseError{Phase: PhasePin, Endpoint: "pve2:8006", Err: pinCause},
	}
	verify := verifyFailure("pve1:8006", certVerifyError(x509.UnknownAuthorityError{}))
	handshake := EndpointFailure{
		Endpoint: "pve3:8006",
		Err:      &ConnectPhaseError{Phase: PhaseTLSHandshake, Endpoint: "pve3:8006", Err: errors.New("reset")},
	}
	headerTimeout := EndpointFailure{Endpoint: "pve4:8006", Err: errors.New("response header timeout")}

	for name, attempts := range map[string][]EndpointFailure{
		"pin only":       {pin},
		"verify and pin": {verify, pin},
		"pin and verify": {pin, verify},
	} {
		err := error(NewEntryUnreachableError(attempts))
		var op *net.OpError
		if errors.As(err, &op) {
			t.Errorf("%s: an all-verification walk must not read as a dial error", name)
		}
		if name == "pin only" && !errors.Is(err, pinCause) {
			t.Errorf("%s: pin cause not reachable", name)
		}
		for _, a := range attempts {
			if !errors.Is(err, a.Err) {
				t.Errorf("%s: cause for %s not reachable", name, a.Endpoint)
			}
		}
	}
	for name, attempts := range map[string][]EndpointFailure{
		"pin and dial":      {pin, dialFailure("pve5:8006")},
		"dial and pin":      {dialFailure("pve5:8006"), pin},
		"verify pin dial":   {verify, pin, dialFailure("pve5:8006")},
		"pin and handshake": {pin, handshake},
		"pin and header":    {pin, headerTimeout},
		"verify and header": {verify, headerTimeout},
	} {
		var op *net.OpError
		if !errors.As(NewEntryUnreachableError(attempts), &op) || op.Op != "dial" {
			t.Errorf("%s: should unwrap to a dial error", name)
		}
	}
}
