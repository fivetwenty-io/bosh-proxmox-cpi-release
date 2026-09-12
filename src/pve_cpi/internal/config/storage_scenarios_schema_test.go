package config_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestCertificationScenarioConfigs uses the runner's real Python construction
// and the production Go loader. No PVE client is constructed in either process.
func TestCertificationScenarioConfigs(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const script = `
import json, pathlib, sys
sys.path.insert(0, str(pathlib.Path(sys.argv[1]) / "scripts"))
from _storage_placement_scenarios import ScenarioRunner, STRATEGIES
runner = ScenarioRunner.__new__(ScenarioRunner)
runner.base_config = json.loads((pathlib.Path(sys.argv[1]) / "manifests/examples/multi-storage-placement/separate-sets.json").read_text())
runner.base_config["strict_config_validation"] = True
runner.policy = {"ephemeral_storage_ids":["e1","e2","e3"], "persistent_storage_ids":["p1","p2"], "root_storage_ids":["r1"], "fixed_iso_storage_id":"iso"}
configs = [runner.configured_policy(e_strategy=e, p_strategy=p) for e in STRATEGIES for p in STRATEGIES]
configs += [runner.configured_policy(e_members=["e1"], p_members=["p1"]), runner.configured_policy(root_members=["r1"]), runner.configured_policy(root_members=["r1"], iso="fixed")]
configs += [runner.removed_membership_config(configs[0])]
print(json.dumps(configs))
`
	command := exec.CommandContext(t.Context(), "python3", "-c", script, root)
	raw, err := command.Output()
	if err != nil {
		t.Fatalf("construct Python certification configs: %v", err)
	}
	var generated []json.RawMessage
	if err := json.Unmarshal(raw, &generated); err != nil {
		t.Fatal(err)
	}
	if len(generated) != 13 {
		t.Fatalf("expected 13 strategy/layout/removal configurations, got %d", len(generated))
	}
	for index, document := range generated {
		cfg, err := config.Load(bytes.NewReader(document))
		if err != nil {
			t.Fatalf("generated config %d: %v", index, err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("generated config %d: %v", index, err)
		}
		if cfg.StoragePlacementNamespace != "production-director-pve" || cfg.StorageAllocationJournalDir != "/var/vcap/store/pve_cpi/allocations" {
			t.Fatal("scenario changed original journal authority")
		}
	}
}

// TestCertificationTokenConsumesProductionCID prevents fixture-only token
// spelling from diverging from the journal and the real disk envelope encoder.
func TestCertificationTokenConsumesProductionCID(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	token, err := allocationjournal.DiskCorrelationToken("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	cid, err := pve.EncodeDiskCID("p1:images/disk.qcow2", &pve.DiskCIDMeta{ID: token})
	if err != nil {
		t.Fatal(err)
	}
	const script = `import pathlib,sys
sys.path.insert(0,str(pathlib.Path(sys.argv[1])/"scripts"))
from _storage_placement_scenarios import disk_correlation_token
assert disk_correlation_token(sys.argv[2]) == sys.argv[3]
`
	command := exec.CommandContext(t.Context(), "python3", "-c", script, root, cid, token)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("production CID Python readback: %v: %s", err, output)
	}
}

func TestCertificationRecordChecksProductionEnvelopeAndAuditHash(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	record := allocationjournal.Record{Version: 1, ID: "11111111-1111-4111-8111-111111111111", CID: "100", Reason: "<π>&\u2028", Intent: allocationjournal.Intent{Plan: json.RawMessage(`{"small":1e-9,"large":1e+30}`)}}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	envelope, err := json.Marshal(struct {
		Version int             `json:"version"`
		SHA256  string          `json:"sha256"`
		Payload json.RawMessage `json:"payload"`
	}{1, hex.EncodeToString(sum[:]), payload})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, _, err := allocationjournal.VerificationEvidence(record)
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"envelope": string(envelope), "summary": map[string]string{"ID": record.ID, "CID": record.CID, "SHA256": fingerprint}})
	if err != nil {
		t.Fatal(err)
	}
	const script = `import json,pathlib,sys
sys.path.insert(0,str(pathlib.Path(sys.argv[1])/"scripts"))
from _storage_placement_scenarios import verified_record_payload
value=json.load(sys.stdin)
assert verified_record_payload(value["envelope"].encode(),value["summary"])["cid"] == "100"
`
	command := exec.CommandContext(t.Context(), "python3", "-c", script, root)
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("production journal Python readback: %v: %s", err, output)
	}
}

