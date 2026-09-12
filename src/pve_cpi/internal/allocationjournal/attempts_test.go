package allocationjournal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func retryIntent() Intent {
	i := intent()
	i.FrozenInputsFingerprint = strings.Repeat("c", 64)
	return i
}
func retryPlan(i Intent) AttemptPlan {
	p := initialPlan(i)
	p.Plan = json.RawMessage(`{"storage":"nfs-b","seed":"new","observation":"refreshed"}`)
	return p
}
func retryProof(id string) AttemptVerification {
	return AttemptVerification{Verification: Verification{EvidenceID: id, Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}, OutcomesKnown: true}
}
func retryVM(t *testing.T, j *Journal) *Handle {
	t.Helper()
	h, err := j.AcquireVM(context.Background(), "retry-agent", retryIntent())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAttemptRetryDiskPreservesIdentityAndHistoricalEvidence(t *testing.T) {
	j, _ := fixture(t)
	id, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.CreateDisk(context.Background(), id, retryIntent())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, h)
	observed(t, h)
	before := h.Record()
	if err := h.BeginAttempt(retryPlan(before.Intent), retryProof("cleanup-first")); err != nil {
		t.Fatal(err)
	}
	after := h.Record()
	if after.ID != before.ID || after.DiskToken != before.DiskToken || !reflect.DeepEqual(after.Intent, before.Intent) || !reflect.DeepEqual(after.Steps, before.Steps) || after.ActiveAttempt() != 1 || after.State != Planned {
		t.Fatalf("lost allocation evidence: %#v", after)
	}
	if string(after.ActivePlan().Plan) == string(before.Intent.Plan) {
		t.Fatal("active plan is original")
	}
	copyPlan := after.ActivePlan()
	copyPlan.Plan[0] = 'x'
	if !json.Valid(h.Record().ActivePlan().Plan) {
		t.Fatal("plan alias")
	}
	copyRecord := h.Record()
	copyRecord.Attempts[0].Completion.EvidenceID = "changed"
	copyRecord.Attempts[1].Plan.Plan[0] = 'x'
	if h.Record().Attempts[0].Completion.EvidenceID != "cleanup-first" || !json.Valid(h.Record().ActivePlan().Plan) {
		t.Fatal("record alias")
	}
	oldStep := h.Record()
	oldStep.Steps[0].VolIDs = append(oldStep.Steps[0].VolIDs, "extra")
	if err := h.Save(oldStep); !errors.Is(err, ErrConflict) {
		t.Fatalf("historical mutation accepted: %v", err)
	}
	planEdit := h.Record()
	planEdit.Attempts[1].Plan = initialPlan(before.Intent)
	if err := h.Save(planEdit); !errors.Is(err, ErrConflict) {
		t.Fatalf("plan rewrite accepted: %v", err)
	}
	wrongStep := h.Record()
	s := step()
	s.ID = "retry-root"
	wrongStep.Steps = append(wrongStep.Steps, s)
	if err := h.Save(wrongStep); err == nil {
		t.Fatal("step bound to closed attempt")
	}
	current := h.Record()
	s.Attempt = current.ActiveAttempt()
	current.Steps = append(current.Steps, s)
	save(t, h, current)
	current = h.Record()
	current.Steps[1].State = Observed
	current.Steps[1].VolIDs = []string{"nfs-b:retry-volume"}
	current.Steps[1].Charges[0].AcquiredBytes = 100
	current.Steps[1].Charges[0].OutstandingBytes = 0
	current.State = Observed
	save(t, h, current)
	current = h.Record()
	current.State = ReadyToReturn
	current.CID = "disk-result"
	save(t, h, current)
	if err := h.BeginAttempt(retryPlan(current.Intent), retryProof("after-ready")); !errors.Is(err, ErrConflict) {
		t.Fatalf("ready retried: %v", err)
	}
}

