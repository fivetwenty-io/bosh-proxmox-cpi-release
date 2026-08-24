package handlers

import (
	"context"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// haRegistrationFeature names a specific create_vm feature that registers the
// VM as a PVE HA resource (directly or transitively). Any one of these being
// active is sufficient to trigger the config-drive ISO migration-safety check
// in checkISOStorageForHA.
type haRegistrationFeature string

const (
	haFeatureDLB          haRegistrationFeature = "placement.dlb"
	haFeatureAntiAffinity haRegistrationFeature = "placement.anti_affinity.use_ha_rules"
	haFeatureAZPin        haRegistrationFeature = "placement.pin_az_via_ha_rules"
)

// haRegistrationFeatures returns every HA-registration feature create_vm
// would attempt for this VM, given its cloud properties, the node it was
// placed on, and its BOSH env. It mirrors the eligibility gate each
// apply*/ensure* function checks at its own entry point:
//
//   - DLB:            deps.Config.DLBEligibleForAZ (same predicate the step 10
//     block in create_vm.go guards on).
//   - Anti-affinity:  deps.Config.AntiAffinityUseHaRulesEnabled() and a
//     non-empty sanitized instance-group name (mirrors
//     applyAntiAffinityMembership's early-return gate).
//   - AZ node-affinity pin: deps.Config.HANodeAffinityPinEnabled() and a
//     resolvable pin AZ with a non-empty node candidate set (mirrors
//     applyAZNodeAffinityPin's early-return gates).
//
// This is an eligibility check, not a replay of PVE API calls: it never
// blocks and never fails on its own account. Called after the three apply*
// steps have already run (or attempted to run) so the resulting feature list
// accurately reflects "was HA registration attempted for this VM".
func haRegistrationFeatures(deps Deps, cp createVMCloudProps, node string, env map[string]any) []haRegistrationFeature {
	var out []haRegistrationFeature

	if deps.Config.DLBEligibleForAZ(cp.AvailabilityZone) {
		out = append(out, haFeatureDLB)
	}

	if deps.Config.AntiAffinityUseHaRulesEnabled() && sanitizeTagValue(instanceGroupName(env)) != "" {
		out = append(out, haFeatureAntiAffinity)
	}

	if deps.Config.HANodeAffinityPinEnabled() {
		if az := pinAZForNode(cp, deps.Config, node); az != "" {
			if _, ok := deps.Config.AZCandidates(az); ok {
				out = append(out, haFeatureAZPin)
			}
		}
	}

	return out
}

// warnHAResurrectorConflictOnce emits the single warning create_vm makes per
// CPI process when it registers a VM under any HA-registration feature (DLB,
// anti-affinity HA rules, or AZ node-affinity pinning). PVE HA and the BOSH
// resurrector independently detect and restart a failed guest: PVE HA
// relocates the guest to another node while the Director, seeing the
// original VMID become unresponsive, issues its own create_vm, producing a
// duplicate that conflicts with the HA-recovered guest on IP, VMID, or agent
// credentials. The CPI cannot detect the resurrector's runtime state, so it
// can only warn, not prevent the race — see docs/dlb-aware-placement.md and
// docs/ha-and-resurrection.md for the full ownership matrix and the
// `bosh update-resurrection off` remediation.
//
// One warning per CPI process is enough to alert the operator without
// flooding logs on every subsequent HA-registered create_vm — mirrors the
// vniZoneListWarnOnce once-per-process idiom (internal/pve/vni.go). The gate
// itself and its test seam live in ha_warn_seam.go.
//
// Called from checkISOStorageForHA, which already computes
// haRegistrationFeatures on every create_vm — piggybacking there avoids a
// second create_vm.go call site. A no-op when features is empty (no
// HA-registration feature fired for this VM) or logger is nil.
//
// NOTE for tests asserting on checkISOStorageForHA's logger: that function
// writes BOTH this once-per-process warning and the per-call ISO
// migration-safety warning to the same logger. Assert on the specific message
// under test rather than on buffer emptiness or length — otherwise whichever
// test reaches this gate first in the binary captures an extra line and the
// assertion becomes order-dependent.
func warnHAResurrectorConflictOnce(vmid int, features []haRegistrationFeature, logger *log.Logger) {
	if len(features) == 0 || logger == nil {
		return
	}
	haResurrectorWarnOnce.Load().Do(func() {
		featureNames := make([]string, len(features))
		for i, f := range features {
			featureNames[i] = string(f)
		}
		logger.Warn("create_vm: this VM is registered as a PVE HA resource; the BOSH resurrector "+
			"must be disabled for HA-managed deployments, or PVE HA and the resurrector can both "+
			"try to recover the same failed guest independently, producing a duplicate VM that "+
			"conflicts on IP, VMID, or agent credentials -- run `bosh update-resurrection off` "+
			"(or set resurrector_enabled: false in the Director manifest) for any deployment using "+
			"placement.dlb, placement.anti_affinity.use_ha_rules, or placement.pin_az_via_ha_rules; "+
			"see docs/ha-and-resurrection.md",
			log.Int(metadataKeyVMID, vmid),
			log.String("features", strings.Join(featureNames, ",")),
		)
	})
}

// isoStorageScanTarget returns deps.Config.ISOStorage when it is a pool
// distinct from vmStorage and deps.Config.DiskStorage, or "" otherwise.
//
// Feeds pve.WithExtraStorageScan at the create_vm VMID-allocation call sites
// (allocateVM / allocateVMForFallback in create_vm.go): pve.WithStorageScan
// there only covers vmStorage, so on a cluster whose iso_storage pool is
// ALSO shared with another independent PVE cluster (a second BOSH-Proxmox
// AZ), a VMID already holding files under images/<vmid>/ on that shared ISO
// pool — because the other cluster owns that VM or template — was
// previously invisible to this cluster's VMID allocation (see
// pve.WithStorageScan's doc comment for the general co-mingling hazard
// this closes for vmStorage).
//
// "" is a safe default: pve.WithExtraStorageScan treats an empty storage
// argument as a no-op, so a call site that always passes this helper's
// result costs nothing extra when ISOStorage is unset, matches vmStorage, or
// matches DiskStorage (already covered, or nothing to add).
func isoStorageScanTarget(deps Deps, vmStorage string) string {
	if deps.Config == nil {
		return ""
	}
	iso := deps.Config.ISOStorage
	if iso == "" || iso == vmStorage || iso == deps.Config.DiskStorage {
		return ""
	}
	return iso
}

// checkISOStorageForHA is create_vm step 10b: the config-drive ISO migration-
// safety check. The ConfigDrive ISO CD-ROM attached at scsi30 lives for the
// VM's whole life on deps.Config.ISOStorage (see internal/agent/configdrive.go),
// not only at boot. PVE refuses to live-migrate a VM whose CD-ROM volume sits
// on non-shared storage, and HA recovery on another node fails at start
// because the ISO file does not exist there — silently defeating DLB
// rebalancing, HA AZ pinning, and HA anti-affinity.
//
// A no-op (zero PVE calls) when haRegistrationFeatures reports no active
// feature: most deployments never register any VM as a PVE HA resource, so
// this check costs nothing for them. When at least one feature is active, the
// same fail-open shared-storage lookup the DLB guard uses (dlbStorageIsShared)
// classifies deps.Config.ISOStorage:
//
//   - shared, or shared-ness undeterminable (lookup error) -> no-op, matching
//     the DLB guard's fail-open-on-facts contract. A lookup failure is logged
//     at Debug, never escalated — an ISO-storage classification hiccup must
//     never fail create_vm on its own.
//   - confirmed non-shared -> logs a structured Warn naming the pool and the
//     triggering feature(s). When RequireSharedISOForHAEnabled() is true, also
//     returns a non-retriable CloudError so create_vm fails (triggering the
//     caller's rollback) instead of only warning.
func checkISOStorageForHA(
	ctx context.Context, deps Deps, vmid int, cp createVMCloudProps, node string, env map[string]any, logger *log.Logger,
) error {
	features := haRegistrationFeatures(deps, cp, node, env)
	if len(features) == 0 {
		return nil
	}

	// D11: one-per-process HA-vs-resurrector Warn. Independent of the
	// ISO-storage migration-safety check below — it fires whenever any
	// HA-registration feature is active, regardless of iso_storage sharing.
	warnHAResurrectorConflictOnce(vmid, features, logger)

	isoPool := deps.Config.ISOStorage
	if isoPool == "" {
		// ApplyDefaults always fills ISOStorage ("local" when unset), so this
		// is defensive only — never observed with a config that went through
		// the normal load path.
		return nil
	}

	shared, knownErr := dlbStorageIsShared(ctx, deps, isoPool)
	if knownErr != nil {
		logger.Debug("create_vm: iso_storage shared-ness undeterminable, skipping HA migration-safety check",
			log.Int(metadataKeyVMID, vmid), log.String("storage", isoPool), log.Err(knownErr))
		return nil
	}
	if shared {
		return nil
	}

	featureNames := make([]string, len(features))
	for i, f := range features {
		featureNames[i] = string(f)
	}
	joined := strings.Join(featureNames, ",")

	logger.Warn("create_vm: live migration and HA recovery of this VM will fail: config-drive ISO on non-shared storage",
		log.Int(metadataKeyVMID, vmid),
		log.String("storage", isoPool),
		log.String("features", joined),
	)

	if deps.Config.RequireSharedISOForHAEnabled() {
		return cpierrors.Cloud(
			"create_vm: vmid %d: live migration and HA recovery of this VM will fail: "+
				"config-drive ISO on non-shared storage %q (triggered by %s); "+
				"set pve.iso_storage to a shared storage pool, or disable %s",
			vmid, isoPool, joined, joined)
	}
	return nil
}
