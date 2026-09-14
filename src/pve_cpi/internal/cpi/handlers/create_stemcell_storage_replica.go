package handlers

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"

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
	if !deps.Config.StemcellReplicateStorageSetEnabled() || sha8Of(sha256hex) == "" {
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

// maybeReplicateTemplateToStorageSet builds a cache template on every
// eligible member of the effective root set. Best-effort like the per-node
// fan-out: failures are logged, never returned, and the next create_stemcell
// or upload-stemcell --fix fills the gaps through the dedup path. One
// tolerant cluster scan feeds every member's dedup check, so an M-member set
// costs one enumeration, not M, and one unlistable node degrades the scan
// instead of failing every member.
func maybeReplicateTemplateToStorageSet(
	ctx context.Context,
	deps Deps,
	templateNode, storage, qcow2Filename, sha256hex string,
	stemcellCID, directorUUID string,
	kind pve.StemcellKind,
	cp stemcellCloudProps,
	source string,
) {
	setName, ok := storageSetReplicasNeeded(deps, sha256hex)
	if !ok {
		return
	}
	logger := deps.Log(ctx)
	sha8 := sha8Of(sha256hex)
	existing, findErr := pve.FindTemplatesBySHATagClusterTolerant(ctx, deps.PVE, sha8)
	if findErr != nil {
		logger.Warn("create_stemcell: storage-set replication: cannot list existing templates (skipping)",
			log.String("set", setName), log.String("sha8", sha8), log.Err(findErr))
		return
	}
	// The primary's root bus must match the configured bus before any
	// member builds: observeManagedVMRootSources fails every set-managed
	// create_vm when any sha-tagged template carries a different root
	// bus, so a replica minted on the new bus beside an old-bus primary
	// would wedge every deploy.
	anchor, _, found := selectStemcellAnchor(existing)
	if !found {
		logger.Warn("create_stemcell: storage-set replication: no primary template found for sha8 (skipping)",
			log.String("set", setName), log.String("sha8", sha8))
		return
	}
	_, primaryRootKey, busErr := resolveTemplateDiskStorage(ctx, deps, anchor.Node, anchor.VMID)
	if busErr != nil {
		logger.Warn("create_stemcell: storage-set replication: cannot read the primary's root disk (skipping)",
			log.Int64(metadataKeyVMID, anchor.VMID), log.String("node", anchor.Node), log.Err(busErr))
		return
	}
	if want := rootDiskKey(deps.Config); primaryRootKey != want {
		logger.Warn("create_stemcell: storage-set replication: primary root bus differs from pve.root_disk_bus; rerun create_stemcell after the bus change (skipping)",
			log.Int64(metadataKeyVMID, anchor.VMID), log.String("primary_root_key", primaryRootKey), log.String("configured_root_key", want))
		return
	}
	targets, err := storageSetReplicaTargets(ctx, deps, setName, templateNode, deps.Config.VMStorage)
	if err != nil {
		logger.Warn("create_stemcell: storage-set replication: cannot resolve members (skipping)",
			log.String("set", setName), log.Err(err))
		return
	}
	if len(targets) == 0 {
		return
	}
	replicateStemcellToStorages(ctx, deps, templateNode, storage, qcow2Filename, sha256hex,
		targets, existing, stemcellCID, directorUUID, kind, cp, source)
}

// storageReplicaOutcome records how one member's replica attempt ended.
// Err is nil on success and non-nil exactly when the member ended up
// without a usable replica.
type storageReplicaOutcome struct {
	Storage string
	Stage   string
	Err     error
}

// logStorageReplicationSummary emits exactly one summary line for a
// completed storage fan-out, keyed by storage ID rather than node so the
// task log reads correctly.
func logStorageReplicationSummary(logger *log.Logger, label string, outcomes []storageReplicaOutcome) {
	if len(outcomes) == 0 {
		return
	}
	var failed []string
	fields := []log.Field{log.Int("replica_storages", len(outcomes))}
	for _, o := range outcomes {
		if o.Err == nil {
			continue
		}
		failed = append(failed, o.Storage)
		fields = append(fields, log.String("error_"+o.Storage, o.Stage+": "+o.Err.Error()))
	}
	held := len(outcomes) - len(failed)
	fields = append(fields, log.Int("held", held))
	if len(failed) == 0 {
		logger.Info(fmt.Sprintf("%s: %d of %d members hold a replica", label, held, len(outcomes)), fields...)
		return
	}
	fields = append(fields,
		log.Int("failed", len(failed)),
		log.String("failed_storages", strings.Join(failed, ",")),
	)
	logger.Warn(fmt.Sprintf("%s: %d of %d members hold a replica (non-fatal; the stemcell itself succeeded; run bosh upload-stemcell --fix to build the missing ones)",
		label, held, len(outcomes)), fields...)
}

// replicateStemcellToStorages mirrors replicateStemcellToNodes: the same
// bounded worker pool from stemcell_replication_concurrency, the same
// per-worker panic recovery, and one summary line. Every replica creates on
// node, so this fan-out deliberately does not take deps.Inflight; the
// per-node gate would serialize the whole set to one worker.
func replicateStemcellToStorages(
	ctx context.Context,
	deps Deps,
	node, storage, qcow2Filename, sha256hex string,
	targets []storageReplicaTarget,
	existing []pve.TemplateRef,
	stemcellCID, creatingDirectorUUID string,
	kind pve.StemcellKind,
	cp stemcellCloudProps,
	source string,
) {
	logger := deps.Log(ctx)
	workerLimit := 1
	if deps.Config != nil {
		workerLimit = deps.Config.StemcellReplicationConcurrencyValue()
	}
	sem := make(chan struct{}, workerLimit)
	outcomes := make([]storageReplicaOutcome, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		memberLogger := logger.With(log.String("storage", target.StorageID))
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					memberLogger.Error("create_stemcell: storage-replica worker panicked (recovered); re-run create_stemcell to retry this member",
						log.Any("panic", r), log.String("stack", string(debug.Stack())))
					outcomes[i] = storageReplicaOutcome{Storage: target.StorageID, Stage: "panic", Err: fmt.Errorf("worker panicked: %v", r)}
				}
			}()
			vmid, err := ensureStorageReplicaTemplateVM(ctx, deps, node, target, existing, storage, qcow2Filename, sha256hex, kind, stemcellCID, creatingDirectorUUID, cp, source)
			if err != nil {
				memberLogger.Warn("create_stemcell: storage-set replication: member failed", log.Err(err))
				outcomes[i] = storageReplicaOutcome{Storage: target.StorageID, Stage: "ensure-template", Err: err}
				return
			}
			memberLogger.Info("create_stemcell: storage-set replication: member holds a replica", log.Int64(metadataKeyVMID, vmid))
			outcomes[i] = storageReplicaOutcome{Storage: target.StorageID}
		}()
	}
	wg.Wait()
	logStorageReplicationSummary(logger, "create_stemcell: storage-set replication", outcomes)
}

