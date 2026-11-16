package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// replicationRefusalText is the sentence qemu-server's move_disk reassign
// check dies with when the target VM has a replication job and the volume's
// storage cannot replicate (API2/Qemu.pm, the check before fork_worker).
const replicationRefusalText = "Cannot move disk to a replicated VM. Storage does not support replication!\n"

// labReplicationRefusal is the shape the refusal reached the CPI in: PVE's
// sentence inside a POST failure that carries no HTTP status.
func labReplicationRefusal() error {
	return errors.New(`nodes.CreateQemuMoveDisk: failed to execute POST request to "/nodes/n1/qemu/90000/move_disk" ` +
		`with context: HTTP POST request failed: API request failed: ` + replicationRefusalText + ` (code: 0)`)
}

func TestIsMoveReplicationRefusalNamesOnlyTheReplicationRefusal(t *testing.T) {
	answer := func(code int, message string) error {
		return sdkerrors.ParseAPIError(code, []byte(fmt.Sprintf(`{"message":%q}`, message)))
	}
	for name, err := range map[string]error{
		"answered":      answer(500, replicationRefusalText),
		"wrapped":       cpierrors.Wrap(WrapMutationError(answer(500, replicationRefusalText)), "move_disk"),
		"without code":  labReplicationRefusal(),
		"cpi sentinel":  fmt.Errorf("move scsi0: %w", ErrMoveDiskReplicationRefused),
		"stopped error": stopOnMoveReplicationRefusal(answer(500, replicationRefusalText)),
	} {
		if !IsMoveReplicationRefusal(err) {
			t.Errorf("%s: not classified as the replication refusal: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"nil":              nil,
		"snapshot refusal": answer(500, "Can't move disk used by a snapshot to another VM\n"),
		"digest refusal":   answer(500, "VM 777: detected modified configuration - file changed by other user? Try again.\n"),
		"config refusal":   answer(500, "cannot add non-replicatable volume to a replicated VM\n"),
		"other failure":    answer(500, "storage migration failed: unable to rename volume\n"),
	} {
		if IsMoveReplicationRefusal(err) {
			t.Errorf("%s: classified as the replication refusal: %v", name, err)
		}
	}
}

// TestMoveReplicationRefusalIsNeverTransient pins that the refusal stops
// every retry loop. PVE answers it with a 500, which the blanket 5xx rule
// would retry, and the job that causes it stays until someone removes it.
func TestMoveReplicationRefusalIsNeverTransient(t *testing.T) {
	answered := sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`, replicationRefusalText)))
	if !errors.Is(answered, sdkerrors.ErrServer) {
		t.Fatalf("PVE's 500 no longer carries the server error, so this row proves nothing: %v", answered)
	}
	for name, err := range map[string]error{"answered": answered, "without code": labReplicationRefusal()} {
		if IsTransientTransport(err) {
			t.Errorf("%s: the refusal reads as a transient transport fault: %v", name, err)
		}
		stopped := stopOnMoveReplicationRefusal(err)
		if IsTransientTransport(stopped) || IsPVEPushback(stopped) || IsStorageLockTimeout(stopped) {
			t.Errorf("%s: the loop would still retry %v", name, stopped)
		}
		if stopped.Error() != err.Error() {
			t.Errorf("%s: the stop changed the refusal's text: %q, want %q", name, stopped, err)
		}
	}
	other := sdkerrors.ParseAPIError(500, []byte(`{"message":"storage migration failed\n"}`))
	if stopped := stopOnMoveReplicationRefusal(other); !errors.Is(stopped, other) {
		t.Errorf("the stop changed an error it should pass through: %v", stopped)
	}
}

// TestTransferDiskFromParker_ReplicationRefusalIsPermanent drives a transfer
// into a replicated VM. The fake refuses the first move only and logs every
// move it lands, so a loop that retried would land the disk on its second
// POST. The refusal must instead come back from the first POST as a permanent
// CPI error that callers can detect, with the disk still on the parker and its
// protection back on.
func TestTransferDiskFromParker_ReplicationRefusalIsPermanent(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{
		"answered":     sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`, replicationRefusalText))),
		"without code": labReplicationRefusal(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newScanFakeClient(map[int]map[string]any{
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"scsi0":    "nfs:700/vm-700-disk-0.qcow2,serial=" + transferStableID,
				},
				710: {},
			})
			c.moveErr = refusal
			parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
			landed, err := TransferDiskFromParker(context.Background(), c, nil, parker, 710, "scsi1",
				"nfs:700/vm-700-disk-0.qcow2", "nfs:700/vm-700-disk-0.qcow2,serial="+transferStableID, transferTestCfg)
			if !errors.Is(err, ErrMoveDiskReplicationRefused) {
				t.Fatalf("err = %v, want ErrMoveDiskReplicationRefused", err)
			}
			var typed *cpierrors.Error
			if !errors.As(err, &typed) || typed.OkToRetry() {
				t.Fatalf("err = %v, want a permanent CPI error", err)
			}
			if landed != "" {
				t.Errorf("landed = %q, want nothing", landed)
			}
			if i := c.eventIndex("move:"); i >= 0 {
				t.Errorf("the refused move was sent again and landed; events=%v", c.events)
			}
			if got, _ := c.configs[90000]["scsi0"].(string); !strings.HasPrefix(got, "nfs:700/vm-700-disk-0.qcow2") {
				t.Errorf("parker slot = %q, want the disk still there", got)
			}
			if _, ok := c.configs[710]["scsi1"]; ok {
				t.Errorf("target slot filled after the refusal: %v", c.configs[710])
			}
			if prot, _ := c.configs[90000][paramProtection].(bool); !prot {
				t.Error("protection not restored after the refused move")
			}
		})
	}
}

// TestMoveReplicationRefusalTaskExitNamesOnlyTheWholeStatus checks the task
// form of the refusal. The move task checks the receiving VM's jobs again
// under the configuration locks, so a job set up after the request makes the
// task exit with PVE's sentence, and only that whole status proves the task
// moved nothing.
func TestMoveReplicationRefusalTaskExitNamesOnlyTheWholeStatus(t *testing.T) {
	const upid = "UPID:n1:000573BE:03504636:6AA1786A:qmmove:90000-scsi0>777-scsi1:root@pam:"
	sentence := strings.TrimSuffix(replicationRefusalText, "\n")
	exit := classifyTaskExit(upid, sentence, false)
	if !IsMoveReplicationRefusalTaskExit(exit) || !IsMoveReplicationRefusal(exit) {
		t.Fatalf("the task's whole status is not the replication refusal: %v", exit)
	}
	for name, status := range map[string]string{
		"prefixed status": "VM 777: " + sentence,
		"longer status":   sentence + " (job 777-0)",
		"other failure":   "storage migration failed: unable to rename volume",
	} {
		if err := classifyTaskExit(upid, status, false); IsMoveReplicationRefusalTaskExit(err) {
			t.Errorf("%s: accepted: %v", name, err)
		}
	}
	answered := sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`, replicationRefusalText)))
	if IsMoveReplicationRefusalTaskExit(answered) {
		t.Errorf("the request's answer reads as a task exit: %v", answered)
	}
}
