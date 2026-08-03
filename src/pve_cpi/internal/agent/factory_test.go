package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
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

// rawEntryClusterStorageSvc returns exactly the raw JSON bytes given, with no
// field re-marshaling — unlike fakeClusterStorageSvc (which always emits a
// "shared" and "content" key), this lets a test omit a key entirely to prove
// the decoder does not carry a value over from a prior entry.
type rawEntryClusterStorageSvc struct {
	sdkclusterstorage.Service
	entries []json.RawMessage
}

func (f *rawEntryClusterStorageSvc) ListStorage(context.Context, *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	resp := sdkclusterstorage.ListStorageResponse(f.entries)
	return &resp, nil
}

// TestVMStorageISOEligibility_DoesNotInheritPriorEntryFields is the F1
// regression test: the decode struct that previously lived outside the scan
// loop left "shared" and "content" at the prior entry's values whenever a
// later /storage entry omitted those optional JSON keys. Here vmpool omits
// both keys entirely (as most non-network storage types do in PVE's real
// response), and directly precedes it is a shared, iso-capable cephfs entry
// — the exact inheritance trap from the review. vmpool must report its own
// (absent) values, not cephfs-a's.
func TestVMStorageISOEligibility_DoesNotInheritPriorEntryFields(t *testing.T) {
	t.Parallel()
	svc := &rawEntryClusterStorageSvc{entries: []json.RawMessage{
		json.RawMessage(`{"storage":"cephfs-a","type":"cephfs","shared":1,"content":"iso,vztmpl"}`),
		json.RawMessage(`{"storage":"vmpool","type":"lvmthin"}`), // no "shared", no "content" key at all
	}}
	pveClient := &fakePVEClient{clusterStorageSvc: svc}

	shared, hasISO, found, err := vmStorageISOEligibility(context.Background(), pveClient, "vmpool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected vmpool to be found in the index")
	}
	if shared {
		t.Error("vmpool carries no \"shared\" key and lvmthin is not shared-by-type: must report shared=false, not inherit cephfs-a's shared=true")
	}
	if hasISO {
		t.Error("vmpool carries no \"content\" key: must report hasISO=false, not inherit cephfs-a's iso content")
	}

	// The earlier entry itself must still decode correctly (scan order does
	// not corrupt the first entry either).
	sharedA, hasISOA, foundA, errA := vmStorageISOEligibility(context.Background(), pveClient, "cephfs-a")
	if errA != nil {
		t.Fatalf("unexpected error: %v", errA)
	}
	if !foundA || !sharedA || !hasISOA {
		t.Errorf("cephfs-a: want found=true shared=true hasISO=true, got found=%v shared=%v hasISO=%v", foundA, sharedA, hasISOA)
	}
}
