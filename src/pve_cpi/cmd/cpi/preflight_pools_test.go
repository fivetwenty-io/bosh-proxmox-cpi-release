package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// fakePreflightPoolService implements pve.PoolService with a scriptable
// GetPoolComment. AddVM/CreatePool/DeletePool panic on any call: the
// preflight probe must never mutate PVE state.
type fakePreflightPoolService struct {
	// getPoolCommentFn is called once per probed pool. Nil means "return
	// (found=false, nil)" -- an always-visible-but-nonexistent pool.
	getPoolCommentFn func(poolID string) (string, bool, error)
	calls            []string
}

func (f *fakePreflightPoolService) AddVM(context.Context, string, int64) error {
	panic("preflightPoolAccess must never call AddVM")
}

func (f *fakePreflightPoolService) MoveVMToPool(context.Context, string, int64) error {
	panic("preflightPoolAccess must never call MoveVMToPool")
}

func (f *fakePreflightPoolService) CreatePool(context.Context, string, string) error {
	panic("preflightPoolAccess must never call CreatePool")
}

func (f *fakePreflightPoolService) DeletePool(context.Context, string) error {
	panic("preflightPoolAccess must never call DeletePool")
}

func (f *fakePreflightPoolService) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	f.calls = append(f.calls, poolID)
	if f.getPoolCommentFn == nil {
		return "", false, nil
	}
	return f.getPoolCommentFn(poolID)
}

var _ pve.PoolService = (*fakePreflightPoolService)(nil)

// preflightTestClient wraps nilPVEClient, overriding only Pools() so
// preflightPoolAccess sees a configured pool service while every other
// service stays nil (unused by the probe).
type preflightTestClient struct {
	nilPVEClient
	pools pve.PoolService
}

func (c preflightTestClient) Pools() pve.PoolService { return c.pools }

var _ pve.Client = preflightTestClient{}

func TestPreflightPoolAccess_SkipsWhenBothPoolsEmpty(t *testing.T) {
	t.Parallel()
	fake := &fakePreflightPoolService{}
	cfg := &config.CPIConfig{}
	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected zero PVE calls when both pools are empty, got %v", fake.calls)
	}
}

func TestPreflightPoolAccess_NilCfgLoggerClientPoolsAreNoOps(t *testing.T) {
	t.Parallel()
	fake := &fakePreflightPoolService{}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	logger := log.NewNopLogger()

	if err := preflightPoolAccess(context.Background(), nil, preflightTestClient{pools: fake}, logger); err != nil {
		t.Errorf("nil cfg: unexpected error: %v", err)
	}
	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, nil); err != nil {
		t.Errorf("nil logger: unexpected error: %v", err)
	}
	if err := preflightPoolAccess(context.Background(), cfg, nil, logger); err != nil {
		t.Errorf("nil client: unexpected error: %v", err)
	}
	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: nil}, logger); err != nil {
		t.Errorf("nil Pools(): unexpected error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected zero PVE calls across the nil-guard cases, got %v", fake.calls)
	}
}

func TestPreflightPoolAccess_VisiblePool_NoError(t *testing.T) {
	t.Parallel()
	fake := &fakePreflightPoolService{
		getPoolCommentFn: func(string) (string, bool, error) { return "managed by bosh-pve-cpi", true, nil },
	}
	cfg := &config.CPIConfig{VMPool: "bosh", StemcellTemplatePool: "bosh-templates"}
	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Errorf("expected 2 probes (vm_pool + stemcell_template_pool), got %v", fake.calls)
	}
}

func TestPreflightPoolAccess_NotYetExistingPool_NoError(t *testing.T) {
	t.Parallel()
	// found=false, err=nil -- the pool does not exist yet; the CPI creates it
	// lazily on first use. Must not fail the preflight.
	fake := &fakePreflightPoolService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, nil },
	}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error for a not-yet-existing pool: %v", err)
	}
}

// TestPreflightPoolAccess_NotYetExistingPool_LogsQuietDebug pins the log level
// for the normal zero-config first boot, where neither shipped default pool
// exists yet. That state must be reported at Debug and say lazy creation
// handles it -- an operator seeing a Warn here cannot tell it apart from a real
// API fault, which is exactly what the live run reported.
func TestPreflightPoolAccess_NotYetExistingPool_LogsQuietDebug(t *testing.T) {
	t.Parallel()
	fake := &fakePreflightPoolService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, nil },
	}
	cfg := &config.CPIConfig{VMPool: "bosh", StemcellTemplatePool: "bosh-templates"}
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := obs.All()
	if len(entries) != 2 {
		t.Fatalf("expected one entry per probed pool, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Level != log.LevelDebug {
			t.Errorf("entry %q logged at %v; want Debug", e.Message, e.Level)
		}
		if !strings.Contains(e.Message, "does not exist yet") {
			t.Errorf("entry %q does not say the pool will be created on first use", e.Message)
		}
	}
}

func TestPreflightPoolAccess_DuplicatePoolNames_ProbedOnce(t *testing.T) {
	t.Parallel()
	fake := &fakePreflightPoolService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", true, nil },
	}
	cfg := &config.CPIConfig{VMPool: "shared", StemcellTemplatePool: "shared"}
	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("expected a single probe for the shared pool name, got %v", fake.calls)
	}
}

func TestPreflightPoolAccess_PermissionDenied_FailsFastNamingGrant(t *testing.T) {
	t.Parallel()
	permErr := &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied", Code: 403}
	fake := &fakePreflightPoolService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, permErr },
	}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected a fail-fast error for a permission-denied probe")
	}
	if !strings.Contains(err.Error(), "Pool.Audit") || !strings.Contains(err.Error(), "Pool.Allocate") {
		t.Errorf("expected the error to name both Pool.Audit and Pool.Allocate, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "bosh") {
		t.Errorf("expected the error to name the pool %q, got %q", "bosh", err.Error())
	}
}

func TestPreflightPoolAccess_TransientError_WarnsButDoesNotFail(t *testing.T) {
	t.Parallel()
	transientErr := errors.New("dial tcp: connection refused")
	fake := &fakePreflightPoolService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, transientErr },
	}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, log.NewNopLogger()); err != nil {
		t.Fatalf("transient error must not fail the preflight, got: %v", err)
	}
}

func TestPreflightPoolAccess_StemcellTemplatePoolOnly_Probed(t *testing.T) {
	t.Parallel()
	fake := &fakePreflightPoolService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", true, nil },
	}
	cfg := &config.CPIConfig{StemcellTemplatePool: "bosh-templates"}
	if err := preflightPoolAccess(context.Background(), cfg, preflightTestClient{pools: fake}, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "bosh-templates" {
		t.Errorf("expected a single probe for stemcell_template_pool, got %v", fake.calls)
	}
}

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (f *fakePreflightPoolService) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}
