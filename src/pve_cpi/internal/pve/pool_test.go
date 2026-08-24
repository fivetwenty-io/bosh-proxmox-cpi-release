package pve_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakePoolServiceForEnsure implements pve.PoolService, wiring only CreatePool
// (the only method EnsurePoolExists calls); every other method panics on an
// unexpected call so a test that never exercises them stays honest about it.
type fakePoolServiceForEnsure struct {
	createPoolFn func(ctx context.Context, poolID, comment string) error
	createCalls  int
	lastPoolID   string
	lastComment  string
}

func (f *fakePoolServiceForEnsure) AddVM(_ context.Context, _ string, _ int64) error {
	panic("fakePoolServiceForEnsure: AddVM unexpected call")
}

func (f *fakePoolServiceForEnsure) MoveVMToPool(_ context.Context, _ string, _ int64) error {
	panic("fakePoolServiceForEnsure: MoveVMToPool unexpected call")
}

func (f *fakePoolServiceForEnsure) CreatePool(ctx context.Context, poolID, comment string) error {
	f.createCalls++
	f.lastPoolID = poolID
	f.lastComment = comment
	if f.createPoolFn != nil {
		return f.createPoolFn(ctx, poolID, comment)
	}
	return nil
}

func (f *fakePoolServiceForEnsure) DeletePool(_ context.Context, _ string) error {
	panic("fakePoolServiceForEnsure: DeletePool unexpected call")
}

func (f *fakePoolServiceForEnsure) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	panic("fakePoolServiceForEnsure: GetPoolComment unexpected call")
}

var _ pve.PoolService = (*fakePoolServiceForEnsure)(nil)

// fakeEnsureClient implements pve.Client, exposing only a configurable Pools()
// return value. The other service accessors are never called by
// EnsurePoolExists and return nil.
type fakeEnsureClient struct {
	pools pve.PoolService
}

func (c *fakeEnsureClient) QEMU() qemu.Service                     { return nil }
func (c *fakeEnsureClient) Storage() storage.Service               { return nil }
func (c *fakeEnsureClient) CloudInit() cloudinit.Service           { return nil }
func (c *fakeEnsureClient) Tasks() tasks.Service                   { return nil }
func (c *fakeEnsureClient) Nodes() nodes.Service                   { return nil }
func (c *fakeEnsureClient) Cluster() cluster.Service               { return nil }
func (c *fakeEnsureClient) ClusterStorage() clusterstorage.Service { return nil }
func (c *fakeEnsureClient) Pools() pve.PoolService                 { return c.pools }

var _ pve.Client = (*fakeEnsureClient)(nil)

// ---------------------------------------------------------------------------
// EnsurePoolExists
// ---------------------------------------------------------------------------

