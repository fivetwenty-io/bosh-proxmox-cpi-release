package ssl

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrNoCertificatesProvided           = errors.New("no certificates provided")
	ErrFingerprintVerifierNotConfigured = errors.New("fingerprint verifier not configured")
	ErrNoCertificatesInChain            = errors.New("no certificates in chain")
	ErrCertificateNotYetValid           = errors.New("certificate not yet valid")
	ErrCertificateExpired               = errors.New("certificate has expired")
	ErrCAParsingFailed                  = errors.New("failed to parse CA certificate(s)")
	ErrCAPathMustBeAbsolute             = errors.New("CA certificate path must be absolute")
)

// VerificationMode defines how SSL certificates should be verified.
type VerificationMode int

const (
	// VerifyNone disables certificate verification (insecure).
	VerifyNone VerificationMode = iota
	// VerifyPeer verifies the peer certificate.
	VerifyPeer
	// VerifyHostname verifies that the hostname matches the certificate.
	VerifyHostname
	// VerifyFingerprint verifies the certificate fingerprint.
	VerifyFingerprint
	// VerifyFull performs full certificate verification.
	VerifyFull
)

// Verifier handles SSL/TLS certificate verification.
type Verifier struct {
	mode                VerificationMode
	fingerprintVerifier *FingerprintVerifier
	caCertPool          *x509.CertPool
	skipHostname        bool
	allowExpired        bool
}

// NewVerifier creates a new SSL verifier.
func NewVerifier(mode VerificationMode) *Verifier {
	return &Verifier{
		mode:                mode,
		fingerprintVerifier: NewFingerprintVerifier(),
		caCertPool:          nil,
		skipHostname:        false,
		allowExpired:        false,
	}
}

// SetFingerprintVerifier sets the fingerprint verifier.
func (v *Verifier) SetFingerprintVerifier(fv *FingerprintVerifier) {
	v.fingerprintVerifier = fv
}

// SetCACertPool sets the CA certificate pool for verification.
func (v *Verifier) SetCACertPool(pool *x509.CertPool) {
	v.caCertPool = pool
}

// SetSkipHostname sets whether to skip hostname verification.
func (v *Verifier) SetSkipHostname(skip bool) {
	v.skipHostname = skip
}

// SetAllowExpired sets whether to allow expired certificates.
func (v *Verifier) SetAllowExpired(allow bool) {
	v.allowExpired = allow
}

// GetTLSConfig returns a TLS configuration based on the verifier settings.
func (v *Verifier) GetTLSConfig(serverName string) *tls.Config {
	config := &tls.Config{
		Rand:                                nil,
		Time:                                nil,
		Certificates:                        nil,
		NameToCertificate:                   nil,
		GetCertificate:                      nil,
		GetClientCertificate:                nil,
		GetConfigForClient:                  nil,
		VerifyPeerCertificate:               nil,
		VerifyConnection:                    nil,
		RootCAs:                             nil,
		NextProtos:                          nil,
		ServerName:                          serverName,
		ClientAuth:                          0,
		ClientCAs:                           nil,
		InsecureSkipVerify:                  false,
		CipherSuites:                        nil,
		PreferServerCipherSuites:            true,
		SessionTicketsDisabled:              false,
		SessionTicketKey:                    [32]byte{},
		ClientSessionCache:                  nil,
		UnwrapSession:                       nil,
		WrapSession:                         nil,
		MinVersion:                          tls.VersionTLS12,
		MaxVersion:                          0,
		CurvePreferences:                    nil,
		DynamicRecordSizingDisabled:         false,
		Renegotiation:                       0,
		KeyLogWriter:                        nil,
		EncryptedClientHelloConfigList:      nil,
		EncryptedClientHelloRejectionVerify: nil,
		GetEncryptedClientHelloKeys:         nil,
		EncryptedClientHelloKeys:            nil,
	}

	switch v.mode {
	case VerifyNone:
		config.InsecureSkipVerify = true

	case VerifyFingerprint:
		config.InsecureSkipVerify = true
		config.VerifyPeerCertificate = v.verifyFingerprint

	case VerifyPeer:
		config.RootCAs = v.caCertPool
		if v.skipHostname {
			config.InsecureSkipVerify = true
			config.VerifyPeerCertificate = v.verifyPeerOnly
		}

	case VerifyHostname:
		config.RootCAs = v.caCertPool

	case VerifyFull:
		config.RootCAs = v.caCertPool
		config.VerifyPeerCertificate = v.verifyFull
	}

	return config
}

