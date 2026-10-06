// parker_description_write_internal_test.go — white-box tests for the
// digest-guarded parker description writes that add or update a record: the
// provenance write behind ParkDisk and the transfer and mover paths, and the
// drive-option overlay update_disk records on a parked disk. Each race test
// puts another holder's write between our config read and our description
// write and checks that the other holder's entry survives. The give-up tests
// make the config change under every round.
package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const (
	writeTestOursKey   = "bpd-ours"
	writeTestOursVolid = "local-lvm:vm-90000-disk-5"
)

// writeTestNow is the clock every test here stamps and collects against, so
// each entry is young and nothing is collected as stale.
var writeTestNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func writeTestConfig() ParkerConfig {
	return ParkerConfig{NowFunc: func() time.Time { return writeTestNow }}
}

// writeTestStart returns a parker that already holds one standing entry.
func writeTestStart(t *testing.T) (*digestProvClient, parkerProvEntry) {
	t.Helper()
	standing := removeTestEntry("local-lvm:vm-90000-disk-2", "scsi2", writeTestNow)
	return newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-standing": standing})), standing
}

// theirsOnWrite makes another holder add its own entry before each of our
// writes numbered in rounds, keyed bpd-theirs-<write>.
func theirsOnWrite(t *testing.T, rounds ...int) func(c *digestProvClient, write int) {
	t.Helper()
	return func(c *digestProvClient, write int) {
		for _, r := range rounds {
			if r == write {
				key := fmt.Sprintf("bpd-theirs-%d", write)
				volid := fmt.Sprintf("local-lvm:vm-90000-disk-%d", 10+write)
				c.otherHolderWrites(withEntry(t, c, key, removeTestEntry(volid, fmt.Sprintf("scsi%d", 10+write), writeTestNow)))
			}
		}
	}
}

// assertTheirsKept fails when any entry another holder wrote before one of the
// given writes is missing or changed.
func assertTheirsKept(t *testing.T, disks map[string]parkerProvEntry, desc string, rounds ...int) {
	t.Helper()
	for _, write := range rounds {
		key := fmt.Sprintf("bpd-theirs-%d", write)
		got, ok := disks[key]
		if !ok {
			t.Errorf("the entry another holder wrote before our write %d was overwritten; description now %q", write, desc)
			continue
		}
		if want := fmt.Sprintf("local-lvm:vm-90000-disk-%d", 10+write); got.Volid != want {
			t.Errorf("the other holder's entry %s changed: volid %q, want %q", key, got.Volid, want)
		}
	}
}

// TestWriteParkerProvenance_KeepsAnEntryAnotherHolderWroteAfterOurRead is the
// race behind ParkDisk's provenance write, which runs after the parker lock is
// released. Another holder writes its entry after our read and before our
// write. Our write carries the read's digest, PVE refuses it, and the second
// round adds our record to what the other holder left.
func TestWriteParkerProvenance_KeepsAnEntryAnotherHolderWroteAfterOurRead(t *testing.T) {
	t.Parallel()

	c, standing := writeTestStart(t)
	c.beforeOurWrite = theirsOnWrite(t, 1)
	ours := removeTestEntry(writeTestOursVolid, "scsi5", writeTestNow)

	err := writeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, writeTestOursKey, ours, writeTestConfig())
	if err != nil {
		t.Fatalf("writeParkerProvenance: %v", err)
	}

	desc := c.description()
	nonBOSH, disks, _ := parseParkerSentinel(desc)
	assertTheirsKept(t, disks, desc, 1)
	if got, ok := disks["bpd-standing"]; !ok || got.Volid != standing.Volid {
		t.Errorf("the entry that was already there was dropped or changed; description now %q", desc)
	}
	if got, ok := disks[writeTestOursKey]; !ok || got.Volid != writeTestOursVolid {
		t.Errorf("our record did not land; description now %q", desc)
	}
	if strings.TrimSpace(nonBOSH) != "operator note" {
		t.Errorf("free text outside the sentinel changed: got %q", nonBOSH)
	}
	if c.reads != 2 || c.ourWrites != 2 || c.refused != 1 || c.accepted != 2 {
		t.Errorf("reads=%d ourWrites=%d refused=%d accepted=%d, want 2, 2, 1, and 2 (the other holder's write and our second)",
			c.reads, c.ourWrites, c.refused, c.accepted)
	}
}

