package handlers

import (
	"context"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"os"
	"reflect"
	"testing"
)

func lifecycleJournalFixture(t *testing.T) (*aj.Journal, *aj.Handle) {
	t.Helper()
	_, plan, intent := runtimePlanFixture(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := aj.Initialize(context.Background(), dir, plan.Namespace, aj.Enrollment{ClusterID: "cluster", AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.CreateDisk(context.Background(), id, intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	step, err := storageMutationIntent(h, "create", aj.Target{Node: "n1", Storage: "a", Backing: "backing", IntendedVolume: "a:100/birth.raw"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(h, step, []string{"a:100/birth.raw"}, false); err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	r.CID = "disk-cid"
	r.State = aj.ReadyToReturn
	if err := h.Save(r); err != nil {
		t.Fatal(err)
	}
	return j, h
}
func lifecycleProof(t *testing.T, id string, deleted bool) aj.Verification {
	t.Helper()
	hash, body, err := aj.VerificationEvidence(map[string]any{"audit": id, "complete": true, "absent": deleted})
	if err != nil {
		t.Fatal(err)
	}
	return aj.Verification{EvidenceID: hash, EvidenceJSON: body, Complete: true, OwnershipVerified: !deleted, AbsenceVerified: deleted, ArtifactDispositionVerified: deleted}
}
func TestStorageLifecycleRetainsAllocationAndTransferHistory(t *testing.T) {
	_, h := lifecycleJournalFixture(t)
	initial := h.Record()
	s, err := beginStorageLifecycle(h, "attach_disk", lifecycleProof(t, "before", false))
	if err != nil {
		t.Fatal(err)
	}
	step, err := s.Intent("move", aj.Target{Node: "n1", Storage: "a", Backing: "backing", IntendedVolume: "a:100/birth.raw"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Submitted(step, "UPID:n1:move"); err != nil {
		t.Fatal(err)
	}
	if err := s.Observed(step, []string{"a:100/birth.raw", "a:200/moved.raw"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(lifecycleProof(t, "after", false), false); err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	if r.ID != initial.ID || r.DiskToken != initial.DiskToken || r.CID != initial.CID || !reflect.DeepEqual(r.Intent, initial.Intent) || !reflect.DeepEqual(r.Steps[0], initial.Steps[0]) {
		t.Fatal("allocation or old evidence changed")
	}
	if r.State != aj.ReadyToReturn || len(r.Steps) != 2 || r.Steps[1].UPID != "UPID:n1:move" || len(r.Steps[1].VolIDs) != 2 {
		t.Fatalf("incomplete lineage: %+v", r)
	}
}
func TestStorageLifecycleUnknownAndRetainedPreventTombstone(t *testing.T) {
	_, h := lifecycleJournalFixture(t)
	s, err := beginStorageLifecycle(h, "delete_disk", lifecycleProof(t, "before", false))
	if err != nil {
		t.Fatal(err)
	}
	step, err := s.Intent("delete", aj.Target{Node: "n1", Backing: "backing", IntendedVolume: "a:100/birth.raw"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(lifecycleProof(t, "unknown", true), true); err == nil {
		t.Fatal("unknown submission became tombstone")
	}
	if err := s.Observed(step, nil); err != nil {
		t.Fatal(err)
	}
	retained := lifecycleProof(t, "retained", true)
	retained.ArtifactDispositionVerified = false
	if err := s.Finish(retained, true); err == nil {
		t.Fatal("retained artifact became tombstone")
	}
	if err := s.Finish(lifecycleProof(t, "deleted", true), true); err != nil {
		t.Fatal(err)
	}
	if h.Record().State != aj.Deleted {
		t.Fatal("verified deletion not durable")
	}
	if _, err := beginStorageLifecycle(h, "attach_disk", lifecycleProof(t, "new", false)); err == nil {
		t.Fatal("terminal allocation reopened")
	}
}
func TestStorageLifecycleReopenCannotReplayUnknownMutation(t *testing.T) {
	j, h := lifecycleJournalFixture(t)
	s, err := beginStorageLifecycle(h, "detach_disk", lifecycleProof(t, "before", false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Intent("move", aj.Target{Node: "n1", Backing: "backing"}); err != nil {
		t.Fatal(err)
	}
	id := h.Record().ID
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := j.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := beginStorageLifecycle(reopened, "detach_disk", lifecycleProof(t, "restart", false)); err == nil {
		t.Fatal("planned without UPID treated as unsubmitted")
	}
}
func TestStorageLifecycleAdmissionRejectsTamperedOrReusedAudit(t *testing.T) {
	_, h := lifecycleJournalFixture(t)
	bad := lifecycleProof(t, "bad", false)
	bad.EvidenceJSON = `{"complete":false}`
	if _, err := beginStorageLifecycle(h, "attach_disk", bad); err == nil {
		t.Fatal("tampered payload accepted")
	}
	if h.Record().State != aj.ReadyToReturn {
		t.Fatal("invalid evidence changed journal")
	}
	proof := lifecycleProof(t, "one", false)
	s, err := beginStorageLifecycle(h, "attach_disk", proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(proof, false); err == nil {
		t.Fatal("admission audit reused for completion")
	}
}
