// detach_disk_snapshot_defer_internal_test.go — the deferred-park contract
// for stable-ID disks under the snapshot bypass: PVE refuses to reassign a
// snapshot-referenced volume, so a bypassed detach reports success with the
// disk off the bus and the park deferred behind the parker's intent record.
// A retry while the snapshot exists is idempotent success; the first
// mutating call after the snapshot is gone converges the disk to parked.
package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

func TestHandleDetachStableID_SnapshotRefusalDefersPark(t *testing.T) {
	t.Parallel()

	const birth = "data:vm-9001-disk-0"
	const attachedVolid = "data:vm-700-disk-1"
	snapshotRefusal := func() error {
		return errors.New("API request failed: Can't move disk used by a snapshot to another VM")
	}
	c := newIDFakeClient(map[int]map[string]any{
		700:   {"scsi1": attachedVolid + ",serial=" + idTestToken + ",size=10G"},
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
	deps := idTestDeps(c)
	diskCID := overlayCID(t, birth, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
	ctx := context.Background()

	resolve := func() resolvedDisk {
		t.Helper()
		bare, meta, decErr := decodeDiskCID(ctx, deps, "detach_disk", diskCID)
		if decErr != nil {
			t.Fatalf("decode: %v", decErr)
		}
		rd, resolveErr := resolveDiskForOp(ctx, deps, "detach_disk", diskCID, bare, meta)
		if resolveErr != nil {
			t.Fatalf("resolve: %v", resolveErr)
		}
		return rd
	}

	// 1. Bypassed detach: the reassignment is snapshot-refused, yet the
	// detach succeeds — off the bus, park deferred.
	c.moveErr = snapshotRefusal()
	if err := handleDetachStableID(ctx, deps, "700", 700, resolve()); err != nil {
		t.Fatalf("detach under snapshot refusal: %v", err)
	}
	if _, present := c.configs[700]["scsi1"]; present {
		t.Error("source slot still active after the bypassed detach")
	}
	if got, _ := c.configs[700]["unused0"].(string); got != attachedVolid {
		t.Errorf("unused0 = %q, want the demoted volume %q", got, attachedVolid)
	}
	parkerDesc, _ := c.configs[90000]["description"].(string)
	if !strings.Contains(parkerDesc, idTestToken) {
		t.Fatalf("parker sentinel %q must carry the intent record", parkerDesc)
	}

	// 2. Detach retry while the snapshot still exists: idempotent success,
	// state unchanged.
	rd2 := resolve()
	if rd2.intent == nil {
		t.Fatal("retry must resolve the disk through the parker's intent record")
	}
	c.moveErr = snapshotRefusal()
	if err := handleDetachStableID(ctx, deps, "700", 700, rd2); err != nil {
		t.Fatalf("detach retry under snapshot refusal: %v", err)
	}
	if got, _ := c.configs[700]["unused0"].(string); got != attachedVolid {
		t.Errorf("unused0 after retry = %q, want %q untouched", got, attachedVolid)
	}

	// 3. Snapshot deleted: the next detach resumes the transfer and the disk
	// converges to parked with its serial applied.
	if err := handleDetachStableID(ctx, deps, "700", 700, resolve()); err != nil {
		t.Fatalf("detach after snapshot deletion: %v", err)
	}
	_, parkedStr := findSlotWithSerial(t, c.configs[90000], idTestToken)
	if !strings.HasPrefix(parkedStr, "data:vm-90000-disk-") {
		t.Errorf("parked drive string = %q, want a parker-named volume", parkedStr)
	}
	if _, present := c.configs[700]["unused0"]; present {
		t.Error("source unused entry remains after the resumed transfer")
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volumes destroyed: %v", c.destroyed)
	}
}
