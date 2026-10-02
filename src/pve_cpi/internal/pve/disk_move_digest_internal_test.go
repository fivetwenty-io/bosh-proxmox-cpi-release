package pve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

func TestIsMoveDigestRefusalNamesOnlyPVEAnswersAndTaskVerdicts(t *testing.T) {
	text := "VM 777: detected modified configuration - file changed by other user? Try again.\n"
	answered := sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`, text)))
	verdict := WrapError(fmt.Errorf("task %s failed: exit status %q", "UPID:n1:000573BD:03504636:6AA1786A:qmmove:777-unused0>90030-scsi0:root@pam:", text))
	for name, err := range map[string]error{"answered": answered, "task verdict": verdict, "wrapped sentinel": fmt.Errorf("%w: x", ErrMoveDiskDigestRefused)} {
		if !IsMoveDigestRefusal(err) {
			t.Errorf("%s: not classified as a digest refusal: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"nil":                    nil,
		"other answer":           sdkerrors.ParseAPIError(500, []byte(`{"message":"Can't move disk used by a snapshot to another VM"}`)),
		"text without an answer": errors.New(text),
		"other task verdict":     fmt.Errorf("task UPID:n1:x failed: exit status %q", "storage migration failed"),
	} {
		if IsMoveDigestRefusal(err) {
			t.Errorf("%s: classified as a digest refusal: %v", name, err)
		}
	}
}

// TestPVEAnsweredSeparatesAnswersFromLostRequests covers the helper
// moveDiskToVM uses to decide that a move POST went unanswered. An answer
// from PVE, such as a 500 with PVE's text, a lock timeout, a 4xx, or
// pveproxy's 595 for a connection it never made, means no task forked. A
// transport fault, a client timeout, an ended context, a proxy's own status,
// or a status pveproxy relays from its client after the backend may have
// forked a task leaves open whether PVE forked one.
func TestPVEAnsweredSeparatesAnswersFromLostRequests(t *testing.T) {
	apiError := func(code int, message string) error {
		return sdkerrors.ParseAPIError(code, []byte(fmt.Sprintf(`{"message":%q}`, message)))
	}
	for name, err := range map[string]error{
		"500 with PVE's text": apiError(500, "VM 777: detected modified configuration - file changed by other user? Try again.\n"),
		"lock timeout":        apiError(500, "can't lock file '/var/lock/qemu-server/lock-777.conf' - got timeout\n"),
		"403":                 apiError(403, "Permission check failed (/vms/777, VM.Config.Disk)\n"),
		"400":                 apiError(400, "Parameter verification failed.\n"),
		"wrapped 500":         cpierrors.Wrap(WrapMutationError(apiError(500, "Disk 'unused0' for VM '777' does not exist\n")), "move_disk"),
		"595":                 apiError(595, "Connection refused"),
	} {
		if _, answered := pveAnswered(err); !answered {
			t.Errorf("%s: not counted as PVE's answer: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"nil":                nil,
		"dropped connection": &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "connection reset by peer"},
		"client timeout":     &sdkerrors.TimeoutError{Operation: "POST /nodes/pve1/qemu/777/move_disk", Duration: "30s"},
		"deadline":           fmt.Errorf("post move_disk: %w", context.DeadlineExceeded),
		"cancelled":          fmt.Errorf("post move_disk: %w", context.Canceled),
		"596":                apiError(596, "Connection refused"),
		"502":                apiError(502, "Bad Gateway"),
		"503":                apiError(503, "Service Unavailable"),
		"504":                apiError(504, "Gateway Timeout"),
		"597":                apiError(597, "Connection reset by peer"),
		"598":                apiError(598, "Request aborted"),
		"599":                apiError(599, "Broken pipe"),
		"plain error":        errors.New("move_disk response lost"),
	} {
		if _, answered := pveAnswered(err); answered {
			t.Errorf("%s: counted as PVE's answer: %v", name, err)
		}
	}
}

// unansweredMoveClient is a parker with one disk on scsi0, and the first move
// off it loses its response after PVE forked the task. Without readErr, the
// second POST finds that task mid-commit. It has written the parker without
// the slot and hasn't written the target yet, so PVE refuses the second POST
// on the parker's changed digest. With readErr, the second attempt's config
// read of the parker fails with it before any second POST, and the first task
// hasn't run yet, so it can still land after the readback.
//
// The parker's protection restore hangs on cutOff, a context the fake cancels
// at the mid-commit read, which is the readback's pending read of the parker.
// The restore then ends without an answer from PVE, so the cut-off restore
// reports how the transfer ended, and the row waits on no real deadline.
type unansweredMoveClient struct {
	*scanFakeClient
	parker  int
	readErr error

	posts        int
	readFailed   bool
	cutOff       context.Context
	cancelCutOff context.CancelFunc
	// restoreEarly records a protection restore that reached the parker
	// before the mid-commit read cancelled cutOff.
	restoreEarly bool
}

func newUnansweredMoveClient(readErr error) *unansweredMoveClient {
	cutOff, cancel := context.WithCancel(context.Background())
	return &unansweredMoveClient{parker: 90000, readErr: readErr, cutOff: cutOff, cancelCutOff: cancel,
		scanFakeClient: newScanFakeClient(map[int]map[string]any{
			90000: {
				cfgKeyTags:      "bosh-cpi;bosh-parker",
				paramProtection: true,
				"scsi0":         "data:vm-90000-disk-0,serial=" + transferStableID,
				"digest":        "parker-before-move",
			},
			700: {"digest": "target-before-move"},
		})}
}

func (c *unansweredMoveClient) QEMU() qemu.Service {
	svc := c.scanFakeClient.QEMU()
	inner, ok := svc.(*fakeQEMUService)
	if !ok {
		panic("scanFakeClient.QEMU is not a *fakeQEMUService")
	}
	passThrough := inner.configFn
	inner.configFn = func(ctx context.Context, node string, vmid int) (map[string]any, error) {
		if c.readErr != nil && vmid == c.parker && c.posts == 1 && !c.readFailed {
			c.readFailed = true
			return nil, c.readErr
		}
		return passThrough(ctx, node, vmid)
	}
	return svc
}

func (c *unansweredMoveClient) Nodes() sdknodes.Service {
	nodes := c.scanFakeClient.Nodes()
	inner, ok := nodes.(*fakeNodesService)
	if !ok {
		panic("scanFakeClient.Nodes is not a *fakeNodesService")
	}
	inner.createQemuMoveDiskFn = func(_ context.Context, _, vmid string, params *sdknodes.CreateQemuMoveDiskParams) (*sdknodes.CreateQemuMoveDiskResponse, error) {
		c.posts++
		if c.posts == 1 {
			return nil, droppedConnection()
		}
		c.mu.Lock()
		delete(c.configs[c.parker], params.Disk)
		c.configs[c.parker]["digest"] = "parker-after-first-task"
		c.mu.Unlock()
		if params.Digest != nil && *params.Digest != "parker-after-first-task" {
			return nil, sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":"VM %s: detected modified configuration - file changed by other user? Try again.\n"}`, vmid)))
		}
		return nil, sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":"Disk '%s' for VM '%s' does not exist\n"}`, params.Disk, vmid)))
	}
	pendingPassThrough := inner.listQemuPendingFn
	inner.listQemuPendingFn = func(ctx context.Context, node, vmid string) (*sdknodes.ListQemuPendingResponse, error) {
		if vmid == strconv.Itoa(c.parker) && c.posts > 0 {
			c.cancelCutOff()
		}
		return pendingPassThrough(ctx, node, vmid)
	}
	updatePassThrough := inner.updateQemuConfigFn
	inner.updateQemuConfigFn = func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
		if params.Protection != nil && *params.Protection && vmid == strconv.Itoa(c.parker) {
			select {
			case <-c.cutOff.Done():
			default:
				c.restoreEarly = true
			}
			return fmt.Errorf("put protection on parker %d: %w", c.parker, context.Canceled)
		}
		return updatePassThrough(ctx, node, vmid, params)
	}
	return nodes
}

