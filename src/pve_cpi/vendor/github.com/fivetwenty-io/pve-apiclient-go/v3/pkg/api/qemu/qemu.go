package qemu

import (
	"context"
	"fmt"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"
)

// Service defines QEMU VM helpers.
type Service interface {
	Create(ctx context.Context, node string, params map[string]interface{}) (string, error)
	Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	Status(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	Start(ctx context.Context, node string, vmid int) (string, error)
	Stop(ctx context.Context, node string, vmid int) (string, error)
	Reset(ctx context.Context, node string, vmid int) (string, error)
	Clone(ctx context.Context, node string, vmid int, params map[string]interface{}) (string, error)
	Template(ctx context.Context, node string, vmid int) (string, error)
	AttachDisk(ctx context.Context, node string, vmid int, volid string, bus string, opts *AttachOpts) (string, error)
	DetachDisk(ctx context.Context, node string, vmid int, diskID string) error
	ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error)
	Snapshot(ctx context.Context, node string, vmid int, name string, opts map[string]interface{}) (string, error)
	DeleteSnapshot(ctx context.Context, node string, vmid int, name string) error
	ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error)
	RollbackSnapshot(ctx context.Context, node string, vmid int, name string) (string, error)
}

type service struct {
	c client.Client
}

// New returns a new QEMU service.
//
//nolint:ireturn // Factory pattern - returns interface to encapsulate implementation and enable mocking
func New(c client.Client) Service { return &service{c: c} }

func (s *service) Create(ctx context.Context, node string, params map[string]interface{}) (string, error) {
	data, err := s.c.PostCtx(ctx, fmt.Sprintf("/nodes/%s/qemu", node), params)
	if err != nil {
		return "", fmt.Errorf("failed to create VM: %w", err)
	}

	if upid, ok := data.(string); ok {
		return upid, nil
	}

	if m, ok := data.(map[string]interface{}); ok {
		if v, ok := m["upid"].(string); ok {
			return v, nil
		}
	}

	return "", nil
}

func (s *service) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	data, err := s.c.GetCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM config: %w", err)
	}

	if m, ok := data.(map[string]interface{}); ok {
		return m, nil
	}

	return map[string]interface{}{}, nil
}

func (s *service) Status(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	data, err := s.c.GetCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM status: %w", err)
	}

	if m, ok := data.(map[string]interface{}); ok {
		return m, nil
	}

	return map[string]interface{}{}, nil
}

func (s *service) Start(ctx context.Context, node string, vmid int) (string, error) {
	return s.postUPID(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", node, vmid), nil)
}
func (s *service) Stop(ctx context.Context, node string, vmid int) (string, error) {
	return s.postUPID(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", node, vmid), nil)
}
func (s *service) Reset(ctx context.Context, node string, vmid int) (string, error) {
	return s.postUPID(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/reset", node, vmid), nil)
}
func (s *service) Clone(ctx context.Context, node string, vmid int, params map[string]interface{}) (string, error) {
	return s.postUPID(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, vmid), params)
}
func (s *service) Template(ctx context.Context, node string, vmid int) (string, error) {
	return s.postUPID(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/template", node, vmid), nil)
}

func (s *service) Snapshot(ctx context.Context, node string, vmid int, name string, opts map[string]interface{}) (string, error) {
	if opts == nil {
		opts = map[string]interface{}{}
	}

	opts["snapname"] = name

	return s.postUPID(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot", node, vmid), opts)
}

func (s *service) DeleteSnapshot(ctx context.Context, node string, vmid int, name string) error {
	_, err := s.c.DeleteCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot/%s", node, vmid, name), nil)
	if err != nil {
		return fmt.Errorf("failed to delete snapshot: %w", err)
	}

	return nil
}

func (s *service) ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error) {
	data, err := s.c.GetCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot", node, vmid), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshots for VM %d on node %q: %w", vmid, node, err)
	}
	// PVE returns a list; normalize to []map[string]interface{}
	if list, ok := data.([]interface{}); ok {
		out := make([]map[string]interface{}, 0, len(list))
		for _, it := range list {
			if m, ok := it.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}

		return out, nil
	}

	return []map[string]interface{}{}, nil
}

func (s *service) RollbackSnapshot(ctx context.Context, node string, vmid int, name string) (string, error) {
	return s.postUPID(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot/%s/rollback", node, vmid, name), nil)
}

func (s *service) postUPID(ctx context.Context, path string, params map[string]interface{}) (string, error) {
	data, err := s.c.PostCtx(ctx, path, params)
	if err != nil {
		return "", fmt.Errorf("failed to execute QEMU operation: %w", err)
	}

	if upid, ok := data.(string); ok {
		return upid, nil
	}

	if m, ok := data.(map[string]interface{}); ok {
		if v, ok := m["upid"].(string); ok {
			return v, nil
		}
	}

	return "", nil
}
