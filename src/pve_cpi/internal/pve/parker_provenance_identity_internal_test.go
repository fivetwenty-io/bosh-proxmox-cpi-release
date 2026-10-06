// parker_provenance_identity_internal_test.go — white-box tests for how a
// provenance write finds the record it owns. A removal matches the disk's
// stable ID rather than the volume name, because PVE hands a freed name to the
// next volume parked on the same parker, and it leaves the record alone while
// the parker still holds the volume. A rewrite in update mode keeps the option
// overrides update_disk recorded, and the collection log reports only records
// a landed write removed.
package pve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const identityTestReused = "local-lvm:vm-90000-disk-0"

var identityTestNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// TestRemoveParkerProvenance_KeepsTheRecordOfADiskParkedUnderTheReusedName
// covers PVE reusing the name our disk just left. Another holder parks its
// disk under that name and writes its record after our read, so our first
// write is refused and the second round reads both records naming the same
// volume. Only our record, found by our stable ID, goes.
func TestRemoveParkerProvenance_KeepsTheRecordOfADiskParkedUnderTheReusedName(t *testing.T) {
	t.Parallel()

	ours := removeTestEntry(identityTestReused, "scsi0", identityTestNow.Add(-time.Hour))
	theirs := removeTestEntry(identityTestReused, "scsi1", identityTestNow)
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": ours}))
	c.beforeOurWrite = func(c *digestProvClient, write int) {
		if write == 1 {
			c.otherHolderWrites(withEntry(t, c, "bpd-theirs", theirs))
		}
	}

	removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, identityTestReused, "bpd-ours", ParkerConfig{})

	_, disks, _ := parseParkerSentinel(c.description())
	if _, ok := disks["bpd-theirs"]; !ok {
		t.Fatalf("the record of the disk parked under the reused name was removed; disks=%v", disks)
	}
	if _, ok := disks["bpd-ours"]; ok {
		t.Errorf("our own record should be gone; disks=%v", disks)
	}
}

// TestRemoveParkerProvenance_RemovesOnlyOursWhileAnotherDiskHoldsTheName
// covers the parker already holding another disk under our old name, with
// that disk's serial on the slot. The name is held, but by a disk that is
// provably not ours, so our record goes and the other disk's record stays.
func TestRemoveParkerProvenance_RemovesOnlyOursWhileAnotherDiskHoldsTheName(t *testing.T) {
	t.Parallel()

	ours := removeTestEntry(identityTestReused, "scsi0", identityTestNow.Add(-time.Hour))
	theirs := removeTestEntry(identityTestReused, "scsi1", identityTestNow)
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": ours, "bpd-theirs": theirs}))
	c.cfg["scsi1"] = identityTestReused + ",serial=bpd-theirs"

	removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, identityTestReused, "bpd-ours", ParkerConfig{})

	_, disks, _ := parseParkerSentinel(c.description())
	if _, ok := disks["bpd-theirs"]; !ok {
		t.Fatalf("the record of the disk holding the name was removed; disks=%v", disks)
	}
	if _, ok := disks["bpd-ours"]; ok || c.ourWrites != 1 {
		t.Errorf("ourWrites=%d, our record kept=%v; want one write that removes it", c.ourWrites, ok)
	}
}

// TestRemoveParkerProvenance_SkipsWhileTheParkerStillHoldsTheVolume covers a
// removal whose volume is still on the parker in a way that may be our own
// disk: on a slot with our serial, on a slot without a serial, or on an
// unusedN entry. The record may belong to a disk that is really there, so
// nothing is written and every record stays.
func TestRemoveParkerProvenance_SkipsWhileTheParkerStillHoldsTheVolume(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		key      string
		value    string
		stableID string
	}{
		{"our serial on a slot", "scsi0", identityTestReused + ",serial=bpd-ours", "bpd-ours"},
		{"a slot without a serial", "scsi0", identityTestReused + ",discard=on", "bpd-ours"},
		{"an unused entry", "unused0", identityTestReused, "bpd-ours"},
		{"another serial while we can't name ours", "scsi0", identityTestReused + ",serial=bpd-theirs", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ours := removeTestEntry(identityTestReused, "scsi0", identityTestNow.Add(-time.Hour))
			c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": ours}))
			c.cfg[tc.key] = tc.value
			before := c.description()

			removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, identityTestReused, tc.stableID, ParkerConfig{})

			if c.ourWrites != 0 || c.description() != before {
				t.Errorf("ourWrites=%d, description changed=%v; want no write while the parker holds the volume",
					c.ourWrites, c.description() != before)
			}
		})
	}
}

