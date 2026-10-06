// parker_provenance_remove_internal_test.go — white-box tests for the guarded
// description write in removeParkerProvenance. The removal can run after the
// protection claim has expired, so these tests put another holder's
// provenance write between our config read and our description write and
// check that the other holder's entry survives.
package pve

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// digestProvClient is a single-parker PVE that checks config digests the way
// qemu-server's update_vm_api does. A write that carries a digest other than
// the current one is refused with PVE's checksum-mismatch answer and changes
// nothing. A write without a digest always lands. Every accepted write moves
// the digest on.
type digestProvClient struct {
	parkerLockClient

	mu  sync.Mutex
	cfg map[string]any
	// generation numbers the digest, so each accepted write gives a new one.
	generation int
	// omitDigest makes reads answer without a digest key.
	omitDigest bool
	// omitDigestReads makes that many first reads answer without a digest.
	omitDigestReads int
	// beforeOurWrite, when set, runs before each of our description writes is
	// checked, with the 1-based number of that write. It stands for another
	// holder whose write lands between our read and our write.
	beforeOurWrite func(c *digestProvClient, write int)
	// writeErr, when set, is returned for each of our writes instead of the
	// digest check.
	writeErr error

	reads     int
	ourWrites int
	refused   int
	// accepted counts every write that landed, ours and the other holder's.
	accepted int
}

func newDigestProvClient(desc string) *digestProvClient {
	c := &digestProvClient{cfg: map[string]any{
		"tags":        ParkerTag,
		"description": desc,
	}}
	c.cfg["digest"] = c.digestLocked()
	return c
}

func (c *digestProvClient) digestLocked() string {
	return fmt.Sprintf("digest-%04d", c.generation)
}

// otherHolderWrites applies a description write from another holder. It
// carries no digest check of its own because the test only needs it to land.
func (c *digestProvClient) otherHolderWrites(desc string) {
	c.cfg["description"] = desc
	c.accepted++
	c.generation++
	c.cfg["digest"] = c.digestLocked()
}

func (c *digestProvClient) description() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	desc, _ := c.cfg["description"].(string)
	return desc
}

func (c *digestProvClient) QEMU() qemu.Service {
	return &fakeQEMUService{
		configFn: func(context.Context, string, int) (map[string]any, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.reads++
			out := maps.Clone(c.cfg)
			if c.omitDigest || c.reads <= c.omitDigestReads {
				delete(out, "digest")
			}
			return out, nil
		},
	}
}