// assertTransferOutcomeUnknown requires what a transfer whose move has an
// unknown outcome returns. The error is retriable, it doesn't read as a
// digest refusal, the cut-off restore says the transfer's outcome is unknown,
// and no volume is reported as landed.
func assertTransferOutcomeUnknown(t *testing.T, c *unansweredMoveClient, landed string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("a move with an unknown outcome returned no error")
	}
	t.Logf("error: %s", err)
	msg := err.Error()
	if c.restoreEarly {
		t.Fatal("the protection restore reached the parker before the mid-commit read")
	}
	if !strings.Contains(msg, "has an unknown outcome") {
		t.Fatalf("error %q is not the move's unknown outcome", msg)
	}
	if !strings.Contains(msg, "the outcome of the disk transfer to vm 700 slot scsi1 is unknown") || strings.Contains(msg, "scsi1 failed") {
		t.Fatalf("error %q does not say the transfer's outcome is unknown", msg)
	}
	if errors.Is(err, ErrMoveDiskDigestRefused) || IsMoveDigestRefusal(err) {
		t.Fatalf("error %q still reads as a digest refusal", msg)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error %q is not retriable", msg)
	}
	if landed != "" {
		t.Fatalf("landed volid = %q, want none for a move with an unknown outcome", landed)
	}
}

