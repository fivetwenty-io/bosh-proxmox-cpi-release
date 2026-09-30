package pve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// hangingRestoreClient is a scanFakeClient whose protection restore on the
// parker never answers: the write blocks until its context ends and then
// fails with an error that embeds a credential, the way a real request URL
// can. By default the error wraps the context's error, which the retry loop
// treats as transient, so the loop gives up on the expired context and
// returns that bare error. With permanentCause the error is plain text the
// loop does not retry, so the credential-bearing text is what comes back.
type hangingRestoreClient struct {
	*scanFakeClient
	parker         int
	permanentCause bool
	// refuse, when set, is PVE's answer to every protection restore on the
	// parker, returned at once instead of hanging.
	refuse error
	// dropped, when set, fails every protection restore on the parker in
	// transport, with no answer from PVE, instead of hanging.
	dropped bool

	mu       sync.Mutex
	restores int
	endedBy  error
}

const hangingRestoreSecret = "s3cr3t-token-value"

func (c *hangingRestoreClient) Nodes() sdknodes.Service {
	inner, ok := c.scanFakeClient.Nodes().(*fakeNodesService)
	if !ok {
		panic("scanFakeClient.Nodes is not a *fakeNodesService")
	}
	passThrough := inner.updateQemuConfigFn
	inner.updateQemuConfigFn = func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
		if params.Protection != nil && *params.Protection && vmid == strconv.Itoa(c.parker) {
			if c.refuse != nil {
				return c.refuse
			}
			if c.dropped {
				c.mu.Lock()
				c.restores++
				c.mu.Unlock()
				return &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "connection reset by peer"}
			}
			<-ctx.Done()
			c.mu.Lock()
			c.restores++
			c.endedBy = ctx.Err()
			c.mu.Unlock()
			if c.permanentCause {
				return fmt.Errorf("Put \"https://root@pam!cpi:%s@pve1:8006/api2/json/nodes/pve1/qemu/%s/config\": %s",
					hangingRestoreSecret, vmid, ctx.Err().Error())
			}
			return fmt.Errorf("Put \"https://root@pam!cpi:%s@pve1:8006/api2/json/nodes/pve1/qemu/%s/config\": %w",
				hangingRestoreSecret, vmid, ctx.Err())
		}
		return passThrough(ctx, node, vmid, params)
	}
	return inner
}

func (c *hangingRestoreClient) outcome() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restores, c.endedBy
}

// assertRestoreCutOff checks what every hung restore must produce: the
// deadline ended the write, the error names the protection restore, says how
// the window's own change ended (work), and gives the check to run, the error
// is retriable, and neither the log line nor the scrubbed error text carries
// the credential from the underlying error.
func assertRestoreCutOff(t *testing.T, c *hangingRestoreClient, err error, elapsed, timeout time.Duration, logged, work string) {
	t.Helper()
	restores, endedBy := c.outcome()
	if restores == 0 {
		t.Fatal("the protection restore was never attempted")
	}
	if !errors.Is(endedBy, context.DeadlineExceeded) {
		t.Fatalf("the hung restore ended with %v, want the restore deadline", endedBy)
	}
	if elapsed < timeout {
		t.Fatalf("returned after %s, before the %s restore deadline", elapsed, timeout)
	}
	if elapsed > timeout+10*time.Second {
		t.Fatalf("returned after %s; the restore deadline of %s did not bound it", elapsed, timeout)
	}
	if err == nil {
		t.Fatal("a restore cut off by its deadline returned no error")
	}
	msg := err.Error()
	for _, want := range []string{"protection restore on parker vmid 90000", "outcome is unknown", work, "qm config 90000", "qm set 90000 --protection 1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
	t.Logf("error: %s", msg)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error %q is not retriable; a retry comes back to the same parker and the restore is idempotent", msg)
	}
	if c.permanentCause {
		// The raw text carries the credential, as the underlying error did;
		// the dispatcher's scrub is what keeps it from the Director.
		if !strings.Contains(msg, hangingRestoreSecret) {
			t.Fatalf("error %q lost the underlying cause; the scrub check below would prove nothing", msg)
		}
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %q does not carry the deadline as its cause", msg)
	}
	if strings.Contains(log.ScrubMessage(msg), hangingRestoreSecret) {
		t.Fatalf("scrubbed error still carries the credential: %q", log.ScrubMessage(msg))
	}
	if !strings.Contains(logged, "protection restore on parker did not finish within") || !strings.Contains(logged, "outcome is unknown") {
		t.Fatalf("no warning about the timed-out restore was logged: %s", logged)
	}
	if strings.Contains(logged, hangingRestoreSecret) {
		t.Fatalf("log output carries the credential: %s", logged)
	}
}

