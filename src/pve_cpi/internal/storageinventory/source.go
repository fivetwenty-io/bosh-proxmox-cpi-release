package storageinventory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// PVESource uses the existing cluster-definition and per-node status APIs.
// Neither method submits tasks, mounts storage, or changes PVE configuration.
type PVESource struct{ Client pve.Client }

// Definitions reads cluster storage definitions without changing PVE state.
func (s PVESource) Definitions(ctx context.Context) ([]json.RawMessage, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("nil PVE inventory client")
	}
	r, err := s.Client.ClusterStorage().ListStorage(ctx, nil)
	if err != nil {
		return nil, err
	}
	if r == nil || *r == nil {
		return nil, fmt.Errorf("nil cluster storage response")
	}
	return []json.RawMessage(*r), nil
}

// Statuses reads the requested node's current storage capacity.
func (s PVESource) Statuses(ctx context.Context, node string) ([]json.RawMessage, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("nil PVE inventory client")
	}
	r, err := s.Client.Nodes().ListStorage(ctx, node, nil)
	if err != nil {
		return nil, err
	}
	if r == nil || *r == nil {
		return nil, fmt.Errorf("nil node storage response")
	}
	return []json.RawMessage(*r), nil
}
