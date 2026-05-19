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

// FingerprintVerifier handles certificate fingerprint verification.
type FingerprintVerifier struct {
	mu                 sync.RWMutex
	cache              map[string]bool
	lastUnknown        string
	manualVerification bool
	registerCallback   func(string)
	verifyCallback     func(*x509.Certificate) bool
}

// NewFingerprintVerifier creates a new fingerprint verifier.
func NewFingerprintVerifier() *FingerprintVerifier {
	return &FingerprintVerifier{
		mu:                 sync.RWMutex{},
		cache:              make(map[string]bool),
		lastUnknown:        "",
		manualVerification: false,
		registerCallback:   nil,
		verifyCallback:     nil,
	}
}

// SetManualVerification enables or disables manual verification mode.
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

// SetVerifyCallback sets the callback for verifying certificates.
func (fv *FingerprintVerifier) SetVerifyCallback(callback func(*x509.Certificate) bool) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	fv.verifyCallback = callback
}

// VerifyCertificate verifies a certificate against known fingerprints.
func (fv *FingerprintVerifier) VerifyCertificate(cert *x509.Certificate) error {
	if cert == nil {
		return ErrCertificateNil
	}

	// Calculate SHA256 fingerprint
	fingerprint := CalculateFingerprint(cert)

	fv.mu.Lock()
	defer fv.mu.Unlock()

	// Check if fingerprint is in cache
	if trusted, exists := fv.cache[fingerprint]; exists {
		if trusted {
			return nil
		}

		return fmt.Errorf("%w: %s", ErrCertificateFingerprintNotTrusted, fingerprint)
	}

	// Store as last unknown
	fv.lastUnknown = fingerprint

	// If we have a verify callback, use it
	if fv.verifyCallback != nil {
		if fv.verifyCallback(cert) {
			fv.cache[fingerprint] = true
			if fv.registerCallback != nil {
				fv.registerCallback(fingerprint)
			}

			return nil
		}

		fv.cache[fingerprint] = false

		return fmt.Errorf("%w for fingerprint %s", ErrCertificateVerificationFailed, fingerprint)
	}

	// If manual verification is enabled, prompt user
	if fv.manualVerification {
		// In a real implementation, this would interact with the user
		// For now, we'll reject unknown certificates
		return fmt.Errorf("%w: %s", ErrUnknownCertificateFingerprint, fingerprint)
	}

	// No verification method available, reject
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

// GetLastUnknownFingerprint returns the last unknown fingerprint encountered.
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

// CompareFingerprintscleaning compares two fingerprint strings for equality.
func CompareFingerprints(fp1, fp2 string) bool {
	return NormalizeFingerprint(fp1) == NormalizeFingerprint(fp2)
}
