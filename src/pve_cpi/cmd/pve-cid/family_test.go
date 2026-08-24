package main

import (
	"reflect"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestDecodeCID_StemcellLight(t *testing.T) {
	cid := ":light:nfs:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2"
	d, err := DecodeCID(cid)
	if err != nil {
		t.Fatalf("DecodeCID(%q) error = %v", cid, err)
	}
	if d.Family != FamilyStemcellLight {
		t.Errorf("Family = %q, want %q", d.Family, FamilyStemcellLight)
	}
	if d.Storage != "nfs" {
		t.Errorf("Storage = %q, want %q", d.Storage, "nfs")
	}
	if d.VolumePath != "import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2" {
		t.Errorf("VolumePath = %q", d.VolumePath)
	}
	if d.Filename != "bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2" {
		t.Errorf("Filename = %q", d.Filename)
	}
	if d.SHA8 != "cafebabe" {
		t.Errorf("SHA8 = %q, want %q", d.SHA8, "cafebabe")
	}
}

func TestDecodeCID_StemcellHeavy(t *testing.T) {
	cid := ":heavy:local:import/bosh-stemcell-centos-7-3586.10-deadbeef.qcow2"
	d, err := DecodeCID(cid)
	if err != nil {
		t.Fatalf("DecodeCID(%q) error = %v", cid, err)
	}
	if d.Family != FamilyStemcellHeavy {
		t.Errorf("Family = %q, want %q", d.Family, FamilyStemcellHeavy)
	}
	if d.Storage != "local" {
		t.Errorf("Storage = %q, want %q", d.Storage, "local")
	}
	if d.SHA8 != "deadbeef" {
		t.Errorf("SHA8 = %q, want %q", d.SHA8, "deadbeef")
	}
}

// TestDecodeCID_StorageNamedLightOrHeavy proves the edge case (storage literally
// named "light"/"heavy" misclassified) stays fixed: the leading ':'
// discriminator makes the kind segment unambiguous regardless of what the
// storage happens to be named.
func TestDecodeCID_StorageNamedLightOrHeavy(t *testing.T) {
	cid := ":light:light:import/bosh-stemcell-x-1-aaaaaaaa.qcow2"
	d, err := DecodeCID(cid)
	if err != nil {
		t.Fatalf("DecodeCID(%q) error = %v", cid, err)
	}
	if d.Family != FamilyStemcellLight {
		t.Errorf("Family = %q, want %q", d.Family, FamilyStemcellLight)
	}
	if d.Storage != "light" {
		t.Errorf("Storage = %q, want %q (storage named 'light' must be preserved)", d.Storage, "light")
	}
}

func TestDecodeCID_DiskPVD(t *testing.T) {
	bareCID := "local-lvm:vm-100-disk-0"
	cid, err := pve.EncodeDiskCID(bareCID, &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "az1"})
	if err != nil {
		t.Fatalf("EncodeDiskCID: %v", err)
	}
	d, err := DecodeCID(cid)
	if err != nil {
		t.Fatalf("DecodeCID(%q) error = %v", cid, err)
	}
	if d.Family != FamilyDiskPVD {
		t.Errorf("Family = %q, want %q", d.Family, FamilyDiskPVD)
	}
	if d.Volid != bareCID {
		t.Errorf("Volid = %q, want %q", d.Volid, bareCID)
	}
	if d.DiskStorage != "local-lvm" || d.DiskVolume != "vm-100-disk-0" {
		t.Errorf("DiskStorage/DiskVolume = %q/%q", d.DiskStorage, d.DiskVolume)
	}
	if d.Pool != "local-lvm" || d.Node != "pve1" || d.AZ != "az1" {
		t.Errorf("meta = pool=%q node=%q az=%q", d.Pool, d.Node, d.AZ)
	}
	if d.OverTarget {
		t.Errorf("OverTarget = true for a short CID")
	}
}

func TestDecodeCID_DiskPVD_NoMeta(t *testing.T) {
	bareCID := "local:9001/vm-9001-disk-0.qcow2"
	cid, err := pve.EncodeDiskCID(bareCID, nil)
	if err != nil {
		t.Fatalf("EncodeDiskCID: %v", err)
	}
	d, err := DecodeCID(cid)
	if err != nil {
		t.Fatalf("DecodeCID(%q) error = %v", cid, err)
	}
	if d.Volid != bareCID {
		t.Errorf("Volid = %q, want %q", d.Volid, bareCID)
	}
	if d.Pool != "" || d.Node != "" || d.AZ != "" || d.Opts != nil {
		t.Errorf("expected empty meta, got pool=%q node=%q az=%q opts=%v", d.Pool, d.Node, d.AZ, d.Opts)
	}
}

