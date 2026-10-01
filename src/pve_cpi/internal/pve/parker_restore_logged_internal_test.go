package pve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// loggedRestoreTimeout is the protection restore deadline these tests set
// through WithParkerProtectionRestoreTimeoutForTest.
const loggedRestoreTimeout = 100 * time.Millisecond

// loggedRestoreCeiling is how long a window whose restore hangs may take to
// return. It leaves the 100-millisecond restore deadline room on a loaded
// machine. A restore with no deadline of its own never returns against a PVE
// that never answers, and the ceiling turns that hang into a failure instead
// of a stuck test.
const loggedRestoreCeiling = 5 * time.Second

// callWithCeiling runs call and returns its result and how long it took, or
// fails the test when call has not returned within loggedRestoreCeiling.
func callWithCeiling[T any](t *testing.T, call func() T) (T, time.Duration) {
	t.Helper()
	done := make(chan T, 1)
	start := time.Now()
	go func() { done <- call() }()
	select {
	case got := <-done:
		return got, time.Since(start)
	case <-time.After(loggedRestoreCeiling):
		t.Fatalf("the call had not returned after %s; its protection restore has no deadline", loggedRestoreCeiling)
		var zero T
		return zero, 0
	}
}

// restoreLogLines returns the log lines about a protection restore.
func restoreLogLines(logged string) []string {
	var lines []string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "protection restore on parker") || strings.Contains(line, "could not restore protection") {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertLoggedRestoreCutOff checks what a restore whose contract is to log
// rather than return must produce when PVE never answers it: the restore's
// own deadline ended the write, the window returned soon after that deadline,
// and exactly one warning, prefixed with op, says the restore's outcome is
// unknown without carrying the credential from the underlying error.
func assertLoggedRestoreCutOff(t *testing.T, restores int, endedBy error, elapsed time.Duration, logged, op string) {
	t.Helper()
	if restores == 0 {
		t.Fatal("the protection restore was never attempted")
	}
	if !errors.Is(endedBy, context.DeadlineExceeded) {
		t.Fatalf("the hung restore ended with %v, want the restore deadline", endedBy)
	}
	if elapsed < loggedRestoreTimeout {
		t.Fatalf("returned after %s, before the %s restore deadline", elapsed, loggedRestoreTimeout)
	}
	if elapsed > loggedRestoreTimeout+2*time.Second {
		t.Fatalf("returned after %s; the restore deadline of %s did not bound it", elapsed, loggedRestoreTimeout)
	}
	lines := restoreLogLines(logged)
	if len(lines) != 1 {
		t.Fatalf("logged %d restore warnings, want one: %s", len(lines), logged)
	}
	for _, want := range []string{op + ": protection restore on parker did not finish within", "outcome is unknown", `"level":"WARN"`} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("restore warning %s does not contain %q", lines[0], want)
		}
	}
	if strings.Contains(logged, hangingRestoreSecret) {
		t.Fatalf("log output carries the credential: %s", logged)
	}
}

// TestUnparkDiskAt_HungRestoreHitsItsDeadline unparks a disk from a parker
// whose protection restore never answers. The restore ends at its own
// deadline, the unpark still returns the detach's result, and the log says
// the restore's outcome is unknown.
func TestUnparkDiskAt_HungRestoreHitsItsDeadline(t *testing.T) {
	t.Parallel()
	ctx := WithParkerProtectionRestoreTimeoutForTest(context.Background(), loggedRestoreTimeout)
	c := &hangingRestoreClient{parker: 90000, scanFakeClient: newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"scsi1":         "data:vm-9001-disk-0",
		},
	})}
	logger, logged := newRestoreTestLogger(t)
	holder := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi1"}
	err, elapsed := callWithCeiling(t, func() error {
		return UnparkDiskAt(ctx, c, logger, "data:vm-9001-disk-0", holder, transferTestCfg)
	})
	if err != nil {
		t.Fatalf("UnparkDiskAt = %v, want the detach's own result, which succeeded", err)
	}
	restores, endedBy := c.outcome()
	assertLoggedRestoreCutOff(t, restores, endedBy, elapsed, logged.String(), "UnparkDisk")
	c.scanFakeClient.mu.Lock()
	defer c.scanFakeClient.mu.Unlock()
	if _, present := c.configs[90000]["scsi1"]; present {
		t.Fatal("the disk is still on the parker's slot after the unpark")
	}
}

// TestSweepDemotedUnderProtection_HungRestoreHitsItsDeadline unparks a disk
// that an earlier attempt left on an unusedN key, so the unpark opens the
// window for the sweep alone, and the restore after that sweep never answers.
func TestSweepDemotedUnderProtection_HungRestoreHitsItsDeadline(t *testing.T) {
	t.Parallel()
	ctx := WithParkerProtectionRestoreTimeoutForTest(context.Background(), loggedRestoreTimeout)
	c := &hangingRestoreClient{parker: 90000, scanFakeClient: newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"unused0":       "data:vm-9001-disk-0",
		},
	})}
	logger, logged := newRestoreTestLogger(t)
	holder := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi1"}
	err, elapsed := callWithCeiling(t, func() error {
		return UnparkDiskAt(ctx, c, logger, "data:vm-9001-disk-0", holder, transferTestCfg)
	})
	if err != nil {
		t.Fatalf("UnparkDiskAt = %v, want the sweep's own result, which succeeded", err)
	}
	restores, endedBy := c.outcome()
	assertLoggedRestoreCutOff(t, restores, endedBy, elapsed, logged.String(), "UnparkDisk")
	c.scanFakeClient.mu.Lock()
	defer c.scanFakeClient.mu.Unlock()
	if _, present := c.configs[90000]["unused0"]; present {
		t.Fatal("the demoted reference is still on the parker after the sweep")
	}
}

