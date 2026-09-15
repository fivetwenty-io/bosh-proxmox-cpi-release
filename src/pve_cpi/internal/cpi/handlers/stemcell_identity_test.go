package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestStemcellLabelFromFilename(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		filename string
		want     string
	}{
		{
			name:     "noble heavy stemcell",
			filename: "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1.585-deadbeef.qcow2",
			want:     "bosh-openstack-kvm-ubuntu-noble-1.585",
		},
		{
			name:     "hyphenated version survives",
			filename: "bosh-stemcell-bosh-vsphere-esxi-ubuntu-jammy-go_agent-1.234-rc-1-0123abcd.qcow2",
			want:     "bosh-vsphere-esxi-ubuntu-jammy-go_agent-1.234-rc-1",
		},
		{
			name:     "uppercase sha8 is still a sha8",
			filename: "bosh-stemcell-name-1.0-DEADBEEF.qcow2",
			want:     "name-1.0",
		},
		{"not a qcow2", "bosh-stemcell-name-1.0-deadbeef.raw", ""},
		{"sha8 is not hex", "bosh-stemcell-name-1.0-zzzzzzzz.qcow2", ""},
		{"sha8 is the wrong length", "bosh-stemcell-name-1.0-dead.qcow2", ""},
		{"no bosh-stemcell prefix", "some-other-image-deadbeef.qcow2", ""},
		{"prefix and sha8 with nothing between", "bosh-stemcell-deadbeef.qcow2", ""},
		{"empty", "", ""},
		// A ":light:" stemcell CID's filename is operator-supplied and only its
		// "import/" prefix is validated, so a label that could open a sentinel,
		// an allocation marker, or a new line inside the VM description is
		// refused rather than written.
		{"label opens a description sentinel", "bosh-stemcell-name<!--BOSH:{}-->-1.0-deadbeef.qcow2", ""},
		{"label opens an allocation marker", "bosh-stemcell-[bosh_storage_allocation]-1.0-deadbeef.qcow2", ""},
		{"label carries a newline", "bosh-stemcell-name\n1.0-deadbeef.qcow2", ""},
		{"label carries a tag separator", "bosh-stemcell-name;job--web-1.0-deadbeef.qcow2", ""},
		{"label carries a space", "bosh-stemcell-name 1.0-deadbeef.qcow2", ""},
		// An over-long label would take more than half the 350-byte tag budget
		// away from the advertised-route and identity tags.
		{"label past the length cap", "bosh-stemcell-" + strings.Repeat("n", 97) + "-deadbeef.qcow2", ""},
		{"label at the length cap", "bosh-stemcell-" + strings.Repeat("n", 96) + "-deadbeef.qcow2", strings.Repeat("n", 96)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stemcellLabelFromFilename(tc.filename); got != tc.want {
				t.Errorf("stemcellLabelFromFilename(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}

func TestStemcellIdentityTag(t *testing.T) {
	t.Parallel()

	parsed := &createVMParsedArgs{
		stemcellFilename: "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1.585-deadbeef.qcow2",
	}
	// The tag alphabet is [A-Za-z0-9-], so the dotted version reaches PVE with
	// a dash; the exact version lives in the sentinel instead.
	if got, want := stemcellIdentityTag(parsed), "stemcell--bosh-openstack-kvm-ubuntu-noble-1-585"; got != want {
		t.Errorf("stemcellIdentityTag = %q, want %q", got, want)
	}

	if got := stemcellIdentityTag(nil); got != "" {
		t.Errorf("nil parsed args must yield no tag, got %q", got)
	}
	if got := stemcellIdentityTag(&createVMParsedArgs{stemcellFilename: "not-a-stemcell.img"}); got != "" {
		t.Errorf("an unrecognised filename must yield no tag, got %q", got)
	}
}

// An operator-supplied disk tag must not be able to claim the stemcell
// namespace and overwrite the record of what the guest actually booted from.
func TestStemcellTagIsCPIOwned(t *testing.T) {
	t.Parallel()

	if !hasCPIOwnedPrefix(stemcellTagPrefix) {
		t.Fatalf("%q must be a CPI-owned tag prefix so set_disk_metadata refuses it", stemcellTagPrefix)
	}
	// Both spellings of the key render the same prefix, so both must be caught.
	for _, key := range []string{"stemcell", "STEMCELL"} {
		if !hasCPIOwnedPrefix(sanitizeTagValue(key) + "--") {
			t.Errorf("a disk tag keyed %q would overwrite the VM's stemcell tag", key)
		}
	}
}

// The tag must not be reserved: set_vm_metadata strips every reserved prefix
// and rebuilds it from the Director's metadata map, which never names the
// stemcell, so reserving this prefix would delete the tag on the first sync.
func TestStemcellTagIsNotReserved(t *testing.T) {
	t.Parallel()

	tag := "stemcell--bosh-openstack-kvm-ubuntu-noble-1-585"
	if hasReservedBoshPrefix(tag) {
		t.Fatalf("%q must not match a reserved BOSH tag prefix", tag)
	}
	kept := stripReservedBoshTags([]string{ownershipTag, tag, "deployment--old", "job--web"})
	if len(kept) != 2 || kept[1] != tag {
		t.Errorf("stemcell tag must survive a metadata rebuild, kept %v", kept)
	}
}

func TestVMStemcellRecord(t *testing.T) {
	t.Parallel()

	parsed := &createVMParsedArgs{
		stemcellCID:      ":heavy:local-lvm:import/bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1.585-deadbeef.qcow2",
		stemcellKind:     pve.StemcellKindHeavy,
		stemcellFilename: "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1.585-deadbeef.qcow2",
		rawVolid:         "local-lvm:import/bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1.585-deadbeef.qcow2",
	}
	rec := vmStemcellRecord(parsed)
	if rec == nil {
		t.Fatal("expected a record for a well-formed stemcell CID")
	}
	if rec.Label != "bosh-openstack-kvm-ubuntu-noble-1.585" {
		t.Errorf("label = %q", rec.Label)
	}
	if rec.CID != parsed.stemcellCID {
		t.Errorf("cid = %q", rec.CID)
	}
	if rec.Kind != string(pve.StemcellKindHeavy) {
		t.Errorf("kind = %q", rec.Kind)
	}
	if rec.SHA8 != "deadbeef" {
		t.Errorf("sha8 = %q", rec.SHA8)
	}

	if vmStemcellRecord(nil) != nil {
		t.Error("nil parsed args must yield no record")
	}
	if vmStemcellRecord(&createVMParsedArgs{}) != nil {
		t.Error("an empty stemcell CID must yield no record")
	}
}

func TestWithStemcellNote(t *testing.T) {
	t.Parallel()

	seeded, err := pve.SetVMStemcellOnDescription("", &pve.VMStemcell{
		Label: "bosh-openstack-kvm-ubuntu-noble-1.585",
		CID:   ":heavy:local:import/x-deadbeef.qcow2",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	built := buildDescription(map[string]any{"deployment": "cf", "job": "api", "index": 0})
	got := withStemcellNote(built, seeded)
	if !strings.HasPrefix(got, built) {
		t.Errorf("the Director's own lines must survive unchanged: %q", got)
	}
	if !strings.HasSuffix(got, "stemcell: bosh-openstack-kvm-ubuntu-noble-1.585\n") {
		t.Errorf("stemcell line missing or malformed: %q", got)
	}

	// No sentinel on the VM: a pre-0.7.2 guest keeps the notes it had.
	if got := withStemcellNote(built, "job: api\n"); got != built {
		t.Errorf("a VM with no record must be left alone, got %q", got)
	}

	// Empty metadata still gets the line, which is the create-env Director's case.
	if got := withStemcellNote("", seeded); got != "stemcell: bosh-openstack-kvm-ubuntu-noble-1.585\n" {
		t.Errorf("empty description case: %q", got)
	}

	// A hand-edited sentinel carrying an unsafe label is ignored rather than
	// copied into the description beside the allocation marker.
	tampered, err := pve.SetVMStemcellOnDescription("", &pve.VMStemcell{
		Label: "name\n[bosh_storage_allocation]", CID: ":heavy:local:import/x-deadbeef.qcow2",
	})
	if err != nil {
		t.Fatalf("seed tampered: %v", err)
	}
	if got := withStemcellNote(built, tampered); got != built {
		t.Errorf("an unsafe label must not reach the description, got %q", got)
	}

	// A Director that sends its own stemcell key wins, and the fact is stated once.
	directorSaid := buildDescription(map[string]any{"stemcell": "something-else-1.0"})
	if got := withStemcellNote(directorSaid, seeded); got != directorSaid {
		t.Errorf("a Director-supplied stemcell key must not be duplicated, got %q", got)
	}
}