func TestAttemptCheckpointRestartRetainsActiveVMGeneration(t *testing.T) {
	j, dir := fixture(t)
	h := retryVM(t, j)
	id := h.Record().ID
	r := h.Record()
	r.Steps = []Step{step()}
	save(t, h, r)
	proof := retryProof("cleanup-checkpoint")
	if err := h.CompleteAttempt(proof); err != nil {
		t.Fatal(err)
	}
	checkpoint := h.Record()
	if checkpoint.State != ReconciliationRequired || terminal(checkpoint.State) {
		t.Fatal("allocation terminalized")
	}
	closeHandle(t, h)
	reopened, err := Open(dir, "namespace", enrollment().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	h, err = reopened.AcquireVM(context.Background(), "retry-agent", retryIntent())
	if err != nil {
		t.Fatal(err)
	}
	if !h.Resumed || h.Record().ID != id || h.Record().ActiveAttempt() != 0 || !attemptClosed(h.Record()) {
		t.Fatal("checkpoint restart lost identity")
	}
	if err := h.BeginAttempt(retryPlan(retryIntent()), proof); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("replayed stale proof accepted: %v", err)
	}
	if err := h.BeginAttempt(retryPlan(retryIntent()), retryProof("fresh-post-restart")); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	r.Steps = append(r.Steps, Step{Attempt: 1, ID: "retry", Kind: "clone", Target: step().Target, State: Planned})
	save(t, h, r)
	closeHandle(t, h)
	h, err = reopened.AcquireVM(context.Background(), "retry-agent", Intent{IntentFingerprint: retryIntent().IntentFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, h)
	if h.Record().ActiveAttempt() != 1 || h.Record().ID != id || string(h.Record().ActivePlan().Plan) != string(retryPlan(retryIntent()).Plan) {
		t.Fatal("restart uses wrong plan")
	}
	if err := h.Save(h.Record()); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("resume bypassed external reconciliation: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		other, err := reopened.AcquireVM(ctx, "retry-agent", retryIntent())
		if other != nil {
			err = errors.Join(err, other.Close())
		}
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("concurrent generation escaped: %v", err)
	}
}

func TestAttemptRejectsUnsafeProofAndChangedBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		change func(*AttemptVerification, *AttemptPlan)
	}{
		{"unknown", func(v *AttemptVerification, p *AttemptPlan) { v.OutcomesKnown = false }},
		{"incomplete", func(v *AttemptVerification, p *AttemptPlan) { v.Complete = false }},
		{"retained", func(v *AttemptVerification, p *AttemptPlan) { v.RetainedArtifacts = true }},
		{"present", func(v *AttemptVerification, p *AttemptPlan) { v.AbsenceVerified = false }},
		{"undisposed", func(v *AttemptVerification, p *AttemptPlan) { v.ArtifactDispositionVerified = false }},
		{"policy", func(v *AttemptVerification, p *AttemptPlan) { p.PolicyFingerprint = strings.Repeat("d", 64) }},
		{"membership", func(v *AttemptVerification, p *AttemptPlan) { p.FrozenInputsFingerprint = strings.Repeat("e", 64) }},
		{"attempt-version", func(v *AttemptVerification, p *AttemptPlan) { p.Version = AttemptVersion + 1 }},
		{"plan-json", func(v *AttemptVerification, p *AttemptPlan) { p.Plan = json.RawMessage(`null`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, _ := fixture(t)
			h := retryVM(t, j)
			defer closeHandle(t, h)
			r := h.Record()
			r.Steps = []Step{step()}
			save(t, h, r)
			before := h.Record()
			v, p := retryProof(tc.name), retryPlan(retryIntent())
			tc.change(&v, &p)
			if err := h.BeginAttempt(p, v); err == nil {
				t.Fatal("unsafe retry accepted")
			}
			if !reflect.DeepEqual(before, h.Record()) {
				t.Fatal("failed retry changed record")
			}
		})
	}
}

