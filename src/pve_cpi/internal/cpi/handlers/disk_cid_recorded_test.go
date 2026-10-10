package handlers

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestRecordedDiskCID pins which CID delete_vm hands the transfer that
// preserves a disk. It takes the CID the VM recorded under the disk's stable
// ID or its volid, and only one that decodes to the same stable ID, because
// the transfer checks the disk's birth name against it.
func TestRecordedDiskCID(t *testing.T) {
	t.Parallel()
	const (
		stableID = "bpd-00112233aabbccdd"
		volid    = "data:vm-700-disk-1"
	)
	encode := func(t *testing.T, id string) string {
		t.Helper()
		cid, err := pve.EncodeDiskCID("data:vm-700-disk-1", &pve.DiskCIDMeta{Pool: "data", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		return cid
	}
	desc := func(t *testing.T, cids map[string]string) string {
		t.Helper()
		b, err := json.Marshal(map[string]any{"bosh_attached_disks": cids})
		if err != nil {
			t.Fatal(err)
		}
		return "<!--BOSH:" + string(b) + "-->"
	}
	own := encode(t, stableID)
	for name, tc := range map[string]struct {
		cids map[string]string
		want string
	}{
		"keyed by the stable ID":  {map[string]string{stableID: own}, own},
		"keyed by the volid":      {map[string]string{volid: own}, own},
		"stable ID key wins":      {map[string]string{stableID: own, volid: encode(t, "bpd-ffffffffffffffff")}, own},
		"another disk's CID":      {map[string]string{stableID: encode(t, "bpd-ffffffffffffffff")}, ""},
		"a bare CID":              {map[string]string{stableID: "data:vm-700-disk-1"}, ""},
		"a CID that won't decode": {map[string]string{stableID: "pvd-!!!"}, ""},
		"nothing recorded":        {map[string]string{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := recordedDiskCID(desc(t, tc.cids), stableID, volid); got != tc.want {
				t.Fatalf("recordedDiskCID = %q, want %q", got, tc.want)
			}
		})
	}
	if got := recordedDiskCID("", stableID, volid); got != "" {
		t.Fatalf("recordedDiskCID of an empty description = %q, want none", got)
	}
}
