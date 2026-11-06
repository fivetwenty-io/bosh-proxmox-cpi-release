// The cross-cluster check for clusters that mount one export.
//
// No CPI call sees a sibling cpi_config entry, so the CPI cannot refuse two
// clusters whose VMID bands overlap on a shared export. A command that reads
// every entry, such as `pve-cid ha --cpi-config`, gathers each cluster's
// storage index, effective bands, and datacenter next-id range, and hands them
// to CheckSharedExports, which is pure and reads nothing from PVE.
package pve

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// The datacenter next-id range PVE uses when datacenter.cfg sets none. PVE
// reads upper as an exclusive bound.
const (
	DefaultNextIDLower = 100
	DefaultNextIDUpper = 1000000
)

// NextIDRange converts the lower and exclusive upper bounds of a datacenter
// next-id setting into an inclusive band. A bound of zero, which is how an
// unset bound reads, takes PVE's default.
func NextIDRange(lower, upper int) VMIDBand {
	if lower <= 0 {
		lower = DefaultNextIDLower
	}
	if upper <= 0 {
		upper = DefaultNextIDUpper
	}
	return VMIDBand{Start: lower, End: upper - 1}
}

// ClusterVMIDLayout is what the cross-cluster check needs to know about one
// cluster: its storage index, the effective bands of the cpi_config entry that
// targets it, and its datacenter next-id range. Unchecked, when not empty,
// says why the cluster could not be read, and the check then skips every
// comparison that involves it.
type ClusterVMIDLayout struct {
	Name      string
	Storages  []StorageInfo
	Bands     VMIDBands
	NextID    VMIDBand
	Unchecked string
}

// SharedExportFindingKind names one kind of cross-cluster finding.
type SharedExportFindingKind string

// The finding kinds, in the order CheckSharedExports reports them.
const (
	// FindingUnchecked reports a cluster that could not be read, or a pair
	// the check could not compare because one side could not be read.
	FindingUnchecked SharedExportFindingKind = "unchecked"
	// FindingBandOverlap reports two clusters on one backing whose bands
	// overlap.
	FindingBandOverlap SharedExportFindingKind = "band_overlap"
	// FindingNextIDOverOwnBand reports a next-id range that overlaps a band
	// of its own cluster.
	FindingNextIDOverOwnBand SharedExportFindingKind = "next_id_over_own_band"
	// FindingNextIDOverPeer reports, for two clusters on one backing, a
	// next-id range that overlaps a band or the next-id range of the other.
	FindingNextIDOverPeer SharedExportFindingKind = "next_id_over_peer"
	// FindingPossibleSharedBacking reports two clusters whose storages name
	// the same export path behind different server strings, which may be one
	// export reached by two names, while their bands or next-id ranges would
	// conflict if it is.
	FindingPossibleSharedBacking SharedExportFindingKind = "possible_shared_backing"
)

var findingKindOrder = map[SharedExportFindingKind]int{
	FindingUnchecked:             0,
	FindingBandOverlap:           1,
	FindingNextIDOverOwnBand:     2,
	FindingNextIDOverPeer:        3,
	FindingPossibleSharedBacking: 4,
}

// SharedExportFinding is one result of the cross-cluster check. Clusters holds
// one name for a finding about a single cluster and two for a pair, in input
// order. Backings lists the shared backing keys a pair finding rests on.
type SharedExportFinding struct {
	Kind     SharedExportFindingKind
	Clusters []string
	Backings []string
	Detail   string
}

// Failing reports whether the finding should fail the run. Every kind fails
// except a possible shared backing, which the check cannot confirm.
func (f SharedExportFinding) Failing() bool {
	return f.Kind != FindingPossibleSharedBacking
}

// String renders the finding as one line.
func (f SharedExportFinding) String() string {
	out := string(f.Kind) + " [" + strings.Join(f.Clusters, ", ") + "]"
	if len(f.Backings) > 0 {
		out += " on " + strings.Join(f.Backings, ", ")
	}
	return out + ": " + f.Detail
}