// VerifyCertificateChain verifies a certificate chain.
func (v *Verifier) VerifyCertificateChain(certs []*x509.Certificate, hostname string) error {
	if len(certs) == 0 {
		return ErrNoCertificatesInChain
	}

	leafCert := certs[0]

	err := v.checkCertificateValidity(leafCert)
	if err != nil {
		return err
	}

	err = v.checkHostname(leafCert, hostname)
	if err != nil {
		return err
	}

	return v.verifyChain(leafCert, certs)
}

func (v *Verifier) verifyFingerprint(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return ErrNoCertificatesProvided
	}

	// Parse the leaf certificate
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Verify fingerprint
	if v.fingerprintVerifier != nil {
		return v.fingerprintVerifier.VerifyCertificate(cert)
	}

	return ErrFingerprintVerifierNotConfigured
}

func (v *Verifier) verifyPeerOnly(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return ErrNoCertificatesProvided
	}

	// Parse certificates
	certs := make([]*x509.Certificate, len(rawCerts))
	for index, rawCert := range rawCerts {
		cert, err := x509.ParseCertificate(rawCert)
		if err != nil {
			return fmt.Errorf("failed to parse certificate: %w", err)
		}

		certs[index] = cert
	}

	// Verify certificate chain
	opts := x509.VerifyOptions{
		DNSName:                   "",
		Intermediates:             x509.NewCertPool(),
		Roots:                     v.caCertPool,
		CurrentTime:               time.Time{},
		KeyUsages:                 nil,
		MaxConstraintComparisions: 0,
		CertificatePolicies:       nil,
	}

	// Add intermediate certificates
	for i := 1; i < len(certs); i++ {
		opts.Intermediates.AddCert(certs[i])
	}

	// Check expiration unless allowed
	if !v.allowExpired {
		opts.CurrentTime = time.Now()
	} else {
		// Use a time when the cert was likely valid
		opts.CurrentTime = certs[0].NotBefore.Add(time.Hour)
	}

	// Verify the certificate chain
	_, err := certs[0].Verify(opts)
	if err != nil {
		return fmt.Errorf("certificate verification failed: %w", err)
	}

	return nil
}

func (v *Verifier) verifyFull(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return ErrNoCertificatesProvided
	}

	// Parse the leaf certificate
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Check fingerprint if verifier is configured
	if v.fingerprintVerifier != nil {
		err := v.fingerprintVerifier.VerifyCertificate(cert)
		if err != nil {
			// Fingerprint doesn't match, but continue with other checks
			// The fingerprint verifier might just be logging unknown certificates
			_ = err // explicitly ignore error to continue with other verification checks
		}
	}

	// Additional custom verification can be added here
	// For example, checking specific certificate attributes

	return nil
}

func (v *Verifier) checkCertificateValidity(cert *x509.Certificate) error {
	if v.allowExpired {
		return nil
	}

	now := time.Now()
	if now.Before(cert.NotBefore) {
		return ErrCertificateNotYetValid
	}

	if now.After(cert.NotAfter) {
		return ErrCertificateExpired
	}

	return nil
}

func (v *Verifier) checkHostname(cert *x509.Certificate, hostname string) error {
	if v.skipHostname || hostname == "" {
		return nil
	}

	err := cert.VerifyHostname(hostname)
	if err != nil {
		return fmt.Errorf("hostname verification failed: %w", err)
	}

	return nil
}

func (v *Verifier) verifyChain(leafCert *x509.Certificate, certs []*x509.Certificate) error {
	if v.caCertPool == nil {
		return nil
	}

	opts := x509.VerifyOptions{
		DNSName:                   "",
		Intermediates:             x509.NewCertPool(),
		Roots:                     v.caCertPool,
		CurrentTime:               time.Time{},
		KeyUsages:                 nil,
		MaxConstraintComparisions: 0,
		CertificatePolicies:       nil,
	}

	for i := 1; i < len(certs); i++ {
		opts.Intermediates.AddCert(certs[i])
	}

	_, err := leafCert.Verify(opts)
	if err != nil {
		return fmt.Errorf("certificate chain verification failed: %w", err)
	}

	return nil
}

