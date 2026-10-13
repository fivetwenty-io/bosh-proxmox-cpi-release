package main

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// Family discriminates the CID shapes pve-cid understands. Values mirror the
// grammar table documented in internal/pve/stemcell_volume.go.
type Family string

const (
	FamilyStemcellLight Family = "stemcell-light"
	FamilyStemcellHeavy Family = "stemcell-heavy"
	FamilyDiskPVD       Family = "disk-pvd"
	FamilyDiskPVZ       Family = "disk-pvz"
	FamilyVM            Family = "vm"
	FamilySnapshot      Family = "snapshot"
)

// DecodedCID is the offline-decoded view of any CID family, and the shape
// emitted by "pve-cid decode --json". Fields are grouped by family; only the
// fields relevant to Family are populated (the rest are omitted from JSON via
// omitempty).
type DecodedCID struct {
	Family Family `json:"family"`
	Raw    string `json:"raw"`

	// Stemcell fields (Family == stemcell-light/stemcell-heavy).
	Storage    string `json:"storage,omitempty"`
	VolumePath string `json:"volume_path,omitempty"`
	Filename   string `json:"filename,omitempty"`
	SHA8       string `json:"sha8,omitempty"`

	// Disk fields (Family == disk-pvd/disk-pvz).
	Volid         string            `json:"volid,omitempty"`
	DiskStorage   string            `json:"disk_storage,omitempty"`
	DiskVolume    string            `json:"disk_volume,omitempty"`
	Pool          string            `json:"pool,omitempty"`
	Node          string            `json:"node,omitempty"`
	AZ            string            `json:"az,omitempty"`
	Opts          map[string]string `json:"opts,omitempty"`
	Anchor        bool              `json:"anchor,omitempty"`
	Format        string            `json:"format,omitempty"`
	StableID      string            `json:"stable_id,omitempty"`
	EncodedLength int               `json:"encoded_length,omitempty"`
	LengthTarget  int               `json:"length_target,omitempty"`
	OverTarget    bool              `json:"over_target,omitempty"`

	// VM fields (Family == vm, and the VM part of Family == snapshot). A
	// stamped CID also reports the cluster stamp and the per-VM token it
	// carries, and a bare CID leaves both empty.
	VMID         int    `json:"vmid,omitempty"`
	ClusterStamp string `json:"cluster_stamp,omitempty"`
	Token        string `json:"token,omitempty"`

	// Snapshot fields (Family == snapshot).
	VMCID        string `json:"vm_cid,omitempty"`
	SnapshotName string `json:"snapshot_name,omitempty"`
}

// stemcellFilenameSHA8Pattern extracts the trailing sha8 segment from a
// canonical stemcell qcow2 filename built by
// internal/pve.BuildStemcellFilename: "bosh-stemcell-<name>-<version>-<sha8>.qcow2".
var stemcellFilenameSHA8Pattern = regexp.MustCompile(`-([0-9a-f]{8})\.qcow2$`)

