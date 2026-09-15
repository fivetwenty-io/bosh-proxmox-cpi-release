package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

//nolint:gocognit // Keep the complete failure/recovery scenario and its evidence assertions together.
func TestStorageJournalInitializeRequiresCompleteReadOnlyAudit(t *testing.T) {
	for _, scenario := range []struct {
		name                    string
		denyStorage, denyImages bool
	}{
		{name: "complete"}, {name: "missing-storage-audit", denyStorage: true}, {name: "missing-image-access", denyImages: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			restricted := scenario.denyStorage || scenario.denyImages
			var writes atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
					http.Error(w, "unexpected mutation", 400)
					return
				}
				path := strings.TrimPrefix(r.URL.Path, "/api2/json")
				var data any
				switch path {
				case "/nodes":
					data = []any{map[string]any{"node": "pve", "status": "online"}}
				case "/nodes/pve/certificates/info":
					data = []any{map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.TrimSuffix(strings.Repeat("ab:", 32), ":")}}
				case "/access/permissions":
					path := r.URL.Query().Get("path")
					privileges := map[string]any{}
					if path == "/access" {
						privileges["Sys.Audit"] = 1
					}
					if path == "/vms" {
						privileges["VM.Audit"] = 1
						if !scenario.denyImages {
							privileges["VM.Config.Disk"] = 1
						}
					}
					if path == "/storage" && !scenario.denyStorage {
						privileges["Datastore.Audit"] = 1
					}
					data = map[string]any{path: privileges}
				case "/access/acl":
					data = []any{}
				case "/cluster/status":
					data = []any{map[string]any{"type": "node", "name": "pve", "online": 1}}
				case "/cluster/config/nodes":
					data = []any{map[string]any{"name": "pve"}}
				case "/nodes/pve/qemu", "/storage":
					data = []any{}
				default:
					t.Errorf("unexpected read %s", path)
					http.Error(w, "unknown", 404)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			cfg := minimalCfg()
			u, _ := url.Parse(server.URL)
			host, port, _ := net.SplitHostPort(u.Host)
			cfg.Host = host
			cfg.Port, _ = strconv.Atoi(port)
			cfg.VerifySSL = boolPtr(false)
			base := provisionTemp(t)
			directory := filepath.Join(base, "allocations")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			cfg.StorageAllocationJournalDir = directory
			cfg.StoragePlacementNamespace = "director"
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(base, "cpi.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			args := []string{"storage-journal", "initialize", "--config", path, "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"}
			code := runWithArgs(args, strings.NewReader(""), &out, &stderr, runOptions{})
			if restricted {
				if code == 0 {
					t.Fatal("filtered visibility enrolled authority")
				}
				entries, err := os.ReadDir(directory)
				if err != nil || len(entries) != 0 {
					t.Fatal("failed audit created journal evidence")
				}
			} else {
				if code != 0 {
					t.Fatalf("initialize failed: %s", stderr.String())
				}
				status, err := aj.InspectEnrollment(directory, "director")
				if err != nil || status.Enrollment.AuditID == "" {
					t.Fatalf("enrollment audit missing: %+v %v", status, err)
				}
				if _, err = os.Stat(filepath.Join(directory, "audit-"+status.Enrollment.AuditID+".json")); err != nil {
					t.Fatal("audit was not retained before enrollment")
				}
				auditBytes, readErr := os.ReadFile(filepath.Join(directory, "audit-"+status.Enrollment.AuditID+".json"))
				if readErr != nil || !bytes.Contains(auditBytes, []byte(`"RemoteTasksSettled":true`)) {
					t.Fatalf("task-settlement attestation not retained: %s %v", auditBytes, readErr)
				}
				if code = runWithArgs(args, strings.NewReader(""), &out, &stderr, runOptions{}); code == 0 {
					t.Fatal("reinitialization replaced authority")
				}
			}
			if writes.Load() != 0 {
				t.Fatal("initialization mutated PVE")
			}
			if strings.Contains(out.String()+stderr.String(), "root@pam!tok=secret") {
				t.Fatal("initialization leaked token")
			}
		})
	}
}
func TestStorageJournalRequiresExplicitFencingBeforeClient(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := runStorageJournal([]string{"initialize", "--config", "/unopened", "--authority-id", "writer"}, &out, &stderr, runOptions{}); code != 2 {
		t.Fatal("missing fencing attestation accepted")
	}
}

