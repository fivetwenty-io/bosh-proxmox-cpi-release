package pvetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// CA is a throwaway certificate authority that signs leaf certificates for the
// test servers. Two CAs never trust each other's leaves.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	pool *x509.CertPool
}

// LeafOptions describes one leaf certificate. When both DNSNames and IPs are
// empty the leaf covers localhost, 127.0.0.1, and ::1, so a client that dials a
// loopback server verifies the name. A leaf with other names only fails that
// check, which is how the SAN mismatch cases are built.
type LeafOptions struct {
	CommonName string
	DNSNames   []string
	IPs        []net.IP
	NotBefore  time.Time
	NotAfter   time.Time
}

// Leaf is a certificate and key signed by a CA.
type Leaf struct {
	Cert tls.Certificate
	X509 *x509.Certificate
	DER  []byte
	PEM  []byte
}

// NewCA creates a certificate authority with a fresh P-256 key.
func NewCA(tb testing.TB) *CA {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("pvetest: generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(tb),
		Subject:               pkix.Name{CommonName: "pvetest CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatalf("pvetest: create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatalf("pvetest: parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pool: pool,
	}
}

// CertPEM returns the CA certificate in PEM form, as an operator would supply
// it in pve.ca_cert.
func (c *CA) CertPEM() []byte { return append([]byte(nil), c.pem...) }

// Pool returns a pool that trusts only this CA.
func (c *CA) Pool() *x509.CertPool { return c.pool }

// ClientConfig returns a TLS client configuration that trusts this CA and
// verifies the server name serverName.
func (c *CA) ClientConfig(serverName string) *tls.Config {
	return &tls.Config{RootCAs: c.pool, ServerName: serverName, MinVersion: tls.VersionTLS12}
}

// NewLeaf signs a leaf certificate with a fresh P-256 key.
func (c *CA) NewLeaf(tb testing.TB, opts LeafOptions) *Leaf {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("pvetest: generate leaf key: %v", err)
	}
	if len(opts.DNSNames) == 0 && len(opts.IPs) == 0 {
		opts.DNSNames = []string{"localhost"}
		opts.IPs = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	}
	if opts.CommonName == "" {
		opts.CommonName = "pvetest leaf"
	}
	if opts.NotBefore.IsZero() {
		opts.NotBefore = time.Now().Add(-time.Hour)
	}
	if opts.NotAfter.IsZero() {
		opts.NotAfter = time.Now().Add(24 * time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(tb),
		Subject:      pkix.Name{CommonName: opts.CommonName},
		NotBefore:    opts.NotBefore,
		NotAfter:     opts.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     opts.DNSNames,
		IPAddresses:  opts.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		tb.Fatalf("pvetest: create leaf certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatalf("pvetest: parse leaf certificate: %v", err)
	}
	return &Leaf{
		Cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed},
		X509: parsed,
		DER:  der,
		PEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// Fingerprint returns the SHA-256 digest of the leaf's DER encoding as 64
// lowercase hexadecimal characters.
func (l *Leaf) Fingerprint() string {
	sum := sha256.Sum256(l.DER)
	return hex.EncodeToString(sum[:])
}

// FingerprintColon returns the same digest in the uppercase, colon-separated
// form that Proxmox displays.
func (l *Leaf) FingerprintColon() string {
	h := strings.ToUpper(l.Fingerprint())
	parts := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func serial(tb testing.TB) *big.Int {
	tb.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		tb.Fatalf("pvetest: serial number: %v", err)
	}
	return n
}
