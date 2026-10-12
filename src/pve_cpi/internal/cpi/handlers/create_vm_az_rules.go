// create_vm_az_rules.go holds the availability-zone rule helpers shared by the
// plain placement path (resolveTargetNodeWithFallbacks) and the storage-plan
// path (ObserveStoragePlanCompute). Every rule that can run in warn or strict
// mode goes through azViolation, so the two modes cannot drift.
package handlers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// azRejectionMissingNode is the reason recorded for a node that az_map lists
// for an AZ but that the cluster does not report, which is what an az_map typo
// looks like.
const azRejectionMissingNode = "node not found in the cluster"

// azViolationInfo describes one availability-zone rule violation.
//
// ErrorMessage is the full text of the error returned under az_enforcement
// strict. WarnMessage is the log line written under warn, and Fields carry the
// structured detail for that line. Retriable selects the error class under
// strict.
type azViolationInfo struct {
	ErrorMessage string
	WarnMessage  string
	Fields       []log.Field
	Retriable    bool
}

// azViolation applies the configured az_enforcement mode to a violation. Under
// strict it returns the error. Under warn it logs the Warn line and returns nil
// so the caller continues with its existing behavior.
func azViolation(cfg *config.CPIConfig, logger *log.Logger, v azViolationInfo) error {
	if cfg.AZEnforcementStrict() {
		if v.Retriable {
			return cpierrors.Retriable("%s", v.ErrorMessage)
		}
		return cpierrors.Cloud("%s", v.ErrorMessage)
	}
	logger.Warn(v.WarnMessage, v.Fields...)
	return nil
}

// onlyDLBSentinel reports whether every entry of azOrder is the DLB sentinel
// AZ. DLB deliberately un-pins guests, so a sentinel-only order keeps the
// config.node fallback. An order that mixes the sentinel with real AZs is not
// sentinel-only: the sentinel contributes no candidates and enforcement applies
// to the real AZs. An empty order returns false.
func onlyDLBSentinel(azOrder []string, cfg *config.CPIConfig) bool {
	sentinel := cfg.DLBAZName()
	if sentinel == "" || len(azOrder) == 0 {
		return false
	}
	for _, az := range azOrder {
		if az != sentinel {
			return false
		}
	}
	return true
}

// azRejectionRecord is the set of rejection reasons for one AZ attempt. Nodes
// holds the AZ's az_map list in operator order (nil for the no-AZ attempt) and
// Reasons holds a reason for each listed node that could not host the VM, so a
// reason recorded while filtering one AZ never leaks into another.
type azRejectionRecord struct {
	AZ      string
	Nodes   []string
	Reasons map[string]string
}

// newAZRejectionRecord builds the record for one AZ attempt. It keeps only the
// reasons for nodes in the AZ's node list and marks a listed node that is
// absent from the cluster facts with azRejectionMissingNode. When nodes is nil
// (no AZ requested) every rejection is kept.
func newAZRejectionRecord(az string, nodes []string, rejections map[string]string, inCluster map[string]struct{}) azRejectionRecord {
	rec := azRejectionRecord{AZ: az, Nodes: nodes, Reasons: make(map[string]string)}
	if nodes == nil {
		for n, reason := range rejections {
			rec.Reasons[n] = reason
		}
		return rec
	}
	for _, n := range nodes {
		if _, present := inCluster[n]; !present {
			rec.Reasons[n] = azRejectionMissingNode
			continue
		}
		if reason, rejected := rejections[n]; rejected {
			rec.Reasons[n] = reason
		}
	}
	return rec
}

// azRecordsRetriable reports whether every record's own reasons are transient,
// so one AZ's permanent reason is never hidden by another AZ's transient one.
func azRecordsRetriable(records []azRejectionRecord) bool {
	if len(records) == 0 {
		return true
	}
	for _, rec := range records {
		if !classifyFilterResult(rec.Reasons) {
			return false
		}
	}
	return true
}

// flattenAZRecords merges the records into one node-to-reason map, for the
// no-config.node error path that has no AZ to blame.
func flattenAZRecords(records []azRejectionRecord) map[string]string {
	flat := make(map[string]string)
	for _, rec := range records {
		mergeRejections(flat, rec.Reasons)
	}
	return flat
}

// formatAZExhaustion describes every AZ tried, its node list, and each node's
// rejection reason, in the order the AZs were tried.
func formatAZExhaustion(records []azRejectionRecord) string {
	parts := make([]string, 0, len(records))
	for _, rec := range records {
		reasons := make([]string, 0, len(rec.Reasons))
		for _, n := range rec.Nodes {
			if reason, ok := rec.Reasons[n]; ok {
				reasons = append(reasons, fmt.Sprintf("%s: %s", n, reason))
			}
		}
		parts = append(parts, fmt.Sprintf("no node in availability_zone %q (%s) can host the VM (%s)",
			rec.AZ, strings.Join(rec.Nodes, ", "), strings.Join(reasons, "; ")))
	}
	return strings.Join(parts, "; ")
}

// azExhaustionViolation builds the violation for an availability-zone order
// that ran out of viable nodes. fallback is the config.node value the warn
// mode places the VM on.
func azExhaustionViolation(records []azRejectionRecord, fallback string) azViolationInfo {
	detail := formatAZExhaustion(records)
	azs := make([]string, 0, len(records))
	for _, rec := range records {
		azs = append(azs, rec.AZ)
	}
	sort.Strings(azs)
	return azViolationInfo{
		ErrorMessage: "create_vm: " + detail + ". Free capacity in the availability zone, " +
			"or set az_enforcement to warn in this placement block to allow placement on pve.node",
		WarnMessage: "create_vm: placement: no viable candidates in the requested availability zones; " +
			"falling back to config.node (set az_enforcement to strict to fail instead)",
		Fields: []log.Field{
			log.String("fallback", fallback),
			log.String("availability_zones", strings.Join(azs, ",")),
			log.String("rejections", detail),
		},
		Retriable: azRecordsRetriable(records),
	}
}

// warnAZIgnoredPlacementDisabled logs a Warn when placement is disabled and the
// request names an AZ that az_map defines, because the AZ no longer influences
// the node. It never fails the create: operators who disabled placement and
// left availability_zone in a vm_type would otherwise break on upgrade.
func warnAZIgnoredPlacementDisabled(cfg *config.CPIConfig, logger *log.Logger, azOrder []string, node string) {
	var mapped []string
	for _, az := range azOrder {
		if _, ok := cfg.AZCandidates(az); ok {
			mapped = append(mapped, az)
		}
	}
	if len(mapped) == 0 {
		return
	}
	logger.Warn("create_vm: placement: placement is disabled, so the requested availability zone is ignored; "+
		"the VM goes to config.node (enable placement to honor az_map)",
		log.String("availability_zones", strings.Join(mapped, ",")),
		log.String("node", node),
	)
}
