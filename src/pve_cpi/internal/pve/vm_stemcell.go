// Stemcell provenance on a workload VM: records which stemcell the guest was
// booted from, on the VM's description sentinel (a distinct top-level key from
// bosh_pool/bosh_attached_disks/bosh_parked_disks, so the codecs coexist — see
// sentinel.go).
//
// Why this exists: the guest knows its own stemcell, but PVE does not. Nothing
// on the hypervisor side named it, because the Director's set_vm_metadata
// payload carries deployment, instance_group, job, index, and name, and never
// the stemcell. An operator asking which guests still run the old stemcell
// after a stemcell bump had to log into each one. create_vm is the one call
// that knows the answer, so it writes the record here and stamps a matching
// "stemcell--<name>-<version>" tag. A create-env Director gets the same
// treatment, and that is the case with no set_vm_metadata call at all.
package pve

import (
	"encoding/json"
)

// vmStemcellSentinelKey is the top-level sentinel JSON key holding the
// create-time stemcell record on a workload VM's description.
const vmStemcellSentinelKey = "bosh_stemcell"

// VMStemcell is the persisted record of the stemcell a workload VM booted
// from. Label is the "<name>-<version>" identity carried in the stemcell's
// qcow2 filename, for example "bosh-openstack-kvm-ubuntu-noble-1.585", and it
// is the same value the "stemcell--" tag holds. CID is the full path-identity
// stemcell CID create_vm was called with, Kind is "light" or "heavy", and SHA8
// is the content-digest prefix the cache template is keyed on.
type VMStemcell struct {
	Label string `json:"label,omitempty"`
	CID   string `json:"cid,omitempty"`
	Kind  string `json:"kind,omitempty"`
	SHA8  string `json:"sha8,omitempty"`
}

// GetVMStemcell parses a VM description and returns its bosh_stemcell record.
// An absent sentinel, an absent key, or corrupted JSON all yield (nil, false),
// which callers read as a VM created before this provenance existed. A record
// that carries neither a label nor a CID says nothing, so it reads the same
// way.
func GetVMStemcell(desc string) (*VMStemcell, bool) {
	_, raw := ParseSentinel(desc)
	rawSC, ok := raw[vmStemcellSentinelKey]
	if !ok {
		return nil, false
	}
	sc := &VMStemcell{}
	if err := json.Unmarshal(rawSC, sc); err != nil {
		return nil, false
	}
	if sc.Label == "" && sc.CID == "" {
		return nil, false
	}
	return sc, true
}

// SetVMStemcellOnDescription returns desc with its bosh_stemcell sentinel key
// replaced by sc, preserving the nonBOSH text and every other sentinel key. A
// nil sc deletes the key.
func SetVMStemcellOnDescription(desc string, sc *VMStemcell) (string, error) {
	nonBOSH, raw := ParseSentinel(desc)
	if sc == nil {
		delete(raw, vmStemcellSentinelKey)
	} else {
		b, err := json.Marshal(sc)
		if err != nil {
			return "", err
		}
		raw[vmStemcellSentinelKey] = json.RawMessage(b)
	}
	return RenderSentinel(nonBOSH, raw)
}