// ensureStorageReplicaTemplateVM builds, or finds, the cache template whose
// root disk lives on target.StorageID. It mirrors ensureReplicaTemplateVM
// with three differences: the dedup is an in-memory match over the refs
// the caller already scanned, TargetStorage is the member instead of
// vm_storage, and ExtraBaseTags carries the storage tag. There is no
// per-storage upload, because the qcow2 stays on stemcell_storage and the
// import-from volid reads it from there.
func ensureStorageReplicaTemplateVM(
	ctx context.Context,
	deps Deps,
	node string,
	target storageReplicaTarget,
	existing []pve.TemplateRef,
	storage, qcow2Filename, sha256hex string,
	kind pve.StemcellKind,
	stemcellCID, creatingDirectorUUID string,
	cp stemcellCloudProps,
	source string,
) (int64, error) {
	sha8 := sha8Of(sha256hex)
	logger := deps.Log(ctx).With(log.String("storage", target.StorageID))
	templateName := pve.BuildTemplateNameWithSHA(cp.Name, cp.Version, sha8)

	if ref, found := pve.SelectStorageReplica(existing, target.Tag); found {
		logger.Info("ensureStorageReplicaTemplateVM: replica already exists", log.Int64(metadataKeyVMID, ref.VMID), log.String("node", ref.Node))
		return ref.VMID, nil
	}

	// Never write the degenerate "bosh-stemcell-sha-" tag; an empty sha8
	// means no sha tag at all, matching ensureReplicaTemplateVM. The gate
	// keeps an empty sha8 out of this function, so this is belt and braces.
	shaTag := ""
	if sha8 != "" {
		shaTag = stemcellSHATagPrefix + sha8
	}
	spec := templateBuildSpec{
		TemplateName:         templateName,
		ImportVolid:          storage + ":import/" + qcow2Filename,
		ShaTag:               shaTag,
		SHA256Hex:            sha256hex,
		TargetStorage:        target.StorageID,
		Kind:                 kind,
		CID:                  stemcellCID,
		CreatingDirectorUUID: creatingDirectorUUID,
		ExtraBaseTags:        []string{target.Tag},
	}
	if deps.Config.StemcellTemplatePool != "" {
		if err := pve.EnsurePoolExists(ctx, deps.PVE, deps.Config.StemcellTemplatePool, pve.PoolProvenance(""), logger); err != nil {
			return 0, fmt.Errorf("ensureStorageReplicaTemplateVM: ensure pool %q: %w", deps.Config.StemcellTemplatePool, err)
		}
	}
	isRetryable := func(e error) bool {
		return pve.IsVMIDConflict(e) || pve.IsStorageLockTimeout(e) || pve.IsTransientTransport(e)
	}
	allocated, err := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			return attemptCreateTemplateVM(ctx, deps, logger, node, candidate, spec, cp, source)
		},
		isRetryable, 0,
		pve.WithRange(deps.Config.StemcellTemplateVMIDRangeStart, deps.Config.StemcellTemplateVMIDRangeEnd),
		pve.WithStorageScan(node, target.StorageID),
	)
	if err != nil {
		return 0, fmt.Errorf("ensureStorageReplicaTemplateVM: allocate+create on %q: %w", target.StorageID, err)
	}
	vmid := int64(allocated)
	freezeUPID, err := pve.MakeTemplate(ctx, deps.PVE, node, vmid)
	if err != nil {
		cleanupLeakedTemplateVM(ctx, deps, node, vmid, logger, "freeze")
		return 0, fmt.Errorf("ensureStorageReplicaTemplateVM: freeze vmid=%d: %w", vmid, err)
	}
	if freezeUPID != "" {
		if err := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, freezeUPID, logger, pve.WithMaxWait(pve.StemcellMaxWait)); err != nil {
			cleanupLeakedTemplateVM(ctx, deps, node, vmid, logger, "await freeze")
			return 0, fmt.Errorf("ensureStorageReplicaTemplateVM: await freeze vmid=%d upid=%s: %w", vmid, freezeUPID, err)
		}
	}
	logger.Info("ensureStorageReplicaTemplateVM: replica template frozen", log.Int64(metadataKeyVMID, vmid))
	return vmid, nil
}
