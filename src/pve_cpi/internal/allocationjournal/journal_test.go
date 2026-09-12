package allocationjournal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func enrollment() Enrollment {
	return Enrollment{ClusterID: "cluster-identity", AuthorityID: "director-authority", AuditID: "initial-full-scan", CompleteHistoricalAudit: true, PreviousWriterFenced: true}
}
func intent() Intent {
	return Intent{IntentFingerprint: strings.Repeat("a", 64), PolicyFingerprint: strings.Repeat("b", 64), PlanVersion: 1, Plan: json.RawMessage(`{"storage":"nfs-a","seed":"fixed"}`)}
}
func fixture(t *testing.T) (*Journal, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := Initialize(context.Background(), dir, "namespace", enrollment())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	return j, dir
}
func closeHandle(t *testing.T, h *Handle) {
	t.Helper()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}
func acquireVM(t *testing.T, j *Journal) *Handle {
	t.Helper()
	h, err := j.AcquireVM(context.Background(), "agent", intent())
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func step() Step {
	return Step{ID: "root", Kind: "clone", Target: Target{Node: "node-a", Storage: "nfs-a", Backing: "nfs:server/export", VMID: 101, IntendedVolume: "vm-101-disk-0"}, State: Planned, Charges: []Charge{{Backing: "nfs:server/export", PlannedBytes: 100, OutstandingBytes: 100}}}
}
func save(t *testing.T, h *Handle, r Record) {
	t.Helper()
	if err := h.Save(r); err != nil {
		t.Fatal(err)
	}
}
func observed(t *testing.T, h *Handle) {
	t.Helper()
	r := h.Record()
	r.Steps = []Step{step()}
	save(t, h, r)
	r = h.Record()
	r.State = Submitted
	r.Steps[0].State = Submitted
	r.Steps[0].UPID = "UPID:node-a:clone"
	save(t, h, r)
	r = h.Record()
	r.State = Observed
	r.Steps[0].State = Observed
	r.Steps[0].VolIDs = []string{"nfs-a:101/vm-101-disk-0.qcow2"}
	r.Steps[0].Charges[0].AcquiredBytes = 100
	r.Steps[0].Charges[0].OutstandingBytes = 0
	save(t, h, r)
}
func terminalize(t *testing.T, h *Handle, state State) {
	t.Helper()
	r := h.Record()
	r.State = state
	r.Verifications = append(r.Verifications, Verification{EvidenceID: "verified-complete-cleanup", Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true})
	save(t, h, r)
}

func TestUUIDAndToken(t *testing.T) {
	ids := map[string]bool{}
	for range 1000 {
		id, err := NewAllocationID()
		if err != nil {
			t.Fatal(err)
		}
		if !allocationIDPattern.MatchString(id) || ids[id] {
			t.Fatalf("invalid/duplicate UUID %q", id)
		}
		ids[id] = true
		token, err := DiskCorrelationToken(id)
		if err != nil || len(token) != 20 || !strings.HasPrefix(token, "bpd-") {
			t.Fatalf("token %q: %v", token, err)
		}
	}
	token, err := DiskCorrelationToken("00112233-4455-4677-8899-aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	// Golden derived independently from the domain-separated SHA-256 contract.
	if token != "bpd-39f73999cb3fd4f1" {
		t.Fatalf("token golden: %s", token)
	}
	if _, err = DiskCorrelationToken("../../etc/passwd"); err == nil {
		t.Fatal("accepted invalid ID")
	}
}
func TestVMGenerationRecoveryAndPolicyIsolation(t *testing.T) {
	j, dir := fixture(t)
	h := acquireVM(t, j)
	original := h.Record()
	closeHandle(t, h)
	reopened, err := Open(dir, "namespace", "cluster-identity")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	changed := intent()
	changed.PolicyFingerprint = strings.Repeat("c", 64)
	changed.Plan = nil
	h, err = reopened.AcquireVM(context.Background(), "agent", changed)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Resumed || h.Record().ID != original.ID || h.Record().Intent.PolicyFingerprint != original.Intent.PolicyFingerprint {
		t.Fatal("policy edit changed generation")
	}
	snapshot := h.Record()
	snapshot.Intent.Plan[0] = '!'
	if h.Record().Intent.Plan[0] != '{' {
		t.Fatal("mutable snapshot leaked")
	}
	closeHandle(t, h)
	changed.IntentFingerprint = strings.Repeat("d", 64)
	if _, err = reopened.AcquireVM(context.Background(), "agent", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed caller intent: %v", err)
	}
	h, err = reopened.Acquire(context.Background(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	terminalize(t, h, Deleted)
	closeHandle(t, h)
	h, err = reopened.AcquireVM(context.Background(), "agent", intent())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	if h.Record().ID == original.ID {
		t.Fatal("deleted generation reused")
	}
	old, err := reopened.Inspect(original.ID)
	if err != nil || old.State != Deleted {
		t.Fatalf("tombstone missing: %v", err)
	}
}
func TestDiskIdentityBeforePlanAndNoDedup(t *testing.T) {
	j, _ := fixture(t)
	a, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.CreateDisk(context.Background(), a, intent())
	if err != nil {
		t.Fatal(err)
	}
	closeHandle(t, h)
	h, err = j.CreateDisk(context.Background(), b, intent())
	if err != nil {
		t.Fatal(err)
	}
	closeHandle(t, h)
	if _, err = j.CreateDisk(context.Background(), a, intent()); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate UUID: %v", err)
	}
	records, err := j.List()
	if err != nil || len(records) != 2 {
		t.Fatalf("independent disks: %v %d", err, len(records))
	}
	if records[0].DiskToken == records[1].DiskToken {
		t.Fatal("same token")
	}
}
func TestMultiStepEvidenceAndTransitions(t *testing.T) {
	j, _ := fixture(t)
	h := acquireVM(t, j)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	observed(t, h)
	r := h.Record()
	r.State = Planned
	r.Steps = append(r.Steps, Step{ID: "iso", Kind: "upload", Target: Target{Node: "node-a", Storage: "iso", IntendedVolume: "config.iso"}, State: Planned})
	save(t, h, r)
	r = h.Record()
	r.State = ReconciliationRequired
	r.Reason = "process crashed before recording UPID"
	save(t, h, r)
	r = h.Record()
	r.State = Observed
	r.Steps[1].State = Observed
	r.Steps[1].VolIDs = []string{"iso:iso/config.iso"}
	if err := h.Save(r); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("resume without audit: %v", err)
	}
	r.Verifications = append(r.Verifications, Verification{EvidenceID: "complete-upid-and-resource-scan", Complete: true, OwnershipVerified: true})
	save(t, h, r)
	r = h.Record()
	r.State = ReadyToReturn
	r.CID = "node-a/101"
	save(t, h, r)
	if len(h.Record().Steps[0].VolIDs) != 1 || h.Record().Steps[0].UPID == "" {
		t.Fatal("earlier acquired resource erased")
	}
	r = h.Record()
	r.State = Adopted
	r.Verifications = append(r.Verifications, Verification{EvidenceID: "director-adoption", Complete: true, OwnershipVerified: true})
	save(t, h, r)
	r = h.Record()
	r.Intent.PolicyFingerprint = strings.Repeat("e", 64)
	if err := h.Save(r); !errors.Is(err, ErrConflict) {
		t.Fatalf("policy mutation: %v", err)
	}
	r = h.Record()
	r.Steps = nil
	if err := h.Save(r); !errors.Is(err, ErrConflict) {
		t.Fatalf("resource erase: %v", err)
	}
	terminalize(t, h, Deleted)
}
func TestDurabilityFailuresPoisonHandleAndPreserveEvidence(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*fileOps)
	}{
		{"short-write", func(o *fileOps) { o.write = func(f *os.File, b []byte) (int, error) { return f.Write(b[:len(b)/2]) } }},
		{"full", func(o *fileOps) { o.write = func(*os.File, []byte) (int, error) { return 0, syscall.ENOSPC } }},
		{"file-fsync", func(o *fileOps) { o.syncFile = func(*os.File) error { return syscall.EIO } }},
		{"rename", func(o *fileOps) { o.rename = func(*os.Root, string, string) error { return syscall.EIO } }},
		{"directory-fsync", func(o *fileOps) { o.syncDir = func(*os.Root) error { return syscall.EIO } }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j, _ := fixture(t)
			h := acquireVM(t, j)
			defer func() {
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			}()
			before := h.Record()
			tt.inject(&j.ops)
			r := h.Record()
			r.Steps = []Step{step()}
			err := h.Save(r)
			var durable *DurabilityError
			if !errors.As(err, &durable) {
				t.Fatalf("expected durability error: %v", err)
			}
			if err = h.Save(r); !errors.Is(err, ErrReconciliationRequired) {
				t.Fatalf("failed handle accepted another write: %v", err)
			}
			persisted, err := j.Inspect(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.ID != before.ID {
				t.Fatal("lost identity")
			}
			want := 0
			if tt.name == "directory-fsync" {
				want = 1
			}
			if len(persisted.Steps) != want {
				t.Fatalf("atomic replacement left %d steps want %d", len(persisted.Steps), want)
			}
		})
	}
}
func TestUnsafePathsCorruptionAndReadOnlyInspection(t *testing.T) {
	j, dir := fixture(t)
	h := acquireVM(t, j)
	id := h.Record().ID
	closeHandle(t, h)
	before, err := os.ReadDir(filepath.Join(dir, pathKey("namespace")))
	if err != nil {
		t.Fatal(err)
	}
	status, err := InspectEnrollment(dir, "namespace")
	if err != nil || status.Enrollment.AuthorityID != "director-authority" {
		t.Fatalf("inspect: %v", err)
	}
	if _, err = j.List(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(filepath.Join(dir, pathKey("namespace")))
	if err != nil || len(before) != len(after) {
		t.Fatal("inspection wrote files")
	}
	for _, ns := range []string{"../escape", "..", "/absolute", "", " spaced "} {
		if _, err = Open(dir, ns, "cluster-identity"); err == nil {
			t.Fatalf("accepted namespace %q", ns)
		}
	}
	if _, err = Open(dir, "namespace", "other-cluster"); !errors.Is(err, ErrAuthority) {
		t.Fatalf("cluster switch: %v", err)
	}
	recordPath := filepath.Join(dir, pathKey("namespace"), recordName(id))
	if err = os.WriteFile(recordPath, []byte(`{"version":900}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = j.Inspect(id); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corruption: %v", err)
	}
	if _, err = j.List(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("partial scan masqueraded as success: %v", err)
	}
	if err = os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(dir, "outside"), recordPath); err != nil {
		t.Fatal(err)
	}
	if _, err = j.Inspect(id); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err = Initialize(context.Background(), dir, "namespace", enrollment()); err == nil {
		t.Fatal("existing authority initialized again")
	}
}
func TestAuthorityRecoveryAndStaleProvenance(t *testing.T) {
	j, dir := fixture(t)
	h := acquireVM(t, j)
	id := h.Record().ID
	e := enrollment()
	e.AuthorityID = "replacement"
	e.AuditID = "restored-backup-audit"
	e.ProvenanceIDs = []string{id}
	if err := j.RecoverAuthority(context.Background(), e); !errors.Is(err, ErrAuthority) {
		t.Fatalf("active writer accepted: %v", err)
	}
	closeHandle(t, h)
	missing, _ := NewAllocationID()
	if err := j.ValidateProvenance([]string{missing}, true); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("stale backup: %v", err)
	}
	if err := j.ValidateProvenance([]string{id}, false); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("partial audit: %v", err)
	}
	switched := e
	switched.ClusterID = "other-cluster"
	if err := j.RecoverAuthority(context.Background(), switched); !errors.Is(err, ErrAuthority) {
		t.Fatalf("live cluster rebind: %v", err)
	}
	if err := j.RecoverAuthority(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Acquire(context.Background(), id); !errors.Is(err, ErrAuthority) {
		t.Fatalf("old local authority not fenced: %v", err)
	}
	reopened, err := Open(dir, "namespace", "cluster-identity")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	h, err = reopened.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	terminalize(t, h, Cleaned)
	closeHandle(t, h)
	if err = reopened.RecoverAuthority(context.Background(), switched); err != nil {
		t.Fatal(err)
	}
}

// TestJournalChild uses the test binary as a real independent CPI-like process.
// Its synchronization is stdout/stdin, not scheduler sleeps.
func TestJournalChild(t *testing.T) {
	mode := os.Getenv("ALLOCATION_JOURNAL_CHILD")
	if mode == "" {
		return
	}
	j, err := Open(os.Getenv("ALLOCATION_JOURNAL_DIRECTORY"), "namespace", "cluster-identity")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	}()
	if mode == "conflicting-intent" {
		if _, err = os.Stdout.WriteString("attempt\n"); err != nil {
			t.Fatal(err)
		}
		changed := intent()
		changed.IntentFingerprint = strings.Repeat("d", 64)
		h, err := j.AcquireVM(context.Background(), "agent", changed)
		if h != nil {
			closeHandle(t, h)
		}
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected different intent conflict: %v", err)
		}
		if _, err = os.Stdout.WriteString("conflict\n"); err != nil {
			t.Fatal(err)
		}
		return
	}
	h, err := j.AcquireVM(context.Background(), "agent", intent())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	if mode == "external-before-upid" {
		r := h.Record()
		r.Steps = []Step{step()}
		save(t, h, r)
		// A fake external resource store, deliberately separate from the journal.
		// The child dies after this side effect but before saving any returned UPID.
		resource := os.Getenv("ALLOCATION_JOURNAL_EXTERNAL_EVIDENCE")
		if err = os.WriteFile(resource, []byte(r.ID+" nfs-a:101/vm-101-disk-0.qcow2"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "submitted" {
		r := h.Record()
		r.Steps = []Step{step()}
		save(t, h, r)
		r = h.Record()
		r.State = Submitted
		r.Steps[0].State = Submitted
		r.Steps[0].UPID = "UPID:node:child"
		save(t, h, r)
	}
	if mode == "ready" {
		observed(t, h)
		r := h.Record()
		r.State = ReadyToReturn
		r.CID = "node-a/101"
		save(t, h, r)
	}
	if _, err = os.Stdout.WriteString(h.Record().ID + "\n"); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err = os.Stdin.Read(b[:]); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

//nolint:gocognit // Keep the complete failure/recovery scenario and its evidence assertions together.
func TestSeparateProcessLockDeathAndRecovery(t *testing.T) {
	for _, mode := range []string{"planned", "submitted", "ready", "external-before-upid"} {
		t.Run(mode, func(t *testing.T) {
			j, dir := fixture(t)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestJournalChild$")
			cmd.Env = append(os.Environ(), "ALLOCATION_JOURNAL_CHILD="+mode, "ALLOCATION_JOURNAL_DIRECTORY="+dir, "ALLOCATION_JOURNAL_EXTERNAL_EVIDENCE="+filepath.Join(dir, "external-owned-resource"))
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = stdin.Close()
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			data := make([]byte, 37)
			if _, err = io.ReadFull(stdout, data); err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSpace(string(data))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err = j.AcquireVM(ctx, "agent", intent()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("parallel process entered locked allocation: %v", err)
			}
			// Index is not held while waiting for the allocation: an unrelated create works.
			other, err := j.AcquireVM(context.Background(), "other-agent", intent())
			if err != nil {
				t.Fatal(err)
			}
			closeHandle(t, other)
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("killed child unexpectedly succeeded")
			}
			h := acquireVM(t, j)
			defer func() {
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			}()
			if h.Record().ID != id || !h.Resumed {
				t.Fatal("process death lost active generation")
			}
			want := Planned
			if mode == "submitted" {
				want = Submitted
			}
			if mode == "ready" {
				want = ReadyToReturn
			}
			if h.Record().State != want {
				t.Fatalf("remaining state %s want %s", h.Record().State, want)
			}
			if mode == "ready" && h.Record().CID != "node-a/101" {
				t.Fatal("return CID lost")
			}
			if mode == "external-before-upid" {
				evidence, err := os.ReadFile(filepath.Join(dir, "external-owned-resource"))
				if err != nil || !strings.Contains(string(evidence), id) {
					t.Fatalf("external resource missing: %v", err)
				}
				r := h.Record()
				if len(r.Steps) != 1 || r.Steps[0].UPID != "" {
					t.Fatal("unexpected journal submission evidence")
				}
				unverified := h.Record()
				unverified.State = Submitted
				unverified.Steps[0].State = Submitted
				unverified.Steps[0].UPID = "UPID:unverified-retry"
				if err = h.Save(unverified); !errors.Is(err, ErrReconciliationRequired) {
					t.Fatalf("planned reopen permitted unverified submission: %v", err)
				}
				r.State = ReconciliationRequired
				r.Reason = "external allocation exists but submitted UPID was not persisted"
				save(t, h, r)
				changed := h.Record()
				changed.Intent.Plan = json.RawMessage(`{"storage":"alternative"}`)
				if err = h.Save(changed); !errors.Is(err, ErrConflict) {
					t.Fatalf("unknown outcome allowed alternative target: %v", err)
				}
				closeHandle(t, h)
				again := acquireVM(t, j)
				defer func() {
					if err := again.Close(); err != nil {
						t.Error(err)
					}
				}()
				if again.Record().ID != id || again.Record().State != ReconciliationRequired {
					t.Fatal("uncertain allocation replaced")
				}
			}
		})
	}
}

func TestIndexFirstCrashBlocksAndRequiresVerifiedRecovery(t *testing.T) {
	j, _ := fixture(t)
	calls := 0
	write := j.ops.write
	j.ops.write = func(f *os.File, b []byte) (int, error) {
		calls++
		if calls == 2 {
			return 0, syscall.ENOSPC
		}
		return write(f, b)
	}
	if _, err := j.AcquireVM(context.Background(), "agent", intent()); err == nil {
		t.Fatal("record failure ignored")
	}
	j.ops = defaultFileOps()
	if _, err := j.AcquireVM(context.Background(), "agent", intent()); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("missing indexed generation treated as fresh: %v", err)
	}
	idx, err := j.readIndex()
	if err != nil {
		t.Fatal(err)
	}
	id := idx.ActiveVMs[pathKey("agent")]
	if err = j.ResolveMissingVMGeneration(context.Background(), "agent", id, intent(), Verification{}); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("absence not required: %v", err)
	}
	proof := Verification{EvidenceID: "full-historical-absence", Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}
	if err = j.ResolveMissingVMGeneration(context.Background(), "agent", id, intent(), proof); err != nil {
		t.Fatal(err)
	}
	h := acquireVM(t, j)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	if h.Record().ID == id {
		t.Fatal("new generation reused cleaned ID")
	}
	old, err := j.Inspect(id)
	if err != nil || old.State != Cleaned {
		t.Fatalf("tombstone: %v", err)
	}
}
func TestRecordValidationRejectsUnsafeProgress(t *testing.T) {
	j, _ := fixture(t)
	h := acquireVM(t, j)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	r := h.Record()
	s := step()
	s.State = Observed
	r.Steps = []Step{s}
	r.State = Observed
	if err := h.Save(r); err == nil {
		t.Fatal("allowed mutation evidence before durable step intent")
	}
	r = h.Record()
	r.Steps = []Step{step()}
	r.Steps[0].Charges[0].AcquiredBytes = 1
	if err := h.Save(r); err == nil {
		t.Fatal("allowed inconsistent ledger")
	}
	observed(t, h)
	r = h.Record()
	r.Steps[0].Charges = nil
	if err := h.Save(r); !errors.Is(err, ErrConflict) {
		t.Fatalf("acquired charges erased: %v", err)
	}
	r = h.Record()
	r.Steps[0].Charges[0].AcquiredBytes = 0
	r.Steps[0].Charges[0].OutstandingBytes = 100
	if err := h.Save(r); !errors.Is(err, ErrConflict) {
		t.Fatalf("acquisition regressed: %v", err)
	}
	r = h.Record()
	r.State = ReadyToReturn
	r.CID = strings.Repeat("x", 256)
	if err := h.Save(r); err == nil {
		t.Fatal("oversized CID accepted")
	}
	token := "bpd-0000000000000000"
	if err := validateTokenUnique(token, []Record{{DiskToken: token, State: Deleted}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("historical token collision ignored: %v", err)
	}
}
func TestUnknownVersionAndPrivatePermissions(t *testing.T) {
	j, dir := fixture(t)
	h := acquireVM(t, j)
	r := h.Record()
	closeHandle(t, h)
	r.Version = 2
	if err := atomicJSON(j.root, recordName(r.ID), r, j.ops); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Inspect(r.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unknown record version accepted: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, pathKey("namespace"), recordName(r.ID)), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Inspect(r.ID); err == nil {
		t.Fatal("world-readable evidence accepted")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "namespace", "cluster-identity"); err == nil {
		t.Fatal("unsafe root accepted")
	}
}
func TestCreateDeleteAndCleanupRecreateAcrossProcesses(t *testing.T) {
	j, dir := fixture(t)
	h := acquireVM(t, j)
	first := h.Record().ID
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestJournalChild$")
	cmd.Env = append(os.Environ(), "ALLOCATION_JOURNAL_CHILD=planned", "ALLOCATION_JOURNAL_DIRECTORY="+dir)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	terminalize(t, h, Cleaned)
	closeHandle(t, h)
	data := make([]byte, 37)
	if _, err = io.ReadFull(output, data); err != nil {
		t.Fatal(err)
	}
	next := strings.TrimSpace(string(data))
	if next == first {
		t.Fatal("child reused cleaned generation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err = j.Acquire(ctx, next); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delete bypassed active create lock: %v", err)
	}
	if err = input.Close(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	h, err = j.Acquire(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	terminalize(t, h, Deleted)
	closeHandle(t, h)
	h = acquireVM(t, j)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	if h.Record().ID == next {
		t.Fatal("recreated deleted generation")
	}
}

func TestChecksummedStaleIndexCannotHideLiveGeneration(t *testing.T) {
	for _, mode := range []string{"empty", "older-terminal"} {
		t.Run(mode, func(t *testing.T) {
			j, _ := fixture(t)
			h := acquireVM(t, j)
			older := h.Record().ID
			terminalize(t, h, Deleted)
			closeHandle(t, h)
			h = acquireVM(t, j)
			live := h.Record().ID
			closeHandle(t, h)
			stale := index{Version: Version, ActiveVMs: map[string]string{}}
			if mode == "older-terminal" {
				stale.ActiveVMs[pathKey("agent")] = older
			}
			if err := atomicJSON(j.root, "index.json", stale, j.ops); err != nil {
				t.Fatal(err)
			}
			if _, err := j.AcquireVM(context.Background(), "agent", intent()); !errors.Is(err, ErrReconciliationRequired) {
				t.Fatalf("stale index admitted create: %v", err)
			}
			e := enrollment()
			e.ProvenanceIDs = []string{live}
			if err := j.RecoverAuthority(context.Background(), e); !errors.Is(err, ErrReconciliationRequired) {
				t.Fatalf("stale index recovered as valid: %v", err)
			}
			records, err := j.List()
			if err != nil || len(records) != 2 {
				t.Fatalf("duplicate generation written: %d %v", len(records), err)
			}
		})
	}
}

func TestDifferentIntentAcrossProcessesConflicts(t *testing.T) {
	j, dir := fixture(t)
	h := acquireVM(t, j)
	id := h.Record().ID
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestJournalChild$")
	cmd.Env = append(os.Environ(), "ALLOCATION_JOURNAL_CHILD=conflicting-intent", "ALLOCATION_JOURNAL_DIRECTORY="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	started := make([]byte, 8)
	if _, err = io.ReadFull(stdout, started); err != nil {
		t.Fatal(err)
	}
	if string(started) != "attempt\n" {
		t.Fatalf("unexpected handshake %q", started)
	}
	closeHandle(t, h)
	result := make([]byte, 9)
	if _, err = io.ReadFull(stdout, result); err != nil {
		t.Fatal(err)
	}
	if string(result) != "conflict\n" {
		t.Fatalf("unexpected result %q", result)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	records, err := j.List()
	if err != nil || len(records) != 1 || records[0].ID != id {
		t.Fatalf("conflict mutated generation: %v", err)
	}
}

func TestExplicitFencedIndexRecovery(t *testing.T) {
	j, dir := fixture(t)
	h := acquireVM(t, j)
	id := h.Record().ID
	closeHandle(t, h)
	if err := os.WriteFile(filepath.Join(dir, pathKey("namespace"), "index.json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	e := enrollment()
	e.ProvenanceIDs = []string{id}
	e.AuditID = "full-historical-index-recovery"
	incomplete := e
	incomplete.CompleteHistoricalAudit = false
	if err := j.RecoverIndex(context.Background(), incomplete); !errors.Is(err, ErrAuthority) {
		t.Fatalf("incomplete index recovery: %v", err)
	}
	if err := j.RecoverIndex(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, "namespace", "cluster-identity")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	h = acquireVM(t, reopened)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	if h.Record().ID != id {
		t.Fatal("index recovery changed active generation")
	}
}
