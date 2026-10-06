package handlers

// A clone or a backup restore of a guest copies its drive lines, serials
// included, onto volumes of the copy's own. These tests put the serial of a
// stable-ID disk A on VM 777 and on VM 888, each with its own volume, and call
// every disk handler with A's CID. Before the fix the resolver took whichever
// guest it read first, so a handler could act on the copy's volume as A.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// buildCopiedSerial gives VMs 777 and 888 each a volume of their own whose
// drive line carries A's serial, the way a clone of 777 numbered 888 would.
func buildCopiedSerial(t *testing.T) *strandedDisk {
	t.Helper()
	s := legacyBirthNameFixture(t)
	s.mintStableDisk(t, birthNameCollision)
	for _, vmid := range []int{777, 888} {
		volid := fmt.Sprintf("a:%d/vm-%d-disk-1.raw", vmid, vmid)
		s.client.state.volumes[volid] = &nodes.GetStorageContentResponse{Size: 3 << 30, Format: "raw"}
		s.client.state.configs[vmid]["scsi1"] = volid + ",serial=" + s.token + ",size=3G"
	}
	return s
}

// TestCopiedSerialRefusesEveryHandler calls each handler with A's CID while
// two guests carry A's serial. Every call refuses permanently, names both
// guests and how to tell the copy, and writes nothing.
func TestCopiedSerialRefusesEveryHandler(t *testing.T) {
	for _, tc := range birthNameCalls {
		t.Run(tc.name, func(t *testing.T) {
			s := buildCopiedSerial(t)
			before := s.clusterState(t)
			err := tc.call(t, s)
			var typed *cpierrors.Error
			if err == nil || !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
				t.Fatalf("%s = %v, want a permanent CloudError", tc.name, err)
			}
			copied, ok := pve.IsDiskIdentityCopied(err)
			if !ok || copied.StableID != s.token || len(copied.Holders) != 2 {
				t.Fatalf("%s = %v, want a DiskIdentityCopiedError with two holders", tc.name, err)
			}
			for _, want := range []string{
				"slot scsi1 of VM 777 on node n1 with volume a:777/vm-777-disk-1.raw",
				"slot scsi1 of VM 888 on node n1 with volume a:888/vm-888-disk-1.raw",
				"a qmclone task under the VM it copied", "bosh instances --details", "serial=" + s.token, pve.DiskIdentityCopiedRunbook,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s = %v\nwant it to contain %q", tc.name, err, want)
				}
			}
			if after := s.clusterState(t); after != before {
				t.Fatalf("%s wrote to the cluster:\nbefore %s\nafter  %s", tc.name, before, after)
			}
			c := s.client
			if c.resizeCalls != 0 || len(c.snapshots) != 0 || c.deletes != 0 || c.moves != 0 || c.moveCalls != 0 || len(s.recorder.submissions) != 0 {
				t.Fatalf("%s resized %d, snapshotted %d, deleted %d, moved %d, and destroyed %d", tc.name,
					c.resizeCalls, len(c.snapshots), c.deletes, c.moves, len(s.recorder.submissions))
			}
		})
	}
}

// TestCopiedSerialHasDiskReportsTheDiskPresent shows has_disk answering true,
// because one of the two guests holds A. Answering false would send bosh cck
// after a disk that exists.
func TestCopiedSerialHasDiskReportsTheDiskPresent(t *testing.T) {
	s := buildCopiedSerial(t)
	before := s.clusterState(t)
	result, err := HandleHasDisk(s.deps).Handle(context.Background(), []json.RawMessage{planJSON(t, s.cid)}, jsonrpc.Context{})
	if err != nil || result != true {
		t.Fatalf("has_disk = %v, %v; want true", result, err)
	}
	if after := s.clusterState(t); after != before {
		t.Fatalf("has_disk wrote to the cluster:\nbefore %s\nafter  %s", before, after)
	}
}

// TestDiskIdentityCopiedRunbookHeadingExists pins the heading the refusal
// quotes, so a rename of the section can't leave the pointer dangling.
func TestDiskIdentityCopiedRunbookHeadingExists(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	heading := strings.TrimSuffix(strings.TrimPrefix(pve.DiskIdentityCopiedRunbook, `see "`), `" in docs/troubleshooting.md of bosh-proxmox-cpi-release`)
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "#") && strings.TrimSpace(strings.TrimLeft(line, "#")) == heading {
			return
		}
	}
	t.Fatalf("docs/troubleshooting.md has no heading %q", heading)
}