func (c *digestProvClient) Nodes() sdknodes.Service {
	return &fakeNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.ourWrites++
			if c.beforeOurWrite != nil {
				c.beforeOurWrite(c, c.ourWrites)
			}
			if c.writeErr != nil {
				return c.writeErr
			}
			if params.Digest != nil && *params.Digest != c.cfg["digest"] {
				c.refused++
				return sdkerrors.ParseAPIError(500, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`))
			}
			if params.Description != nil {
				c.cfg["description"] = *params.Description
			}
			c.accepted++
			c.generation++
			c.cfg["digest"] = c.digestLocked()
			return nil
		},
	}
}

// removeTestEntry builds a provenance entry for volid parked at now.
func removeTestEntry(volid, slot string, now time.Time) parkerProvEntry {
	return parkerProvEntry{
		DiskCID:  volid,
		ParkedAt: now.Format(time.RFC3339),
		Node:     "pve1",
		Volid:    volid,
		Slot:     slot,
	}
}

// removeTestDescription renders the operator's free text followed by the
// sentinel carrying disks.
func removeTestDescription(t *testing.T, disks map[string]parkerProvEntry) string {
	t.Helper()
	desc, err := renderParkerSentinel("operator note\n", disks, nil)
	if err != nil {
		t.Fatalf("render sentinel: %v", err)
	}
	return desc
}

// withEntry returns c's current description with key set to entry, the way a
// new holder's provenance write adds its record.
func withEntry(t *testing.T, c *digestProvClient, key string, entry parkerProvEntry) string {
	t.Helper()
	desc, _ := c.cfg["description"].(string)
	nonBOSH, disks, raw := parseParkerSentinel(desc)
	disks[key] = entry
	out, err := renderParkerSentinel(nonBOSH, disks, raw)
	if err != nil {
		t.Fatalf("render sentinel: %v", err)
	}
	return out
}

const (
	removeTestNode   = "pve1"
	removeTestParker = 90000
	removeTestOurs   = "local-lvm:vm-9001-disk-0"
)

// TestRemoveParkerProvenance_KeepsAnEntryAnotherHolderWroteAfterOurRead is the
// race the removal guards against once the claim has expired. A new holder
// parks another disk and writes its entry after our read and before our
// write. Our write carries the read's digest, PVE refuses it, and the second
// round removes only our entry from what the new holder left.
func TestRemoveParkerProvenance_KeepsAnEntryAnotherHolderWroteAfterOurRead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ours := removeTestEntry(removeTestOurs, "scsi1", now.Add(-time.Hour))
	standing := removeTestEntry("local-lvm:vm-9002-disk-0", "scsi2", now.Add(-time.Hour))
	theirs := removeTestEntry("local-lvm:vm-9003-disk-0", "scsi3", now)

	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{
		"bpd-ours":     ours,
		"bpd-standing": standing,
	}))
	c.beforeOurWrite = func(c *digestProvClient, write int) {
		if write == 1 {
			c.otherHolderWrites(withEntry(t, c, "bpd-theirs", theirs))
		}
	}

	removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, removeTestOurs, "bpd-ours", ParkerConfig{})

	desc := c.description()
	nonBOSH, disks, _ := parseParkerSentinel(desc)
	if got, ok := disks["bpd-theirs"]; !ok {
		t.Fatalf("the entry another holder wrote between our read and our write was overwritten; description now %q", desc)
	} else if got.Volid != theirs.Volid || got.Slot != theirs.Slot || got.ParkedAt != theirs.ParkedAt {
		t.Errorf("the other holder's entry changed: got %+v, want %+v", got, theirs)
	}
	if _, ok := disks["bpd-standing"]; !ok {
		t.Errorf("an entry that was already there was dropped; description now %q", desc)
	}
	if _, ok := disks["bpd-ours"]; ok {
		t.Errorf("our entry is still there after the removal; description now %q", desc)
	}
	if strings.TrimSpace(nonBOSH) != "operator note" {
		t.Errorf("free text outside the sentinel changed: got %q", nonBOSH)
	}
	if c.reads != 2 || c.ourWrites != 2 || c.refused != 1 || c.accepted != 2 {
		t.Errorf("reads=%d ourWrites=%d refused=%d accepted=%d, want 2, 2, 1, and 2 (the other holder's write and our second)",
			c.reads, c.ourWrites, c.refused, c.accepted)
	}
}

// TestRemoveParkerProvenance_GivesUpQuietlyAfterBoundedRounds covers a parker
// whose config changes under every round. The removal stops after its bounded
// number of rounds, writes nothing over the other writer, and leaves our
// entry for a later provenance write to collect.
func TestRemoveParkerProvenance_GivesUpQuietlyAfterBoundedRounds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ours := removeTestEntry(removeTestOurs, "scsi1", now.Add(-time.Hour))
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": ours}))
	c.beforeOurWrite = func(c *digestProvClient, write int) {
		key := fmt.Sprintf("bpd-theirs-%d", write)
		volid := fmt.Sprintf("local-lvm:vm-%d-disk-0", 9100+write)
		c.otherHolderWrites(withEntry(t, c, key, removeTestEntry(volid, fmt.Sprintf("scsi%d", 3+write), now)))
	}

	removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, removeTestOurs, "bpd-ours", ParkerConfig{})

	if c.ourWrites != parkerDescriptionWriteAttempts || c.reads != parkerDescriptionWriteAttempts {
		t.Fatalf("reads=%d ourWrites=%d, want %d of each", c.reads, c.ourWrites, parkerDescriptionWriteAttempts)
	}
	if c.refused != parkerDescriptionWriteAttempts || c.accepted != parkerDescriptionWriteAttempts {
		t.Errorf("refused=%d accepted=%d, want every one of our writes refused and every other write accepted",
			c.refused, c.accepted)
	}
	_, disks, _ := parseParkerSentinel(c.description())
	if _, ok := disks["bpd-ours"]; !ok {
		t.Errorf("our entry should be left for later collection when every round is refused")
	}
	for write := 1; write <= parkerDescriptionWriteAttempts; write++ {
		if _, ok := disks[fmt.Sprintf("bpd-theirs-%d", write)]; !ok {
			t.Errorf("the other writer's entry from round %d was lost", write)
		}
	}
}

// TestRemoveParkerProvenance_StopsWhenOurEntryIsAlreadyGone covers a refused
// round after which another writer has already removed our entry. The second
// read finds nothing of ours, so nothing more is written.
func TestRemoveParkerProvenance_StopsWhenOurEntryIsAlreadyGone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ours := removeTestEntry(removeTestOurs, "scsi1", now.Add(-time.Hour))
	theirs := removeTestEntry("local-lvm:vm-9003-disk-0", "scsi3", now)
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": ours}))
	c.beforeOurWrite = func(c *digestProvClient, write int) {
		if write != 1 {
			return
		}
		desc, _ := c.cfg["description"].(string)
		nonBOSH, disks, raw := parseParkerSentinel(desc)
		delete(disks, "bpd-ours")
		disks["bpd-theirs"] = theirs
		out, err := renderParkerSentinel(nonBOSH, disks, raw)
		if err != nil {
			t.Errorf("render sentinel: %v", err)
			return
		}
		c.otherHolderWrites(out)
	}

	removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, removeTestOurs, "bpd-ours", ParkerConfig{})

	if c.reads != 2 || c.ourWrites != 1 {
		t.Errorf("reads=%d ourWrites=%d, want 2 reads and only the first, refused, write", c.reads, c.ourWrites)
	}
	_, disks, _ := parseParkerSentinel(c.description())
	if _, ok := disks["bpd-theirs"]; !ok || len(disks) != 1 {
		t.Errorf("want only the other writer's entry left, got %v", disks)
	}
}

// TestRemoveParkerProvenance_WritesNothingWithoutADigest covers a config read
// that carries no digest. Such a write could not be guarded, so the removal
// leaves our entry for later collection rather than risk overwriting
// another holder's entry.
func TestRemoveParkerProvenance_WritesNothingWithoutADigest(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ours := removeTestEntry(removeTestOurs, "scsi1", now.Add(-time.Hour))
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{"bpd-ours": ours}))
	c.omitDigest = true
	before := c.description()

	removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, removeTestOurs, "bpd-ours", ParkerConfig{})

	if c.ourWrites != 0 {
		t.Errorf("ourWrites=%d, want none without a digest to guard the write", c.ourWrites)
	}
	if got := c.description(); got != before {
		t.Errorf("description changed without a write: got %q, want %q", got, before)
	}
}

// TestRemoveParkerProvenance_StopsOnAnyOtherWriteFailure covers a write PVE
// refuses for a reason other than a stale digest. A second round would only
// meet the same refusal, so the removal logs and stops after one write.
func TestRemoveParkerProvenance_StopsOnAnyOtherWriteFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ours := removeTestEntry(removeTestOurs, "scsi1", now.Add(-time.Hour))
	c := newDigestProvClient(removeTestDescription(t, map[string]parkerProvEntry{removeTestOurs: ours}))
	c.writeErr = sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed (/vms/90000, VM.Config.Options)\n"}`))

	removeParkerProvenance(context.Background(), c, log.NewNopLogger(), removeTestNode, removeTestParker, removeTestOurs, "", ParkerConfig{})

	if c.reads != 1 || c.ourWrites != 1 {
		t.Errorf("reads=%d ourWrites=%d, want one of each", c.reads, c.ourWrites)
	}
	_, disks, _ := parseParkerSentinel(c.description())
	if _, ok := disks[removeTestOurs]; !ok {
		t.Errorf("our legacy-keyed entry should stay after a refused write")
	}
}
