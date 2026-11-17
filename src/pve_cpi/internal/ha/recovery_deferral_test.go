package ha_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

func TestRecoveryDeferral_RecordIsReadBackAndAges(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)

	if _, held, err := store.LastRecoveryDeferral("vm:101"); err != nil || held {
		t.Fatalf("an empty directory holds no deferral: held=%v err=%v", held, err)
	}
	if err := store.RecordRecoveryDeferral("vm:101"); err != nil {
		t.Fatalf("record: %v", err)
	}
	first := clk.t
	clk.t = clk.t.Add(2 * time.Minute)
	if err := store.RecordRecoveryDeferral("vm:101"); err != nil {
		t.Fatalf("record again: %v", err)
	}
	clk.t = clk.t.Add(90 * time.Second)

	got, held, err := store.LastRecoveryDeferral("vm:101")
	if err != nil || !held {
		t.Fatalf("the deferral must read back: held=%v err=%v", held, err)
	}
	if !got.At.Equal(first.Add(2*time.Minute)) || got.Age() != 90*time.Second {
		t.Fatalf("the record keeps the last deferral: at=%s age=%s", got.At, got.Age())
	}
	info, err := os.Lstat(filepath.Join(dir, ha.RecoveryDeferralFileName))
	if err != nil {
		t.Fatalf("the record was not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %04o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(dir, ha.EpisodeFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a deferral must never touch the episode record: %v", err)
	}
	if _, held, _ := store.LastRecoveryDeferral("vm:102"); held {
		t.Error("another service must hold no deferral")
	}
}

func TestRecoveryDeferral_ClearRemovesOnlyTheService(t *testing.T) {
	t.Parallel()
	store, _, dir := newEpisodeStore(t)

	if err := store.ClearRecoveryDeferral("vm:101"); err != nil {
		t.Fatalf("a clear with no record is not an error: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ha.RecoveryDeferralFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a clear with no record must write nothing: %v", err)
	}
	for _, sid := range []string{"vm:101", "vm:102"} {
		if err := store.RecordRecoveryDeferral(sid); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ClearRecoveryDeferral("vm:101"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, held, err := store.LastRecoveryDeferral("vm:101"); err != nil || held {
		t.Fatalf("the cleared deferral must be gone: held=%v err=%v", held, err)
	}
	if _, held, err := store.LastRecoveryDeferral("vm:102"); err != nil || !held {
		t.Fatalf("the other service's deferral must stay: held=%v err=%v", held, err)
	}
}

func TestRecoveryDeferral_FutureDeferralIsNotTrusted(t *testing.T) {
	t.Parallel()
	store, clk, _ := newEpisodeStore(t)
	if err := store.RecordRecoveryDeferral("vm:101"); err != nil {
		t.Fatal(err)
	}
	// Within the skew the deferral is trusted and reads as age zero.
	clk.t = clk.t.Add(-10 * time.Second)
	got, held, err := store.LastRecoveryDeferral("vm:101")
	if err != nil || !held || got.Age() != 0 {
		t.Fatalf("a deferral within the skew must be trusted at age zero: held=%v age=%s err=%v",
			held, got.Age(), err)
	}
	// A clock that stepped back further than the skew does not trust it.
	clk.t = clk.t.Add(-time.Hour)
	if _, held, err := store.LastRecoveryDeferral("vm:101"); err != nil || held {
		t.Fatalf("a deferral ahead of the clock must not be trusted: held=%v err=%v", held, err)
	}
}

func TestRecoveryDeferral_OldDeferralsArePruned(t *testing.T) {
	t.Parallel()
	store, clk, _ := newEpisodeStore(t)
	if err := store.RecordRecoveryDeferral("vm:101"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(25 * time.Hour)
	if err := store.RecordRecoveryDeferral("vm:102"); err != nil {
		t.Fatal(err)
	}
	// Step the clock back so the pruned entry would still be trusted if it
	// were there.
	clk.t = clk.t.Add(-24*time.Hour - time.Minute)
	if _, held, err := store.LastRecoveryDeferral("vm:101"); err != nil || held {
		t.Fatalf("a deferral a day old must be pruned by the next write: held=%v err=%v", held, err)
	}
}

func TestRecoveryDeferral_ScopesKeepClustersApart(t *testing.T) {
	t.Parallel()
	storeA, clk, dir := newEpisodeStore(t)
	storeA.Scope = "aaaaaaaaaaaaaaaa"
	storeB := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: dir}, Now: clk.now, Scope: "bbbbbbbbbbbbbbbb"}
	if err := storeA.RecordRecoveryDeferral("vm:105"); err != nil {
		t.Fatal(err)
	}
	if _, held, err := storeB.LastRecoveryDeferral("vm:105"); err != nil || held {
		t.Fatalf("another cluster must not read this cluster's deferral: held=%v err=%v", held, err)
	}
	if err := storeB.ClearRecoveryDeferral("vm:105"); err != nil {
		t.Fatal(err)
	}
	if _, held, err := storeA.LastRecoveryDeferral("vm:105"); err != nil || !held {
		t.Fatalf("another cluster's clear must leave this deferral: held=%v err=%v", held, err)
	}
}

func TestRecoveryDeferral_DamagedRecordReadsAsAnErrorAndIsRewritten(t *testing.T) {
	t.Parallel()
	store, _, dir := newEpisodeStore(t)
	if err := os.WriteFile(filepath.Join(dir, ha.RecoveryDeferralFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, held, err := store.LastRecoveryDeferral("vm:101"); !errors.Is(err, privatefile.ErrCorrupt) || held {
		t.Fatalf("want a corrupt-record error and no deferral, got held=%v err=%v", held, err)
	}
	err := store.RecordRecoveryDeferral("vm:101")
	if !errors.Is(err, privatefile.ErrCorrupt) {
		t.Fatalf("the write must report the damaged record it replaced, got %v", err)
	}
	if _, held, err := store.LastRecoveryDeferral("vm:101"); err != nil || !held {
		t.Fatalf("the rewritten record must read back: held=%v err=%v", held, err)
	}
}

func TestRecoveryDeferral_RejectsAnEmptyServiceID(t *testing.T) {
	t.Parallel()
	store, _, _ := newEpisodeStore(t)
	if err := store.RecordRecoveryDeferral(" "); err == nil {
		t.Error("record must refuse an empty service ID")
	}
	if _, _, err := store.LastRecoveryDeferral(""); err == nil {
		t.Error("read must refuse an empty service ID")
	}
	if err := store.ClearRecoveryDeferral(""); err == nil {
		t.Error("clear must refuse an empty service ID")
	}
}

func TestRecoveryDeferralNameIsAValidDisplayName(t *testing.T) {
	t.Parallel()
	if !strings.Contains(ha.RecoveryDeferralName, "deferral") {
		t.Fatalf("unexpected name %q", ha.RecoveryDeferralName)
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	held, err := endpointcache.LockFile(t.Context(), root, "probe.lock", ha.RecoveryDeferralName, time.Second)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer func() { _ = held.Close() }()
	_, err = endpointcache.LockFile(t.Context(), root, "probe.lock", ha.RecoveryDeferralName, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), ha.RecoveryDeferralName) {
		t.Fatalf("a busy lock must name the record, got %v", err)
	}
}