// CheckSharedExports compares every pair of clusters and returns the
// findings, sorted so the same input always gives the same output. Two
// clusters share a backing when an nfs export, a cifs share, or the path of a
// shared dir storage resolves to the same key on both, counting every storage
// that can hold guest images, container volumes, or ISOs, disabled or not,
// because the files stay on the export either way. On a shared backing, any
// band of one cluster that overlaps any band of the other is an overlap, and
// so is a next-id range that overlaps a band or the next-id range of the
// other. A next-id range that overlaps a band of its own cluster is reported
// only for a cluster that has a shared backing key which another checked
// cluster also has, so a lone cluster or one on local storage reports nothing.
func CheckSharedExports(clusters []ClusterVMIDLayout) []SharedExportFinding {
	var findings []SharedExportFinding
	keys := make([]map[string]bool, len(clusters))
	loose := make([]map[string]string, len(clusters))
	for i := range clusters {
		c := &clusters[i]
		if c.Unchecked != "" {
			findings = append(findings, SharedExportFinding{
				Kind:     FindingUnchecked,
				Clusters: []string{c.Name},
				Detail:   "the cluster could not be read: " + c.Unchecked,
			})
			continue
		}
		keys[i], loose[i] = sharedBackingKeys(c.Storages)
	}
	for i := range clusters {
		c := &clusters[i]
		if c.Unchecked != "" || !sharesBackingWithPeer(keys, i) {
			continue
		}
		if overlaps := nextIDOverlaps(c.NextID, c.Bands); len(overlaps) > 0 {
			findings = append(findings, SharedExportFinding{
				Kind:     FindingNextIDOverOwnBand,
				Clusters: []string{c.Name},
				Detail: fmt.Sprintf("the datacenter next-id range %s overlaps this cluster's %s, and another "+
					"cluster mounts one of its shared exports; set next-id outside every band", c.NextID,
					strings.Join(overlaps, " and ")),
			})
		}
	}
	for i := range clusters {
		for j := i + 1; j < len(clusters); j++ {
			findings = append(findings, comparePair(clusters[i], clusters[j], keys[i], keys[j], loose[i], loose[j])...)
		}
	}
	sort.SliceStable(findings, func(a, b int) bool {
		fa, fb := findings[a], findings[b]
		if findingKindOrder[fa.Kind] != findingKindOrder[fb.Kind] {
			return findingKindOrder[fa.Kind] < findingKindOrder[fb.Kind]
		}
		if ca, cb := strings.Join(fa.Clusters, "\x00"), strings.Join(fb.Clusters, "\x00"); ca != cb {
			return ca < cb
		}
		return fa.Detail < fb.Detail
	})
	return findings
}

// sharesBackingWithPeer reports whether cluster i has a shared backing key that
// another checked cluster also has. An unchecked cluster has no keys.
func sharesBackingWithPeer(keys []map[string]bool, i int) bool {
	for j := range keys {
		if j != i && len(intersectKeys(keys[i], keys[j])) > 0 {
			return true
		}
	}
	return false
}

// comparePair returns the findings for clusters a and b.
func comparePair(a, b ClusterVMIDLayout, keysA, keysB map[string]bool, looseA, looseB map[string]string) []SharedExportFinding {
	pair := []string{a.Name, b.Name}
	if a.Unchecked != "" || b.Unchecked != "" {
		missing := a.Name
		if a.Unchecked == "" {
			missing = b.Name
		}
		return []SharedExportFinding{{
			Kind:     FindingUnchecked,
			Clusters: pair,
			Detail:   "the pair was not compared because " + missing + " could not be read, so a shared export between them stays unknown",
		}}
	}
	bandConflicts := bandOverlaps(a, b)
	nextConflicts := nextIDPeerOverlaps(a, b)
	shared := intersectKeys(keysA, keysB)
	if len(shared) > 0 {
		var out []SharedExportFinding
		if len(bandConflicts) > 0 {
			out = append(out, SharedExportFinding{
				Kind:     FindingBandOverlap,
				Clusters: pair,
				Backings: shared,
				Detail: "the clusters mount one export and their VMID bands overlap (" + strings.Join(bandConflicts, "; ") +
					"); give every band of every cluster on the export its own range",
			})
		}
		if len(nextConflicts) > 0 {
			out = append(out, SharedExportFinding{
				Kind:     FindingNextIDOverPeer,
				Clusters: pair,
				Backings: shared,
				Detail: "the clusters mount one export and a datacenter next-id range overlaps the other cluster (" +
					strings.Join(nextConflicts, "; ") + "); set each next-id range outside every band on the export " +
					"and apart from the other's",
			})
		}
		return out
	}
	if len(bandConflicts) == 0 && len(nextConflicts) == 0 {
		return nil
	}
	possible := intersectLooseKeys(looseA, looseB)
	if len(possible) == 0 {
		return nil
	}
	return []SharedExportFinding{{
		Kind:     FindingPossibleSharedBacking,
		Clusters: pair,
		Backings: possible,
		Detail: "storages on both clusters name the same export path behind different server names, which may be " +
			"one export reached two ways, and the clusters' VMID ranges would conflict if it is (" +
			strings.Join(append(bandConflicts, nextConflicts...), "; ") + ")",
	}}
}