func newRestoreTestLogger(t *testing.T) (*log.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatal(err)
	}
	return logger, &buf
}

// TestTransferDiskFromParker_HungRestoreHitsItsDeadline moves a disk off a
// parker whose protection restore never answers, and checks that the restore's
// own deadline ends it and that the transfer reports it.
func TestTransferDiskFromParker_HungRestoreHitsItsDeadline(t *testing.T) {
	const timeout = 200 * time.Millisecond
	t.Parallel()
	ctx := WithParkerProtectionRestoreTimeoutForTest(context.Background(), timeout)
	c := &hangingRestoreClient{parker: 90000, scanFakeClient: newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"scsi0":         "data:vm-90000-disk-0,serial=" + transferStableID,
		},
		700: {},
	})}
	logger, logged := newRestoreTestLogger(t)
	parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
	start := time.Now()
	landed, err := TransferDiskFromParker(ctx, c, logger, parker, 700, "scsi1",
		"data:vm-90000-disk-0", "data:vm-90000-disk-0,serial="+transferStableID, transferTestCfg)
	elapsed := time.Since(start)
	if err != nil && !strings.HasPrefix(err.Error(), "transfer out:") {
		t.Fatalf("error %q does not name the transfer", err)
	}
	assertRestoreCutOff(t, c, err, elapsed, timeout, logged.String(), "the disk transfer to vm 700 slot scsi1 completed")
	slot, _ := c.configs[700]["scsi1"].(string)
	if !strings.HasPrefix(slot, "data:vm-700-disk-") {
		t.Fatalf("target slot = %q, want the disk moved before the restore was cut off", slot)
	}
	// The disk landed, so its new name comes back beside the cut-off for the
	// caller's receiving-side bookkeeping.
	if landed == "" || !strings.HasPrefix(slot, landed) {
		t.Fatalf("landed volid = %q, want the name on the target slot %q", landed, slot)
	}
	var cutOff *ProtectionRestoreCutOffError
	if !errors.As(err, &cutOff) || cutOff.ParkerVMID != 90000 || !cutOff.WorkCompleted {
		t.Fatalf("error %q is not a cut-off restore for parker 90000 after completed work", err)
	}
}

// TestDeleteParkedOwnedDisk_HungRestoreHitsItsDeadline is the same check for
// the parked-disk deletion window.
func TestDeleteParkedOwnedDisk_HungRestoreHitsItsDeadline(t *testing.T) {
	const timeout = 200 * time.Millisecond
	t.Parallel()
	ctx := WithParkerProtectionRestoreTimeoutForTest(context.Background(), timeout)
	desc := `<!--BOSH:{"bosh_parked_disks":{"` + transferStableID + `":{"disk_cid":"pvd-x","parked_at":"t",` +
		`"node":"pve1","volid":"data:vm-90000-disk-2","slot":"scsi1"}}}-->`
	c := &hangingRestoreClient{parker: 90000, permanentCause: true, scanFakeClient: newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"scsi1":         "data:vm-90000-disk-2,serial=" + transferStableID,
			"description":   desc,
		},
	})}
	logger, logged := newRestoreTestLogger(t)
	start := time.Now()
	err := DeleteParkedOwnedDisk(ctx, c, logger, "pve1", 90000, "data:vm-90000-disk-2", transferTestCfg)
	elapsed := time.Since(start)
	if err != nil && !strings.HasPrefix(err.Error(), "delete parked:") {
		t.Fatalf("error %q does not name the deletion", err)
	}
	assertRestoreCutOff(t, c, err, elapsed, timeout, logged.String(), `the deletion of "data:vm-90000-disk-2" completed`)
	var cutOff *ProtectionRestoreCutOffError
	if !errors.As(err, &cutOff) || cutOff.ParkerVMID != 90000 || !cutOff.WorkCompleted {
		t.Fatalf("error %q is not a cut-off restore for parker 90000 after completed work", err)
	}
	if len(c.destroyed) != 1 {
		t.Fatalf("destroyed = %v, want the parked volume deallocated before the restore", c.destroyed)
	}
	// The volume is gone, so its provenance entry goes too, even though the
	// restore was cut off.
	if entries := c.parkedEntries(t); len(entries) != 0 {
		t.Fatalf("provenance entries remain after the deletion completed: %+v", entries)
	}
}