func TestDecodeCID_DiskPVZ_SelfGenerated(t *testing.T) {
	// Force a bareCID long enough that the plain pvd- form exceeds
	// DiskCIDLengthTarget, guaranteeing EncodeDiskCIDCompressed emits the
	// pvz- form (the "generate one via EncodeDiskCIDCompressed in the test
	// itself" fixture). A repeating-character payload compresses so well
	// that the resulting pvz- CID would land back under the target (which is
	// the correct, intended behavior of compression — not a useful fixture
	// for OverTarget=true), so this uses a low-entropy-resistant pseudo-
	// random hex payload instead.
	bareCID := "ceph-rbd-nvme-tier1:" + pseudoRandomHex(600)
	meta := &pve.DiskCIDMeta{
		Pool: "ceph-rbd-nvme-tier1",
		Node: "prod-pmx-node-07",
		AZ:   "az-rack-2",
		Opts: map[string]string{"iothread": "1", "cache": "writeback"},
	}
	cid, err := pve.EncodeDiskCIDCompressed(bareCID, meta)
	if err != nil {
		t.Fatalf("EncodeDiskCIDCompressed: %v", err)
	}
	if len(cid) < 4 || cid[:4] != "pvz-" {
		t.Fatalf("expected a pvz- envelope for an oversized, incompressible bareCID, got %q", cid)
	}

	d, err := DecodeCID(cid)
	if err != nil {
		t.Fatalf("DecodeCID(%q) error = %v", cid, err)
	}
	if d.Family != FamilyDiskPVZ {
		t.Errorf("Family = %q, want %q", d.Family, FamilyDiskPVZ)
	}
	if d.Volid != bareCID {
		t.Errorf("Volid mismatch after pvz- round trip")
	}
	if !reflect.DeepEqual(d.Opts, meta.Opts) {
		t.Errorf("Opts = %v, want %v", d.Opts, meta.Opts)
	}
	if !d.OverTarget {
		t.Errorf("OverTarget = false for a CID this size (target=%d, got=%d)", d.LengthTarget, d.EncodedLength)
	}
}

// TestDecodeCID_DiskPVZ_FrozenGoEncoderFixture pins decode of a pvz- CID
// emitted by the Go encoder (EncodeDiskCIDCompressed), captured verbatim
// from scripts/_pve_verify_test.py's TestPvzGoEncoderFixture (the same
// frozen fixture that pins the Python decoder). This is the
// tool/CPI/scripts wire-format alignment guard: if internal/pve's envelope
// format ever drifts, this test fails alongside the Python fixture rather
// than silently diverging.
func TestDecodeCID_DiskPVZ_FrozenGoEncoderFixture(t *testing.T) {
	const frozen = "pvz-H4sIAAAAAAAC_2yOQWrEMAxF7_LXVquZWbT4MsWxDTGpI1c2SZmQuxcnBLqY5RPvfbRhgYWPZSQdAs1LjtRS1Jt9" +
		"ML8vmR7MFFKdiN9-vKx3GGTYDUXk-3UJg1lChEVRCVTyL3Um_oCBe8LCPUmdn6iPSWm173nnx96smlocnJ9gEFL1TgMs" +
		"ZIZBklK_Dvxk5uuw6v9DGzW6rvQ38nAFtztffPgn13qa-77_BQAA__-aFK-nCAEAAA"

	d, err := DecodeCID(frozen)
	if err != nil {
		t.Fatalf("DecodeCID(frozen fixture) error = %v", err)
	}
	if d.Family != FamilyDiskPVZ {
		t.Errorf("Family = %q, want %q", d.Family, FamilyDiskPVZ)
	}
	wantVolid := "ceph-rbd-nvme-tier1:300/vm-300-disk-0.qcow2"
	if d.Volid != wantVolid {
		t.Errorf("Volid = %q, want %q", d.Volid, wantVolid)
	}
	if d.DiskStorage != "ceph-rbd-nvme-tier1" || d.DiskVolume != "300/vm-300-disk-0.qcow2" {
		t.Errorf("DiskStorage/DiskVolume = %q/%q", d.DiskStorage, d.DiskVolume)
	}
	if d.Pool != "ceph-rbd-nvme-tier1" {
		t.Errorf("Pool = %q, want %q", d.Pool, "ceph-rbd-nvme-tier1")
	}
	if d.Node != "prod-pmx-node-07" {
		t.Errorf("Node = %q, want %q", d.Node, "prod-pmx-node-07")
	}
	if d.AZ != "az-rack-2" {
		t.Errorf("AZ = %q, want %q", d.AZ, "az-rack-2")
	}
	wantOpts := map[string]string{
		"cache":    "writeback",
		"discard":  "on",
		"iops_rd":  "8000",
		"iops_wr":  "8000",
		"iothread": "1",
		"mbps_rd":  "120",
		"mbps_wr":  "120",
		"ssd":      "1",
	}
	if !reflect.DeepEqual(d.Opts, wantOpts) {
		t.Errorf("Opts = %v, want %v", d.Opts, wantOpts)
	}
}