// TestWriteParkerProvenance_GivesUpWithARetriableErrorAfterBoundedRounds
// covers a parker whose config changes under every round. The write stops
// after its bounded rounds with a retriable ErrParkerDescriptionContended, and
// it wrote nothing over any of the other writer's entries.
func TestWriteParkerProvenance_GivesUpWithARetriableErrorAfterBoundedRounds(t *testing.T) {
	t.Parallel()

	c, _ := writeTestStart(t)
	c.beforeOurWrite = theirsOnWrite(t, 1, 2, 3)
	ours := removeTestEntry(writeTestOursVolid, "scsi5", writeTestNow)

	err := writeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, writeTestOursKey, ours, writeTestConfig())
	if !errors.Is(err, ErrParkerDescriptionContended) {
		t.Fatalf("err = %v, want ErrParkerDescriptionContended", err)
	}
	if !isRetriableCPIError(err) {
		t.Errorf("err = %v, want a retriable CPI error", err)
	}
	if c.reads != parkerDescriptionWriteAttempts || c.ourWrites != parkerDescriptionWriteAttempts || c.refused != parkerDescriptionWriteAttempts {
		t.Errorf("reads=%d ourWrites=%d refused=%d, want %d of each",
			c.reads, c.ourWrites, c.refused, parkerDescriptionWriteAttempts)
	}
	desc := c.description()
	_, disks, _ := parseParkerSentinel(desc)
	assertTheirsKept(t, disks, desc, 1, 2, 3)
	if _, ok := disks[writeTestOursKey]; ok {
		t.Errorf("our record landed although every write was refused; description now %q", desc)
	}
}

// TestUpdateParkerProvenance_ReturnsTheGiveUpToThePark covers the ParkDisk
// call site. A write that gave up after every round was refused leaves the
// disk on the parker without its record, so the park gets an error it fails
// on rather than a warning it moves past.
func TestUpdateParkerProvenance_ReturnsTheGiveUpToThePark(t *testing.T) {
	t.Parallel()

	c, _ := writeTestStart(t)
	c.beforeOurWrite = theirsOnWrite(t, 1, 2, 3)

	err := updateParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker,
		writeTestOursVolid, "scsi5", writeTestConfig(), ParkContext{StableID: writeTestOursKey, DiskCID: writeTestOursVolid})
	if !errors.Is(err, ErrParkerDescriptionContended) || !isRetriableCPIError(err) {
		t.Fatalf("err = %v, want a retriable ErrParkerDescriptionContended", err)
	}
	desc := c.description()
	_, disks, _ := parseParkerSentinel(desc)
	assertTheirsKept(t, disks, desc, 1, 2, 3)
}

// TestUpdateParkerProvenance_KeepsOtherFailuresAdvisory covers every failure
// other than the give-up. Those were advisory before the guard and still are:
// the disk is attached, so the park logs and succeeds.
func TestUpdateParkerProvenance_KeepsOtherFailuresAdvisory(t *testing.T) {
	t.Parallel()

	c, _ := writeTestStart(t)
	c.writeErr = sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed (/vms/90000, VM.Config.Options)\n"}`))

	err := updateParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker,
		writeTestOursVolid, "scsi5", writeTestConfig(), ParkContext{StableID: writeTestOursKey})
	if err != nil {
		t.Fatalf("err = %v, want nil for an advisory failure", err)
	}
	if c.ourWrites != 1 {
		t.Errorf("ourWrites=%d, want one write and no retry of a refusal that is not a stale digest", c.ourWrites)
	}
}

// TestWriteParkerProvenance_RefusesAReadWithoutADigest covers a config read
// that carries no digest, such as the empty map the SDK returns for a reply
// that isn't an object. A write built from that read could erase every other
// record and the operator's text, so the writer re-reads on each round and,
// when no read carries a digest, refuses with a retriable error and writes
// nothing.
func TestWriteParkerProvenance_RefusesAReadWithoutADigest(t *testing.T) {
	t.Parallel()

	c, _ := writeTestStart(t)
	c.omitDigest = true
	before := c.description()
	ours := removeTestEntry(writeTestOursVolid, "scsi5", writeTestNow)

	err := writeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, writeTestOursKey, ours, writeTestConfig())
	if !errors.Is(err, errParkerConfigNoDigest) || !isRetriableCPIError(err) {
		t.Fatalf("err = %v, want a retriable errParkerConfigNoDigest", err)
	}
	if c.ourWrites != 0 || c.reads != parkerDescriptionWriteAttempts {
		t.Errorf("ourWrites=%d reads=%d, want no write after %d reads", c.ourWrites, c.reads, parkerDescriptionWriteAttempts)
	}
	if got := c.description(); got != before {
		t.Errorf("description changed without a write: got %q, want %q", got, before)
	}
}