// TestRemoveParkerProvenance_LeavesARecordThatNamesAnotherVolume covers a
// record under our stable ID that names a volume other than the one that left.
// That record is not this disk's history on this name, so it stays.
func TestRemoveParkerProvenance_LeavesARecordThatNamesAnotherVolume(t *testing.T) {
	t.Parallel()

	moved := removeTestEntry("local-lvm:vm-90000-disk-7", "scsi7", identityTestNow.Add(-time.Hour))
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": moved}))
	before := c.description()

	removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, identityTestReused, "bpd-ours", ParkerConfig{})

	if c.ourWrites != 0 || c.description() != before {
		t.Errorf("ourWrites=%d, description changed=%v; want the record that names another volume left alone",
			c.ourWrites, c.description() != before)
	}
}

// TestRewriteParkerProvenance_KeepsTheOptionsUpdateDiskRecorded covers a
// transfer's finalize after update_disk recorded option overrides on the same
// record. The finalize builds its entry from an earlier snapshot, which has no
// overrides, and the rewrite keeps the ones the fresh read shows.
func TestRewriteParkerProvenance_KeepsTheOptionsUpdateDiskRecorded(t *testing.T) {
	t.Parallel()

	recorded := removeTestEntry(identityTestReused, "scsi0", identityTestNow)
	recorded.SourceVMCID = "vm-source"
	recorded.Opts = map[string]string{"cache": "writeback"}
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": recorded}))
	final := removeTestEntry(identityTestReused, "scsi0", identityTestNow)
	final.SourceVMCID = "vm-source"

	if err := rewriteParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, "bpd-ours", final, writeTestConfig()); err != nil {
		t.Fatalf("rewriteParkerProvenance: %v", err)
	}
	_, disks, _ := parseParkerSentinel(c.description())
	if got := disks["bpd-ours"].Opts["cache"]; got != "writeback" {
		t.Fatalf("the rewrite dropped the recorded options; entry=%+v", disks["bpd-ours"])
	}
	if disks["bpd-ours"].Slot != "scsi0" || disks["bpd-ours"].SourceVMCID != "vm-source" {
		t.Errorf("the rest of the entry should come from the caller; entry=%+v", disks["bpd-ours"])
	}
}

// TestRewriteParkerProvenance_KeepsOptionsWrittenBetweenRounds covers
// update_disk recording its overrides after our read and before our write.
// The first write is refused, and the second round keeps the overrides from
// its fresh read.
func TestRewriteParkerProvenance_KeepsOptionsWrittenBetweenRounds(t *testing.T) {
	t.Parallel()

	recorded := removeTestEntry(identityTestReused, "scsi0", identityTestNow)
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": recorded}))
	withOpts := recorded
	withOpts.Opts = map[string]string{"cache": "writeback"}
	c.beforeOurWrite = func(c *digestProvClient, write int) {
		if write == 1 {
			c.otherHolderWrites(withEntry(t, c, "bpd-ours", withOpts))
		}
	}

	if err := rewriteParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, "bpd-ours", recorded, writeTestConfig()); err != nil {
		t.Fatalf("rewriteParkerProvenance: %v", err)
	}
	_, disks, _ := parseParkerSentinel(c.description())
	if got := disks["bpd-ours"].Opts["cache"]; got != "writeback" || c.refused != 1 {
		t.Errorf("refused=%d, entry=%+v; want one refusal and the overrides kept", c.refused, disks["bpd-ours"])
	}
}