// TestCertificationVMFaultReadsProductionSteps checks the Python phase gate
// against the actual journal JSON field names, target, and immutable parameters.
func TestCertificationVMFaultReadsProductionSteps(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	record := allocationjournal.Record{Version: 1, ID: "11111111-1111-4111-8111-111111111111", Kind: "vm", AgentID: "agent", Namespace: "test", Intent: allocationjournal.Intent{Plan: json.RawMessage(`{"Targets":[{"Role":"root","StorageID":"e1"}]}`)}, Steps: []allocationjournal.Step{{ID: "step", Kind: "vm.Cluster.CreateHaRules", State: allocationjournal.Planned, Target: allocationjournal.Target{Node: "n1", VMID: 123}, Parameters: json.RawMessage(`{"version":1,"kind":"ha_rule","rule":"owned","type":"node-affinity","resources":"vm:123"}`)}}}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	const script = `import json,pathlib,sys
sys.path.insert(0,str(pathlib.Path(sys.argv[1])/"scripts"))
from _storage_placement_vm_faults import JournalVMFaultProxy
record=json.load(sys.stdin)
step=record["steps"][0]
fields={"rule":"owned","type":"node-affinity","resources":"vm:123"}
assert JournalVMFaultProxy.matches(record,step,"POST","/cluster/ha/rules",fields)
fields["resources"]="vm:999"
assert not JournalVMFaultProxy.matches(record,step,"POST","/cluster/ha/rules",fields)
`
	command := exec.CommandContext(t.Context(), "python3", "-c", script, root)
	command.Stdin = bytes.NewReader(payload)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("production VM fault journal seam: %v: %s", err, output)
	}
}

// TestCertificationDirectorReadsProductionAuthority checks the real producer.
func TestCertificationDirectorReadsProductionAuthority(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = allocationjournal.Initialize(t.Context(), directory, "local-certification", allocationjournal.Enrollment{ClusterID: "actual-cluster", AuthorityID: "test-authority", AuditID: "test-audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	const script = `import pathlib,sys,types
sys.path.insert(0,str(pathlib.Path(sys.argv[1])/"scripts"))
from _storage_placement_director import DirectorScenarios
value=DirectorScenarios.__new__(DirectorScenarios)
value.runner=types.SimpleNamespace(base_config={"storage_allocation_journal_dir":sys.argv[2],"storage_placement_namespace":"local-certification"})
value.bosh=lambda *a,**k:{"uuid":"director-uuid"}
value.observer=types.SimpleNamespace(snapshot=lambda:{"cluster_id":"actual-cluster","director_uuid":"director-uuid"})
assert value.snapshot()["cluster_id"] == "actual-cluster"
value.observer=types.SimpleNamespace(snapshot=lambda:{"cluster_id":"foreign","director_uuid":"director-uuid"})
try: value.snapshot()
except RuntimeError: pass
else: raise AssertionError("foreign cluster accepted")
`
	command := exec.CommandContext(t.Context(), "python3", "-c", script, root, directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("production Director authority seam: %v: %s", err, output)
	}
}

// TestCertificationPendingVMArtifacts preserves unresolved production journal
// states while observing exact root, ephemeral, and ISO submission effects.
func TestCertificationPendingVMArtifacts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"vm.QEMU.Create", "vm.Nodes.CreateQemuClone", "vm.Storage.CreateVolume", "vm.Storage.Upload"} {
		t.Run(kind, func(t *testing.T) {
			target := allocationjournal.Target{Node: "n1", VMID: 123, Storage: "e1", Backing: "nfs:host:/export"}
			if kind == "vm.Storage.CreateVolume" {
				target.IntendedVolume = "e1:123/vm-123-ephemeral-0.qcow2"
			}
			if kind == "vm.Storage.Upload" {
				target.IntendedVolume = "e1:iso/vm-123-config.iso"
			}
			record := allocationjournal.Record{Version: 1, ID: "11111111-1111-4111-8111-111111111111", Kind: "vm", State: allocationjournal.ReconciliationRequired, Intent: allocationjournal.Intent{Plan: json.RawMessage(`{"Targets":[{"Role":"root","StorageID":"e1","VirtualBytes":1024},{"Role":"ephemeral","StorageID":"e1","VirtualBytes":1024},{"Role":"iso","StorageID":"e1","VirtualBytes":1024}]}`)}, Steps: []allocationjournal.Step{{ID: "pending", Kind: kind, State: allocationjournal.Planned, Target: target}}}
			payload, marshalErr := json.Marshal(record)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			const script = `import json,pathlib,sys,types
sys.path.insert(0,str(pathlib.Path(sys.argv[1])/"scripts"))
from _storage_placement_vm_faults import pending_vm_artifacts
record=json.load(sys.stdin)
step=record["steps"][0]
volume=step["target"].get("intended_volume") or "e1:123/vm-123-disk-0.qcow2"
fault={"kind":step["kind"],"target":step["target"],"response_volume":volume if step["kind"] == "vm.Storage.CreateVolume" else ""}
runner=types.SimpleNamespace(verifier=types.SimpleNamespace(qemu_config=lambda *args:{"scsi0":volume},_get=lambda path:{"size":1024}))
observed=pending_vm_artifacts(runner,record,fault,{volume})
assert observed[0]["volume_id"] == volume
assert "ownership remains unverified" in observed[0]["evidence_kind"]
assert step["state"] == "planned" and not step.get("volids") and record["state"] == "reconciliation_required"
try: pending_vm_artifacts(runner,record,fault,{volume,"e1:123/foreign.raw"})
except RuntimeError: pass
else: raise AssertionError("foreign same-storage artifact accepted")
`
			command := exec.CommandContext(t.Context(), "python3", "-c", script, root)
			command.Stdin = bytes.NewReader(payload)
			if output, runErr := command.CombinedOutput(); runErr != nil {
				t.Fatalf("pending production journal seam: %v: %s", runErr, output)
			}
		})
	}
}
