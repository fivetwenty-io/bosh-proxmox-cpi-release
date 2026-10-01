package handlers

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// SlotDeleteNodes is the nodes seam for a test whose VM configs live in a
// QEMU fake. Where the SDK's DetachDisk used to run, the slot-delete helper
// sends a raw config delete and then reads the pending endpoint, so this
// routes a delete-only UpdateQemuConfig to Delete, which edits the fake's own
// config, and serves ListQemuPending from Config through
// PendingFromConfigRead. Every other call goes to the embedded service, which
// may be nil when the test makes no other nodes call.
//
// Delete removes the key and nothing else, the way PVE removes a slot whose
// volume the VM doesn't own, so the seam never stands in for the SDK's
// unused-entry sweep.
//
// It's exported so the external test package can use it too.
type SlotDeleteNodes struct {
	nodes.Service
	Config func(ctx context.Context, node string, vmid int) (map[string]any, error)
	Delete func(node string, vmid int, key string) error
}

// UpdateQemuConfig sends a delete-only write to Delete and anything else to
// the embedded service.
func (n *SlotDeleteNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, params *nodes.UpdateQemuConfigParams) error {
	if IsDeleteOnlyWrite(params) {
		id, err := strconv.Atoi(vmid)
		if err != nil {
			return fmt.Errorf("fake slot delete: vmid %q: %w", vmid, err)
		}
		return n.Delete(node, id, *params.Delete)
	}
	if n.Service == nil {
		panic(fmt.Sprintf("SlotDeleteNodes.UpdateQemuConfig: unexpected write to vm %s", vmid))
	}
	return n.Service.UpdateQemuConfig(ctx, node, vmid, params)
}

// ListQemuPending serves the pending read from the fake's config read.
func (n *SlotDeleteNodes) ListQemuPending(ctx context.Context, node, vmid string) (*nodes.ListQemuPendingResponse, error) {
	return PendingFromConfigRead(ctx, n.Config, node, vmid)
}

// IsDeleteOnlyWrite reports whether params carries a delete and nothing else
// but a digest, which is the write the slot-delete helper sends.
func IsDeleteOnlyWrite(params *nodes.UpdateQemuConfigParams) bool {
	if params == nil || params.Delete == nil {
		return false
	}
	rest := *params
	rest.Delete, rest.Digest = nil, nil
	return reflect.DeepEqual(rest, nodes.UpdateQemuConfigParams{})
}