func TestAttemptNoSubmissionRequiresExternalProofAndNoContradiction(t *testing.T) {
	j, _ := fixture(t)
	h := retryVM(t, j)
	defer closeHandle(t, h)
	r := h.Record()
	r.Steps = []Step{step()}
	save(t, h, r)
	v := retryProof("proven-not-submitted")
	v.OutcomesKnown = false
	v.NoSubmissionVerified = true
	if err := h.BeginAttempt(retryPlan(retryIntent()), v); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	s := step()
	s.ID = "second"
	s.Attempt = 1
	r.Steps = append(r.Steps, s)
	save(t, h, r)
	r = h.Record()
	r.State = Submitted
	r.Steps[1].State = Submitted
	r.Steps[1].UPID = "UPID:second"
	save(t, h, r)
	v.EvidenceID = "contradicted"
	if err := h.BeginAttempt(retryPlan(retryIntent()), v); !errors.Is(err, ErrConflict) {
		t.Fatalf("ignored UPID: %v", err)
	}
}

func TestAttemptLegacyReadAndMissingFrozenBoundaryRejectRetry(t *testing.T) {
	j, _ := fixture(t)
	h := acquireVM(t, j)
	r := h.Record()
	closeHandle(t, h)
	r.Attempts = nil
	if err := atomicJSON(j.root, recordName(r.ID), r, j.ops); err != nil {
		t.Fatal(err)
	}
	h, err := j.Acquire(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, h)
	if h.Record().ActiveAttempt() != 0 || !reflect.DeepEqual(h.Record().ActivePlan(), initialPlan(r.Intent)) {
		t.Fatal("legacy plan unreadable")
	}
	if err := h.BeginAttempt(retryPlan(r.Intent), retryProof("legacy")); !errors.Is(err, ErrConflict) {
		t.Fatalf("invented frozen boundary: %v", err)
	}
}

func TestAttemptCleanupCrashBeforeNewPlanWrite(t *testing.T) {
	j, dir := fixture(t)
	h := retryVM(t, j)
	id := h.Record().ID
	if err := h.CompleteAttempt(retryProof("checkpoint")); err != nil {
		t.Fatal(err)
	}
	originalOps := j.ops
	j.ops.rename = func(*os.Root, string, string) error { return errors.New("simulated new-plan crash before rename") }
	if err := h.BeginAttempt(retryPlan(retryIntent()), retryProof("fresh")); err == nil {
		t.Fatal("fault not propagated")
	}
	closeHandle(t, h)
	j.ops = originalOps
	reopened, err := Open(dir, "namespace", enrollment().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	r, err := reopened.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.ActiveAttempt() != 0 || !attemptClosed(r) {
		t.Fatal("partial new plan visible")
	}
	h, err = reopened.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, h)
	if err := h.BeginAttempt(retryPlan(retryIntent()), retryProof("post-crash")); err != nil {
		t.Fatal(err)
	}
	if h.Record().ID != id || h.Record().ActiveAttempt() != 1 {
		t.Fatal("identity changed")
	}
}

func TestAttemptHistoryCorruptionRejected(t *testing.T) {
	j, _ := fixture(t)
	h := retryVM(t, j)
	if err := h.BeginAttempt(retryPlan(retryIntent()), retryProof("first")); err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	closeHandle(t, h)
	r.Attempts[0].Completion = nil
	if err := atomicJSON(j.root, recordName(r.ID), r, j.ops); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Inspect(r.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing closure accepted: %v", err)
	}
}

// Ensure a retry does not permit a different intent to create a fresh generation.
func TestAttemptDifferentIntentConflictAfterRetry(t *testing.T) {
	j, _ := fixture(t)
	h := retryVM(t, j)
	if err := h.BeginAttempt(retryPlan(retryIntent()), retryProof("retry")); err != nil {
		t.Fatal(err)
	}
	id := h.Record().ID
	closeHandle(t, h)
	other := retryIntent()
	other.IntentFingerprint = strings.Repeat("f", 64)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if h, err := j.AcquireVM(ctx, "retry-agent", other); !errors.Is(err, ErrConflict) {
		if h != nil {
			closeHandle(t, h)
		}
		t.Fatalf("different intent admitted: %v", err)
	}
	r, err := j.Inspect(id)
	if err != nil || r.ActiveAttempt() != 1 {
		t.Fatalf("lost generation: %v", err)
	}
}

