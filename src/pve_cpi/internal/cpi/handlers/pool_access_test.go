package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// fakePoolAccessService implements pve.PoolService with a scriptable
// GetPoolComment. AddVM/CreatePool/DeletePool panic on any call: the
// preflight probe must never mutate PVE state.
type fakePoolAccessService struct {
	// getPoolCommentFn is called once per probed pool. Nil means "return
	// (found=false, nil)" -- an always-visible-but-nonexistent pool.
	getPoolCommentFn func(poolID string) (string, bool, error)
	calls            []string
}

func (f *fakePoolAccessService) AddVM(context.Context, string, int64) error {
	panic("ensurePoolAccess must never call AddVM")
}

func (f *fakePoolAccessService) MoveVMToPool(context.Context, string, int64) error {
	panic("ensurePoolAccess must never call MoveVMToPool")
}

func (f *fakePoolAccessService) CreatePool(context.Context, string, string) error {
	panic("ensurePoolAccess must never call CreatePool")
}

func (f *fakePoolAccessService) DeletePool(context.Context, string) error {
	panic("ensurePoolAccess must never call DeletePool")
}

func (f *fakePoolAccessService) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	f.calls = append(f.calls, poolID)
	if f.getPoolCommentFn == nil {
		return "", false, nil
	}
	return f.getPoolCommentFn(poolID)
}

var _ pve.PoolService = (*fakePoolAccessService)(nil)

// poolAccessClient overrides only Pools() so ensurePoolAccess sees a
// configured pool service while every other service stays nil (unused by the
// probe).
type poolAccessClient struct {
	pve.Client
	pools pve.PoolService
}

func (c poolAccessClient) Pools() pve.PoolService { return c.pools }

var _ pve.Client = poolAccessClient{}

func TestEnsurePoolAccess_SkipsWhenBothPoolsEmpty(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{}
	cfg := &config.CPIConfig{}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected zero PVE calls when both pools are empty, got %v", fake.calls)
	}
}

// TestEnsurePoolAccess_OnlyParkerPoolSet_NotSkipped is the all-empty
// guard's other half. pve.vm_pool and pve.stemcell_template_pool are both
// empty, so the two-pool guard alone would skip the probe entirely, but the
// parked strategy is active and pve.parker_pool resolves to a name, so the
// preflight must still run one probe rather than returning early.
func TestEnsurePoolAccess_OnlyParkerPoolSet_NotSkipped(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", true, nil },
	}
	cfg := &config.CPIConfig{
		DetachedDiskStrategy: config.DetachedDiskStrategyParked,
		ParkerPool:           "bosh-parker",
	}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "bosh-parker" {
		t.Errorf("expected a single probe for the parker pool, got %v", fake.calls)
	}
}

func TestEnsurePoolAccess_NilCfgClientPoolsAreNoOps(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	logger := log.NewNopLogger()

	if err := ensurePoolAccess(context.Background(), Deps{Config: nil, PVE: poolAccessClient{pools: fake}, Logger: logger}); err != nil {
		t.Errorf("nil cfg: unexpected error: %v", err)
	}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: nil, Logger: logger}); err != nil {
		t.Errorf("nil client: unexpected error: %v", err)
	}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: nil}, Logger: logger}); err != nil {
		t.Errorf("nil Pools(): unexpected error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected zero PVE calls across the nil-guard cases, got %v", fake.calls)
	}
}

// A Deps without a logger still probes: Deps.Log falls back to a nop logger
// rather than skipping the check.
func TestEnsurePoolAccess_NilLoggerStillProbes(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("expected one probe, got %v", fake.calls)
	}
}

func deniedPoolService(t *testing.T) *fakePoolAccessService {
	t.Helper()
	return &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) {
			return "", false, &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied", Code: 403}
		},
	}
}

func assertNonRetriablePoolDenial(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a pool-denied error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
		t.Fatalf("want a non-retriable CloudError, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "Pool.Audit") {
		t.Errorf("error should name the grant to fix, got %q", err.Error())
	}
}

