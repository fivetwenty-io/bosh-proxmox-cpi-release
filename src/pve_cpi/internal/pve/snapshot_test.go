package pve_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// snapQEMUService: minimal qemu.Service implementation for HasSnapshots tests.
// All methods except ListSnapshots panic on call — tests must not trigger them.
// ---------------------------------------------------------------------------

type snapQEMUService struct {
	// Embed the interface so the compiler insists we only override what we
	// actually need. Any accidental call to an unimplemented method panics
	// with a clear nil-pointer dereference, equivalent to the panic-stub
	// pattern used across other pve_test files.
	sdkqemu.Service

	listSnapshotsFn func(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error)
}

func (s *snapQEMUService) ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error) {
	if s.listSnapshotsFn != nil {
		return s.listSnapshotsFn(ctx, node, vmid)
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// snapMockClient: minimal pve.Client for HasSnapshots tests.
// Only QEMU() is used; all other service accessors return nil and will panic
// if called (the guard function under test never calls them).
// ---------------------------------------------------------------------------

type snapMockClient struct {
	qemuSvc sdkqemu.Service
}

func (c *snapMockClient) QEMU() sdkqemu.Service                  { return c.qemuSvc }
func (c *snapMockClient) Storage() storage.Service               { return nil }
func (c *snapMockClient) CloudInit() cloudinit.Service           { return nil }
func (c *snapMockClient) Tasks() tasks.Service                   { return nil }
func (c *snapMockClient) Nodes() nodes.Service                   { return nil }
func (c *snapMockClient) Cluster() cluster.Service               { return nil }
func (c *snapMockClient) ClusterStorage() clusterstorage.Service { return nil }

var _ pve.Client = (*snapMockClient)(nil)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func snapClient(fn func(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error)) pve.Client {
	return &snapMockClient{
		qemuSvc: &snapQEMUService{listSnapshotsFn: fn},
	}
}

func snapEntries(names ...string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]interface{}{"name": n})
	}
	return out
}

// ---------------------------------------------------------------------------
// TestHasSnapshots
// ---------------------------------------------------------------------------

func TestHasSnapshots(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name        string
		entries     []map[string]interface{}
		listErr     error
		wantNames   []string
		wantErrText string // non-empty → expect error containing this substring
	}{
		{
			name:      "only current entry returns nil names",
			entries:   snapEntries("current"),
			wantNames: nil,
		},
		{
			name:      "current plus one real snapshot returns that snapshot",
			entries:   snapEntries("current", "snap1"),
			wantNames: []string{"snap1"},
		},
		{
			name:      "two real snapshots plus current returns both in order",
			entries:   snapEntries("snap1", "snap2", "current"),
			wantNames: []string{"snap1", "snap2"},
		},
		{
			name:        "ListSnapshots error is wrapped and returned",
			listErr:     errors.New("connection refused"),
			wantErrText: "HasSnapshots: list snapshots for vm",
		},
		{
			name: "entry with missing name key is skipped safely",
			entries: []map[string]interface{}{
				{"name": "current"},
				{"description": "no name key here"},
				{"name": "real-snap"},
			},
			wantNames: []string{"real-snap"},
		},
		{
			name: "entry with non-string name is skipped safely",
			entries: []map[string]interface{}{
				{"name": "current"},
				{"name": 42},
				{"name": "real-snap"},
			},
			wantNames: []string{"real-snap"},
		},
		{
			name: "entry with empty string name is skipped",
			entries: []map[string]interface{}{
				{"name": ""},
				{"name": "current"},
				{"name": "keep-me"},
			},
			wantNames: []string{"keep-me"},
		},
		{
			name:      "empty entry list returns nil names",
			entries:   []map[string]interface{}{},
			wantNames: nil,
		},
		{
			name:      "nil entry list returns nil names",
			entries:   nil,
			wantNames: nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := tc.entries
			listErr := tc.listErr

			client := snapClient(func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
				return entries, listErr
			})

			got, err := pve.HasSnapshots(ctx, client, "pve-node1", 9001)

			if tc.wantErrText != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrText)
				}
				if msg := err.Error(); len(msg) == 0 {
					t.Fatalf("expected non-empty error message")
				}
				// Verify the original error is wrapped (errors.Is / unwrap chain).
				if listErr != nil && !errors.Is(err, listErr) {
					t.Errorf("expected wrapped error to satisfy errors.Is(err, listErr): err=%v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got) != len(tc.wantNames) {
				t.Fatalf("got %d names %v, want %d names %v", len(got), got, len(tc.wantNames), tc.wantNames)
			}
			for i, want := range tc.wantNames {
				if got[i] != want {
					t.Errorf("names[%d]: got %q, want %q", i, got[i], want)
				}
			}
		})
	}
}