// TestSweepParkerUnusedSlotsProtectedLocked_HungRestoreHitsItsDeadline runs
// the park path's deferred sweep on a context with a deadline of its own, as
// the park path does, against a parker whose restore never answers. The
// restore gets its own deadline rather than none, and the sweep still
// reports that the reference is gone.
func TestSweepParkerUnusedSlotsProtectedLocked_HungRestoreHitsItsDeadline(t *testing.T) {
	t.Parallel()
	ctx := WithParkerProtectionRestoreTimeoutForTest(context.Background(), loggedRestoreTimeout)
	sweepCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c := &hangingRestoreClient{parker: 90000, scanFakeClient: newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"unused0":       "data:vm-9001-disk-0",
		},
	})}
	logger, logged := newRestoreTestLogger(t)
	swept, elapsed := callWithCeiling(t, func() bool {
		return sweepParkerUnusedSlotsProtectedLocked(sweepCtx, c, logger, "pve1", 90000, "data:vm-9001-disk-0")
	})
	if !swept {
		t.Fatal("the sweep reported that the reference may still stand, but it removed it")
	}
	restores, endedBy := c.outcome()
	assertLoggedRestoreCutOff(t, restores, endedBy, elapsed, logged.String(), "parker")
}

// hungRestoreMoverClient is an rpClient whose protection restore on the mover
// never answers: the write blocks until its context ends and then fails with
// an error that embeds a credential, as a real request URL can.
type hungRestoreMoverClient struct {
	*rpClient
	mover int

	restoreMu sync.Mutex
	restores  int
	endedBy   error
}

func (c *hungRestoreMoverClient) Nodes() sdknodes.Service {
	return &hungRestoreMoverNodes{rpNodes: &rpNodes{c: c.rpClient}, owner: c}
}

func (c *hungRestoreMoverClient) outcome() (int, error) {
	c.restoreMu.Lock()
	defer c.restoreMu.Unlock()
	return c.restores, c.endedBy
}

type hungRestoreMoverNodes struct {
	*rpNodes
	owner *hungRestoreMoverClient
}

func (n *hungRestoreMoverNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if params.Protection == nil || !*params.Protection || vmid != strconv.Itoa(n.owner.mover) {
		return n.rpNodes.UpdateQemuConfig(ctx, node, vmid, params)
	}
	<-ctx.Done()
	n.owner.restoreMu.Lock()
	n.owner.restores++
	n.owner.endedBy = ctx.Err()
	n.owner.restoreMu.Unlock()
	return fmt.Errorf("Put \"https://root@pam!cpi:%s@pve1:8006/api2/json/nodes/pve1/qemu/%s/config\": %w",
		hangingRestoreSecret, vmid, ctx.Err())
}

// TestMigrateDiskViaMover_HungRestoreAfterAFailedMigrateHitsItsDeadline
// fails a mover's migration, once when PVE refuses the migrate request and
// once when the migrate task itself fails, and the restore that puts the
// mover's protection back never answers. The restore ends at its own
// deadline, the migration's error comes back, and the log says the restore's
// outcome is unknown.
func TestMigrateDiskViaMover_HungRestoreAfterAFailedMigrateHitsItsDeadline(t *testing.T) {
	const moverVMID = 90007
	cases := []struct {
		name      string
		migrate   func(call int) (*sdknodes.CreateQemuMigrateResponse, error)
		wait      func(call int, upid string) (*sdktasks.Status, error)
		wantError string
	}{
		{
			name: "migrate request refused",
			migrate: func(int) (*sdknodes.CreateQemuMigrateResponse, error) {
				return nil, &sdkerrors.APIError{HTTPCode: 500, Code: 500, Message: "migration aborted"}
			},
			wantError: "disk migrate: migrate mover vmid 90007 from node pve1 to node pve2",
		},
		{
			name: "migrate task failed",
			migrate: func(int) (*sdknodes.CreateQemuMigrateResponse, error) {
				resp := sdknodes.CreateQemuMigrateResponse(`"UPID:pve1:0000AB12:00512345:65D0AA11:qmigrate:90007:root@pam:"`)
				return &resp, nil
			},
			wait: func(int, string) (*sdktasks.Status, error) {
				return nil, errors.New("task failed: migration aborted (node maintenance)")
			},
			wantError: "disk migrate: migrate task for mover vmid 90007 to node pve2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := WithParkerProtectionRestoreTimeoutForTest(crCtx(), loggedRestoreTimeout)
			rp := newRPClient()
			rp.configs[moverVMID] = map[string]any{
				cfgKeyTags:      "bosh-cpi;bosh-parker;bosh-disk-mover",
				paramProtection: true,
				"scsi0":         "data:vm-90007-disk-0,serial=" + dmToken,
			}
			rp.migrateFn = tc.migrate
			rp.waitFn = tc.wait
			c := &hungRestoreMoverClient{rpClient: rp, mover: moverVMID}
			logger, logged := newRestoreTestLogger(t)
			type result struct{ err error }
			got, elapsed := callWithCeiling(t, func() result {
				_, _, err := MigrateDiskViaMover(ctx, c, logger, DiskMigrationSpec{
					Holder: dmMoverHolder(moverVMID), TargetNode: "pve2",
					Volid: "data:vm-90007-disk-0", StableID: dmToken, AwaitBudget: time.Second,
				}, dmBand())
				return result{err}
			})
			if got.err == nil || !strings.Contains(got.err.Error(), tc.wantError) {
				t.Fatalf("MigrateDiskViaMover = %v, want the migration's own error %q", got.err, tc.wantError)
			}
			restores, endedBy := c.outcome()
			assertLoggedRestoreCutOff(t, restores, endedBy, elapsed, logged.String(), "disk migrate")
		})
	}
}
