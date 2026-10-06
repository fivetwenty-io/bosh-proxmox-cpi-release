package pve

import (
	"context"
	"strings"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// TestMoverRestoreWarningDoesNotSendUsToTheLockPool fails a mover's migrate
// and its protection restore. The migration never takes the mover's lock, so
// the lock's pool proves nothing about the mover, and the warning must not
// send us to read it. It asks us to look for a running attach_disk or delete_disk instead.
func TestMoverRestoreWarningDoesNotSendUsToTheLockPool(t *testing.T) {
	t.Parallel()
	const moverVMID = 90007
	ctx := WithParkerProtectionRestoreTimeoutForTest(crCtx(), loggedRestoreTimeout)
	rp := newRPClient()
	rp.configs[moverVMID] = map[string]any{
		cfgKeyTags:      "bosh-cpi;bosh-parker;bosh-disk-mover",
		paramProtection: true,
		"scsi0":         "data:vm-90007-disk-0,serial=" + dmToken,
	}
	rp.migrateFn = func(int) (*sdknodes.CreateQemuMigrateResponse, error) {
		return nil, &sdkerrors.APIError{HTTPCode: 500, Code: 500, Message: "migration aborted"}
	}
	c := &hungRestoreMoverClient{rpClient: rp, mover: moverVMID}
	logger, logged := newRestoreTestLogger(t)
	if _, _, err := MigrateDiskViaMover(ctx, c, logger, DiskMigrationSpec{
		Holder: dmMoverHolder(moverVMID), TargetNode: "pve2",
		Volid: "data:vm-90007-disk-0", StableID: dmToken, AwaitBudget: time.Second,
	}, dmBand()); err == nil {
		t.Fatal("a refused migrate returned no error")
	}
	lines := restoreLogLines(logged.String())
	if len(lines) != 1 {
		t.Fatalf("restore warnings = %d, want 1: %s", len(lines), logged.String())
	}
	if strings.Contains(lines[0], "pvesh") || strings.Contains(lines[0], "bosh-lock-vm-") {
		t.Errorf("the mover's warning sends us to a lock pool its migration never takes: %s", lines[0])
	}
	if want := "only when bosh tasks lists no running attach_disk or delete_disk task for the disk, run qm set 90007 --protection 1"; !strings.Contains(lines[0], want) {
		t.Errorf("the mover's warning %s does not contain %q", lines[0], want)
	}
}

// TestReassertWarningGivesTheLockCheck fails the protection write that
// follows a park. The warning gives the qm set command, so it gives the lock
// check before it too, with the text for a cluster without the lock pool when
// the window ran without the lock.
func TestReassertWarningGivesTheLockCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{
			name: "serialized window",
			ctx:  context.Background(),
			want: "parker: could not re-assert protection on parker 90000 after a park; confirm with pvesh get /pools --poolid bosh-lock-vm-90000 that no CPI operation holds the parker's lock, and only when it answers that the pool does not exist, run qm set 90000 --protection 1",
		},
		{
			name: "window without the lock",
			ctx:  context.WithValue(context.Background(), parkerLockUnserializedKey{}, true),
			want: "parker: could not re-assert protection on parker 90000 after a park; the CPI can't take the parker's lock on this cluster, so only when bosh tasks lists no running task and no storage-journal command is running, run qm set 90000 --protection 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &hangingRestoreClient{parker: 90000, refuse: &sdkerrors.APIError{HTTPCode: 500, Message: "unable to update VM 90000"},
				scanFakeClient: newScanFakeClient(map[int]map[string]any{90000: {cfgKeyTags: "bosh-parker"}})}
			logger, logged := newRestoreTestLogger(t)
			reassertParkerProtection(tc.ctx, c, logger, "pve1", 90000, parkerWindowLockCheck(tc.ctx, 90000))
			if !strings.Contains(logged.String(), tc.want) {
				t.Fatalf("the warning %s does not contain %q", logged.String(), tc.want)
			}
			if strings.Contains(logged.String(), "—") {
				t.Errorf("the warning has an em dash: %s", logged.String())
			}
		})
	}
}