// TestTransferDiskFromParker_MidCommitReadbackIsUnknown drops the response to
// the move off the parker, and the second POST is refused on its digest while
// the first task is mid-commit. The readback finds the parker's slot gone and
// the target's slot still empty, which proves neither outcome. The transfer
// comes back retriable without the digest refusal, and the cut-off restore
// says the transfer's outcome is unknown.
func TestTransferDiskFromParker_MidCommitReadbackIsUnknown(t *testing.T) {
	t.Parallel()
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	c := newUnansweredMoveClient(nil)
	parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
	landed, err := TransferDiskFromParker(ctx, c, nil, parker, 700, "scsi1",
		"data:vm-90000-disk-0", "data:vm-90000-disk-0,serial="+transferStableID, transferTestCfg)
	if c.posts != 2 {
		t.Fatalf("want two move POSTs, got %d", c.posts)
	}
	assertTransferOutcomeUnknown(t, c, landed, err)
}

// TestTransferDiskFromParker_ReadErrorAfterUnansweredMoveIsUnknown drops the
// response to the move off the parker, and the second attempt's config read
// of the parker fails with a permission error, which the loop doesn't retry.
// The readback finds the disk still on the parker and the target's slot
// empty. A read error, unlike a refusal, doesn't prove that a configuration
// changed, so the first task could still land after the readback. The
// transfer comes back retriable without the read error or the digest
// refusal, and the cut-off restore says the transfer's outcome is unknown.
func TestTransferDiskFromParker_ReadErrorAfterUnansweredMoveIsUnknown(t *testing.T) {
	t.Parallel()
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	c := newUnansweredMoveClient(sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed (/vms/90000, VM.Audit)\n"}`)))
	parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
	landed, err := TransferDiskFromParker(ctx, c, nil, parker, 700, "scsi1",
		"data:vm-90000-disk-0", "data:vm-90000-disk-0,serial="+transferStableID, transferTestCfg)
	if c.posts != 1 || !c.readFailed {
		t.Fatalf("want one move POST and the failed read, got %d POSTs and failed read %t", c.posts, c.readFailed)
	}
	if held, _ := ConfigString(c.configs[90000], "scsi0"); !strings.HasPrefix(held, "data:vm-90000-disk-0") {
		t.Fatalf("parker scsi0 holds %q, want the disk still in place", held)
	}
	if strings.Contains(err.Error(), "Permission check failed") {
		t.Fatalf("error %q lets the read error stand", err)
	}
	assertTransferOutcomeUnknown(t, c, landed, err)
}
