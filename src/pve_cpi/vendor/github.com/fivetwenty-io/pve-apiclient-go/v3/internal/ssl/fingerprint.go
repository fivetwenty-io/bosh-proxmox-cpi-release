package ssl

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
)

var (
	ErrCertificateNil                   = errors.New("certificate is nil")
	ErrCertificateFingerprintNotTrusted = errors.New("certificate fingerprint is not trusted")
	ErrCertificateVerificationFailed    = errors.New("certificate verification failed")
	ErrUnknownCertificateFingerprint    = errors.New("unknown certificate fingerprint (manual verification required)")
	ErrCannotVerifyFingerprint          = errors.New("cannot verify certificate fingerprint")
	ErrInvalidFingerprintLength         = errors.New("invalid fingerprint length")
)

// ManualVerificationRequest carries the details a manual verification
// callback needs to render an accept/reject decision for an unknown
// certificate (e.g. an interactive prompt or an operator-facing UI).
type ManualVerificationRequest struct {
	// Fingerprint is the normalized (colon-separated, uppercase) SHA256
	// fingerprint of the presented certificate.
	Fingerprint string
	// Certificate is the presented certificate; callers may inspect
	// Certificate.Subject, Issuer, NotBefore/NotAfter, etc.
	Certificate *x509.Certificate
	// Host is the server host the certificate was presented for, or ""
	// if the caller did not supply host context (see VerifyCertificate
	// vs. VerifyCertificateForHost).
	Host string
}

// FingerprintVerifier handles certificate fingerprint verification.
type FingerprintVerifier struct {
	mu                   sync.RWMutex
	cache                map[string]bool
	lastUnknown          string
	manualVerification   bool
	registerCallback     func(string)
	verifyCallback       func(*x509.Certificate) bool
	manualVerifyCallback func(ManualVerificationRequest) bool
}

// NewFingerprintVerifier creates a new fingerprint verifier.
func NewFingerprintVerifier() *FingerprintVerifier {
	return &FingerprintVerifier{
		mu:                   sync.RWMutex{},
		cache:                make(map[string]bool),
		lastUnknown:          "",
		manualVerification:   false,
		registerCallback:     nil,
		verifyCallback:       nil,
		manualVerifyCallback: nil,
	}
}

// SetManualVerification enables or disables manual verification mode.
//
// When enabled and no generic verify callback (SetVerifyCallback) is
// configured, an unknown certificate is resolved via the manual verify
// callback (SetManualVerifyCallback) if one is set: the callback is
// invoked with the certificate's fingerprint, the certificate itself,
// and (if known) the host, and its accept/reject decision is honored.
// Accepted fingerprints are cached as trusted for the lifetime of this
// verifier. If manual verification is enabled but no manual verify
// callback is set, unknown certificates are rejected with
// ErrUnknownCertificateFingerprint and recorded as the last unknown
// fingerprint (see GetLastUnknownFingerprint), so a caller can retrieve
// it and decide out-of-band (e.g. prompt separately, then call
// AddTrustedFingerprint and retry).
func (fv *FingerprintVerifier) SetManualVerification(enabled bool) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	fv.manualVerification = enabled
}

// SetRegisterCallback sets the callback for registering new fingerprints.
func (fv *FingerprintVerifier) SetRegisterCallback(callback func(string)) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	fv.registerCallback = callback
}

// SetVerifyCallback sets the callback for verifying certificates. When
// set, it takes priority over manual verification: it is consulted for
// every unknown certificate regardless of SetManualVerification.
func (fv *FingerprintVerifier) SetVerifyCallback(callback func(*x509.Certificate) bool) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	fv.verifyCallback = callback
}

// SetManualVerifyCallback sets the callback consulted for unknown
// certificates when manual verification mode is enabled (see
// SetManualVerification) and no generic verify callback is configured.
// The callback receives the fingerprint, certificate, and host (host is
// "" unless the caller used VerifyCertificateForHost) and returns true
// to trust the certificate for the remainder of this verifier's
// lifetime, or false to reject it.
func (fv *FingerprintVerifier) SetManualVerifyCallback(callback func(ManualVerificationRequest) bool) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	fv.manualVerifyCallback = callback
}

