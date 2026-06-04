package agent

import (
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

func TestNewRegistryAgentIfConfigured_NoEndpoint(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("auto")
	// RegistryEndpoint is empty by default in minimalCfg.

	a, err := NewRegistryAgentIfConfigured(cfg, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected (nil, nil) when endpoint empty, got err: %v", err)
	}
	if a != nil {
		t.Fatalf("expected nil Agent when endpoint empty, got %T", a)
	}
}

func TestNewRegistryAgentIfConfigured_WithEndpoint(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("auto")
	cfg.RegistryEndpoint = "http://registry.example.com:25777"
	cfg.RegistryUser = "admin"
	cfg.RegistryPassword = "regpass"
	// AllowPrivateIP must be true so construction skips DNS resolution
	// for the non-routable example.com host in unit tests.
	cfg.RegistryAllowPrivateIP = boolPtr(true)

	a, err := NewRegistryAgentIfConfigured(cfg, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected non-nil Agent, got err: %v", err)
	}
	if a == nil {
		t.Fatal("expected non-nil Agent, got nil")
	}
	if _, ok := a.(*RegistryAgent); !ok {
		t.Fatalf("expected *RegistryAgent, got %T", a)
	}
}

func TestNewRegistryAgentIfConfigured_BadCACert(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("auto")
	cfg.RegistryEndpoint = "http://registry.example.com:25777"
	cfg.RegistryUser = "admin"
	cfg.RegistryPassword = "regpass"
	cfg.RegistryAllowPrivateIP = boolPtr(true)
	// Supplying an unparseable CACertPEM triggers NewClientWithOptions to return
	// an error at construction time without requiring network access.
	cfg.RegistryCACertPEM = "not-a-valid-pem-certificate"

	a, err := NewRegistryAgentIfConfigured(cfg, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected Cloud error for bad CACertPEM, got nil")
	}
	if a != nil {
		t.Fatalf("expected nil Agent on error, got %T", a)
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Fatalf("expected Cloud error type, got %T: %v", err, err)
	}
}

func TestNewRegistryAgentIfConfigured_NilCfg(t *testing.T) {
	t.Parallel()
	_, err := NewRegistryAgentIfConfigured(nil, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error for nil cfg")
	}
}

func TestNewRegistryAgentIfConfigured_NilLogger(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("auto")
	_, err := NewRegistryAgentIfConfigured(cfg, nil)
	if err == nil {
		t.Fatal("expected error for nil logger")
	}
}
