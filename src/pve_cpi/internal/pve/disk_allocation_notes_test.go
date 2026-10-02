package pve

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// notesRemovalClient holds one VM's description and digest. Its write
// refuses a stale digest with the answer qemu-server gives, and its readback
// passes the stored description through readback, so a row can stand in for
// another writer that changes the description right after the write lands.
type notesRemovalClient struct {
	parkerLockClient
	desc     string
	digest   string
	refuse   bool
	readback func(stored string) string
	writes   []sdknodes.UpdateQemuConfigParams
	reads    int
}

func (c *notesRemovalClient) Nodes() sdknodes.Service {
	return &fakeNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			c.writes = append(c.writes, *params)
			if c.refuse || params.Digest == nil || *params.Digest != c.digest {
				return sdkerrors.ParseAPIError(500, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`))
			}
			c.desc = *params.Description
			c.digest += "-next"
			return nil
		},
	}
}

func (c *notesRemovalClient) QEMU() qemu.Service {
	return &fakeQEMUService{
		configFn: func(context.Context, string, int) (map[string]any, error) {
			c.reads++
			desc := c.desc
			if c.readback != nil {
				desc = c.readback(desc)
			}
			return map[string]any{"description": desc, "digest": c.digest}, nil
		},
	}
}

// descriptionNotes is the note content of one description, one map per
// carrier.
type descriptionNotes struct {
	allocations map[string]DiskAllocationProvenance
	disks       map[string]string
	overlays    map[string]map[string]string
}

func (n descriptionNotes) render(t *testing.T) string {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for key, carrier := range map[string]any{
		diskAllocationsKey:         n.allocations,
		attachedDisksSentinelKey:   n.disks,
		DiskOptOverlaysSentinelKey: n.overlays,
	} {
		encoded, err := json.Marshal(carrier)
		if err != nil {
			t.Fatal(err)
		}
		raw[key] = encoded
	}
	desc, err := RenderSentinel("operator text", raw)
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

// TestRemoveDescriptionNotesOutcomes covers the three ways the pinned removal
// ends. A clean removal takes out only the notes it names, in one write that
// carries the read's digest. A digest refusal comes back as PVE's answer and
// leaves the description alone. A removed note that's back on the readback
// returns ErrDescriptionNoteReturned for each carrier, which the handlers
// treat as retriable. A note that another writer adds after the write lands
// isn't one the removal named, so it doesn't fail the call.
func TestRemoveDescriptionNotesOutcomes(t *testing.T) {
	ours, theirs := lifecycleEntry(), lifecycleEntry()
	theirs.AllocationID = "fedcba98-7654-4321-8fed-cba987654321"
	theirs.Volid = "pool:100/vm-100-disk-1.raw"
	before := func() descriptionNotes {
		return descriptionNotes{
			allocations: map[string]DiskAllocationProvenance{"ours": ours, "theirs": theirs},
			disks:       map[string]string{"pool:100/vm-100-disk-0.raw": "cid-ours", "pool:100/vm-100-disk-1.raw": "cid-theirs"},
			overlays:    map[string]map[string]string{"pool:100/vm-100-disk-0.raw": {"cache": "none"}, "pool:100/vm-100-disk-1.raw": {"cache": "writeback"}},
		}
	}
	after := func() descriptionNotes {
		n := before()
		delete(n.allocations, "ours")
		delete(n.disks, "pool:100/vm-100-disk-0.raw")
		delete(n.overlays, "pool:100/vm-100-disk-0.raw")
		return n
	}
	keys := DescriptionNoteKeys{
		Allocations:   []string{"ours"},
		AttachedDisks: []string{"pool:100/vm-100-disk-0.raw"},
		Overlays:      []string{"pool:100/vm-100-disk-0.raw"},
	}
	withBack := func(restore func(n, was descriptionNotes)) func(string) string {
		return func(string) string {
			n := after()
			restore(n, before())
			return n.render(t)
		}
	}
	rows := map[string]struct {
		refuse     bool
		readback   func(string) string
		wantReturn string
	}{
		"clean removal":  {},
		"digest refusal": {refuse: true},
		"another writer adds a note after": {readback: withBack(func(n, _ descriptionNotes) {
			n.disks["pool:100/vm-100-disk-2.raw"] = "cid-new"
		})},
		"allocation entry comes back": {wantReturn: "an allocation entry", readback: withBack(func(n, was descriptionNotes) {
			n.allocations["ours"] = was.allocations["ours"]
		})},
		"attached-disk entry comes back": {wantReturn: "an attached-disk entry", readback: withBack(func(n, was descriptionNotes) {
			n.disks["pool:100/vm-100-disk-0.raw"] = was.disks["pool:100/vm-100-disk-0.raw"]
		})},
		"drive-option overlay comes back": {wantReturn: "a drive-option overlay", readback: withBack(func(n, was descriptionNotes) {
			n.overlays["pool:100/vm-100-disk-0.raw"] = was.overlays["pool:100/vm-100-disk-0.raw"]
		})},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			original := before().render(t)
			c := &notesRemovalClient{desc: original, digest: "d1", refuse: row.refuse, readback: row.readback}
			cfg := map[string]any{"description": original, "digest": "d1"}
			err := RemoveDescriptionNotes(context.Background(), c, "node-a", 100, cfg, keys)

			if len(c.writes) != 1 || c.writes[0].Digest == nil || *c.writes[0].Digest != "d1" {
				t.Fatalf("writes = %+v, want one write pinned to the read's digest d1", c.writes)
			}
			if c.writes[0].Delete != nil || c.writes[0].Protection != nil {
				t.Fatalf("write = %+v, want a description-only write", c.writes[0])
			}
			switch {
			case row.refuse:
				if !IsConfigDigestRefusal(err) || errors.Is(err, ErrDescriptionNoteReturned) {
					t.Fatalf("err = %v, want PVE's digest refusal", err)
				}
				if c.desc != original || c.reads != 0 {
					t.Fatalf("refused write changed the description or read it back: reads=%d desc=%q", c.reads, c.desc)
				}
				return
			case row.wantReturn != "":
				if !errors.Is(err, ErrDescriptionNoteReturned) || !strings.HasSuffix(err.Error(), row.wantReturn) || IsConfigDigestRefusal(err) {
					t.Fatalf("err = %v, want ErrDescriptionNoteReturned for %s", err, row.wantReturn)
				}
			case err != nil:
				t.Fatalf("RemoveDescriptionNotes: %v", err)
			}
			if c.reads != 1 {
				t.Fatalf("reads = %d, want one readback", c.reads)
			}
			requireNotes(t, c.desc, after())
		})
	}
}

// requireNotes checks that desc carries exactly want's notes, and that the
// text outside the sentinel came through.
func requireNotes(t *testing.T, desc string, want descriptionNotes) {
	t.Helper()
	nonBOSH, disks, _ := parseAttachedDisksSentinel(desc)
	_, overlays, _ := parseDiskOptOverlaysSentinel(desc)
	allocations, err := ParseDiskAllocationProvenance(desc)
	if err != nil {
		t.Fatal(err)
	}
	if nonBOSH != "operator text" || !maps.Equal(allocations, want.allocations) || !maps.Equal(disks, want.disks) ||
		!maps.EqualFunc(overlays, want.overlays, maps.Equal) {
		t.Fatalf("description = %q, want notes %+v under the operator text", desc, want)
	}
}