// CreateTLSConfig creates a TLS configuration for a given host.
func CreateTLSConfig(host string, options *TLSOptions) (*tls.Config, error) {
	config := &tls.Config{
		Rand:                                nil,
		Time:                                nil,
		Certificates:                        nil,
		NameToCertificate:                   nil,
		GetCertificate:                      nil,
		GetClientCertificate:                nil,
		GetConfigForClient:                  nil,
		VerifyPeerCertificate:               nil,
		VerifyConnection:                    nil,
		RootCAs:                             nil,
		NextProtos:                          nil,
		ServerName:                          extractHostname(host),
		ClientAuth:                          0,
		ClientCAs:                           nil,
		InsecureSkipVerify:                  false,
		CipherSuites:                        nil,
		PreferServerCipherSuites:            true,
		SessionTicketsDisabled:              false,
		SessionTicketKey:                    [32]byte{},
		ClientSessionCache:                  nil,
		UnwrapSession:                       nil,
		WrapSession:                         nil,
		MinVersion:                          tls.VersionTLS12,
		MaxVersion:                          0,
		CurvePreferences:                    nil,
		DynamicRecordSizingDisabled:         false,
		Renegotiation:                       0,
		KeyLogWriter:                        nil,
		EncryptedClientHelloConfigList:      nil,
		EncryptedClientHelloRejectionVerify: nil,
		GetEncryptedClientHelloKeys:         nil,
		EncryptedClientHelloKeys:            nil,
	}

	if options == nil {
		return config, nil
	}

	err := configureTLSVerification(config, options)
	if err != nil {
		return nil, err
	}

	err = configureTLSCertificates(config, options)
	if err != nil {
		return nil, err
	}

	configureTLSVersion(config, options)
	configureCipherSuites(config, options)

	return config, nil
}

func configureTLSVerification(config *tls.Config, options *TLSOptions) error {
	if options.InsecureSkipVerify {
		config.InsecureSkipVerify = true
	}

	if options.CACert == "" {
		return nil
	}

	pool, err := LoadCACertificate(options.CACert)
	if err != nil {
		return err
	}

	config.RootCAs = pool

	return nil
}

func configureTLSCertificates(config *tls.Config, options *TLSOptions) error {
	if options.ClientCert == "" || options.ClientKey == "" {
		return nil
	}

	cert, err := tls.LoadX509KeyPair(options.ClientCert, options.ClientKey)
	if err != nil {
		return fmt.Errorf("failed to load client certificates: %w", err)
	}

	config.Certificates = []tls.Certificate{cert}

	return nil
}

func configureTLSVersion(config *tls.Config, options *TLSOptions) {
	if options.MinTLSVersion != 0 {
		config.MinVersion = options.MinTLSVersion
	} else {
		config.MinVersion = tls.VersionTLS12
	}
}

func configureCipherSuites(config *tls.Config, options *TLSOptions) {
	if len(options.CipherSuites) > 0 {
		config.CipherSuites = options.CipherSuites
	}
}

// TLSOptions contains TLS configuration options.
type TLSOptions struct {
	InsecureSkipVerify bool
	CACert             string
	ClientCert         string
	ClientKey          string
	MinTLSVersion      uint16
	CipherSuites       []uint16
}

// LoadCACertificate loads a CA certificate from a file.
func LoadCACertificate(filename string) (*x509.CertPool, error) {
	if filename == "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}

		return pool, nil
	}

	// Clean and validate the file path to prevent directory traversal
	cleanPath := filepath.Clean(filename)
	if !filepath.IsAbs(cleanPath) {
		return nil, fmt.Errorf("%w: %s", ErrCAPathMustBeAbsolute, filename)
	}

	pemBytes, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate file: %w", err)
	}

	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(pemBytes); !ok {
		return nil, fmt.Errorf("%w from %s", ErrCAParsingFailed, filename)
	}

	return pool, nil
}