// TestWriteParkerProvenance_WritesOnceARereadCarriesADigest covers a read
// without a digest followed by one that has it. The writer builds its write
// from the second read and sends it guarded.
func TestWriteParkerProvenance_WritesOnceARereadCarriesADigest(t *testing.T) {
	t.Parallel()

	c, _ := writeTestStart(t)
	c.omitDigestReads = 1
	ours := removeTestEntry(writeTestOursVolid, "scsi5", writeTestNow)

	if err := writeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, writeTestOursKey, ours, writeTestConfig()); err != nil {
		t.Fatalf("writeParkerProvenance: %v", err)
	}
	_, disks, _ := parseParkerSentinel(c.description())
	if _, ok := disks[writeTestOursKey]; !ok || c.ourWrites != 1 || c.reads != 2 {
		t.Errorf("reads=%d ourWrites=%d record landed=%v; want two reads, one write, and our record", c.reads, c.ourWrites, ok)
	}
	if _, ok := disks["bpd-standing"]; !ok {
		t.Errorf("the standing record was dropped; description now %q", c.description())
	}
}

// TestUpdateParkerProvenance_ReturnsTheRefusalOfAReadWithoutADigest covers
// ParkDisk's write. The park fails with the retriable error rather than
// leaving the disk parked with no record.
func TestUpdateParkerProvenance_ReturnsTheRefusalOfAReadWithoutADigest(t *testing.T) {
	t.Parallel()

	c, _ := writeTestStart(t)
	c.omitDigest = true
	before := c.description()

	err := updateParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker,
		writeTestOursVolid, "scsi5", writeTestConfig(), ParkContext{StableID: writeTestOursKey})
	if !errors.Is(err, errParkerConfigNoDigest) || !isRetriableCPIError(err) {
		t.Fatalf("err = %v, want a retriable errParkerConfigNoDigest", err)
	}
	if c.ourWrites != 0 || c.description() != before {
		t.Errorf("ourWrites=%d, description changed=%v; want neither", c.ourWrites, c.description() != before)
	}
}

// TestApplyParkerDiskOverlay_RefusesAReadWithoutADigest covers update_disk on
// a parked disk whose parker reads come back without a digest. update_disk
// gets a retriable error, and the description is left as it was.
func TestApplyParkerDiskOverlay_RefusesAReadWithoutADigest(t *testing.T) {
	t.Parallel()

	c := overlayTestStart(t)
	c.omitDigest = true
	before := c.description()

	_, err := ApplyParkerDiskOverlay(context.Background(), c, removeTestNode, removeTestParker,
		writeTestOursVolid, writeTestOursKey, writeTestOursVolid, map[string]string{"cache": "writeback"}, writeTestConfig())
	if !errors.Is(err, errParkerConfigNoDigest) || !isRetriableCPIError(err) {
		t.Fatalf("err = %v, want a retriable errParkerConfigNoDigest", err)
	}
	if c.ourWrites != 0 || c.description() != before {
		t.Errorf("ourWrites=%d, description changed=%v; want neither", c.ourWrites, c.description() != before)
	}
}

// staleBeforeSendClient refuses the first description write the way the
// managed lifecycle guard does when the write's digest no longer matches the
// guard's own read: an error wrapping ErrConfigDigestStale, with nothing sent
// to PVE. Another holder's write lands first, which is why the digest is stale.
type staleBeforeSendClient struct {
	*digestProvClient
	t        *testing.T
	refusals int
}

func (s *staleBeforeSendClient) Nodes() sdknodes.Service {
	inner := s.digestProvClient.Nodes()
	return &fakeNodesService{
		updateQemuConfigFn: func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
			if s.refusals == 0 {
				s.refusals++
				s.mu.Lock()
				s.otherHolderWrites(withEntry(s.t, s.digestProvClient, "bpd-theirs-1",
					removeTestEntry("local-lvm:vm-90000-disk-11", "scsi11", writeTestNow)))
				s.mu.Unlock()
				return fmt.Errorf("managed config generation changed before a description write: %w", ErrConfigDigestStale)
			}
			return inner.UpdateQemuConfig(ctx, node, vmid, params)
		},
	}
}

// TestWriteParkerProvenance_RetriesAWrapperStaleRefusal covers a client
// wrapper that refuses a stale digest before sending the write. The writer
// treats it like PVE's own refusal and adds its record to a fresh read.
func TestWriteParkerProvenance_RetriesAWrapperStaleRefusal(t *testing.T) {
	t.Parallel()

	inner, _ := writeTestStart(t)
	c := &staleBeforeSendClient{digestProvClient: inner, t: t}
	ours := removeTestEntry(writeTestOursVolid, "scsi5", writeTestNow)

	if err := writeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, writeTestOursKey, ours, writeTestConfig()); err != nil {
		t.Fatalf("writeParkerProvenance: %v", err)
	}
	desc := inner.description()
	_, disks, _ := parseParkerSentinel(desc)
	assertTheirsKept(t, disks, desc, 1)
	if _, ok := disks[writeTestOursKey]; !ok {
		t.Errorf("our record did not land after the wrapper's refusal; description now %q", desc)
	}
	if c.refusals != 1 || inner.reads != 2 {
		t.Errorf("refusals=%d reads=%d, want 1 refusal and 2 reads", c.refusals, inner.reads)
	}
}

