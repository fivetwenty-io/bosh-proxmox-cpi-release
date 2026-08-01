package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// PCIPassthrough describes a single host PCI device to pass through to a VM.
// The Address field must be a valid PCI bus/device/function address in the
// canonical format "DDDD:BB:SS.F" (e.g. "0000:01:00.0").
type PCIPassthrough struct {
	// Address is the PCI address as reported by /nodes/{node}/hardware/pci.
	// Required. Validated pre-clone so no orphan VM is produced on bad input.
	Address string `json:"address"`
}

// pciAddressPattern matches canonical PCI addresses: domain:bus:slot.func,
// e.g. "0000:01:00.0". Each component is hex; domain is 4 digits, bus 2,
// slot 2, func 1.
var pciAddressPattern = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

// shortPCIAddressPattern matches a domain-less PCI address ("BB:SS.F"), the
// short form some PVE versions report in /nodes/{node}/hardware/pci ids.
var shortPCIAddressPattern = regexp.MustCompile(`^[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)

// normalizePCIAddress lowercases a PCI address and pads a missing domain with
// the default "0000:" so the operator's canonical "0000:01:00.0" matches a PVE
// hardware id reported in short form "01:00.0". Used on both sides of the
// presence comparison in buildPCIChecker.
func normalizePCIAddress(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if shortPCIAddressPattern.MatchString(s) {
		return "0000:" + s
	}
	return s
}

// validatePCIPassthroughs checks each entry in pts for a well-formed PCI
// address. Returns a non-retriable CloudError on the first invalid entry.
// Called from parseCreateVMArgs before any VM is created so bad input never
// produces an orphan.
func validatePCIPassthroughs(pts []PCIPassthrough) error {
	for i, pt := range pts {
		if pt.Address == "" {
			return cpierrors.Cloud(
				"create_vm: pci_passthroughs[%d].address is empty; provide a PCI address in DDDD:BB:SS.F format",
				i,
			)
		}
		if !pciAddressPattern.MatchString(pt.Address) {
			return cpierrors.Cloud(
				"create_vm: pci_passthroughs[%d].address %q is not a valid PCI address; "+
					"expected format DDDD:BB:SS.F (e.g. 0000:01:00.0)",
				i, pt.Address,
			)
		}
	}
	return nil
}

// buildPCIChecker returns a closure that checks whether the named node exposes
// all requested PCI addresses by calling ListHardwarePci. The closure uses ctx
// and the nodes service captured from the call site.
//
// ListHardwarePci returns []json.RawMessage; each entry is decoded to extract
// the "id" field (PVE device address string). Entries missing "id" are skipped
// gracefully. An API error causes the check to return (false, err), which the
// placement.Filter treats as a rejection (fail-safe).
func buildPCIChecker(ctx context.Context, nodeSvc nodesServiceForPCI, addrs []string) func(string) (bool, error) {
	return func(node string) (bool, error) {
		resp, err := nodeSvc.ListHardwarePci(ctx, node, nil)
		if err != nil {
			return false, fmt.Errorf("ListHardwarePci on node %q: %w", node, err)
		}
		if resp == nil {
			return false, nil
		}
		present := make(map[string]bool, len(*resp))
		for _, raw := range *resp {
			var entry struct {
				ID string `json:"id"`
			}
			if decErr := json.Unmarshal(raw, &entry); decErr != nil || entry.ID == "" {
				continue
			}
			present[normalizePCIAddress(entry.ID)] = true
		}
		for _, addr := range addrs {
			if !present[normalizePCIAddress(addr)] {
				return false, nil
			}
		}
		return true, nil
	}
}

// verifyPCIOnNode confirms the resolved target node exposes every requested PCI
// device before any clone/import mutation. The placement filter already checks
// candidates when live placement scoring runs, but several node-resolution
// paths bypass that filter entirely: operator target_node, local-disk pin,
// the config.node fallback after AZ exhaustion, and placement-disabled static
// configs. attemptCreateVM calls this guard for every path so a PCI VM is
// never created on a node that lacks the device.
//
// Empty pts is a no-op (no API call; byte-identical path). A missing device is
// a non-retriable CloudError — the operator must fix the address or the node
// constraint. A ListHardwarePci failure is retriable: a transient API fault
// must not permanently fail the deploy.
func verifyPCIOnNode(
	ctx context.Context,
	deps Deps,
	node string,
	pts []PCIPassthrough,
	logger *log.Logger,
) error {
	if len(pts) == 0 {
		return nil
	}
	addrs := make([]string, len(pts))
	for i, pt := range pts {
		addrs[i] = pt.Address
	}
	present, err := buildPCIChecker(ctx, deps.PVE.Nodes(), addrs)(node)
	if err != nil {
		return cpierrors.Retriable("create_vm: PCI device check on node %q: %s", node, err.Error())
	}
	if !present {
		return cpierrors.Cloud(
			"create_vm: node %q does not expose all requested PCI devices %s; "+
				"verify pci_passthroughs addresses against /nodes/%s/hardware/pci "+
				"and any target_node/config.node/disk placement constraint pinning this node",
			node, strings.Join(addrs, ","), node,
		)
	}
	logger.Debug("create_vm: PCI devices verified on target node",
		log.String("node", node),
		log.Int("device_count", len(addrs)),
	)
	return nil
}

// nodesServiceForPCI is the minimal interface needed by buildPCIChecker.
// Satisfied by the full pve.Nodes() service.
type nodesServiceForPCI interface {
	ListHardwarePci(ctx context.Context, node string, params *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error)
}

// applyPCIPassthrough sets hostpci0..hostpciN on the VM after clone via a
// single UpdateQemuConfig call. The pts slice has already been validated by
// validatePCIPassthroughs. On error the caller (inside attemptCreateVM) must
// call cleanupVM before returning, following the same contract as
// applyPVEConfigWithCleanup.
//
// Empty pts is a no-op (byte-identical; no API call).
func applyPCIPassthrough(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	pts []PCIPassthrough,
	logger *log.Logger,
) error {
	if len(pts) == 0 {
		return nil
	}

	hostpci := make(map[int]string, len(pts))
	for i, pt := range pts {
		hostpci[i] = pt.Address
	}

	vmidStr := strconv.Itoa(vmid)
	params := &sdknodes.UpdateQemuConfigParams{
		Hostpci: hostpci,
	}
	if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmidStr, params); err != nil {
		return cpierrors.Wrap(err,
			fmt.Sprintf("create_vm: apply PCI passthrough to vmid=%d: %s", vmid, err.Error()))
	}

	logger.Info("create_vm: applied PCI passthrough",
		log.Int(metadataKeyVMID, vmid),
		log.Int("device_count", len(pts)),
	)
	return nil
}

// applyPCIPassthroughWithCleanup calls applyPCIPassthrough and, on any error,
// destroys the candidate VM before returning. Used inside attemptCreateVM where
// rollbackOnExit is not yet armed.
func applyPCIPassthroughWithCleanup(
	ctx context.Context,
	deps Deps,
	node string,
	candidate int,
	pts []PCIPassthrough,
	logger *log.Logger,
) error {
	if err := applyPCIPassthrough(ctx, deps, node, candidate, pts, logger); err != nil {
		cleanupVMDetached(ctx, deps, node, candidate, logger)
		return err
	}
	return nil
}

// applyPostCloneConfig applies the two post-clone configuration steps that run
// at the end of every clone/import path in attemptCreateVM:
//  1. pve_config passthrough (pre-validated; cleanup on API fault).
//  2. PCI passthrough (hostpci0..N; cleanup on API fault).
//
// Both are no-ops when the respective cloud_properties are absent/empty.
// Extracting these steps reduces cognitive complexity in attemptCreateVM.
func applyPostCloneConfig(
	ctx context.Context,
	deps Deps,
	node string,
	candidate int,
	parsed *createVMParsedArgs,
	logger *log.Logger,
) error {
	if err := applyPVEConfigWithCleanup(ctx, deps, node, candidate, parsed.cloudProps.PVEConfig, logger); err != nil {
		return err
	}
	return applyPCIPassthroughWithCleanup(ctx, deps, node, candidate, parsed.cloudProps.PCIPassthroughs, logger)
}

// applyPCINodeAffinityPin writes a strict single-node HA pin when the VM has
// PCI passthrough configured. PCI passthrough blocks live migration; a strict
// node pin makes that constraint durable across HA failover and DLB rebalance.
//
// When pts is empty this is a no-op (byte-identical path). Errors follow the
// same selective propagation as applyAZNodeAffinityPin: retriable errors
// (lock-timeout, verify failure) propagate; generic HA-API failures are logged
// as non-fatal warnings.
func applyPCINodeAffinityPin(
	ctx context.Context,
	deps Deps,
	vmid int,
	node string,
	pts []PCIPassthrough,
	logger *log.Logger,
) error {
	if len(pts) == 0 {
		return nil
	}
	// PCI passthrough requires a strict single-node pin so HA cannot relocate
	// the VM to a node that may lack the device.
	if pinErr := ensureNodeAffinityPin(ctx, deps, vmid, []string{node}, true, logger); pinErr != nil {
		if cpierrors.IsType(pinErr, cpierrors.TypeRetriableCloud) {
			return pinErr
		}
		logger.Warn("create_vm: PCI strict node-affinity pin not fully applied (non-fatal)",
			log.Int(metadataKeyVMID, vmid),
			log.String("node", node),
			log.Err(pinErr),
		)
	}
	return nil
}