// sharedBackingKeys returns the cross-cluster keys of storages, and for each
// network key the server-free form that the possible-shared check compares,
// mapped back to the full key. A dir storage counts only when it carries the
// shared flag, and its key leaves out the node restriction, which names nodes
// of one cluster only.
func sharedBackingKeys(storages []StorageInfo) (map[string]bool, map[string]string) {
	keys := map[string]bool{}
	loose := map[string]string{}
	for i := range storages {
		s := &storages[i]
		if !holdsGuestOrISOContent(s.Content) {
			continue
		}
		switch strings.ToLower(s.Type) {
		case StorageTypeNFS, StorageTypeCIFS:
			key := s.BackingKey()
			if key == "" {
				continue
			}
			keys[key] = true
			loose[strings.ToLower(s.Type)+"://*"+path.Clean("/"+s.Export)] = key
		case StorageTypeDir:
			if !s.Shared || s.Path == "" {
				continue
			}
			keys[backingKeyDirScheme+path.Clean(s.Path)] = true
		}
	}
	return keys, loose
}

// holdsGuestOrISOContent reports whether a content list can hold volumes a VM
// ID names: guest images, container volumes, or ISOs. An empty list reads as
// one that can, which errs toward comparing the storage.
func holdsGuestOrISOContent(content string) bool {
	if strings.TrimSpace(content) == "" {
		return true
	}
	for _, kind := range []string{"images", "rootdir", "iso"} {
		if hasContentType(content, kind) {
			return true
		}
	}
	return false
}

func intersectKeys(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// intersectLooseKeys returns, for each server-free key both sides hold behind
// different full keys, the two full keys.
func intersectLooseKeys(a, b map[string]string) []string {
	var out []string
	for k, fullA := range a {
		if fullB, ok := b[k]; ok && fullA != fullB {
			out = append(out, fullA+" ~ "+fullB)
		}
	}
	sort.Strings(out)
	return out
}

// bandOverlaps lists every pair of bands, one from each cluster, that
// overlap.
func bandOverlaps(a, b ClusterVMIDLayout) []string {
	var out []string
	for _, ba := range a.Bands.All() {
		for _, bb := range b.Bands.All() {
			if ba.Band.Overlaps(bb.Band) {
				out = append(out, fmt.Sprintf("%s %s band %s overlaps %s %s band %s",
					a.Name, ba.Name, ba.Band, b.Name, bb.Name, bb.Band))
			}
		}
	}
	return out
}

// nextIDOverlaps names each band of bands that next overlaps.
func nextIDOverlaps(next VMIDBand, bands VMIDBands) []string {
	var out []string
	for _, b := range bands.All() {
		if next.Overlaps(b.Band) {
			out = append(out, b.Name+" band "+b.Band.String())
		}
	}
	return out
}

// nextIDPeerOverlaps lists each overlap of one cluster's next-id range with a
// band of the other, and of the two next-id ranges with each other.
func nextIDPeerOverlaps(a, b ClusterVMIDLayout) []string {
	var out []string
	for _, side := range [][2]*ClusterVMIDLayout{{&a, &b}, {&b, &a}} {
		from, to := side[0], side[1]
		for _, band := range nextIDOverlaps(from.NextID, to.Bands) {
			out = append(out, fmt.Sprintf("%s next-id %s overlaps %s %s", from.Name, from.NextID, to.Name, band))
		}
	}
	if a.NextID.Overlaps(b.NextID) {
		out = append(out, fmt.Sprintf("%s next-id %s overlaps %s next-id %s", a.Name, a.NextID, b.Name, b.NextID))
	}
	return out
}
