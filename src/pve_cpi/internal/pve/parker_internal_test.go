// parker_internal_test.go — white-box tests for unexported parker helpers.
// Package pve gives access to chooseParkSlotExcluding and other unexported
// symbols.
package pve

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	clusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// chooseParkSlotExcluding (nil exclude set)
// ---------------------------------------------------------------------------

func TestChooseParkSlot_EmptyParker(t *testing.T) {
	t.Parallel()
	slot, err := chooseParkSlotExcluding(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi0" {
		t.Errorf("want scsi0, got %q", slot)
	}
}

func TestChooseParkSlot_EmptyMap(t *testing.T) {
	t.Parallel()
	slot, err := chooseParkSlotExcluding(map[string]string{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi0" {
		t.Errorf("want scsi0, got %q", slot)
	}
}

func TestChooseParkSlot_Scsi0Taken(t *testing.T) {
	t.Parallel()
	disks := map[string]string{
		"scsi0": "local-lvm:vm-9000-disk-0",
	}
	slot, err := chooseParkSlotExcluding(disks, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi1" {
		t.Errorf("want scsi1, got %q", slot)
	}
}

func TestChooseParkSlot_HolesInMiddle(t *testing.T) {
	t.Parallel()
	// scsi0 and scsi2 taken; scsi1 is the hole → want scsi1.
	disks := map[string]string{
		"scsi0": "local-lvm:vm-9000-disk-0",
		"scsi2": "local-lvm:vm-9000-disk-1",
	}
	slot, err := chooseParkSlotExcluding(disks, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi1" {
		t.Errorf("want scsi1 (first hole), got %q", slot)
	}
}

func TestChooseParkSlot_AllSlotsOccupied_ErrNoSlots(t *testing.T) {
	t.Parallel()
	disks := make(map[string]string, parkerMaxSlots)
	for i := 0; i < parkerMaxSlots; i++ {
		disks[fmt.Sprintf("scsi%d", i)] = fmt.Sprintf("local-lvm:vm-9000-disk-%d", i)
	}
	_, err := chooseParkSlotExcluding(disks, nil)
	if err == nil {
		t.Fatal("expected ErrNoSlots for fully occupied parker")
	}
	if !errors.Is(err, ErrNoSlots) {
		t.Errorf("expected errors.Is(err, ErrNoSlots); got: %v", err)
	}
}

func TestChooseParkSlot_Last30Taken_Scsi30Free(t *testing.T) {
	t.Parallel()
	// scsi0..scsi29 taken; scsi30 is the last free slot.
	disks := make(map[string]string, 30)
	for i := 0; i < 30; i++ {
		disks[fmt.Sprintf("scsi%d", i)] = fmt.Sprintf("local-lvm:vm-9000-disk-%d", i)
	}
	slot, err := chooseParkSlotExcluding(disks, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi30" {
		t.Errorf("want scsi30, got %q", slot)
	}
}

// ---------------------------------------------------------------------------
// tagContainsParker
// ---------------------------------------------------------------------------

func TestTagContainsParker_Single(t *testing.T) {
	t.Parallel()
	if !tagContainsParker("bosh-parker") {
		t.Error("expected true for single-tag string")
	}
}

func TestTagContainsParker_MultipleWithParker(t *testing.T) {
	t.Parallel()
	if !tagContainsParker("bosh-stemcell;bosh-parker;director--prod") {
		t.Error("expected true when bosh-parker is present among other tags")
	}
}

func TestTagContainsParker_Absent(t *testing.T) {
	t.Parallel()
	if tagContainsParker("bosh-stemcell;director--prod") {
		t.Error("expected false when bosh-parker is absent")
	}
}

func TestTagContainsParker_Empty(t *testing.T) {
	t.Parallel()
	if tagContainsParker("") {
		t.Error("expected false for empty tag string")
	}
}

func TestTagContainsParker_CaseInsensitive(t *testing.T) {
	t.Parallel()
	if !tagContainsParker("Bosh-Parker") {
		t.Error("expected true: tag comparison is case-insensitive")
	}
}

// ---------------------------------------------------------------------------
// sanitizeParkerTagValue
// ---------------------------------------------------------------------------

func TestSanitizeParkerTagValue_AlphanumericUnchanged(t *testing.T) {
	t.Parallel()
	got := sanitizeParkerTagValue("prod-director123")
	if got != "prod-director123" {
		t.Errorf("want %q, got %q", "prod-director123", got)
	}
}

func TestSanitizeParkerTagValue_StripsSpaces(t *testing.T) {
	t.Parallel()
	got := sanitizeParkerTagValue("hello world")
	if got != "helloworld" {
		t.Errorf("want %q, got %q", "helloworld", got)
	}
}

func TestSanitizeParkerTagValue_EmptyInput(t *testing.T) {
	t.Parallel()
	got := sanitizeParkerTagValue("")
	if got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// parkerVMName
// ---------------------------------------------------------------------------

func TestParkerVMName(t *testing.T) {
	t.Parallel()
	got := parkerVMName(90000)
	if got != "bosh-parker-90000" {
		t.Errorf("want %q, got %q", "bosh-parker-90000", got)
	}
}

// ---------------------------------------------------------------------------
// buildParkerTags
// ---------------------------------------------------------------------------

func TestBuildParkerTags_NoDirectorID(t *testing.T) {
	t.Parallel()
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999}
	got := buildParkerTags(cfg)
	// CpiOwnershipTag is always first; ParkerTag follows.
	want := CpiOwnershipTag + ";bosh-parker"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildParkerTags_WithDirectorID(t *testing.T) {
	t.Parallel()
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, DirectorID: "my-director"}
	got := buildParkerTags(cfg)
	want := CpiOwnershipTag + ";bosh-parker;director--my-director"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildParkerTags_DirectorIDWithSpecialChars(t *testing.T) {
	t.Parallel()
	// Special chars stripped by sanitizeParkerTagValue.
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, DirectorID: "my director!"}
	got := buildParkerTags(cfg)
	want := CpiOwnershipTag + ";bosh-parker;director--mydirector"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildParkerTags_DirectorIDBecomesEmpty(t *testing.T) {
	t.Parallel()
	// DirectorID of only special chars sanitizes to ""; director tag omitted.
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, DirectorID: "!!!"}
	got := buildParkerTags(cfg)
	// CpiOwnershipTag still present; director tag omitted.
	want := CpiOwnershipTag + ";bosh-parker"
	if got != want {
		t.Errorf("want %q (director tag omitted), got %q", want, got)
	}
}

// TestParkerBelongsToDirector covers which parkers a director may adopt. The
// answer decides whether two directors sharing one PVE cluster end up filling
// each other's parker VMs.
func TestParkerBelongsToDirector(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		tags       string
		directorID string
		want       bool
	}{
		{"no director id adopts anything", "bosh-cpi;bosh-parker;director--abc", "", true},
		{"own parker", "bosh-cpi;bosh-parker;director--abc", "abc", true},
		{"another director's parker", "bosh-cpi;bosh-parker;director--abc", "xyz", false},
		{"unattributed parker is adoptable", "bosh-cpi;bosh-parker", "xyz", true},
		{"case-insensitive match", "bosh-cpi;bosh-parker;DIRECTOR--ABC", "abc", true},
		{"whitespace tolerated", "bosh-cpi; bosh-parker; director--abc", "abc", true},
		{"sanitized id matches sanitized tag", "bosh-cpi;bosh-parker;director--a-b-c", "a-b-c", true},
		{"id that sanitizes to nothing adopts anything", "bosh-cpi;bosh-parker;director--abc", "///", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parkerBelongsToDirector(tc.tags, tc.directorID); got != tc.want {
				t.Errorf("parkerBelongsToDirector(%q, %q) = %v, want %v", tc.tags, tc.directorID, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parker protection-window lock
// ---------------------------------------------------------------------------

// parkerLockClient is a pve.Client that carries only a pool service; the
// protection-window lock touches nothing else.
type parkerLockClient struct {
	pools PoolService
}

func (p *parkerLockClient) QEMU() qemu.Service                     { return nil }
func (p *parkerLockClient) Storage() storage.Service               { return nil }
func (p *parkerLockClient) CloudInit() cloudinit.Service           { return nil }
func (p *parkerLockClient) Tasks() tasks.Service                   { return nil }
func (p *parkerLockClient) Nodes() nodes.Service                   { return nil }
func (p *parkerLockClient) Cluster() sdkcluster.Service            { return nil }
func (p *parkerLockClient) ClusterStorage() clusterstorage.Service { return nil }
func (p *parkerLockClient) Pools() PoolService                     { return p.pools }

// recordingPoolService records the sentinel-pool calls the lock makes.
type recordingPoolService struct {
	events    []string
	createErr error
}

func (r *recordingPoolService) AddVM(context.Context, string, int64) error { return nil }
func (r *recordingPoolService) CreatePool(_ context.Context, poolID, _ string) error {
	r.events = append(r.events, "create:"+poolID)
	return r.createErr
}

func (r *recordingPoolService) DeletePool(_ context.Context, poolID string) error {
	r.events = append(r.events, "delete:"+poolID)
	return nil
}

func (r *recordingPoolService) GetPoolComment(context.Context, string) (string, bool, error) {
	return "", false, nil
}

// TestWithParkerProtectionLock_SerializesOnVMIDPool proves the window runs
// between the sentinel pool's creation and its release, under the same
// "vm-<vmid>" key the handlers use for per-VMID serialization.
func TestWithParkerProtectionLock_SerializesOnVMIDPool(t *testing.T) {
	t.Parallel()

	pools := &recordingPoolService{}
	c := &parkerLockClient{pools: pools}
	ran := false
	err := withParkerProtectionLock(context.Background(), c, nil, 90000, "unpark", func(context.Context) error {
		ran = true
		pools.events = append(pools.events, "window")
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ran {
		t.Fatal("the window body must run")
	}
	want := []string{"create:bosh-lock-vm-90000", "window", "delete:bosh-lock-vm-90000"}
	if len(pools.events) != len(want) {
		t.Fatalf("events = %v, want %v", pools.events, want)
	}
	for i := range want {
		if pools.events[i] != want[i] {
			t.Fatalf("events = %v, want %v", pools.events, want)
		}
	}
}

// TestWithParkerProtectionLock_NoPoolService_RunsUnserialized covers the
// advisory-not-mandatory contract: a CPI that cannot create sentinel pools
// still unparks disks.
func TestWithParkerProtectionLock_NoPoolService_RunsUnserialized(t *testing.T) {
	t.Parallel()

	ran := false
	err := withParkerProtectionLock(context.Background(), &parkerLockClient{}, nil, 90000, "unpark", func(context.Context) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("a missing pool service must not fail the window: %v", err)
	}
	if !ran {
		t.Fatal("the window body must run without a pool service")
	}
}

// TestWithParkerProtectionLock_PropagatesBodyError confirms the body's error is
// what the caller sees, not the lock's bookkeeping.
func TestWithParkerProtectionLock_PropagatesBodyError(t *testing.T) {
	t.Parallel()

	pools := &recordingPoolService{}
	want := errors.New("detach refused")
	got := withParkerProtectionLock(context.Background(), &parkerLockClient{pools: pools}, nil, 90001, "unpark", func(context.Context) error {
		return want
	})
	if !errors.Is(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(pools.events) == 0 || pools.events[len(pools.events)-1] != "delete:bosh-lock-vm-90001" {
		t.Errorf("the lock must be released after a failing window; events=%v", pools.events)
	}
}

// TestTagContainsParker_SeparatorsAndSpacing pins the tokenizer every parker
// guard depends on. PVE accepts both ";" and "," in a stored tag string, and a
// separator this function does not split on is a guard that silently does not
// fire -- for delete_vm that means a skiplock+purge over a VM holding up to 31
// other deployments' disks.
func TestTagContainsParker_SeparatorsAndSpacing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tags string
		want bool
	}{
		{"bosh-parker", true},
		{"bosh-cpi;bosh-parker", true},
		{"bosh-cpi,bosh-parker", true},
		{"bosh-cpi, bosh-parker ,director--abc", true},
		{"bosh-cpi;bosh-parker,director--abc", true},
		{"BOSH-PARKER", true},
		{"", false},
		{"bosh-cpi", false},
		{"bosh-parker-ish", false},
		{"notbosh-parker", false},
		{";;,,", false},
	} {
		if got := tagContainsParker(tc.tags); got != tc.want {
			t.Errorf("tagContainsParker(%q) = %v, want %v", tc.tags, got, tc.want)
		}
	}
}

// TestParkerBelongsToDirector_CommaSeparated confirms attribution uses the same
// tokenizer, so a comma-separated tag string cannot make one director adopt
// another's parker.
func TestParkerBelongsToDirector_CommaSeparated(t *testing.T) {
	t.Parallel()
	if parkerBelongsToDirector("bosh-cpi,bosh-parker,director--aaa", "bbb") {
		t.Error("a comma-separated attribution tag must still be honored")
	}
	if !parkerBelongsToDirector("bosh-cpi,bosh-parker,director--aaa", "aaa") {
		t.Error("the owning director must still adopt its own parker")
	}
}

// TestWithParkerProtectionLock_ParkWaitsOutUnpark is the mutual-exclusion test
// the whole protection window rests on. A park that writes protection=1 while
// an unpark is mid-detach makes PVE refuse that detach and the sweep behind it,
// which strands an unusedN reference no holder probe can see. So a second
// window on the same parker must not run while the first holds the lock.
func TestWithParkerProtectionLock_ParkWaitsOutUnpark(t *testing.T) {
	t.Parallel()

	pools := newFakeLockPools()
	c := &parkerLockClient{pools: pools}
	// A TTL long enough that the holder is never treated as crashed, and an
	// acquire timeout short enough to spend in a unit test.
	ctx := withTestParkerLockTimeouts(context.Background(), time.Minute, 50*time.Millisecond)

	parkRan := false
	var parkErr error
	unparkErr := withParkerProtectionLock(ctx, c, nil, 90000, "unpark", func(context.Context) error {
		// Inside the unpark's window: a park on the same parker, as a second CPI
		// process would run it.
		parkErr = withParkerProtectionLock(ctx, c, nil, 90000, "park", func(context.Context) error {
			parkRan = true
			return nil
		})
		return nil
	})
	if unparkErr != nil {
		t.Fatalf("the holder's own window must succeed: %v", unparkErr)
	}
	if parkRan {
		t.Fatal("a park ran inside an unpark's protection window on the same parker")
	}
	if parkErr == nil {
		t.Fatal("the blocked park must report a failure rather than proceed unserialized")
	}
	if !errors.Is(parkErr, ErrClusterLockTimeout) {
		t.Errorf("want ErrClusterLockTimeout, got %v", parkErr)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(parkErr, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", parkErr)
	}
	if !cpiErr.OkToRetry() {
		t.Error("contention is transient: the Director has to be able to re-drive it")
	}
}

// TestWithParkerProtectionLock_DifferentParkersDoNotContend confirms the key is
// per-parker. Two parkers on one node are independent, and serializing them
// against each other would turn every concurrent detach on a busy node into a
// queue behind one lock.
func TestWithParkerProtectionLock_DifferentParkersDoNotContend(t *testing.T) {
	t.Parallel()

	pools := newFakeLockPools()
	c := &parkerLockClient{pools: pools}
	ctx := withTestParkerLockTimeouts(context.Background(), time.Minute, 50*time.Millisecond)

	innerRan := false
	err := withParkerProtectionLock(ctx, c, nil, 90000, "unpark", func(context.Context) error {
		return withParkerProtectionLock(ctx, c, nil, 90001, "park", func(context.Context) error {
			innerRan = true
			return nil
		})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !innerRan {
		t.Fatal("a window on a different parker must not wait on this one")
	}
}

// TestWithParkerProtectionLock_StealsExpiredHolder covers the crash case. A CPI
// process killed mid-window leaves its sentinel pool behind, and nothing
// deletes it; without the TTL steal the next park or unpark on that parker
// would wait out its timeout and then run unserialized anyway, every time,
// forever.
func TestWithParkerProtectionLock_StealsExpiredHolder(t *testing.T) {
	t.Parallel()

	pools := newFakeLockPools()
	// A holder that died: its recorded expiry is already in the past.
	pools.pools[ClusterLockPoolName("vm-90000")] =
		encodeLockComment("unpark/999/90000", time.Now().Add(-time.Minute))

	ran := false
	ctx := withTestParkerLockTimeouts(context.Background(), time.Minute, 50*time.Millisecond)
	err := withParkerProtectionLock(ctx, &parkerLockClient{pools: pools}, nil, 90000, "park", func(context.Context) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("an expired holder must be stolen, not waited on: %v", err)
	}
	if !ran {
		t.Fatal("the window body must run after the steal")
	}
	if _, held := pools.pools[ClusterLockPoolName("vm-90000")]; held {
		t.Error("the stolen lock must be released when the window ends")
	}
}

// TestParkerWindowBudget pins the arithmetic that keeps a protection window
// inside its lock's TTL: the budget is the TTL minus the reserve for the three
// detached-context tails (sweep, protection restore, sentinel release), floored
// at one second so a test-sized TTL still runs its body.
func TestParkerWindowBudget(t *testing.T) {
	t.Parallel()
	reserve := parkerLockReleaseTimeout + parkerDemotedSweepTimeout + parkerProtectionRestoreReserve
	if got, want := parkerWindowBudget(parkerProtectionLockTTL), parkerProtectionLockTTL-reserve; got != want {
		t.Errorf("budget(TTL) = %v, want %v", got, want)
	}
	if got := parkerWindowBudget(parkerProtectionLockTTL); got <= 0 {
		t.Errorf("production TTL must leave a positive budget; got %v", got)
	}
	// A TTL at or below the reserve floors at one second rather than going
	// zero or negative, which would expire the window before its first call.
	if got := parkerWindowBudget(reserve); got != time.Second {
		t.Errorf("budget(reserve) = %v, want 1s floor", got)
	}
	if got := parkerWindowBudget(time.Millisecond); got != time.Second {
		t.Errorf("budget(1ms) = %v, want 1s floor", got)
	}
}

