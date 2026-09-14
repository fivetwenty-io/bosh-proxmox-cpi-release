package pve

import "strings"

// ReplicaStorageTagPrefix marks a cache template built on one member of a
// storage set. The suffix is the sanitized storage ID and is an identity
// marker only; the replica's real storage is read from its root disk volid.
const ReplicaStorageTagPrefix = "bosh-stemcell-storage-"

// ReplicaStorageTagForStorage returns the per-member replica tag for
// storageID. config.ReplicaTagPart mirrors the sanitizer so validation and
// the builder agree on collisions; TestReplicaTagSanitizersAgree pins it.
func ReplicaStorageTagForStorage(storageID string) string {
	return ReplicaStorageTagPrefix + dnsSafeStemcellPart(storageID)
}

// IsStorageReplica reports whether this template carries a per-storage
// replica tag.
func (r TemplateRef) IsStorageReplica() bool {
	for _, tok := range splitPVETags(r.Tags) {
		if strings.HasPrefix(tok, ReplicaStorageTagPrefix) {
			return true
		}
	}
	return false
}

// SelectStorageReplica returns the lowest-VMID ref carrying storageTag. It
// is pure so create_stemcell's storage fan-out can run one cluster scan and
// match every member in memory. Callers pass refs from
// FindTemplatesBySHATagCluster or its tolerant form, which already restrict
// to the sha8 and apply the generation gate through
// filterClusterQemuTemplates, so neither is re-checked here.
func SelectStorageReplica(refs []TemplateRef, storageTag string) (TemplateRef, bool) {
	if storageTag == "" {
		return TemplateRef{}, false
	}
	var best TemplateRef
	found := false
	for _, ref := range refs {
		hasTag := false
		for _, tok := range splitPVETags(ref.Tags) {
			if tok == storageTag {
				hasTag = true
				break
			}
		}
		if !hasTag {
			continue
		}
		if !found || ref.VMID < best.VMID {
			best = ref
			found = true
		}
	}
	return best, found
}
