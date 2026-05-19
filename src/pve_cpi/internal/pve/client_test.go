package pve_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

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
	cfg := baseConfig()
	cfg.Password = ""
	cfg.APIToken = ""

	_, err := pve.NewClient(cfg, logger(t))
	if err == nil {
		t.Fatal("expected error for missing auth, got nil")
	}
}

func TestNewClient_VerifySSL_True(t *testing.T) {
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
	cfg := baseConfig()
	cfg.Host = ""

	_, err := pve.NewClient(cfg, logger(t))
	if err == nil {
		t.Fatal("expected error for empty host, got nil")
	}
}

func TestNewClient_NilConfig(t *testing.T) {
	_, err := pve.NewClient(nil, logger(t))
	if err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
}

func TestServiceAccessors(t *testing.T) {
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
