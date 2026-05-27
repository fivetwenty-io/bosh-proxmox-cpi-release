package agent

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

//nolint:modernize // helper supports non-zero bool values; new(bool) only gives false
func boolPtr(b bool) *bool { return &b }

// minimalCfg returns a CPIConfig with required fields set and the given agent mode.
// Storage fields default to "local"; registry fields are empty unless overridden.
func minimalCfg(mode string) *config.CPIConfig {
	return &config.CPIConfig{
		Host:           "pve.example.com",
		Port:           8006,
		User:           "root",
		Password:       "secret",
		Realm:          "pam",
		Node:           "pve",
		VMStorage:      "local",
		DiskStorage:    "local",
		NetworkBridge:  "vmbr0",
		VerifySSL:      boolPtr(true),
		AgentMode:      mode,
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
	}
}

// newFakePVE returns a fakePVEClient with empty storage/nodes services suitable
// for factory smoke tests (the factory never calls into them).
func newFakePVE() *fakePVEClient {
	return &fakePVEClient{
		storageSvc: &fakeStorageSvc{},
		nodesSvc:   &fakeNodesSvc{},
	}
}

func TestNewAgent_Cloudinit_ReturnsConfigDrive(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("cloudinit")
	cfg.VMStorage = "local"

	a, err := NewAgent(cfg, newFakePVE(), log.NewNopLogger())
	if err != nil {
		t.Fatalf("NewAgent(cloudinit): unexpected error: %v", err)
	}
	if _, ok := a.(*ConfigDrive); !ok {
		t.Fatalf("NewAgent(cloudinit): expected *ConfigDrive, got %T", a)
	}
}

func TestNewAgent_Registry(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("registry")
	cfg.RegistryEndpoint = "http://registry.example.com:25777"
	cfg.RegistryUser = "admin"
	cfg.RegistryPassword = "regpass"

	a, err := NewAgent(cfg, nil, log.NewNopLogger())
	if err != nil {
		t.Fatalf("NewAgent(registry): unexpected error: %v", err)
	}
	if _, ok := a.(*RegistryAgent); !ok {
		t.Fatalf("NewAgent(registry): expected *RegistryAgent, got %T", a)
	}
}

func TestNewAgent_NoAgent(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("noagent")

	a, err := NewAgent(cfg, nil, log.NewNopLogger())
	if err != nil {
		t.Fatalf("NewAgent(noagent): unexpected error: %v", err)
	}
	if _, ok := a.(*NoAgent); !ok {
		t.Fatalf("NewAgent(noagent): expected *NoAgent, got %T", a)
	}
}

func TestNewAgent_UnknownMode(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("bogus")

	_, err := NewAgent(cfg, nil, log.NewNopLogger())
	if err == nil {
		t.Fatal("NewAgent(bogus): expected error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeNotSupported) {
		t.Fatalf("NewAgent(bogus): expected NotSupported error type, got %T: %v", err, err)
	}
}

func TestNewAgent_RegistryEndpointMissing(t *testing.T) {
	t.Parallel()
	cfg := minimalCfg("registry")

	_, err := NewAgent(cfg, nil, log.NewNopLogger())
	if err == nil {
		t.Fatal("NewAgent(registry, no endpoint): expected error, got nil")
	}
}

func TestNewAgent_CloudinitStorageFallback(t *testing.T) {
	t.Parallel()

	t.Run("StemcellStorage used when ISOStorage empty", func(t *testing.T) {
		t.Parallel()
		cfg := minimalCfg("cloudinit")
		cfg.VMStorage = "local"
		cfg.StemcellStorage = "stemcell-store"

		a, err := NewAgent(cfg, newFakePVE(), log.NewNopLogger())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cd, ok := a.(*ConfigDrive)
		if !ok {
			t.Fatalf("expected *ConfigDrive, got %T", a)
		}
		if cd.storage != "stemcell-store" {
			t.Fatalf("expected storage=stemcell-store, got %q", cd.storage)
		}
	})

	t.Run("VMStorage used when ISOStorage and StemcellStorage empty", func(t *testing.T) {
		t.Parallel()
		cfg := minimalCfg("cloudinit")
		cfg.VMStorage = "vm-store"
		cfg.StemcellStorage = ""

		a, err := NewAgent(cfg, newFakePVE(), log.NewNopLogger())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cd, ok := a.(*ConfigDrive)
		if !ok {
			t.Fatalf("expected *ConfigDrive, got %T", a)
		}
		if cd.storage != "vm-store" {
			t.Fatalf("expected storage=vm-store, got %q", cd.storage)
		}
	})
}

func TestNewAgent_NilCfg(t *testing.T) {
	t.Parallel()
	_, err := NewAgent(nil, nil, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error for nil cfg")
	}
}

func TestNewAgent_NilLogger(t *testing.T) {
	t.Parallel()
	_, err := NewAgent(minimalCfg("noagent"), nil, nil)
	if err == nil {
		t.Fatal("expected error for nil logger")
	}
}