func TestAttemptProcessChild(t *testing.T) {
	mode := os.Getenv("JOURNAL_ATTEMPT_CHILD")
	if mode == "" {
		return
	}
	j, err := Open(os.Getenv("JOURNAL_ATTEMPT_DIRECTORY"), "namespace", enrollment().ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	}()
	h := retryVM(t, j)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := h.CompleteAttempt(retryProof("child-cleanup")); err != nil {
		t.Fatal(err)
	}
	if mode == "new-plan" {
		if err := h.BeginAttempt(retryPlan(retryIntent()), retryProof("child-begin")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stdout.WriteString(h.Record().ID + "\n"); err != nil {
		t.Fatal(err)
	}
	var token [1]byte
	if _, err := os.Stdin.Read(token[:]); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

//nolint:gocognit // Keep the complete failure/recovery scenario and its evidence assertions together.
func TestAttemptProcessDeathAtCleanupAndNewPlanCheckpoints(t *testing.T) {
	for _, mode := range []string{"cleanup", "new-plan"} {
		t.Run(mode, func(t *testing.T) {
			j, dir := fixture(t)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAttemptProcessChild$")
			cmd.Env = append(os.Environ(), "JOURNAL_ATTEMPT_CHILD="+mode, "JOURNAL_ATTEMPT_DIRECTORY="+dir)
			cmd.Stderr = os.Stderr
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Error(err)
				}
			}()
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			data := make([]byte, 37)
			if _, err := io.ReadFull(output, data); err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSpace(string(data))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			if other, err := j.AcquireVM(ctx, "retry-agent", retryIntent()); !errors.Is(err, context.DeadlineExceeded) {
				if other != nil {
					closeHandle(t, other)
				}
				t.Fatalf("retry released allocation lock: %v", err)
			}
			cancel()
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("child unexpectedly exited successfully")
			}
			h, err := j.AcquireVM(context.Background(), "retry-agent", retryIntent())
			if err != nil {
				t.Fatal(err)
			}
			defer closeHandle(t, h)
			if !h.Resumed || h.Record().ID != id {
				t.Fatal("new generation after crash")
			}
			if mode == "cleanup" {
				if h.Record().ActiveAttempt() != 0 || !attemptClosed(h.Record()) {
					t.Fatal("cleanup checkpoint lost")
				}
				if err := h.BeginAttempt(retryPlan(retryIntent()), retryProof("parent-fresh-audit")); err != nil {
					t.Fatal(err)
				}
			} else {
				if h.Record().ActiveAttempt() != 1 || attemptClosed(h.Record()) {
					t.Fatal("new plan checkpoint lost")
				}
				if err := h.Save(h.Record()); !errors.Is(err, ErrReconciliationRequired) {
					t.Fatalf("new plan restart escaped reconciliation: %v", err)
				}
			}
		})
	}
}

func TestAttemptReadyIgnoresClosedUnsubmittedHistory(t *testing.T) {
	j, _ := fixture(t)
	h := retryVM(t, j)
	defer closeHandle(t, h)
	r := h.Record()
	r.Steps = []Step{step()}
	save(t, h, r)
	v := retryProof("absence")
	v.OutcomesKnown = false
	v.NoSubmissionVerified = true
	if err := h.BeginAttempt(retryPlan(retryIntent()), v); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	s := step()
	s.Attempt = 1
	s.ID = "new-root"
	r.Steps = append(r.Steps, s)
	save(t, h, r)
	r = h.Record()
	r.State = Observed
	r.Steps[1].State = Observed
	save(t, h, r)
	r = h.Record()
	r.State = ReadyToReturn
	r.CID = "node-a/101"
	save(t, h, r)
	if h.Record().Steps[0].State != Planned {
		t.Fatal("historical step was rewritten")
	}
}