// A denied job credential used to exit the process before the request was
// read. It now fails only the create_vm that needs the pool, as a
// non-retriable CloudError, before any VM mutation.
func TestCreateVM_PoolDenied_FailsRequestNonRetriable(t *testing.T) {
	t.Parallel()
	fake := deniedPoolService(t)
	deps := Deps{
		Config: &config.CPIConfig{VMPool: "bosh"},
		PVE:    poolAccessClient{pools: fake},
		Logger: log.NewNopLogger(),
	}
	args := []json.RawMessage{
		json.RawMessage(`"agent-1"`),
		json.RawMessage(`":heavy:local:import/stemcell.qcow2"`),
		json.RawMessage(`{}`),
		json.RawMessage(`{}`),
		json.RawMessage(`[]`),
		json.RawMessage(`{}`),
	}
	_, err := createVM(context.Background(), deps, args)
	assertNonRetriablePoolDenial(t, err)
	if len(fake.calls) != 1 || fake.calls[0] != "bosh" {
		t.Errorf("expected one probe of the vm pool, got %v", fake.calls)
	}
}

func TestCreateStemcell_PoolDenied_FailsRequestNonRetriable(t *testing.T) {
	t.Parallel()
	fake := deniedPoolService(t)
	deps := Deps{
		Config: &config.CPIConfig{StemcellTemplatePool: "bosh-templates"},
		PVE:    poolAccessClient{pools: fake},
		Logger: log.NewNopLogger(),
	}
	dir := t.TempDir()
	image := filepath.Join(dir, "image.tgz")
	if err := os.WriteFile(image, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := HandleCreateStemcell(deps)
	imgArg, _ := json.Marshal(image)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		imgArg, json.RawMessage(`{"name":"s","version":"1","image_id":"","image_url":"https://example.invalid/s.qcow2","sha256":"` + strings.Repeat("a", 64) + `"}`),
	}, jsonrpc.Context{})
	assertNonRetriablePoolDenial(t, err)
}

func TestEnsurePoolAccess_VisiblePool_NoError(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "managed by bosh-proxmox-cpi", true, nil },
	}
	cfg := &config.CPIConfig{VMPool: "bosh", StemcellTemplatePool: "bosh-templates"}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Errorf("expected 2 probes (vm_pool + stemcell_template_pool), got %v", fake.calls)
	}
}

// TestEnsurePoolAccess_VisiblePoolWithParker_ThreeProbes is
// TestEnsurePoolAccess_VisiblePool_NoError's sibling with the parked
// strategy active and a distinct pve.parker_pool set. All three pools are
// visible, so the preflight probes all three and returns no error.
func TestEnsurePoolAccess_VisiblePoolWithParker_ThreeProbes(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "managed by bosh-proxmox-cpi", true, nil },
	}
	cfg := &config.CPIConfig{
		VMPool:               "bosh",
		StemcellTemplatePool: "bosh-templates",
		DetachedDiskStrategy: config.DetachedDiskStrategyParked,
		ParkerPool:           "bosh-parker",
	}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 3 {
		t.Errorf("expected 3 probes (vm_pool + stemcell_template_pool + parker_pool), got %v", fake.calls)
	}
}

func TestEnsurePoolAccess_NotYetExistingPool_NoError(t *testing.T) {
	t.Parallel()
	// found=false, err=nil -- the pool does not exist yet; the CPI creates it
	// lazily on first use. Must not fail the preflight.
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, nil },
	}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error for a not-yet-existing pool: %v", err)
	}
}

