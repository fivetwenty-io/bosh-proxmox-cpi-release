package endpointpool

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrPinMismatch is the cause a discovered endpoint's verify callback gives
// when it refuses the certificate the endpoint presented. The handshake error
// is a *tls.CertificateVerificationError whose Err wraps it, so the SDK still
// reports a certificate failure, and the pool's dialer tags the failure with
// PhasePin.
var ErrPinMismatch = errors.New("certificate is not one the endpoint's node is pinned to")

// errNoPeerCertificate is the cause when a handshake reaches the verify
// callback without a server certificate.
var errNoPeerCertificate = errors.New("the server presented no certificate")

// Pin is the SHA-256 digest of a leaf certificate's DER encoding.
type Pin [sha256.Size]byte

// PinOf returns the pin of a DER-encoded certificate.
func PinOf(der []byte) Pin { return sha256.Sum256(der) }

// ParsePin parses a SHA-256 fingerprint written as 64 hexadecimal digits,
// either bare or as 32 colon-separated pairs, which is the form Proxmox reports
// in ssl_fingerprint. Case is ignored. Any other shape is an error.
func ParsePin(s string) (Pin, error) {
	var pin Pin
	digits := s
	if strings.Contains(s, ":") {
		pairs := strings.Split(s, ":")
		if len(pairs) != sha256.Size {
			return pin, fmt.Errorf("pin %q has %d colon-separated groups, want %d", s, len(pairs), sha256.Size)
		}
		for _, p := range pairs {
			if len(p) != 2 {
				return pin, fmt.Errorf("pin %q has a group %q that is not two hex digits", s, p)
			}
		}
		digits = strings.Join(pairs, "")
	}
	if len(digits) != 2*sha256.Size {
		return pin, fmt.Errorf("pin %q has %d hex digits, want %d", s, len(digits), 2*sha256.Size)
	}
	if _, err := hex.Decode(pin[:], []byte(digits)); err != nil {
		return Pin{}, fmt.Errorf("pin %q is not hexadecimal: %w", s, err)
	}
	return pin, nil
}

// String returns the pin as 64 lowercase hexadecimal digits.
func (p Pin) String() string { return hex.EncodeToString(p[:]) }

// verifier is the VerifyConnection callback of a discovered endpoint or a
// cold-start candidate. Its clones set InsecureSkipVerify, so Go's built-in
// verification does not run, and Go calls the callback on every handshake,
// resumptions included, before any application data. No request byte, and so
// no token or ticket, reaches the address until the callback accepts it.
type verifier struct {
	// host is the name or address the certificate must cover when the
	// hostname check applies, which is the endpoint's own host.
	host string
	// verifySSL mirrors the operator's verify_ssl setting.
	verifySSL bool
	// roots is the operator's CA pool, or nil for the system pool.
	roots *x509.CertPool
	// clock is the TLS config's Time, or nil for time.Now.
	clock func() time.Time
	// pins is the node's pin set. It is empty for a candidate.
	pins map[Pin]struct{}
	// candidate marks a cold-start candidate, which has no pin set and whose
	// refusals are verification failures rather than pin failures.
	candidate bool
	// refreshDue is called when a discovered endpoint is accepted on the
	// hostname check with a leaf outside its pin set. It may be nil.
	refreshDue func()
}

// newDiscoveredVerifier returns the callback for a discovered endpoint. The
// pin set must not be empty, because a discovered node without a valid
// fingerprint never enters the pool.
func newDiscoveredVerifier(host string, verifySSL bool, roots *x509.CertPool, clock func() time.Time,
	pins []Pin, refreshDue func(),
) (*verifier, error) {
	if len(pins) == 0 {
		return nil, fmt.Errorf("discovered endpoint %s has no pins", host)
	}
	set := make(map[Pin]struct{}, len(pins))
	for _, p := range pins {
		set[p] = struct{}{}
	}
	return &verifier{
		host:       host,
		verifySSL:  verifySSL,
		roots:      roots,
		clock:      clock,
		pins:       set,
		refreshDue: refreshDue,
	}, nil
}

// newCandidateVerifier returns the callback for a cold-start candidate, which
// has an empty pin set, so only a verified chain followed by a hostname match
// accepts it. A candidate exists only under verify_ssl true, so the function
// refuses to build one when verification is off.
func newCandidateVerifier(host string, verifySSL bool, roots *x509.CertPool, clock func() time.Time,
) (*verifier, error) {
	if !verifySSL {
		return nil, fmt.Errorf("cold-start candidate %s needs verify_ssl true", host)
	}
	return &verifier{host: host, verifySSL: true, roots: roots, clock: clock, candidate: true}, nil
}

// verifyConnection decides whether to accept the connection.
//
//  1. With verify_ssl true, the leaf's chain must verify against the
//     operator's CA pool, or the system pool when there is none, using the
//     intermediates the peer sent. A chain failure refuses the connection.
//  2. With verify_ssl true and a verified chain, the connection is accepted
//     when the leaf is in the pin set, or when it passes the hostname check
//     against the endpoint's host. A hostname pass on a leaf outside the pin
//     set of a discovered endpoint makes a refresh due.
//  3. With verify_ssl false, the connection is accepted only when the leaf is
//     in the pin set.
func (v *verifier) verifyConnection(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return v.refuse(cs, errNoPeerCertificate)
	}
	leaf := cs.PeerCertificates[0]
	_, pinned := v.pins[PinOf(leaf.Raw)]
	if !v.verifySSL {
		if pinned {
			return nil
		}
		return v.refuse(cs, fmt.Errorf("certificate sha256 %s is not one of the %d pinned for %s",
			PinOf(leaf.Raw), len(v.pins), v.host))
	}
	if err := v.verifyChain(cs); err != nil {
		return v.refuse(cs, err)
	}
	if pinned {
		return nil
	}
	if err := leaf.VerifyHostname(v.host); err != nil {
		return v.refuse(cs, err)
	}
	if !v.candidate && v.refreshDue != nil {
		v.refreshDue()
	}
	return nil
}

// verifyChain verifies the leaf's chain for server authentication without a
// name, because the name is checked separately and only when no pin matched.
func (v *verifier) verifyChain(cs tls.ConnectionState) error {
	opts := x509.VerifyOptions{
		Roots:         v.roots,
		Intermediates: x509.NewCertPool(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if v.clock != nil {
		opts.CurrentTime = v.clock()
	}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(opts)
	return err
}

// refuse builds the handshake error. A candidate's refusal is a verification
// failure. A discovered endpoint's refusal also wraps ErrPinMismatch, so the
// dialer tags it PhasePin and the endpoint can be left out for the rest of the
// process.
func (v *verifier) refuse(cs tls.ConnectionState, cause error) error {
	if !v.candidate {
		cause = fmt.Errorf("%w: %w", ErrPinMismatch, cause)
	}
	return &tls.CertificateVerificationError{UnverifiedCertificates: cs.PeerCertificates, Err: cause}
}