// VerifyCertificate verifies a certificate against known fingerprints.
// It is equivalent to VerifyCertificateForHost(cert, "").
func (fv *FingerprintVerifier) VerifyCertificate(cert *x509.Certificate) error {
	return fv.VerifyCertificateForHost(cert, "")
}

// VerifyCertificateForHost verifies a certificate against known
// fingerprints, threading host through to any manual verify callback so
// it can be surfaced to the user (e.g. "unknown certificate for
// pve.example.com"). host may be "" if unknown.
func (fv *FingerprintVerifier) VerifyCertificateForHost(cert *x509.Certificate, host string) error {
	if cert == nil {
		return ErrCertificateNil
	}

	// Calculate SHA256 fingerprint
	fingerprint := CalculateFingerprint(cert)

	fv.mu.Lock()
	defer fv.mu.Unlock()

	// Check if fingerprint is in cache
	if trusted, exists := fv.cache[fingerprint]; exists {
		return fv.verifyCachedLocked(fingerprint, trusted)
	}

	// A generic verify callback takes priority over manual mode.
	if fv.verifyCallback != nil {
		return fv.verifyWithCallbackLocked(cert, fingerprint)
	}

	if fv.manualVerification {
		return fv.verifyManualLocked(cert, fingerprint, host)
	}

	// No verification method available, reject
	fv.lastUnknown = fingerprint

	return fmt.Errorf("%w: %s", ErrCannotVerifyFingerprint, fingerprint)
}

// AddTrustedFingerprint adds a fingerprint to the trusted cache.
func (fv *FingerprintVerifier) AddTrustedFingerprint(fingerprint string) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	// Normalize fingerprint
	normalized := NormalizeFingerprint(fingerprint)
	fv.cache[normalized] = true
}

// AddTrustedFingerprints adds multiple fingerprints to the trusted cache.
func (fv *FingerprintVerifier) AddTrustedFingerprints(fingerprints []string) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	for _, fp := range fingerprints {
		normalized := NormalizeFingerprint(fp)
		fv.cache[normalized] = true
	}
}

// RemoveTrustedFingerprint removes a fingerprint from the trusted cache.
func (fv *FingerprintVerifier) RemoveTrustedFingerprint(fingerprint string) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	normalized := NormalizeFingerprint(fingerprint)
	delete(fv.cache, normalized)
}

// GetLastUnknownFingerprint returns the most recent fingerprint that was
// not already trusted, i.e. one that was rejected outright, rejected by
// a verify callback, or rejected/unresolved by manual verification. It
// is safe to call concurrently with VerifyCertificate. It returns "" if
// every certificate seen so far was already trusted.
func (fv *FingerprintVerifier) GetLastUnknownFingerprint() string {
	fv.mu.RLock()
	defer fv.mu.RUnlock()

	return fv.lastUnknown
}

// GetTrustedFingerprints returns all trusted fingerprints.
func (fv *FingerprintVerifier) GetTrustedFingerprints() []string {
	fv.mu.RLock()
	defer fv.mu.RUnlock()

	fingerprints := make([]string, 0, len(fv.cache))
	for fp, trusted := range fv.cache {
		if trusted {
			fingerprints = append(fingerprints, fp)
		}
	}

	return fingerprints
}

// ClearCache clears the fingerprint cache.
func (fv *FingerprintVerifier) ClearCache() {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	fv.cache = make(map[string]bool)
	fv.lastUnknown = ""
}

// verifyCachedLocked resolves a fingerprint that already has a cached
// trust decision. Caller must hold fv.mu.
func (fv *FingerprintVerifier) verifyCachedLocked(fingerprint string, trusted bool) error {
	if trusted {
		return nil
	}

	fv.lastUnknown = fingerprint

	return fmt.Errorf("%w: %s", ErrCertificateFingerprintNotTrusted, fingerprint)
}