func TestDecodeCID_VM(t *testing.T) {
	d, err := DecodeCID("5042")
	if err != nil {
		t.Fatalf("DecodeCID error = %v", err)
	}
	if d.Family != FamilyVM {
		t.Errorf("Family = %q, want %q", d.Family, FamilyVM)
	}
	if d.VMID != 5042 {
		t.Errorf("VMID = %d, want 5042", d.VMID)
	}
}

func TestDecodeCID_VM_ZeroRejected(t *testing.T) {
	if _, err := DecodeCID("0"); err == nil {
		t.Fatal("expected error decoding VMID 0")
	}
}

func TestDecodeCID_Snapshot(t *testing.T) {
	d, err := DecodeCID("5042:before-upgrade")
	if err != nil {
		t.Fatalf("DecodeCID error = %v", err)
	}
	if d.Family != FamilySnapshot {
		t.Errorf("Family = %q, want %q", d.Family, FamilySnapshot)
	}
	if d.VMCID != "5042" {
		t.Errorf("VMCID = %q, want %q", d.VMCID, "5042")
	}
	if d.VMID != 5042 {
		t.Errorf("VMID = %d, want 5042", d.VMID)
	}
	if d.SnapshotName != "before-upgrade" {
		t.Errorf("SnapshotName = %q, want %q", d.SnapshotName, "before-upgrade")
	}
}

func TestDecodeCID_GarbageRejected(t *testing.T) {
	cases := []string{
		"",
		"garbage",
		"local:vm-100-disk-0",           // bare volid — no longer accepted (K5)
		"template:6042",                 // retired template CID grammar
		"light:local:import/x.qcow2",    // retired single-colon light: prefix
		":unknown:local:import/x.qcow2", // unknown kind segment
		":light::import/x.qcow2",        // doubled/empty storage segment
		"pvd-",                          // empty envelope payload
		"pvd-not-valid-base64!!!",       // invalid base64url
		"pvz-",                          // empty compressed payload
		"abc:def",                       // non-digit prefix before ':'
		":5042:name",                    // leading colon but not a stemcell kind
	}
	for _, c := range cases {
		if _, err := DecodeCID(c); err == nil {
			t.Errorf("DecodeCID(%q) expected error, got none", c)
		}
	}
}

func TestSHA8FromStemcellFilename(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		{"bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2", "cafebabe"},
		{"bosh-stemcell-x-1-00000000.qcow2", "00000000"},
		{"not-a-stemcell-file.qcow2", ""},
		{"bosh-stemcell-x-1-toolong123.qcow2", ""},
		{"bosh-stemcell-x-1-short.qcow2", ""},
	}
	for _, c := range cases {
		if got := sha8FromStemcellFilename(c.filename); got != c.want {
			t.Errorf("sha8FromStemcellFilename(%q) = %q, want %q", c.filename, got, c.want)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"", false},
		{"0", true},
		{"12345", true},
		{"12a45", false},
		{"-1", false},
		{"1.5", false},
	}
	for _, c := range cases {
		if got := isAllDigits(c.s); got != c.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// repeatString avoids importing strings solely for a test-local repeat helper.
func repeatString(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// pseudoRandomHex returns n deterministic pseudo-random hex characters, using
// a fixed-seed xorshift generator (not crypto/rand: reproducibility across
// runs matters more than unpredictability for a test fixture) with high
// enough entropy that gzip cannot meaningfully shrink it — needed to prove
// the DiskCIDLengthTarget-exceeded (OverTarget) path survives compression.
func pseudoRandomHex(n int) string {
	const hexDigits = "0123456789abcdef"
	state := uint64(0x9E3779B97F4A7C15)
	out := make([]byte, n)
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = hexDigits[state%16]
	}
	return string(out)
}
