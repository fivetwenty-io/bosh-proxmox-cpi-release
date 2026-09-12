package allocationjournal

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInspectVMActiveTerminalAndNoMutation(t *testing.T) {
	j, _ := fixture(t)
	before, err := os.ReadDir(j.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if r, found, err := j.InspectVM("absent"); err != nil || found || r.ID != "" {
		t.Fatalf("absent: %v %v", found, err)
	}
	after, err := os.ReadDir(j.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("lookup created files")
	}
	h := acquireVM(t, j)
	defer closeHandle(t, h)
	r, found, err := j.InspectVM("agent")
	if err != nil || !found || r.ID != h.Record().ID {
		t.Fatalf("active handle lookup: %v", err)
	}
	r.Intent.Plan[0] = 'x'
	if !reflect.DeepEqual(h.Record().Intent, intent()) {
		t.Fatal("lookup aliases handle")
	}
	terminalize(t, h, Cleaned)
	if r, found, err := j.InspectVM("agent"); err != nil || found || r.ID != "" {
		t.Fatalf("terminal found: %v %v", found, err)
	}
}

//nolint:gocognit // Keep the complete failure/recovery scenario and its evidence assertions together.
func TestInspectVMFailsClosedOnLostOrStaleEvidence(t *testing.T) {
	for _, mode := range []string{"stale-index", "missing-index", "corrupt-index", "missing-record", "missing-lock", "symlink-lock"} {
		t.Run(mode, func(t *testing.T) {
			j, _ := fixture(t)
			h := acquireVM(t, j)
			id := h.Record().ID
			closeHandle(t, h)
			switch mode {
			case "stale-index":
				if err := atomicJSON(j.root, "index.json", index{Version: Version, ActiveVMs: map[string]string{}}, j.ops); err != nil {
					t.Fatal(err)
				}
			case "missing-index":
				if err := j.root.Remove("index.json"); err != nil {
					t.Fatal(err)
				}
			case "corrupt-index":
				if err := j.root.WriteFile("index.json", []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-record":
				if err := j.root.Remove(recordName(id)); err != nil {
					t.Fatal(err)
				}
			case "missing-lock":
				if err := j.root.Remove("index.lock"); err != nil {
					t.Fatal(err)
				}
			case "symlink-lock":
				if err := j.root.Remove("index.lock"); err != nil {
					t.Fatal(err)
				}
				if err := j.root.Symlink("index.json", "index.lock"); err != nil {
					t.Fatal(err)
				}
			}
			if r, found, err := j.InspectVM("agent"); err == nil || found || r.ID != "" {
				t.Fatalf("unsafe absence/record returned: %v %v", found, err)
			}
			if mode == "missing-lock" {
				if _, err := j.root.Lstat("index.lock"); !os.IsNotExist(err) {
					t.Fatal("read-only lookup recreated lock")
				}
			}
		})
	}
}
func TestInspectVMIndexLockCancellation(t *testing.T) {
	j, _ := fixture(t)
	l, err := waitLock(context.Background(), j.root, "index.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, found, err := j.InspectVMContext(ctx, "agent"); !errors.Is(err, context.DeadlineExceeded) || found {
		t.Fatalf("lock cancellation: %v", err)
	}
}
func TestInspectVMConcurrentCreateDeleteSnapshots(t *testing.T) {
	j, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		<-start
		for i := 0; i < 10; i++ {
			h, err := j.AcquireVM(ctx, "agent", intent())
			if err != nil {
				writerDone <- err
				return
			}
			r := h.Record()
			r.State = Cleaned
			r.Verifications = append(r.Verifications, Verification{EvidenceID: "cleanup", Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true})
			err = errors.Join(h.Save(r), h.Close())
			if err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()
	var wg sync.WaitGroup
	// Finite overlapping reads bound reader load; flock promises exclusion, not
	// fairness under an infinite stream of newly arriving shared lock holders.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for k := 0; k < 12; k++ {
				r, found, err := j.InspectVMContext(ctx, "agent")
				if errors.Is(err, ErrReconciliationRequired) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				if found && (r.AgentID != "agent" || terminal(r.State)) {
					t.Error("invalid active snapshot")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("writer did not complete after readers quiesced")
	}
}
func TestInspectVMRequiresAcquireRecheckAndPreservesActivePlan(t *testing.T) {
	j, _ := fixture(t)
	h := retryVM(t, j)
	if err := h.BeginAttempt(retryPlan(retryIntent()), retryProof("cleanup")); err != nil {
		t.Fatal(err)
	}
	id := h.Record().ID
	closeHandle(t, h)
	snapshot, found, err := j.InspectVM("retry-agent")
	if err != nil || !found || snapshot.ActiveAttempt() != 1 {
		t.Fatal("active plan lookup failed")
	}
	h, err = j.AcquireVM(context.Background(), "retry-agent", Intent{IntentFingerprint: retryIntent().IntentFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if h.Record().ID != id || !reflect.DeepEqual(h.Record().ActivePlan(), snapshot.ActivePlan()) {
		t.Fatal("resume replanned")
	}
	terminalize(t, h, Deleted)
	closeHandle(t, h)
	if _, err := j.AcquireVM(context.Background(), "retry-agent", Intent{IntentFingerprint: retryIntent().IntentFingerprint}); err == nil {
		t.Fatal("terminal race invented a new plan")
	}
	changed := retryIntent()
	changed.IntentFingerprint = strings.Repeat("f", 64)
	h, err = j.AcquireVM(context.Background(), "retry-agent", changed)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, h)
	if h.Record().ID == id {
		t.Fatal("terminal generation reused")
	}
	if r, found, err := j.InspectVM("retry-agent"); err != nil || !found || r.ID != h.Record().ID {
		t.Fatal("replacement generation not visible")
	}
}
