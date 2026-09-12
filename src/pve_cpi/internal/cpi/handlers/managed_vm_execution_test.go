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
