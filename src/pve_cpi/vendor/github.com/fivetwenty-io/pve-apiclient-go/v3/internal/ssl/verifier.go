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
	ErrNoCertificatesProvided = errors.New("no certificates provided")
	ErrCAParsingFailed        = errors.New("failed to parse CA certificate(s)")
	ErrCAPathMustBeAbsolute   = errors.New("CA certificate path must be absolute")
)

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
