package pvetest_test

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

func verifyLocalhost(t *testing.T, ca *pvetest.CA, leaf *pvetest.Leaf) error {
	t.Helper()
	opts := x509.VerifyOptions{Roots: ca.Pool(), DNSName: "localhost"}
	_, err := leaf.X509.Verify(opts)
	return err
}

func TestCA_LeafVerifiesAgainstItsCAOnly(t *testing.T) {
	t.Parallel()
	ca, other := pvetest.NewCA(t), pvetest.NewCA(t)
	leaf := ca.NewLeaf(t, pvetest.LeafOptions{})

	if err := verifyLocalhost(t, ca, leaf); err != nil {
		t.Fatalf("leaf must verify under its own CA: %v", err)
	}
	if err := verifyLocalhost(t, other, leaf); err == nil {
		t.Fatal("leaf must not verify under a different CA")
	}
	if err := leaf.X509.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatalf("default leaf must cover 127.0.0.1: %v", err)
	}
}

func TestCA_SeveralLeavesAreDistinct(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	a := ca.NewLeaf(t, pvetest.LeafOptions{})
	b := ca.NewLeaf(t, pvetest.LeafOptions{})
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("two leaves signed by one CA must have different fingerprints")
	}
	if a.X509.SerialNumber.Cmp(b.X509.SerialNumber) == 0 {
		t.Fatal("two leaves must have different serial numbers")
	}
	if err := verifyLocalhost(t, ca, b); err != nil {
		t.Fatalf("second leaf must verify: %v", err)
	}
}

func TestCA_LeafNamesAndValidity(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)

	mismatch := ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"other.example"}})
	if err := mismatch.X509.VerifyHostname("localhost"); err == nil {
		t.Fatal("a leaf with only other.example must not match localhost")
	}
	if err := mismatch.X509.VerifyHostname("127.0.0.1"); err == nil {
		t.Fatal("a leaf with only a DNS name must not match 127.0.0.1")
	}

	ipOnly := ca.NewLeaf(t, pvetest.LeafOptions{IPs: []net.IP{net.IPv4(10, 1, 2, 3)}})
	if err := ipOnly.X509.VerifyHostname("10.1.2.3"); err != nil {
		t.Fatalf("IP SAN must match: %v", err)
	}

	expired := ca.NewLeaf(t, pvetest.LeafOptions{
		NotBefore: time.Now().Add(-48 * time.Hour),
		NotAfter:  time.Now().Add(-24 * time.Hour),
	})
	if err := verifyLocalhost(t, ca, expired); err == nil {
		t.Fatal("an expired leaf must fail verification")
	}
}

func TestLeaf_FingerprintForms(t *testing.T) {
	t.Parallel()
	leaf := pvetest.NewCA(t).NewLeaf(t, pvetest.LeafOptions{})
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(leaf.Fingerprint()) {
		t.Fatalf("Fingerprint() = %q, want 64 lowercase hex characters", leaf.Fingerprint())
	}
	colon := leaf.FingerprintColon()
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(colon) {
		t.Fatalf("FingerprintColon() = %q, want 32 colon-separated uppercase pairs", colon)
	}
	if strings.ReplaceAll(strings.ToLower(colon), ":", "") != leaf.Fingerprint() {
		t.Fatal("the two fingerprint forms must carry the same digest")
	}
}

func TestCA_PEMAndClientConfig(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CertPEM must parse as a certificate")
	}
	pem := ca.CertPEM()
	pem[0] = 'X'
	if pool2 := x509.NewCertPool(); !pool2.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("mutating the returned PEM must not change the CA's copy")
	}
	cfg := ca.ClientConfig("pve.example")
	if cfg.ServerName != "pve.example" || cfg.RootCAs == nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("ClientConfig = %+v", cfg)
	}
	leaf := ca.NewLeaf(t, pvetest.LeafOptions{})
	if !strings.Contains(string(leaf.PEM), "BEGIN CERTIFICATE") {
		t.Fatal("leaf PEM must be a certificate block")
	}
}