// TestIsConfigDigestStale covers what the guarded writes retry on, and what
// they don't.
func TestIsConfigDigestStale(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"pve checksum mismatch", sdkerrors.ParseAPIError(500, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`)), true},
		{"wrapper refusal", cpierrors.Wrap(fmt.Errorf("refused: %w", ErrConfigDigestStale), "outer"), true},
		{"permission refusal", sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed\n"}`)), false},
		{"unanswered text", errors.New("checksum mismatch (file change by other user?)"), false},
	}
	for _, tc := range cases {
		if got := IsConfigDigestStale(tc.err); got != tc.want {
			t.Errorf("%s: IsConfigDigestStale = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// overlayTestStart returns a parker holding our disk's record, with no
// overrides yet, and one standing entry.
func overlayTestStart(t *testing.T) *digestProvClient {
	t.Helper()
	return newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{
		"bpd-standing":   removeTestEntry("local-lvm:vm-90000-disk-2", "scsi2", writeTestNow),
		writeTestOursKey: removeTestEntry(writeTestOursVolid, "scsi5", writeTestNow),
	}))
}

// TestApplyParkerDiskOverlay_KeepsAnEntryAnotherHolderWroteAfterOurRead is the
// race update_disk meets on a parked disk, which it records with no parker
// lock. Another holder writes its entry after our read and before our write.
// The second round merges our overrides into a fresh read, so the other
// holder's entry survives and only our entry's options change.
func TestApplyParkerDiskOverlay_KeepsAnEntryAnotherHolderWroteAfterOurRead(t *testing.T) {
	t.Parallel()

	c := overlayTestStart(t)
	c.beforeOurWrite = theirsOnWrite(t, 1)

	merged, err := ApplyParkerDiskOverlay(context.Background(), c, removeTestNode, removeTestParker,
		writeTestOursVolid, writeTestOursKey, writeTestOursVolid, map[string]string{"cache": "writeback"}, writeTestConfig())
	if err != nil {
		t.Fatalf("ApplyParkerDiskOverlay: %v", err)
	}
	if merged["cache"] != "writeback" {
		t.Errorf("merged = %v, want cache=writeback", merged)
	}

	desc := c.description()
	_, disks, _ := parseParkerSentinel(desc)
	assertTheirsKept(t, disks, desc, 1)
	if _, ok := disks["bpd-standing"]; !ok {
		t.Errorf("the entry that was already there was dropped; description now %q", desc)
	}
	if got := disks[writeTestOursKey].Opts["cache"]; got != "writeback" {
		t.Errorf("our entry's cache option = %q, want writeback; description now %q", got, desc)
	}
	if c.reads != 2 || c.ourWrites != 2 || c.refused != 1 {
		t.Errorf("reads=%d ourWrites=%d refused=%d, want 2, 2, and 1", c.reads, c.ourWrites, c.refused)
	}
}

// TestApplyParkerDiskOverlay_GivesUpWithARetriableError covers a parker whose
// config changes under every round. update_disk gets a retriable error, and
// nothing of ours was written over the other writer.
func TestApplyParkerDiskOverlay_GivesUpWithARetriableError(t *testing.T) {
	t.Parallel()

	c := overlayTestStart(t)
	c.beforeOurWrite = theirsOnWrite(t, 1, 2, 3)

	merged, err := ApplyParkerDiskOverlay(context.Background(), c, removeTestNode, removeTestParker,
		writeTestOursVolid, writeTestOursKey, writeTestOursVolid, map[string]string{"cache": "writeback"}, writeTestConfig())
	if !errors.Is(err, ErrParkerDescriptionContended) || !isRetriableCPIError(err) {
		t.Fatalf("err = %v, want a retriable ErrParkerDescriptionContended", err)
	}
	if merged != nil {
		t.Errorf("merged = %v, want nil when nothing was written", merged)
	}
	if c.ourWrites != parkerDescriptionWriteAttempts || c.refused != parkerDescriptionWriteAttempts {
		t.Errorf("ourWrites=%d refused=%d, want %d of each", c.ourWrites, c.refused, parkerDescriptionWriteAttempts)
	}
	desc := c.description()
	_, disks, _ := parseParkerSentinel(desc)
	assertTheirsKept(t, disks, desc, 1, 2, 3)
	if got := disks[writeTestOursKey].Opts["cache"]; got != "" {
		t.Errorf("our entry's cache option = %q, want it unchanged when every write was refused", got)
	}
}
