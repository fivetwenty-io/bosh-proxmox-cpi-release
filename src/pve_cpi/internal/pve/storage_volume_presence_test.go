package pve_test

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// contentProbe scripts a listing for the observation helpers, with the audit
// visibility proof answering cleanly so the membership answer is the only thing
// under test. It reuses the client fakes from volume_absence_test.go.
func contentProbe(listing *nodes.ListStorageContentResponse) absenceProbe {
	return absenceProbe{
		existsFn: func(context.Context, string, string, string) (bool, error) {
			return false, nil
		},
		listFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (
			*nodes.ListStorageContentResponse, error) {
			return listing, nil
		},
		visible: true,
	}
}

// TestObserveStorageVolumePresence_UnchangedByTheCountVariant pins the
// membership answer the managed allocation paths depend on. Those callers were
// not changed when the count-carrying entry point arrived, so their observation
// must still read exactly as it did.
func TestObserveStorageVolumePresence_UnchangedByTheCountVariant(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		listing []string
		want    bool
	}{
		{"the volid is listed", []string{absenceVolid, "nfs-images:9000/vm-9000-disk-0.qcow2"}, true},
		{"another storage's volumes only", []string{"nfs-images:9000/vm-9000-disk-0.qcow2"}, false},
		{"an empty listing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := contentProbe(listingOf(t, tc.listing...))
			found, err := pve.ObserveStorageVolumePresence(context.Background(), probe.client(), "pve-01", absenceVolid)
			if err != nil {
				t.Fatalf("ObserveStorageVolumePresence: %v", err)
			}
			if found != tc.want {
				t.Fatalf("found = %v, want %v", found, tc.want)
			}
		})
	}
}

// TestObserveStorageVolumeContent_ReportsTheListingLength pins the count that
// separates a genuinely empty storage from a dir storage whose mount went away.
// PVE lists the second one as an empty array rather than failing, so the count
// is the only thing that tells the two apart.
func TestObserveStorageVolumeContent_ReportsTheListingLength(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		listing    []string
		wantFound  bool
		wantListed int
	}{
		{"an empty listing", nil, false, 0},
		{"two other volumes", []string{
			"nfs-images:9000/vm-9000-disk-0.qcow2",
			"nfs-images:9001/vm-9001-disk-0.qcow2",
		}, false, 2},
		{"the volid among others", []string{absenceVolid, "nfs-images:9000/vm-9000-disk-0.qcow2"}, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := contentProbe(listingOf(t, tc.listing...))
			found, listed, err := pve.ObserveStorageVolumeContent(
				context.Background(), probe.client(), "pve-01", absenceVolid)
			if err != nil {
				t.Fatalf("ObserveStorageVolumeContent: %v", err)
			}
			if found != tc.wantFound {
				t.Errorf("found = %v, want %v", found, tc.wantFound)
			}
			if listed != tc.wantListed {
				t.Errorf("listed = %d, want %d", listed, tc.wantListed)
			}
		})
	}
}

// TestObserveStorageVolumeContent_RejectsAMissingNode pins that the count
// variant guards its inputs the way the presence variant does.
func TestObserveStorageVolumeContent_RejectsAMissingNode(t *testing.T) {
	t.Parallel()
	probe := contentProbe(listingOf(t))
	found, listed, err := pve.ObserveStorageVolumeContent(context.Background(), probe.client(), "", absenceVolid)
	if err == nil {
		t.Fatal("an observation with no node to read from must fail rather than answer")
	}
	if found || listed != 0 {
		t.Fatalf("a failed observation must carry no answer; found=%v listed=%d", found, listed)
	}
}
