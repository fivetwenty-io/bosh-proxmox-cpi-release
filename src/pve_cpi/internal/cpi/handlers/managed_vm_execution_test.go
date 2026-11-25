package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestManagedVMExecutionFreezeAndCredentialRotation(t *testing.T) {
	cfg := &config.CPIConfig{Password: "first-secret", APIToken: "token-first", NetworkBridge: "bridge-a"}
	balloon := 256
	shape := &createVMShape{node: "n", vmStorage: "s", rootDiskKey: "scsi0", cores: 3, memMiB: 1024, balloonMiB: &balloon, rootDiskPerfOpts: map[string]string{"cache": "none"}}
	frozen, err := freezeManagedVMExecution(cfg, shape)
	if err != nil {
		t.Fatal(err)
	}
	shape.rootDiskPerfOpts["cache"] = "unsafe"
	balloon = 999
	cfg.Password = "rotated-secret"
	cfg.APIToken = "rotated-token"
	cfg.StorageSets = map[string]config.StorageSet{"new": {}}
	restored, err := frozen.shape(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restored.rootDiskKey != "scsi0" || restored.rootDiskPerfOpts["cache"] != "none" || *restored.balloonMiB != 256 {
		t.Fatal("execution snapshot changed")
	}
	raw, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "token-first") {
		t.Fatal("credentials persisted")
	}
	cfg.NetworkBridge = "different"
	if _, err = frozen.shape(cfg); err == nil {
		t.Fatal("changed network defaults accepted")
	}
}

// TestManagedVMExecutionCarriesTheHAOptOut pins that a recorded VM execution
// carries the create's ha opt-out through the journal, so a resumed create
// clones an opted-out VM as a VM without HA. A VM that keeps HA records no
// opt-out key, so its plan reads the same as before the key existed and an
// older release can still decode it.
func TestManagedVMExecutionCarriesTheHAOptOut(t *testing.T) {
	cfg := &config.CPIConfig{NetworkBridge: "bridge-a"}
	for name, haOff := range map[string]bool{"opted out": true, "keeps HA": false} {
		t.Run(name, func(t *testing.T) {
			frozen, err := freezeManagedVMExecution(cfg, &createVMShape{node: "n", vmStorage: "s", haOff: haOff})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(frozen)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "HAOff") != haOff {
				t.Fatalf("recorded execution %s; want the opt-out key present %v", raw, haOff)
			}
			d := json.NewDecoder(strings.NewReader(string(raw)))
			d.DisallowUnknownFields()
			var decoded StorageVMExecution
			if err := d.Decode(&decoded); err != nil {
				t.Fatal(err)
			}
			restored, err := decoded.shape(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if restored.haOff != haOff {
				t.Fatalf("restored haOff = %v; want %v", restored.haOff, haOff)
			}
		})
	}
}