// TestRewriteParkerProvenance_DropsOptionsOfAnotherTransfersRecord covers a
// record under the same key that names another source VM. Its overrides
// belong to that earlier history, so the rewrite doesn't carry them.
func TestRewriteParkerProvenance_DropsOptionsOfAnotherTransfersRecord(t *testing.T) {
	t.Parallel()

	recorded := removeTestEntry(identityTestReused, "scsi0", identityTestNow)
	recorded.SourceVMCID = "vm-earlier"
	recorded.Opts = map[string]string{"cache": "writeback"}
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": recorded}))
	final := removeTestEntry(identityTestReused, "scsi0", identityTestNow)
	final.SourceVMCID = "vm-source"

	if err := rewriteParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, "bpd-ours", final, writeTestConfig()); err != nil {
		t.Fatalf("rewriteParkerProvenance: %v", err)
	}
	_, disks, _ := parseParkerSentinel(c.description())
	if len(disks["bpd-ours"].Opts) != 0 {
		t.Errorf("options of another transfer's record were carried; entry=%+v", disks["bpd-ours"])
	}
}

// TestWriteParkerProvenance_ReplaceModeStillDropsRecordedOptions covers the
// intent and park writes, which keep their replace behavior. A record they
// find under the key is history from an earlier park, so its overrides go.
func TestWriteParkerProvenance_ReplaceModeStillDropsRecordedOptions(t *testing.T) {
	t.Parallel()

	recorded := removeTestEntry(identityTestReused, "scsi0", identityTestNow)
	recorded.Opts = map[string]string{"cache": "writeback"}
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": recorded}))
	fresh := removeTestEntry(identityTestReused, "scsi0", identityTestNow)

	if err := writeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, "bpd-ours", fresh, writeTestConfig()); err != nil {
		t.Fatalf("writeParkerProvenance: %v", err)
	}
	_, disks, _ := parseParkerSentinel(c.description())
	if len(disks["bpd-ours"].Opts) != 0 {
		t.Errorf("replace mode kept the recorded options; entry=%+v", disks["bpd-ours"])
	}
}

// TestWriteParkerProvenance_LogsCollectionOnlyAfterALandedWrite covers a
// write that would collect a stale record but never lands. The collection log
// would tell an operator a record is gone that is still on the parker, so it
// stays quiet. A write that lands logs what it collected.
func TestWriteParkerProvenance_LogsCollectionOnlyAfterALandedWrite(t *testing.T) {
	t.Parallel()

	collectedLogged := func(entries []log.Entry) bool {
		for _, e := range entries {
			if strings.Contains(e.Message, "collected stale parked-disk records") {
				return true
			}
		}
		return false
	}
	start := func(t *testing.T) *digestProvClient {
		stale := removeTestEntry("local-lvm:vm-90000-disk-3", "scsi3", identityTestNow.Add(-2*parkerProvenanceGraceWindow))
		return newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-stale": stale}))
	}
	ours := removeTestEntry(writeTestOursVolid, "scsi5", identityTestNow)

	failed := start(t)
	failed.writeErr = errors.New("connection reset by peer")
	logger, obs := log.NewObservedLogger(log.LevelInfo)
	if err := writeParkerProvenance(context.Background(), failed, logger, removeTestNode, removeTestParker, writeTestOursKey, ours, writeTestConfig()); err == nil {
		t.Fatalf("writeParkerProvenance: want the write failure back")
	}
	if collectedLogged(obs.All()) {
		t.Errorf("a failed write logged records as collected: %+v", obs.All())
	}

	landed := start(t)
	logger, obs = log.NewObservedLogger(log.LevelInfo)
	if err := writeParkerProvenance(context.Background(), landed, logger, removeTestNode, removeTestParker, writeTestOursKey, ours, writeTestConfig()); err != nil {
		t.Fatalf("writeParkerProvenance: %v", err)
	}
	if !collectedLogged(obs.All()) {
		t.Errorf("a landed write that collected a record did not log it: %+v", obs.All())
	}
}
