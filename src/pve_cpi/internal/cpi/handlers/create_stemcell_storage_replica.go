package handlers

import (
	"context"
	"fmt"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

// storageReplicaTarget is one set member that should carry a cache template.
type storageReplicaTarget struct {
	StorageID string
	Tag       string
}

// replicaInventorySource resolves the discovery source for the storage-set
// fan-out: the Deps seam when a test set one, else the live PVE client.
func replicaInventorySource(deps Deps) inv.Source {
	if deps.ReplicaInventory != nil {
		return deps.ReplicaInventory
	}
	return inv.PVESource{Client: deps.PVE}
}

// storageSetReplicasNeeded reports the effective root set when per-member
// replicas are on and a content identity is available. sha8Of returns ""
// for any digest shorter than eight characters, and an empty sha8 must
// never reach the builder, which would otherwise mint a template with no
// sha tag that nothing can dedup against.
func storageSetReplicasNeeded(deps Deps, sha256hex string) (string, bool) {
	if deps.Config == nil || !deps.Config.StemcellReplicateStorageSet || sha8Of(sha256hex) == "" {
		return "", false
	}
	set := deps.Config.EffectiveRootStorageSet()
	return set, set != ""
}

// filterStorageReplicaMembers keeps the members that should carry a
// replica: images-capable, shared, not the primary's own storage ID, and
// with a replica tag no earlier member already claimed. A member that
// aliases the primary's backing under another ID is kept, because sourceFor
// requires exact storage-ID equality for a linked clone. members is
// expected sorted, which Snapshot.Members guarantees, so build order is
// deterministic. It is pure so every skip rule is testable without
// Discover, which rejects most of these shapes before they get here.
func filterStorageReplicaMembers(
	logger *log.Logger,
	members []string,
	definition func(string) (pve.StorageInfo, bool),
	primaryStorage string,
) []storageReplicaTarget {
	claimed := map[string]string{}
	var out []storageReplicaTarget
	for _, id := range members {
		def, ok := definition(id)
		switch {
		case !ok:
			// Discover fails on a missing member today; this guards a
			// future tolerant Discover and a caller-built definition map.
			logger.Warn("create_stemcell: storage-set replication: member absent from discovery; skipping", log.String("storage", id))
			continue
		case id == primaryStorage:
			logger.Info("create_stemcell: storage-set replication: member is the primary's storage; primary serves it", log.String("storage", id))
			continue
		case !planContent(def, "images"):
			logger.Warn("create_stemcell: storage-set replication: member lacks images content; skipping", log.String("storage", id))
			continue
		case !def.IsShared():
			logger.Warn("create_stemcell: storage-set replication: member is node-local; skipping", log.String("storage", id))
			continue
		}
		tag := pve.ReplicaStorageTagForStorage(id)
		if prior, dup := claimed[tag]; dup {
			logger.Warn("create_stemcell: storage-set replication: member tag collides; skipping", log.String("storage", id), log.String("collides_with", prior))
			continue
		}
		claimed[tag] = id
		out = append(out, storageReplicaTarget{StorageID: id, Tag: tag})
	}
	return out
}

// storageSetReplicaTargets discovers setName through the storage inventory
// and filters its members down to the ones that need a replica. Discover
// validates placement, force-selects every globally bound set, and fails on
// a missing member or an aliased pair, so a persistent-set misconfiguration
// surfaces here as an error the caller logs and skips.
func storageSetReplicaTargets(ctx context.Context, deps Deps, setName, primaryNode, primaryStorage string) ([]storageReplicaTarget, error) {
	collector, err := inv.NewCollector(replicaInventorySource(deps), inv.Options{})
	if err != nil {
		return nil, fmt.Errorf("storage-set replication: collector: %w", err)
	}
	snapshot, err := collector.Discover(ctx, deps.Config, inv.Request{
		Nodes:               []string{primaryNode},
		SetNames:            []string{setName},
		CompanionStorageIDs: []string{primaryStorage},
	})
	if err != nil {
		return nil, fmt.Errorf("storage-set replication: discover set %q: %w", setName, err)
	}
	members, ok := snapshot.Members(setName)
	if !ok {
		return nil, fmt.Errorf("storage-set replication: set %q was not discovered", setName)
	}
	return filterStorageReplicaMembers(deps.Log(ctx), members, snapshot.Definition, primaryStorage), nil
}