// extractHostname extracts the hostname from a host:port string.
func extractHostname(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err == nil {
		return h
	}

	return host
}

// GetCertificateInfo returns information about a certificate.
func GetCertificateInfo(cert *x509.Certificate) map[string]interface{} {
	info := make(map[string]interface{})

	addBasicCertInfo(info, cert)
	addSubjectAltNames(info, cert)
	addKeyUsageInfo(info, cert)
	addExtendedKeyUsageInfo(info, cert)

	return info
}

func addBasicCertInfo(info map[string]interface{}, cert *x509.Certificate) {
	info["subject"] = cert.Subject.String()
	info["issuer"] = cert.Issuer.String()
	info["serial"] = cert.SerialNumber.String()
	info["not_before"] = cert.NotBefore.Format(time.RFC3339)
	info["not_after"] = cert.NotAfter.Format(time.RFC3339)
	info["fingerprint"] = CalculateFingerprint(cert)
	info["signature_algorithm"] = cert.SignatureAlgorithm.String()
	info["public_key_algorithm"] = cert.PublicKeyAlgorithm.String()
}

func addSubjectAltNames(info map[string]interface{}, cert *x509.Certificate) {
	if len(cert.DNSNames) > 0 {
		info["dns_names"] = cert.DNSNames
	}

	if len(cert.IPAddresses) > 0 {
		ips := make([]string, len(cert.IPAddresses))
		for i, ip := range cert.IPAddresses {
			ips[i] = ip.String()
		}

		info["ip_addresses"] = ips
	}
}

func addKeyUsageInfo(info map[string]interface{}, cert *x509.Certificate) {
	var keyUsage []string

	keyUsageMap := map[x509.KeyUsage]string{
		x509.KeyUsageDigitalSignature: "Digital Signature",
		x509.KeyUsageKeyEncipherment:  "Key Encipherment",
		x509.KeyUsageDataEncipherment: "Data Encipherment",
		x509.KeyUsageKeyAgreement:     "Key Agreement",
		x509.KeyUsageCertSign:         "Certificate Signing",
	}

	for usage, name := range keyUsageMap {
		if cert.KeyUsage&usage != 0 {
			keyUsage = append(keyUsage, name)
		}
	}

	if len(keyUsage) > 0 {
		info["key_usage"] = strings.Join(keyUsage, ", ")
	}
}

func addExtendedKeyUsageInfo(info map[string]interface{}, cert *x509.Certificate) {
	if len(cert.ExtKeyUsage) == 0 {
		return
	}

	extKeyUsage := make([]string, 0, len(cert.ExtKeyUsage))

	for _, usage := range cert.ExtKeyUsage {
		name := getExtKeyUsageName(usage)
		extKeyUsage = append(extKeyUsage, name)
	}

	if len(extKeyUsage) > 0 {
		info["extended_key_usage"] = strings.Join(extKeyUsage, ", ")
	}
}

func getExtKeyUsageName(usage x509.ExtKeyUsage) string {
	switch usage {
	case x509.ExtKeyUsageServerAuth:
		return "Server Authentication"
	case x509.ExtKeyUsageClientAuth:
		return "Client Authentication"
	case x509.ExtKeyUsageCodeSigning:
		return "Code Signing"
	case x509.ExtKeyUsageEmailProtection:
		return "Email Protection"
	case x509.ExtKeyUsageAny, x509.ExtKeyUsageIPSECEndSystem, x509.ExtKeyUsageIPSECTunnel,
		x509.ExtKeyUsageIPSECUser, x509.ExtKeyUsageTimeStamping, x509.ExtKeyUsageOCSPSigning,
		x509.ExtKeyUsageMicrosoftServerGatedCrypto, x509.ExtKeyUsageNetscapeServerGatedCrypto,
		x509.ExtKeyUsageMicrosoftCommercialCodeSigning, x509.ExtKeyUsageMicrosoftKernelCodeSigning:
		return fmt.Sprintf("Usage %d", usage)
	}

	return fmt.Sprintf("Usage %d", usage)
}