// verifyWithCallbackLocked resolves an unknown fingerprint using the
// generic verify callback. Caller must hold fv.mu.
func (fv *FingerprintVerifier) verifyWithCallbackLocked(cert *x509.Certificate, fingerprint string) error {
	if fv.verifyCallback(cert) {
		fv.trustLocked(fingerprint)

		return nil
	}

	fv.cache[fingerprint] = false
	fv.lastUnknown = fingerprint

	return fmt.Errorf("%w for fingerprint %s", ErrCertificateVerificationFailed, fingerprint)
}

// verifyManualLocked resolves an unknown fingerprint under manual
// verification mode. If no manual verify callback is configured, the
// fingerprint is rejected and recorded so GetLastUnknownFingerprint
// gives the caller a reliable way to retrieve it and decide
// out-of-band. Caller must hold fv.mu.
func (fv *FingerprintVerifier) verifyManualLocked(cert *x509.Certificate, fingerprint, host string) error {
	if fv.manualVerifyCallback == nil {
		fv.lastUnknown = fingerprint

		return fmt.Errorf("%w: %s", ErrUnknownCertificateFingerprint, fingerprint)
	}

	request := ManualVerificationRequest{
		Fingerprint: fingerprint,
		Certificate: cert,
		Host:        host,
	}

	if fv.manualVerifyCallback(request) {
		fv.trustLocked(fingerprint)

		return nil
	}

	fv.cache[fingerprint] = false
	fv.lastUnknown = fingerprint

	return fmt.Errorf("%w: %s", ErrUnknownCertificateFingerprint, fingerprint)
}

// trustLocked marks fingerprint as trusted and fires the register
// callback, if configured. Caller must hold fv.mu.
func (fv *FingerprintVerifier) trustLocked(fingerprint string) {
	fv.cache[fingerprint] = true

	if fv.registerCallback != nil {
		fv.registerCallback(fingerprint)
	}
}

// CalculateFingerprint calculates the SHA256 fingerprint of a certificate.
func CalculateFingerprint(cert *x509.Certificate) string {
	hash := sha256.Sum256(cert.Raw)

	return FormatFingerprint(hash[:])
}

// FormatFingerprint formats a fingerprint byte array as a colon-separated hex string.
func FormatFingerprint(fingerprint []byte) string {
	hex := hex.EncodeToString(fingerprint)

	// Insert colons every 2 characters
	var parts []string
	for i := 0; i < len(hex); i += 2 {
		parts = append(parts, hex[i:i+2])
	}

	return strings.ToUpper(strings.Join(parts, ":"))
}

// NormalizeFingerprint normalizes a fingerprint string for comparison.
func NormalizeFingerprint(fingerprint string) string {
	// Remove all non-hex characters and convert to uppercase
	normalized := strings.ToUpper(fingerprint)
	normalized = strings.ReplaceAll(normalized, ":", "")
	normalized = strings.ReplaceAll(normalized, " ", "")
	normalized = strings.ReplaceAll(normalized, "-", "")

	// Re-add colons in standard format
	var parts []string

	for i := 0; i < len(normalized); i += 2 {
		if i+2 <= len(normalized) {
			parts = append(parts, normalized[i:i+2])
		}
	}

	return strings.Join(parts, ":")
}

// ParseFingerprint parses a fingerprint string and returns the byte array.
func ParseFingerprint(fingerprint string) ([]byte, error) {
	// Normalize the fingerprint
	normalized := NormalizeFingerprint(fingerprint)

	// Remove colons for hex decoding
	hexStr := strings.ReplaceAll(normalized, ":", "")

	// Decode hex string
	bytes, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("invalid fingerprint format: %w", err)
	}

	// SHA256 fingerprints should be 32 bytes
	if len(bytes) != constants.SHA256ByteLength {
		return nil, fmt.Errorf("%w: expected 32 bytes, got %d", ErrInvalidFingerprintLength, len(bytes))
	}

	return bytes, nil
}

// CompareFingerprints compares two fingerprint strings for equality after
// normalization.
func CompareFingerprints(fp1, fp2 string) bool {
	return NormalizeFingerprint(fp1) == NormalizeFingerprint(fp2)
}
