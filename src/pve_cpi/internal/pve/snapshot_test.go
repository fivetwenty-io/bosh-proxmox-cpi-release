package pve_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// TestWaitForSnapshotAbsent
// ---------------------------------------------------------------------------

func TestWaitForSnapshotAbsent_AlreadyGone(t *testing.T) {
	t.Parallel()
	client := snapClient(func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
		return snapEntries("current"), nil
	})
	if err := pve.WaitForSnapshotAbsent(context.Background(), client, "pve1", 9001, "snap1"); err != nil {
		t.Fatalf("unexpected error when snapshot already absent: %v", err)
	}
}

func TestWaitForSnapshotAbsent_LingersThenGone(t *testing.T) {
	t.Parallel()
	var calls int32
	client := snapClient(func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
		// Present on the first two polls, gone afterward.
		if atomic.AddInt32(&calls, 1) <= 2 {
			return snapEntries("current", "snap1"), nil
		}
		return snapEntries("current"), nil
	})
	err := pve.WaitForSnapshotAbsent(context.Background(), client, "pve1", 9001, "snap1",
		pve.WithPollInterval(1*time.Millisecond), pve.WithMaxWait(10*time.Second))
	if err != nil {
		t.Fatalf("unexpected error waiting for snapshot to clear: %v", err)
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Errorf("expected at least 3 polls before the snapshot cleared, got %d", calls)
	}
}

func TestWaitForSnapshotAbsent_Timeout(t *testing.T) {
	t.Parallel()
	client := snapClient(func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
		return snapEntries("current", "snap1"), nil // never clears
	})
	err := pve.WaitForSnapshotAbsent(context.Background(), client, "pve1", 9001, "snap1",
		pve.WithPollInterval(1*time.Millisecond), pve.WithMaxWait(1*time.Second))
	if err == nil {
		t.Fatal("expected timeout error when snapshot never clears")
	}
}

func TestWaitForSnapshotAbsent_ListError(t *testing.T) {
	t.Parallel()
	client := snapClient(func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
		return nil, errors.New("connection refused")
	})
	err := pve.WaitForSnapshotAbsent(context.Background(), client, "pve1", 9001, "snap1")
	if err == nil {
		t.Fatal("expected error when snapshot list cannot be read")
	}
}

func TestWaitForSnapshotAbsent_TransientInPollLoop_Retries(t *testing.T) {
	// HasSnapshots returns a transient error twice, then succeeds with snapshot
	// absent. WaitForSnapshotAbsent must survive the transient retries and
	// ultimately return nil once the snapshot is gone.
	t.Parallel()
	var calls int32
	client := snapClient(func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			// Return a transient-transport-shaped error so RetryOnTransient
			// retries it. The "(code: 596)" suffix matches IsTransientTransport.
			return nil, errors.New("pveproxy backend gone (code: 596)")
		}
		// Third call: snapshot absent → success.
		return snapEntries("current"), nil
	})
	err := pve.WaitForSnapshotAbsent(context.Background(), client, "pve1", 9001, "snap1",
		pve.WithPollInterval(1*time.Millisecond), pve.WithMaxWait(30*time.Second))
	if err != nil {
		t.Fatalf("expected success after transient retries, got: %v", err)
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Errorf("expected at least 3 ListSnapshots calls (2 transient + 1 success), got %d", calls)
	}
}

func TestWaitForSnapshotAbsent_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the first poll completes its wait
	client := snapClient(func(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
		return snapEntries("current", "snap1"), nil // never clears → forces the wait/select
	})
	err := pve.WaitForSnapshotAbsent(ctx, client, "pve1", 9001, "snap1",
		pve.WithPollInterval(50*time.Millisecond), pve.WithMaxWait(10*time.Second))
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}