// TestRestoreParkerProtection_SurvivesACancelledRequest checks that the
// restore still runs, and succeeds, when the request context is already
// cancelled, because it runs on context.WithoutCancel.
func TestRestoreParkerProtection_SurvivesACancelledRequest(t *testing.T) {
	c := newScanFakeClient(map[int]map[string]any{90000: {cfgKeyTags: "bosh-parker", paramProtection: false}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := restoreParkerProtection(ctx, c, nil, "transfer out", "pve1", 90000, "the disk transfer to vm 700 slot scsi1 completed"); err != nil {
		t.Fatalf("restore on a cancelled request: %v", err)
	}
	if prot, _ := c.configs[90000][paramProtection].(bool); !prot {
		t.Fatal("protection was not restored on a cancelled request")
	}
}

// TestRestoreParkerProtection_KnownFailureStaysAWarning checks that a restore
// PVE answers with a failure before the deadline keeps its old behavior: a
// warning, and no error to replace the window's own result.
func TestRestoreParkerProtection_KnownFailureStaysAWarning(t *testing.T) {
	t.Parallel()
	c := &hangingRestoreClient{parker: 90000, refuse: &sdkerrors.APIError{HTTPCode: 500, Message: "unable to update VM 90000"},
		scanFakeClient: newScanFakeClient(map[int]map[string]any{90000: {cfgKeyTags: "bosh-parker"}})}
	logger, logged := newRestoreTestLogger(t)
	if err := restoreParkerProtection(context.Background(), c, logger, "delete parked", "pve1", 90000, `the deletion of "data:vm-90000-disk-2" completed`); err != nil {
		t.Fatalf("a failure PVE answered returned %v, want nil", err)
	}
	if !strings.Contains(logged.String(), "could not restore protection on parker") {
		t.Fatalf("no warning logged for the failed restore: %s", logged.String())
	}
}

// TestParkerRestoreDeadlineFitsInsideTheLockTTL checks that a window that uses
// its whole budget, followed by a restore that uses its whole deadline, the
// demoted-slot sweep, and the lock release, still ends before the protection
// lock's TTL, so no waiter can steal the lock while the restore is in flight.
func TestParkerRestoreDeadlineFitsInsideTheLockTTL(t *testing.T) {
	ttl := ParkerProtectionLockTTL
	// A window's body stops parkerWindowReserve before its claim's expiry
	// (parkerWindowDeadline), so the longest body is the TTL less that reserve.
	window := ttl - parkerWindowReserve
	if parkerProtectionRestoreTimeout > parkerProtectionRestoreReserve {
		t.Fatalf("restore deadline %s exceeds the reserve %s the window leaves for it", parkerProtectionRestoreTimeout, parkerProtectionRestoreReserve)
	}
	if window+parkerProtectionRestoreTimeout >= ttl {
		t.Fatalf("window budget %s plus restore deadline %s reaches the TTL %s", window, parkerProtectionRestoreTimeout, ttl)
	}
	if total := window + parkerProtectionRestoreTimeout + parkerDemotedSweepTimeout + parkerLockReleaseTimeout; total > ttl {
		t.Fatalf("window %s, restore %s, sweep %s, and release %s add up to %s, past the TTL %s",
			window, parkerProtectionRestoreTimeout, parkerDemotedSweepTimeout, parkerLockReleaseTimeout, total, ttl)
	}
	if got := parkerRestoreTimeout(context.Background()); got != parkerProtectionRestoreTimeout {
		t.Fatalf("restore deadline without an override = %s, want %s", got, parkerProtectionRestoreTimeout)
	}
	if got := parkerRestoreTimeout(WithParkerProtectionRestoreTimeoutForTest(context.Background(), time.Second)); got != time.Second {
		t.Fatalf("restore deadline with a 1s override = %s", got)
	}
}

// TestTransferDiskFromParker_FailedMoveAndCutOffRestoreJoin fails the move and
// then cuts the restore off, and checks how the joined error classifies. The
// move's error comes first in the join, so a typed move verdict decides the
// type: a permanent one stays permanent even though the restore error is
// retriable. An untyped move error, the snapshot refusal, leaves the type to
// the restore error and stays detectable for the snapshot fallback. Both
// texts are present either way, and the restore's text says the transfer
// failed rather than completed.
func TestTransferDiskFromParker_FailedMoveAndCutOffRestoreJoin(t *testing.T) {
	cases := []struct {
		name     string
		moveErr  error
		wantType cpierrors.Type
		snapshot bool
	}{
		{
			name:     "permanent move verdict wins",
			moveErr:  errors.New("Configuration file 'nodes/pve1/qemu-server/700.conf' does not exist"),
			wantType: cpierrors.TypeCloud,
		},
		{
			name:     "untyped snapshot refusal leaves the restore's type",
			moveErr:  errors.New("Can't move disk used by a snapshot to another VM"),
			wantType: cpierrors.TypeRetriableCloud,
			snapshot: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := WithParkerProtectionRestoreTimeoutForTest(context.Background(), 100*time.Millisecond)
			c := &hangingRestoreClient{parker: 90000, scanFakeClient: newScanFakeClient(map[int]map[string]any{
				90000: {
					cfgKeyTags:      "bosh-cpi;bosh-parker",
					paramProtection: true,
					"scsi0":         "data:vm-90000-disk-0,serial=" + transferStableID,
				},
				700: {},
			})}
			c.moveErr = tc.moveErr
			parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
			_, err := TransferDiskFromParker(ctx, c, nil, parker, 700, "scsi1",
				"data:vm-90000-disk-0", "data:vm-90000-disk-0,serial="+transferStableID, transferTestCfg)
			if err == nil {
				t.Fatal("a failed move with a cut-off restore returned no error")
			}
			t.Logf("error: %s", err)
			msg := err.Error()
			for _, want := range []string{tc.moveErr.Error(), "protection restore on parker vmid 90000",
				"the disk transfer to vm 700 slot scsi1 failed"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("joined error %q does not contain %q", msg, want)
				}
			}
			if !cpierrors.IsType(err, tc.wantType) {
				var typed *cpierrors.Error
				errors.As(err, &typed)
				t.Fatalf("joined error classifies as %v, want %s", typed, tc.wantType)
			}
			var cutOff *ProtectionRestoreCutOffError
			if !errors.As(err, &cutOff) || cutOff.WorkCompleted {
				t.Fatalf("joined error %q does not carry a cut-off restore marked as after failed work", msg)
			}
			if got := errors.Is(err, ErrMoveDiskSnapshotRefused); got != tc.snapshot {
				t.Fatalf("errors.Is(err, ErrMoveDiskSnapshotRefused) = %t, want %t", got, tc.snapshot)
			}
		})
	}
}