// TestEnsurePoolAccess_NotYetExistingPool_LogsQuietDebug pins the log level
// for the normal zero-config first boot, where neither shipped default pool
// exists yet. That state must be reported at Debug and say lazy creation
// handles it -- an operator seeing a Warn here cannot tell it apart from a real
// API fault, which is exactly what the live run reported.
func TestEnsurePoolAccess_NotYetExistingPool_LogsQuietDebug(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, nil },
	}
	cfg := &config.CPIConfig{
		VMPool:               "bosh",
		StemcellTemplatePool: "bosh-templates",
		DetachedDiskStrategy: config.DetachedDiskStrategyParked,
		ParkerPool:           "bosh-parker",
	}
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: logger}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// One entry per probed pool, including the missing parker pool. It gets
	// its own quiet Debug line rather than being silently skipped or logged
	// at a level that reads as a fault.
	entries := obs.All()
	if len(entries) != 3 {
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

func TestEnsurePoolAccess_DuplicatePoolNames_ProbedOnce(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", true, nil },
	}
	cfg := &config.CPIConfig{VMPool: "shared", StemcellTemplatePool: "shared"}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("expected a single probe for the shared pool name, got %v", fake.calls)
	}
}

// TestEnsurePoolAccess_ParkerPoolDuplicatesVMPool_ProbedOnce extends the
// dedup coverage to the parker pool. An operator who sets pve.parker_pool to
// the same name as pve.vm_pool gets one probe, not two, and that probe keeps
// the fail-fast classification pve.vm_pool carries (the name is already
// represented in the deduped list by the time the parker candidate is
// considered).
func TestEnsurePoolAccess_ParkerPoolDuplicatesVMPool_ProbedOnce(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", true, nil },
	}
	cfg := &config.CPIConfig{
		VMPool:               "shared",
		DetachedDiskStrategy: config.DetachedDiskStrategyParked,
		ParkerPool:           "shared",
	}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("expected a single probe for the shared pool name, got %v", fake.calls)
	}
}

func TestEnsurePoolAccess_PermissionDenied_FailsFastNamingGrant(t *testing.T) {
	t.Parallel()
	permErr := &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied", Code: 403}
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, permErr },
	}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()})
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

// TestEnsurePoolAccess_ParkerPoolDenied_WarnsNotFails covers the
// deliberate exception to the fail-fast rule above. A permission denial on
// the parker pool alone must not fail boot. Parker-pool assignment is a
// best-effort, cosmetic sweep, unlike pve.vm_pool, so treating this the same
// as a workload-pool denial would stop the CPI from booting over something
// that never fails a park.
func TestEnsurePoolAccess_ParkerPoolDenied_WarnsNotFails(t *testing.T) {
	t.Parallel()
	permErr := &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied", Code: 403}
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, permErr },
	}
	cfg := &config.CPIConfig{
		DetachedDiskStrategy: config.DetachedDiskStrategyParked,
		ParkerPool:           "bosh-parker",
	}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("a denied parker pool must warn, not fail boot; got: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "bosh-parker" {
		t.Errorf("expected a single probe for the parker pool, got %v", fake.calls)
	}
}

// TestEnsurePoolAccess_ParkerPoolDenied_LogsWarn pins the log level and
// content for a denied parker pool, the same way
// TestEnsurePoolAccess_NotYetExistingPool_LogsQuietDebug pins Debug for a
// not-yet-existing pool. An operator must be able to tell this apart from
// the fatal pve.vm_pool/pve.stemcell_template_pool denial, which returns an
// error instead of logging.
func TestEnsurePoolAccess_ParkerPoolDenied_LogsWarn(t *testing.T) {
	t.Parallel()
	permErr := &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied", Code: 403}
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, permErr },
	}
	cfg := &config.CPIConfig{
		DetachedDiskStrategy: config.DetachedDiskStrategyParked,
		ParkerPool:           "bosh-parker",
	}
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: logger}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries := obs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one log entry, got %d: %+v", len(entries), entries)
	}
	if entries[0].Level != log.LevelWarn {
		t.Errorf("entry logged at %v, want Warn", entries[0].Level)
	}
	if !strings.Contains(entries[0].Message, "parker") {
		t.Errorf("entry %q does not identify the parker pool", entries[0].Message)
	}
}

