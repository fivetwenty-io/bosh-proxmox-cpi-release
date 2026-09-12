package handlers

import (
	"context"
	"fmt"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func startCreatedVMAndReadConfig(ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, shape *createVMShape, vmid int, plan []nicPlanEntry) (map[string]createVMNetworkSpec, error) {
	if parsed.storageRuntime == nil {
		return startVMAndReadConfig(ctx, deps, logger, parsed, shape, vmid, plan)
	}
	status, err := deps.PVE.QEMU().Status(ctx, shape.node, vmid)
	if err != nil {
		return nil, err
	}
	switch status["status"] {
	case "running":
	case "stopped":
		if _, err = deps.PVE.QEMU().Start(ctx, shape.node, vmid); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("VM power state is indeterminate")
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, shape.node, vmid)
	if err != nil {
		return nil, err
	}
	if err := parsed.storageRuntime.verifyMarker(cfg); err != nil {
		return nil, err
	}
	return buildResponseNetworks(parsed.networks, plan, cfg), nil
}