// TestRestoreParkerProtection_TransportFailureIsUnknown fails every attempt
// of the restore in transport. PVE never answered, so nobody knows whether
// protection went back on: the restore returns the retriable cut-off error,
// which says it ended without an answer, rather than the warning a PVE
// refusal gets.
func TestRestoreParkerProtection_TransportFailureIsUnknown(t *testing.T) {
	t.Parallel()
	c := &hangingRestoreClient{parker: 90000, dropped: true,
		scanFakeClient: newScanFakeClient(map[int]map[string]any{90000: {cfgKeyTags: "bosh-parker"}})}
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	err := restoreParkerProtection(ctx, c, nil, "transfer out", "pve1", 90000, "the disk transfer to vm 700 slot scsi1 completed")
	var cutOff *ProtectionRestoreCutOffError
	if !errors.As(err, &cutOff) || cutOff.ParkerVMID != 90000 {
		t.Fatalf("restore that never got an answer = %v, want a cut-off restore for parker 90000", err)
	}
	for _, want := range []string{"transfer out: protection restore on parker vmid 90000 ended without an answer from PVE and its outcome is unknown", "qm set 90000 --protection 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error %q is not retriable", err)
	}
	if restores, _ := c.outcome(); restores < 2 {
		t.Fatalf("restore attempted %d times, want the retry loop to try again", restores)
	}
}