// sha8FromStemcellFilename extracts the 8-hex-character sha8 suffix from a
// stemcell qcow2 filename. Returns "" when filename does not match the
// "...-<8-hex>.qcow2" shape (including BuildStemcellFilename's own
// "00000000" placeholder for an unknown digest, which this function returns
// verbatim — callers that need to distinguish "no sha known" should check
// for that literal value).
func sha8FromStemcellFilename(filename string) string {
	m := stemcellFilenameSHA8Pattern.FindStringSubmatch(filename)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// isAllDigits reports whether s is non-empty and consists entirely of ASCII
// decimal digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// DecodeCID decodes raw against every CID family pve-cid understands,
// offline (no PVE API calls). Family detection mirrors the discriminator
// table in internal/pve/stemcell_volume.go, tried in this fixed order so no
// two families can ever claim the same input:
//
//  1. leading ':' → stemcell path CID (":light:"/":heavy:")
//  2. "pvd-"/"pvz-" prefix → disk envelope CID
//  3. all-digits or "<digits>-<cluster>-<token>" → VM CID
//  4. "<vm cid>:<name>" → snapshot CID
//
// Any other shape — including a bare, unenveloped PVE volid
// ("storage:volume") — is a hard parse error: this package has no
// backward-compatibility requirement with any CID form the CPI does not
// currently emit.
func DecodeCID(raw string) (*DecodedCID, error) {
	if raw == "" {
		return nil, fmt.Errorf("pve-cid: empty CID")
	}

	switch {
	case strings.HasPrefix(raw, ":"):
		return decodeStemcellCID(raw)
	case strings.HasPrefix(raw, "pvd-"), strings.HasPrefix(raw, "pvz-"):
		return decodeDiskCID(raw)
	case isAllDigits(raw), isStampedVMCID(raw):
		return decodeVMCID(raw)
	default:
		if idx := strings.IndexByte(raw, ':'); idx > 0 && idx < len(raw)-1 &&
			(isAllDigits(raw[:idx]) || isStampedVMCID(raw[:idx])) {
			return decodeSnapshotCID(raw)
		}
		return nil, fmt.Errorf(
			"pve-cid: unrecognized CID %q: expected a stemcell (\":light:\"/\":heavy:\"), "+
				"disk (\"pvd-\"/\"pvz-\"), VM (\"<vmid>\" or \"<vmid>-<cluster>-<token>\"), or snapshot (\"<vm cid>:<name>\") CID",
			raw,
		)
	}
}

func decodeStemcellCID(raw string) (*DecodedCID, error) {
	kind, storage, volumePath, err := pve.ParseStemcellPathCID(raw)
	if err != nil {
		return nil, fmt.Errorf("pve-cid: %w", err)
	}

	family := FamilyStemcellLight
	if kind == pve.StemcellKindHeavy {
		family = FamilyStemcellHeavy
	}
	filename := path.Base(volumePath)

	return &DecodedCID{
		Family:     family,
		Raw:        raw,
		Storage:    storage,
		VolumePath: volumePath,
		Filename:   filename,
		SHA8:       sha8FromStemcellFilename(filename),
	}, nil
}

func decodeDiskCID(raw string) (*DecodedCID, error) {
	bareCID, meta, err := pve.ParseEncodedDiskCID(raw)
	if err != nil {
		return nil, fmt.Errorf("pve-cid: %w", err)
	}
	storage, volume, err := pve.ParseDiskCID(bareCID)
	if err != nil {
		return nil, fmt.Errorf("pve-cid: decoded envelope volid %q: %w", bareCID, err)
	}

	family := FamilyDiskPVD
	if strings.HasPrefix(raw, "pvz-") {
		family = FamilyDiskPVZ
	}

	d := &DecodedCID{
		Family:        family,
		Raw:           raw,
		Volid:         bareCID,
		DiskStorage:   storage,
		DiskVolume:    volume,
		EncodedLength: len(raw),
		LengthTarget:  pve.DiskCIDLengthTarget,
		OverTarget:    len(raw) > pve.DiskCIDLengthTarget,
	}
	if meta != nil {
		d.Pool = meta.Pool
		d.Node = meta.Node
		d.AZ = meta.AZ
		d.Opts = meta.Opts
		d.Anchor = meta.Anchor
		d.Format = meta.Format
		d.StableID = meta.ID
	}
	return d, nil
}

// isStampedVMCID reports whether s is a stamped VM CID.
func isStampedVMCID(s string) bool {
	cid, err := vmcid.Parse(s)
	return err == nil && cid.Stamped()
}

func decodeVMCID(raw string) (*DecodedCID, error) {
	cid, err := vmcid.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("pve-cid: invalid VM CID %q: must be a positive integer VMID, optionally followed by "+
			"-<cluster>-<token>", raw)
	}
	return &DecodedCID{Family: FamilyVM, Raw: raw, VMID: cid.VMID, ClusterStamp: cid.Cluster, Token: cid.Token}, nil
}

func decodeSnapshotCID(raw string) (*DecodedCID, error) {
	vmCID, snapName, err := pve.ParseSnapshotCID(raw)
	if err != nil {
		return nil, fmt.Errorf("pve-cid: invalid snapshot CID %q: %w", raw, err)
	}
	// vmCID passed the caller's dispatch check, so this parse cannot fail;
	// vmid stays 0 (omitted from JSON) in the defensive error case rather
	// than propagating a spurious failure for a CID that ParseSnapshotCID
	// already accepted.
	cid, _ := vmcid.Parse(vmCID)
	return &DecodedCID{
		Family:       FamilySnapshot,
		Raw:          raw,
		VMCID:        vmCID,
		SnapshotName: snapName,
		VMID:         cid.VMID,
		ClusterStamp: cid.Cluster,
		Token:        cid.Token,
	}, nil
}
