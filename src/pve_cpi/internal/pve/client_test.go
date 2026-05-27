package pve_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

//nolint:modernize // helper supports non-zero bool values; new(bool) only gives false
func boolPtr(b bool) *bool { return &b }

func baseConfig() *config.CPIConfig {
	return &config.CPIConfig{
		Host:      "pve.example.com",
		Port:      8006,
		User:      "root",
		Password:  "secret",
		Realm:     "pam",
		VerifySSL: boolPtr(true),
	}
}

func logger(t *testing.T) *log.Logger {
	return log.NewNopLogger()
}

func TestNewClient_TokenAuth(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Password = ""
	cfg.APIToken = "root@pam!mytoken=abc123"

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_PasswordAuth(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_NoAuth(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Password = ""
	cfg.APIToken = ""

	_, err := pve.NewClient(cfg, logger(t))
	if err == nil {
		t.Fatal("expected error for missing auth, got nil")
	}
}

func TestNewClient_VerifySSL_True(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.VerifySSL = boolPtr(true)

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_VerifySSL_False(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.VerifySSL = boolPtr(false)

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("expected no error with VerifySSL=false, got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_BadURL(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Host = ""

	_, err := pve.NewClient(cfg, logger(t))
	if err == nil {
		t.Fatal("expected error for empty host, got nil")
	}
}

func TestNewClient_NilConfig(t *testing.T) {
	t.Parallel()
	_, err := pve.NewClient(nil, logger(t))
	if err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
}

func TestServiceAccessors(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if c.QEMU() == nil {
		t.Error("QEMU() returned nil")
	}
	if c.Storage() == nil {
		t.Error("Storage() returned nil")
	}
	if c.CloudInit() == nil {
		t.Error("CloudInit() returned nil")
	}
	if c.Tasks() == nil {
		t.Error("Tasks() returned nil")
	}
	if c.Nodes() == nil {
		t.Error("Nodes() returned nil")
	}
	if c.Cluster() == nil {
		t.Error("Cluster() returned nil")
	}
}

func TestNewClient_DefaultPort(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Port = 0

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("expected no error with zero port (should default 8006), got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_DefaultRealm(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Realm = ""

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("expected no error with empty realm (should default pam), got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

// selfSignedCAPEM generates a minimal self-signed CA certificate and returns
// it as a PEM-encoded string. Used only in tests.
func selfSignedCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestNewClient_PVECACert_Empty verifies that an empty PVECACertPEM does not
// change NewClient behavior: the call succeeds and returns a non-nil client
// (byte-identical code path to prior releases).
func TestNewClient_PVECACert_Empty(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.PVECACertPEM = ""

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("empty PVECACertPEM: expected no error, got: %v", err)
	}
	if c == nil {
		t.Fatal("empty PVECACertPEM: expected non-nil client")
	}
}

// TestNewClient_PVECACert_ValidPEM verifies that a well-formed PEM CA bundle is
// accepted: NewClient must succeed and return a non-nil client. The cert pool
// is baked into the SDK's tls.Config at construction time; the temp file is
// removed before this assertion runs.
func TestNewClient_PVECACert_ValidPEM(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.PVECACertPEM = selfSignedCAPEM(t)

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("valid PVECACertPEM: expected no error, got: %v", err)
	}
	if c == nil {
		t.Fatal("valid PVECACertPEM: expected non-nil client")
	}
}

// TestNewClient_PVECACert_VerifySSLFalse verifies that when VerifySSL is false
// the CA cert is ignored and NewClient succeeds (insecure-skip-verify path is
// unchanged; no PEM parsing attempted).
func TestNewClient_PVECACert_VerifySSLFalse(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.VerifySSL = boolPtr(false)
	// Supply malformed PEM to confirm it is NOT parsed (verify_ssl=false wins).
	cfg.PVECACertPEM = "not-a-pem"

	c, err := pve.NewClient(cfg, logger(t))
	if err != nil {
		t.Fatalf("verify_ssl=false with CA cert: expected no error, got: %v", err)
	}
	if c == nil {
		t.Fatal("verify_ssl=false with CA cert: expected non-nil client")
	}
}