type storageJournalFixture struct {
	configPath, directory string
	fingerprint           atomic.Bool
	restricted            atomic.Bool
	writes                atomic.Int32
}

func newStorageJournalFixture(t *testing.T) *storageJournalFixture {
	t.Helper()
	f := &storageJournalFixture{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			f.writes.Add(1)
			http.Error(w, "forbidden write", 400)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		var data any
		switch path {
		case "/nodes":
			data = []any{map[string]any{"node": "pve", "status": "online"}}
		case "/nodes/pve/certificates/info":
			fingerprint := "ab"
			if f.fingerprint.Load() {
				fingerprint = "cd"
			}
			data = []any{map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.TrimSuffix(strings.Repeat(fingerprint+":", 32), ":")}}
		case "/access/permissions":
			scope := r.URL.Query().Get("path")
			priv := map[string]any{}
			switch scope {
			case "/access":
				priv["Sys.Audit"] = 1
			case "/vms":
				priv["VM.Audit"] = 1
				priv["VM.Config.Disk"] = 1
			case "/storage":
				if !f.restricted.Load() {
					priv["Datastore.Audit"] = 1
				}
			}
			data = map[string]any{scope: priv}
		case "/access/acl", "/nodes/pve/qemu", "/storage":
			data = []any{}
		case "/cluster/status":
			data = []any{map[string]any{"type": "node", "name": "pve", "online": 1}}
		case "/cluster/config/nodes":
			data = []any{map[string]any{"name": "pve"}}
		default:
			t.Errorf("unexpected read %s", path)
			http.Error(w, "unknown", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	cfg := minimalCfg()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host = host
	cfg.Port, err = strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	cfg.VerifySSL = boolPtr(false)
	base := provisionTemp(t)
	f.directory = filepath.Join(base, "allocations")
	if err = os.Mkdir(f.directory, 0700); err != nil {
		t.Fatal(err)
	}
	cfg.StorageAllocationJournalDir = f.directory
	cfg.StoragePlacementNamespace = "director"
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.configPath = filepath.Join(base, "cpi.json")
	if err = os.WriteFile(f.configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run("initialize", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"); code != 0 {
		t.Fatalf("initialize: %d %s", code, out)
	}
	t.Cleanup(func() {
		if f.writes.Load() != 0 {
			t.Error("journal CLI mutated PVE")
		}
	})
	return f
}
func (f *storageJournalFixture) run(action string, args ...string) (int, string) {
	var stdout, stderr bytes.Buffer
	command := append([]string{action, "--config", f.configPath}, args...)
	code := runStorageJournal(command, &stdout, &stderr, runOptions{})
	return code, stdout.String() + stderr.String()
}
func (f *storageJournalFixture) open(t *testing.T) *aj.Journal {
	t.Helper()
	enrollment, err := aj.InspectEnrollment(f.directory, "director")
	if err != nil {
		t.Fatal(err)
	}
	j, err := aj.Open(f.directory, "director", enrollment.Enrollment.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func journalFixtureFile(t *testing.T, base, name string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("missing fixture file %s", name)
	}
	return found
}
func journalFixtureSnapshot(t *testing.T, base string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			files[path] = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
func journalFixtureVM(t *testing.T, f *storageJournalFixture) (string, string) {
	t.Helper()
	j := f.open(t)
	fingerprint := strings.Repeat("a", 64)
	plan, err := json.Marshal(map[string]any{"Version": 1, "Namespace": "director", "AllocationKey": "agent", "PolicyFingerprint": fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: fingerprint, PolicyFingerprint: fingerprint, PlanVersion: 1, Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	id := h.Record().ID
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	return "agent", id
}
func TestStorageJournalRecoveryAuthorityAndIndex(t *testing.T) {
	f := newStorageJournalFixture(t)
	before, err := aj.InspectEnrollment(f.directory, "director")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := f.run("recover-authority", "--authority-id", "next-writer", "--previous-writer-fenced"); code != 0 {
		t.Fatalf("recover authority %d: %s", code, out)
	}
	after, err := aj.InspectEnrollment(f.directory, "director")
	if err != nil || after.Epoch == before.Epoch || after.Enrollment.AuthorityID != "next-writer" {
		t.Fatalf("authority not rotated: %+v %v", after, err)
	}
	agent, id := journalFixtureVM(t, f)
	index := journalFixtureFile(t, f.directory, "index.json")
	if err = os.Remove(index); err != nil {
		t.Fatal(err)
	}
	snapshot := journalFixtureSnapshot(t, f.directory)
	if code, out := f.run("audit"); code == 0 || !strings.Contains(out, `"generation_index_healthy":false`) {
		t.Fatalf("audit missed index loss %d: %s", code, out)
	}
	if !reflect.DeepEqual(snapshot, journalFixtureSnapshot(t, f.directory)) {
		t.Fatal("audit wrote journal")
	}
	if code, out := f.run("recover-index", "--authority-id", "index-writer", "--previous-writer-fenced", "--remote-tasks-settled"); code != 0 {
		t.Fatalf("recover index %d: %s", code, out)
	}
	j := f.open(t)
	record, found, err := j.InspectVMContext(t.Context(), agent)
	if err != nil || !found || record.ID != id {
		t.Fatalf("generation not restored: %+v %t %v", record, found, err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestStorageJournalRecoveryRefusesPartialAuditAndLiveClusterRebind(t *testing.T) {
	f := newStorageJournalFixture(t)
	journalFixtureVM(t, f)
	before, err := aj.InspectEnrollment(f.directory, "director")
	if err != nil {
		t.Fatal(err)
	}
	f.restricted.Store(true)
	for _, action := range []string{"recover-authority", "recover-index"} {
		if code, out := f.run(action, "--authority-id", "new-writer", "--previous-writer-fenced", "--remote-tasks-settled"); code == 0 {
			t.Fatalf("partial audit permitted %s: %s", action, out)
		}
	}
	f.restricted.Store(false)
	f.fingerprint.Store(true)
	for _, action := range []string{"recover-authority", "recover-index"} {
		if code, out := f.run(action, "--authority-id", "new-writer", "--previous-writer-fenced", "--remote-tasks-settled"); code == 0 {
			t.Fatalf("live cluster rebind permitted %s: %s", action, out)
		}
	}
	after, err := aj.InspectEnrollment(f.directory, "director")
	if err != nil || after.Epoch != before.Epoch {
		t.Fatal("failed recovery changed authority")
	}
}
func TestStorageJournalMissingGenerationRequiresIndependentCrashProof(t *testing.T) {
	f := newStorageJournalFixture(t)
	agent, id := journalFixtureVM(t, f)
	record := journalFixtureFile(t, f.directory, "allocation-"+id+".json")
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	args := []string{"--agent-id", agent, "--allocation-id", id, "--authority-id", "writer", "--previous-writer-fenced"}
	if code, out := f.run("resolve-missing-vm", args...); code != 2 {
		t.Fatalf("absence alone certified no submission %d: %s", code, out)
	}
	if code, out := f.run("recover-index", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"); code == 0 {
		t.Fatalf("index recovery discarded missing generation: %s", out)
	}
	if code, out := f.run("resolve-missing-vm", append(args, "--index-first-crash-confirmed")...); code != 0 {
		t.Fatalf("confirmed index-first recovery failed %d: %s", code, out)
	}
	j := f.open(t)
	r, err := j.Inspect(id)
	if err != nil || r.State != aj.Cleaned || len(r.Verifications) != 1 || !strings.Contains(r.Verifications[0].EvidenceJSON, `"IndexFirstCrashConfirmed":true`) {
		t.Fatalf("missing durable reconstruction: %+v %v", r, err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run("audit"); code != 0 {
		t.Fatalf("reconstructed tombstone cannot be audited %d: %s", code, out)
	}
}

func TestStorageJournalAuditNeverPrintsMalformedPlanSecrets(t *testing.T) {
	f := newStorageJournalFixture(t)
	j := f.open(t)
	fp := strings.Repeat("a", 64)
	h, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: fp, PolicyFingerprint: fp, PlanVersion: 1, Plan: json.RawMessage(`{"secret":"DO-NOT-PRINT-PLAN"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	before := journalFixtureSnapshot(t, f.directory)
	if code, out := f.run("audit"); code == 0 || strings.Contains(out, "DO-NOT-PRINT-PLAN") || !strings.Contains(out, `"SHA256"`) {
		t.Fatalf("unsafe malformed audit %d: %s", code, out)
	}
	if !reflect.DeepEqual(before, journalFixtureSnapshot(t, f.directory)) {
		t.Fatal("malformed audit modified evidence")
	}
}
func TestStorageJournalCorruptIndexAndEmptyClusterReenrollment(t *testing.T) {
	f := newStorageJournalFixture(t)
	path := journalFixtureFile(t, f.directory, "index.json")
	if err := os.WriteFile(path, []byte("corrupt-index-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run("audit"); code == 0 || strings.Contains(out, "corrupt-index-secret") {
		t.Fatalf("invalid audit %d: %s", code, out)
	}
	if code, out := f.run("recover-index", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"); code != 0 {
		t.Fatalf("corrupt index repair failed %d: %s", code, out)
	}
	prior, err := aj.InspectEnrollment(f.directory, "director")
	if err != nil {
		t.Fatal(err)
	}
	f.fingerprint.Store(true)
	if code, out := f.run("recover-authority", "--authority-id", "replacement", "--previous-writer-fenced"); code != 0 {
		t.Fatalf("empty authority re-enrollment failed %d: %s", code, out)
	}
	after, err := aj.InspectEnrollment(f.directory, "director")
	if err != nil || after.Enrollment.ClusterID == prior.Enrollment.ClusterID {
		t.Fatalf("cluster not re-enrolled %+v %v", after, err)
	}
}
func TestStorageJournalResolveMissingRejectsUUIDAndAgentProvenance(t *testing.T) {
	f := newStorageJournalFixture(t)
	cfg, err := config.LoadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	j := f.open(t)
	defer func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	}()
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	agentHash := sha256.Sum256([]byte("agent"))
	for _, e := range []handlers.StorageAllocationEvidence{{AllocationID: id}, {AllocationID: "other", AgentSHA256: hex.EncodeToString(agentHash[:])}} {
		var out, stderr bytes.Buffer
		now := time.Now()
		report := handlers.StorageAllocationAudit{StartedAt: now, CompletedAt: now, Complete: true, VMScanComplete: true, Evidence: []handlers.StorageAllocationEvidence{e}}
		code := resolveStorageJournalMissingVM(t.Context(), cfg, j, "cluster", "cluster", report, storageJournalMissingGeneration{AgentID: "agent", AllocationID: id, IndexFirstConfirmed: true}, "writer", true, &out, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "still has allocation or agent provenance") {
			t.Fatalf("provenance ignored %d %s", code, &stderr)
		}
	}
}
func TestStorageJournalDecisionsRequireReferencesAndClusterContinuity(t *testing.T) {
	f := newStorageJournalFixture(t)
	_, id := journalFixtureVM(t, f)
	if code, out := f.run("adopt", "--allocation-id", id, "--decision-id", "audit-ticket"); code != 2 {
		t.Fatalf("adopt accepted missing CID %d %s", code, out)
	}
	f.fingerprint.Store(true)
	if code, out := f.run("finalize-cleanup", "--allocation-id", id, "--decision-id", "audit-ticket"); code == 0 || !strings.Contains(out, "verified cluster continuity") {
		t.Fatalf("cross-cluster decision admitted %d %s", code, out)
	}
	f.fingerprint.Store(false)
	if code, out := f.run("finalize-cleanup", "--allocation-id", id, "--decision-id", "audit-ticket"); code != 0 {
		t.Fatalf("settled absent generation cannot be finalized %d %s", code, out)
	}
	j := f.open(t)
	record, err := j.Inspect(id)
	if err != nil || record.State != aj.Cleaned {
		t.Fatalf("decision not persisted %+v %v", record, err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStorageJournalRequiresSettledRemoteTasksBeforeInitializationOrIndexRepair(t *testing.T) {
	for _, action := range []string{"initialize", "recover-index"} {
		var out, stderr bytes.Buffer
		code := runStorageJournal([]string{action, "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced"}, &out, &stderr, runOptions{})
		if code != 2 || !strings.Contains(stderr.String(), "remote-tasks-settled") {
			t.Fatalf("%s accepted absent task-settlement proof: %d %s", action, code, &stderr)
		}
	}
}

func TestStorageJournalHelpExplainsIndependentRecoveryProof(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := runStorageJournal([]string{"resolve-missing-vm", "--help"}, &out, &stderr, runOptions{}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(stderr.String(), "missing file alone is insufficient") || !strings.Contains(stderr.String(), "unknown-response submissions") {
		t.Fatal("missing recovery attestation meaning")
	}
}

func TestStorageJournalExplicitCleanupRequiresDecisionAndClosesUnsubmittedGeneration(t *testing.T) {
	f := newStorageJournalFixture(t)
	_, id := journalFixtureVM(t, f)
	before := journalFixtureSnapshot(t, f.directory)
	if code, out := f.run("cleanup", "--allocation-id", id); code != 2 {
		t.Fatalf("missing decision accepted %d %s", code, out)
	}
	if !reflect.DeepEqual(before, journalFixtureSnapshot(t, f.directory)) {
		t.Fatal("flag refusal changed evidence")
	}
	f.fingerprint.Store(true)
	if code, out := f.run("cleanup", "--allocation-id", id, "--decision-id", "operator-cleanup"); code == 0 || !strings.Contains(out, "verified cluster continuity") {
		t.Fatalf("wrong cluster cleanup %d %s", code, out)
	}
	f.fingerprint.Store(false)
	if code, out := f.run("cleanup", "--allocation-id", id, "--decision-id", "operator-cleanup"); code != 0 || !strings.Contains(out, `"state":"cleaned"`) {
		t.Fatalf("unsubmitted cleanup %d %s", code, out)
	}
	j := f.open(t)
	defer func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	}()
	record, err := j.Inspect(id)
	if err != nil || record.State != aj.Cleaned {
		t.Fatalf("cleanup record %+v %v", record, err)
	}
	if len(record.Verifications) != 1 || !strings.Contains(record.Verifications[0].EvidenceJSON, "operator-cleanup") {
		t.Fatal("cleanup decision evidence missing")
	}
}

func TestStorageJournalPendingCleanupRequiresFencingBeforeIO(t *testing.T) {
	for _, extra := range [][]string{nil, {"--authority-id", "writer"}, {"--previous-writer-fenced"}} {
		args := make([]string, 0, 8+len(extra))
		args = append(args, "cleanup", "--config", "/unopened", "--allocation-id", "allocation", "--decision-id", "incident", "--remote-tasks-settled")
		args = append(args, extra...)
		var out, stderr bytes.Buffer
		if code := runStorageJournal(args, &out, &stderr, runOptions{}); code != 2 {
			t.Fatalf("invalid settlement flags reached IO: %d %s", code, stderr.String())
		}
	}
}

// storageJournalAuditOutput mirrors the audit command's JSON shape from the
// consumer's side, decoding loosely the way a real consumer would.
type storageJournalAuditOutput struct {
	Records []struct {
		ID, Kind, State, CID, SHA256 string
		CreatedAt, UpdatedAt         time.Time
		Charging                     bool `json:"charging"`
	} `json:"records"`
	ChargingSummary struct {
		Count     int    `json:"count"`
		OldestID  string `json:"oldest_id"`
		OldestAge string `json:"oldest_age"`
	} `json:"charging_summary"`
	IndexHealthy      bool `json:"generation_index_healthy"`
	ClusterContinuity bool `json:"cluster_continuity"`
}

// storageJournalAuditTestRecord builds a minimal record with no journal and no
// encoded plan, which is enough for writeStorageJournalAudit: it never decodes
// the plan, it only reads state and the two timestamps.
func storageJournalAuditTestRecord(id string, state aj.State, created, updated time.Time) aj.Record {
	return aj.Record{
		Version: aj.Version, ID: id, Namespace: "director", Kind: "vm",
		State: state, CreatedAt: created, UpdatedAt: updated,
	}
}

func decodeStorageJournalAuditOutput(t *testing.T, raw []byte) storageJournalAuditOutput {
	t.Helper()
	var output storageJournalAuditOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("decode audit output: %v %s", err, raw)
	}
	return output
}

func TestStorageJournalAuditRecordSummaryChargingMatchesPredicateForEveryState(t *testing.T) {
	// One table drives both the command's per-record field and the exported
	// predicate the sibling reader also calls, so the two views cannot drift.
	states := []aj.State{
		aj.Planned, aj.Submitted, aj.Observed, aj.ReconciliationRequired,
		aj.ReadyToReturn, aj.Adopted, aj.VMDeletedRetained, aj.Deleted, aj.Cleaned,
	}
	created := time.Now().UTC().Add(-time.Hour)
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			report := handlers.StorageAllocationAudit{
				Complete: true, VMScanComplete: true,
				Records: []aj.Record{storageJournalAuditTestRecord("record-"+string(state), state, created, created)},
			}
			var out, stderr bytes.Buffer
			writeStorageJournalAudit(&out, &stderr, report, nil, true)
			output := decodeStorageJournalAuditOutput(t, out.Bytes())
			if len(output.Records) != 1 {
				t.Fatalf("expected one record summary, got %d", len(output.Records))
			}
			want := handlers.StorageAllocationCharging(state)
			if output.Records[0].Charging != want {
				t.Fatalf("state %s: summary charging %v, predicate %v", state, output.Records[0].Charging, want)
			}
		})
	}
}

func TestStorageJournalAuditPlannedRecordChargesWithTimestamps(t *testing.T) {
	created := time.Now().UTC().Add(-3 * time.Minute)
	updated := time.Now().UTC().Add(-time.Minute)
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{storageJournalAuditTestRecord("planned-1", aj.Planned, created, updated)},
	}
	var out, stderr bytes.Buffer
	writeStorageJournalAudit(&out, &stderr, report, nil, true)
	output := decodeStorageJournalAuditOutput(t, out.Bytes())
	if len(output.Records) != 1 {
		t.Fatalf("expected one record summary, got %d", len(output.Records))
	}
	record := output.Records[0]
	if !record.Charging {
		t.Fatal("planned record must report charging true")
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		t.Fatalf("planned record missing timestamps: %+v", record)
	}
	if !record.CreatedAt.Equal(created) || !record.UpdatedAt.Equal(updated) {
		t.Fatalf("timestamps not carried through: got created=%v updated=%v, want created=%v updated=%v",
			record.CreatedAt, record.UpdatedAt, created, updated)
	}
}

func TestStorageJournalAuditReadyToReturnRecordDoesNotCharge(t *testing.T) {
	created := time.Now().UTC().Add(-24 * time.Hour)
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{storageJournalAuditTestRecord("returned-1", aj.ReadyToReturn, created, created)},
	}
	var out, stderr bytes.Buffer
	writeStorageJournalAudit(&out, &stderr, report, nil, true)
	output := decodeStorageJournalAuditOutput(t, out.Bytes())
	if len(output.Records) != 1 || output.Records[0].Charging {
		t.Fatalf("ready_to_return record must not charge: %+v", output.Records)
	}
	if output.ChargingSummary.Count != 0 {
		t.Fatalf("charging summary must be empty when nothing charges: %+v", output.ChargingSummary)
	}
}

func TestStorageJournalAuditTerminalRecordsDoNotCharge(t *testing.T) {
	created := time.Now().UTC().Add(-24 * time.Hour)
	for _, state := range []aj.State{aj.Deleted, aj.Cleaned} {
		t.Run(string(state), func(t *testing.T) {
			report := handlers.StorageAllocationAudit{
				Complete: true, VMScanComplete: true,
				Records: []aj.Record{storageJournalAuditTestRecord("terminal-"+string(state), state, created, created)},
			}
			var out, stderr bytes.Buffer
			writeStorageJournalAudit(&out, &stderr, report, nil, true)
			output := decodeStorageJournalAuditOutput(t, out.Bytes())
			if len(output.Records) != 1 || output.Records[0].Charging {
				t.Fatalf("%s record must not charge: %+v", state, output.Records)
			}
		})
	}
}

func TestStorageJournalAuditChargingSummaryCountsAndNamesOldest(t *testing.T) {
	now := time.Now().UTC()
	oldest := now.Add(-3 * time.Hour)
	newer := now.Add(-time.Minute)
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{
			storageJournalAuditTestRecord("newer-planned", aj.Planned, newer, newer),
			storageJournalAuditTestRecord("oldest-submitted", aj.Submitted, oldest, oldest),
			storageJournalAuditTestRecord("resting", aj.ReadyToReturn, oldest, oldest),
		},
	}
	var out, stderr bytes.Buffer
	writeStorageJournalAudit(&out, &stderr, report, nil, true)
	output := decodeStorageJournalAuditOutput(t, out.Bytes())
	if output.ChargingSummary.Count != 2 {
		t.Fatalf("expected 2 charging records, got %d: %+v", output.ChargingSummary.Count, output.ChargingSummary)
	}
	if output.ChargingSummary.OldestID != "oldest-submitted" {
		t.Fatalf("expected oldest-submitted to be named oldest, got %q", output.ChargingSummary.OldestID)
	}
	age, err := time.ParseDuration(output.ChargingSummary.OldestAge)
	if err != nil {
		t.Fatalf("oldest age %q did not parse as a duration: %v", output.ChargingSummary.OldestAge, err)
	}
	// The command computes age against its own call to time.Now(), a few
	// milliseconds after "now" above, so allow a little slack on both sides
	// rather than asserting exact equality against a wall clock we do not control.
	if age < 3*time.Hour-time.Second || age > 3*time.Hour+5*time.Second {
		t.Fatalf("oldest age %s not within tolerance of 3h", age)
	}
}