func TestEnsurePoolExists_CreatesWhenAbsent(t *testing.T) {
	t.Parallel()

	fp := &fakePoolServiceForEnsure{
		createPoolFn: func(_ context.Context, _, _ string) error { return nil },
	}
	client := &fakeEnsureClient{pools: fp}

	err := pve.EnsurePoolExists(context.Background(), client, "bosh", pve.PoolProvenanceComment, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fp.createCalls != 1 {
		t.Fatalf("CreatePool calls = %d; want 1", fp.createCalls)
	}
	if fp.lastPoolID != "bosh" {
		t.Errorf("CreatePool poolID = %q; want %q", fp.lastPoolID, "bosh")
	}
	if fp.lastComment != pve.PoolProvenanceComment {
		t.Errorf("CreatePool comment = %q; want %q", fp.lastComment, pve.PoolProvenanceComment)
	}
}

func TestEnsurePoolExists_ToleratesDuplicate500(t *testing.T) {
	t.Parallel()

	// Live PVE 9.2.4 shape: HTTP 500 wrapping perl die() text, never 409.
	dupErr := errors.New("create pool failed: pool 'bosh' already exists\n") //nolint:revive // verbatim live PVE error text incl. trailing newline
	fp := &fakePoolServiceForEnsure{
		createPoolFn: func(_ context.Context, _, _ string) error { return dupErr },
	}
	client := &fakeEnsureClient{pools: fp}

	err := pve.EnsurePoolExists(context.Background(), client, "bosh", "", nil)
	if err != nil {
		t.Fatalf("duplicate-pool creation should be tolerated as success; got %v", err)
	}
	if fp.createCalls != 1 {
		t.Fatalf("CreatePool calls = %d; want 1", fp.createCalls)
	}
}

func TestEnsurePoolExists_PropagatesOtherError(t *testing.T) {
	t.Parallel()

	permErr := makeAPIErr(403, "permission denied - insufficient privileges")
	fp := &fakePoolServiceForEnsure{
		createPoolFn: func(_ context.Context, _, _ string) error { return permErr },
	}
	client := &fakeEnsureClient{pools: fp}

	err := pve.EnsurePoolExists(context.Background(), client, "bosh", "", nil)
	if err == nil {
		t.Fatal("expected error to propagate; got nil")
	}
}

func TestEnsurePoolExists_NilPoolService(t *testing.T) {
	t.Parallel()

	client := &fakeEnsureClient{pools: nil}

	err := pve.EnsurePoolExists(context.Background(), client, "bosh", "", nil)
	if err == nil {
		t.Fatal("expected error for a client with no pool service; got nil (and no panic, which is the point)")
	}
}

// ---------------------------------------------------------------------------
// PoolProvenance
// ---------------------------------------------------------------------------

func TestPoolProvenance_WithAndWithoutDirector(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		director string
		want     string
	}{
		{"no director", "", pve.PoolProvenanceComment},
		{"with director", "bosh-lite", pve.PoolProvenanceComment + " (director bosh-lite)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pve.PoolProvenance(tc.director); got != tc.want {
				t.Errorf("PoolProvenance(%q) = %q; want %q", tc.director, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsCPIManagedPoolComment
// ---------------------------------------------------------------------------

func TestIsCPIManagedPoolComment_AcceptsCurrentAndLegacyPrefixes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		comment string
		want    bool
	}{
		{"current bare", "managed by bosh-proxmox-cpi", true},
		{"current with director", "managed by bosh-proxmox-cpi (director d1)", true},
		{"legacy bare", "managed by bosh-pve-cpi", true},
		{"legacy with director", "managed by bosh-pve-cpi (director d1)", true},
		{"operator pool", "my ops pool", false},
		{"empty", "", false},
		{"prefix not at start", "pool managed by bosh-proxmox-cpi", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pve.IsCPIManagedPoolComment(tc.comment); got != tc.want {
				t.Errorf("IsCPIManagedPoolComment(%q) = %v; want %v", tc.comment, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsPoolPermissionDenied
// ---------------------------------------------------------------------------

func TestIsPoolPermissionDenied(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"403 APIError", &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied", Code: 403}, true},
		{"401 APIError", &sdkerrors.APIError{HTTPCode: 401, Message: "authentication failure", Code: 401}, true},
		{"404 APIError (pool not found -- not a permission issue)", &sdkerrors.APIError{HTTPCode: 404, Message: "no such pool"}, false},
		{"500 APIError (transient server fault)", &sdkerrors.APIError{HTTPCode: 500, Message: "worker busy"}, false},
		{"ErrForbidden sentinel", sdkerrors.ErrForbidden, true},
		{"ErrUnauthorized sentinel", sdkerrors.ErrUnauthorized, true},
		{"PermissionError", &sdkerrors.PermissionError{What: "Pool.Audit"}, true},
		{"AuthenticationError", &sdkerrors.AuthenticationError{Realm: "pam"}, true},
		{"plain non-SDK error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pve.IsPoolPermissionDenied(tc.err); got != tc.want {
				t.Errorf("IsPoolPermissionDenied(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (f *fakePoolServiceForEnsure) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}
