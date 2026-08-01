package pve_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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

	_, err := pve.NewClientWithTracer(cfg, logger(t), nil)
	if err == nil {
		t.Fatal("expected error for missing auth, got nil")
	}
}

func TestNewClient_VerifySSL_True(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.VerifySSL = boolPtr(true)

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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

	_, err := pve.NewClientWithTracer(cfg, logger(t), nil)
	if err == nil {
		t.Fatal("expected error for empty host, got nil")
	}
}

func TestNewClient_NilConfig(t *testing.T) {
	t.Parallel()
	_, err := pve.NewClientWithTracer(nil, logger(t), nil)
	if err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
}

func TestServiceAccessors(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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
	if c.ClusterStorage() == nil {
		t.Error("ClusterStorage() returned nil")
	}
	if c.Pools() == nil {
		t.Error("Pools() returned nil")
	}
}

func TestNewClient_DefaultPort(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Port = 0

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
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

	c, err := pve.NewClientWithTracer(cfg, logger(t), nil)
	if err != nil {
		t.Fatalf("verify_ssl=false with CA cert: expected no error, got: %v", err)
	}
	if c == nil {
		t.Fatal("verify_ssl=false with CA cert: expected non-nil client")
	}
}

// ---------------------------------------------------------------------------
// PoolService wire-level tests — exercises sdkPoolService.CreatePool,
// DeletePool, GetPoolComment, and (indirectly, through GetPoolComment's
// not-found fold) isPoolNotFound end to end against a fake PVE /pools API.
// Reuses hostPort() from useragent_wire_test.go (same package, same dir).
// ---------------------------------------------------------------------------

// newPoolStubClient spins an httptest.TLS server running mux and returns a
// pve.Client pointed at it with TLS verification disabled (stub uses a
// self-signed cert) and API token auth (no auto-login round trip).
func newPoolStubClient(t *testing.T, mux *http.ServeMux) pve.Client {
	t.Helper()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	host, port := hostPort(t, server.URL)
	cfg := &config.CPIConfig{
		Host:      host,
		Port:      port,
		APIToken:  "root@pam!test=tok-pool",
		VerifySSL: boolPtr(false),
	}
	c, err := pve.NewClientWithTracer(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestPoolService_CreatePool_Success confirms CreatePool issues POST /pools
// and returns nil on a 2xx response.
func TestPoolService_CreatePool_Success(t *testing.T) {
	t.Parallel()
	var gotMethod atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/pools", func(w http.ResponseWriter, r *http.Request) {
		gotMethod.Store(r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":null}`))
	})
	c := newPoolStubClient(t, mux)

	if err := c.Pools().CreatePool(context.Background(), "bosh-lock", "held by bosh"); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if m, _ := gotMethod.Load().(string); m != http.MethodPost {
		t.Errorf("method on wire = %q, want POST", m)
	}
}

// TestPoolService_CreatePool_ErrorPropagates confirms a non-2xx response from
// POST /pools is returned as an error, not swallowed.
func TestPoolService_CreatePool_ErrorPropagates(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/pools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"pool bosh-lock already exists","code":500}`))
	})
	c := newPoolStubClient(t, mux)

	err := c.Pools().CreatePool(context.Background(), "bosh-lock", "held by bosh")
	if err == nil {
		t.Fatal("expected error from CreatePool on 500 response, got nil")
	}
}

// TestPoolService_DeletePool_Success confirms DeletePool issues DELETE /pools
// and returns nil on a 2xx response.
func TestPoolService_DeletePool_Success(t *testing.T) {
	t.Parallel()
	var gotMethod atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/pools", func(w http.ResponseWriter, r *http.Request) {
		gotMethod.Store(r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":null}`))
	})
	c := newPoolStubClient(t, mux)

	if err := c.Pools().DeletePool(context.Background(), "bosh-lock"); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	if m, _ := gotMethod.Load().(string); m != http.MethodDelete {
		t.Errorf("method on wire = %q, want DELETE", m)
	}
}

// TestPoolService_DeletePool_ErrorPropagates confirms a non-2xx response from
// DELETE /pools is returned as an error, not swallowed.
func TestPoolService_DeletePool_ErrorPropagates(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/pools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"internal error","code":500}`))
	})
	c := newPoolStubClient(t, mux)

	err := c.Pools().DeletePool(context.Background(), "bosh-lock")
	if err == nil {
		t.Fatal("expected error from DeletePool on 500 response, got nil")
	}
}

// TestPoolService_GetPoolComment_Found confirms a 2xx GET /pools/{poolid}
// response is decoded into (comment, found=true, nil).
func TestPoolService_GetPoolComment_Found(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/pools/bosh-lock", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"comment":"held by acme","members":[]}}`))
	})
	c := newPoolStubClient(t, mux)

	comment, found, err := c.Pools().GetPoolComment(context.Background(), "bosh-lock")
	if err != nil {
		t.Fatalf("GetPoolComment: %v", err)
	}
	if !found {
		t.Fatal("found=false, want true")
	}
	if comment != "held by acme" {
		t.Fatalf("comment=%q, want %q", comment, "held by acme")
	}
}

// TestPoolService_GetPoolComment_NotFound confirms a 404 response from
// GET /pools/{poolid} folds through isPoolNotFound into ("", false, nil) —
// the "pool absent" case the cluster-lock primitive relies on, rather than
// surfacing as a generic error.
func TestPoolService_GetPoolComment_NotFound(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/pools/missing-pool", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"pool 'missing-pool' does not exist","code":404}`))
	})
	c := newPoolStubClient(t, mux)

	comment, found, err := c.Pools().GetPoolComment(context.Background(), "missing-pool")
	if err != nil {
		t.Fatalf("expected 404 to fold to (false, nil), got err=%v", err)
	}
	if found {
		t.Fatal("found=true, want false for a 404 response")
	}
	if comment != "" {
		t.Fatalf("comment=%q, want empty string on not-found", comment)
	}
}

// TestPoolService_GetPoolComment_OtherErrorPropagates confirms a non-404,
// non-2xx response from GET /pools/{poolid} is NOT folded by isPoolNotFound —
// fail-closed: an unknown failure must propagate as an error rather than be
// mistaken for an absent pool.
func TestPoolService_GetPoolComment_OtherErrorPropagates(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/pools/bosh-lock", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"internal error","code":500}`))
	})
	c := newPoolStubClient(t, mux)

	_, found, err := c.Pools().GetPoolComment(context.Background(), "bosh-lock")
	if err == nil {
		t.Fatal("expected a 500 response to propagate as an error, got nil")
	}
	if found {
		t.Fatal("found=true, want false when an error propagated")
	}
}