// TestEnsurePoolAccess_DetachedDiskStrategyFree_ParkerPoolNotProbed
// covers the strategy gate on the parker-pool probe. When
// pve.detached_disk_strategy is "free", no parker VM is ever created, so
// pve.parker_pool, even though it resolves to a non-empty name, must not be
// probed. A boot-time failure for a pool the CPI will never touch would
// contradict the whole "free" opt-out.
func TestEnsurePoolAccess_DetachedDiskStrategyFree_ParkerPoolNotProbed(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{}
	cfg := &config.CPIConfig{
		DetachedDiskStrategy: config.DetachedDiskStrategyFree,
		ParkerPool:           "bosh-parker",
	}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected zero PVE calls when the parked strategy is off, got %v", fake.calls)
	}
}

// TestEnsurePoolAccess_ParkerPoolOptedOut_NotProbed covers the operator
// who is running the parked strategy but has explicitly opted the parker
// pool out (pve.parker_pool: ""), the documented ParkerPoolValue() opt-out.
// vm_pool and stemcell_template_pool are also empty here, so this doubles as
// the "an operator who emptied every pool property gets no new probe"
// regression the parked-strategy gate exists to protect.
func TestEnsurePoolAccess_ParkerPoolOptedOut_NotProbed(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{}
	cfg := &config.CPIConfig{
		DetachedDiskStrategy: config.DetachedDiskStrategyParked,
		ParkerPool:           "",
	}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected zero PVE calls when the parker pool is opted out, got %v", fake.calls)
	}
}

func TestEnsurePoolAccess_TransientError_WarnsButDoesNotFail(t *testing.T) {
	t.Parallel()
	transientErr := errors.New("dial tcp: connection refused")
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", false, transientErr },
	}
	cfg := &config.CPIConfig{VMPool: "bosh"}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("transient error must not fail the preflight, got: %v", err)
	}
}

func TestEnsurePoolAccess_StemcellTemplatePoolOnly_Probed(t *testing.T) {
	t.Parallel()
	fake := &fakePoolAccessService{
		getPoolCommentFn: func(string) (string, bool, error) { return "", true, nil },
	}
	cfg := &config.CPIConfig{StemcellTemplatePool: "bosh-templates"}
	if err := ensurePoolAccess(context.Background(), Deps{Config: cfg, PVE: poolAccessClient{pools: fake}, Logger: log.NewNopLogger()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "bosh-templates" {
		t.Errorf("expected a single probe for stemcell_template_pool, got %v", fake.calls)
	}
}

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (f *fakePoolAccessService) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

// A pre-uploaded light stemcell only records an existing image id and never
// touches a pool, so a denied pool grant must not fail it and no probe runs.
func TestCreateStemcell_PreuploadedLight_SkipsPoolProbe(t *testing.T) {
	t.Parallel()
	fake := deniedPoolService(t)
	deps := Deps{
		Config: &config.CPIConfig{StemcellTemplatePool: "bosh-templates", VMPool: "bosh"},
		PVE:    poolAccessClient{pools: fake},
		Logger: log.NewNopLogger(),
	}
	imgArg, _ := json.Marshal("/dev/null")
	cloudProps := `{"name":"s","version":"1","image_id":"local:import/s.qcow2","sha256":"` + strings.Repeat("a", 64) + `"}`
	// The light handler may fail later on this bare fake, so only the absence
	// of a probe and of a pool-denied error is asserted.
	func() {
		defer func() { _ = recover() }()
		_, err := HandleCreateStemcell(deps).Handle(context.Background(),
			[]json.RawMessage{imgArg, json.RawMessage(cloudProps)}, jsonrpc.Context{})
		if err != nil && strings.Contains(err.Error(), "Pool.Audit") {
			t.Errorf("pre-uploaded light stemcell was failed by the pool check: %v", err)
		}
	}()
	if len(fake.calls) != 0 {
		t.Errorf("expected no pool probe for a pre-uploaded light stemcell, got %v", fake.calls)
	}
}
