// Package vmcid parses the VM CIDs the Director hands the CPI.
//
// A CID is either a bare VMID ("123") or a stamped VMID
// ("123-0a1b2c3d-4e5f6a7b"), where the two suffixes are the eight-character
// cluster stamp and the eight-character per-VM token. Every handler parses its
// CID once at entry and uses PathID for each PVE call, so a stamped CID never
// reaches PVE as a path segment. The package is a leaf with no CPI imports so
// handlers, the pve backends, the hooks, and cmd/pve-cid can share it.
package vmcid

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// stampLen is the length of both the cluster stamp and the per-VM token.
const stampLen = 8

// VMCID is a parsed VM CID. Cluster and Token are empty for a bare CID.
type VMCID struct {
	// VMID is the positive PVE VMID.
	VMID int
	// Cluster is the eight-character cluster stamp, or empty for a bare CID.
	Cluster string
	// Token is the eight-character per-VM token, or empty for a bare CID.
	Token string
}

// Parse reads a VM CID. It accepts a bare positive VMID and the stamped form
// "<vmid>-<cluster8>-<token8>" with lowercase hexadecimal stamps. It rejects
// everything else, including signs, spaces, a zero VMID, a leading zero, and a VMID that does
// not fit an int.
func Parse(raw string) (VMCID, error) {
	if raw == "" {
		return VMCID{}, errors.New("empty")
	}
	parts := strings.Split(raw, "-")
	switch len(parts) {
	case 1:
		vmid, err := parseVMID(parts[0])
		if err != nil {
			return VMCID{}, err
		}
		return VMCID{VMID: vmid}, nil
	case 3:
		vmid, err := parseVMID(parts[0])
		if err != nil {
			return VMCID{}, err
		}
		if !isStamp(parts[1]) || !isStamp(parts[2]) {
			return VMCID{}, fmt.Errorf("stamp and token must each be %d lowercase hex characters", stampLen)
		}
		return VMCID{VMID: vmid, Cluster: parts[1], Token: parts[2]}, nil
	default:
		return VMCID{}, errors.New("not a VMID or <vmid>-<cluster>-<token>")
	}
}

// PathID returns the bare VMID string that PVE accepts as a vmid path segment
// and that durable state stores.
func (c VMCID) PathID() string {
	return strconv.Itoa(c.VMID)
}

// Stamped reports whether the CID carries a cluster stamp and token.
func (c VMCID) Stamped() bool {
	return c.Cluster != "" || c.Token != ""
}

// String returns the CID in the form it was parsed from.
func (c VMCID) String() string {
	if !c.Stamped() {
		return c.PathID()
	}
	return c.PathID() + "-" + c.Cluster + "-" + c.Token
}

// VMID returns the VMID of a CID string, or false when raw is not a valid CID.
// It is for comparison and lookup sites that treat an unparseable CID as
// "no match" instead of an error.
func VMID(raw string) (int, bool) {
	c, err := Parse(raw)
	if err != nil {
		return 0, false
	}
	return c.VMID, true
}

// SameVM reports whether raw names the VM with the given VMID.
func SameVM(raw string, vmid int) bool {
	got, ok := VMID(raw)
	return ok && got == vmid
}

func parseVMID(s string) (int, error) {
	if s == "" {
		return 0, errors.New("vmid is empty")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("vmid must be all digits")
		}
	}
	if s[0] == '0' {
		return 0, errors.New("vmid must not have a leading zero")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("vmid is out of range")
	}
	if n <= 0 {
		return 0, errors.New("vmid must be a positive integer")
	}
	return n, nil
}

func isStamp(s string) bool {
	if len(s) != stampLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// PathOf returns the vmid path segment for a VMID the caller already holds. It
// is the same string PathID returns, for helpers that receive the parsed VMID
// instead of the VMCID.
func PathOf(vmid int) string {
	return strconv.Itoa(vmid)
}
