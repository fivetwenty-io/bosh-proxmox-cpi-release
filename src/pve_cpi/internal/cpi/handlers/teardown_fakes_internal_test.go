package handlers

import (
	"context"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// destroyedQEMUView is the QEMU service a rollback or delete fixture hands
// out once its nodes fake models the destroy: it forwards to the fixture's
// QEMU fake until gone reports a successful DeleteQemu, and answers not found
// for the config and status reads after that, the way PVE does once the VM is
// gone. The teardown's absence proof reads through it after the destroy.
type destroyedQEMUView struct {
	qemu.Service
	gone func() bool
}

func (v destroyedQEMUView) destroyed() bool { return v.gone != nil && v.gone() }

func (v destroyedQEMUView) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if v.destroyed() {
		return nil, &sdkerrors.APIError{HTTPCode: 404}
	}
	return v.Service.Config(ctx, node, vmid)
}

func (v destroyedQEMUView) Status(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if v.destroyed() {
		return nil, &sdkerrors.APIError{HTTPCode: 404}
	}
	return v.Service.Status(ctx, node, vmid)
}

// destroyStepOnlyBypassingGates runs step 5 of the teardown alone and skips
// steps 1 to 4, so a test that calls it covers the destroy's lock recovery and
// none of the HA gate, power, preserve, or verify steps. It configures the
// destroy the way delete_vm does: skiplock allowed and the operator's
// destroy_unreferenced_disks knob applied, against VM 100 on pve01 with no
// retained disk. A destroy that finds the VM gone cleans the agent's state, as
// delete_vm does. The lock-recovery tests drive the destroy's retry curve
// through it without the steps around it.
func destroyStepOnlyBypassingGates(ctx context.Context, deps Deps) (*sdknodes.DeleteQemuResponse, bool, error) {
	const (
		node = "pve01"
		vmid = 100
	)
	logger := log.NewNopLogger()
	teardown := &vmTeardown{
		caller:              "delete_vm",
		vmCID:               "vm-cid",
		vmid:                vmid,
		node:                node,
		logger:              logger,
		skiplock:            true,
		destroyUnreferenced: deps.Config != nil && deps.Config.DestroyUnreferencedDisks,
	}
	resp, alreadyGone, err := teardown.destroy(ctx, deps, false)
	if alreadyGone {
		cleanupAgentForVM(ctx, deps, node, vmid, logger)
	}
	return resp, alreadyGone, err
}
