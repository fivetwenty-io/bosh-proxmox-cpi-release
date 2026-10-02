package pve

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

func TestIsMoveSnapshotRefusalAnswerNamesOnlyPVEAnswers(t *testing.T) {
	text := "Can't move disk used by a snapshot to another VM\n"
	answer := func(code int, message string) error {
		return sdkerrors.ParseAPIError(code, []byte(fmt.Sprintf(`{"message":%q}`, message)))
	}
	for name, err := range map[string]error{
		"answered":         answer(500, text),
		"wrapped answered": cpierrors.Wrap(WrapMutationError(answer(500, text)), "move_disk"),
	} {
		if !IsMoveSnapshotRefusalAnswer(err) {
			t.Errorf("%s: not classified as PVE's snapshot refusal: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"nil":                    nil,
		"text without an answer": errors.New(text),
		"task verdict":           WrapError(fmt.Errorf("task %s failed: exit status %q", "UPID:n1:000573BD:03504636:6AA1786A:qmmove:777-unused0>90030-scsi0:root@pam:", text)),
		"cpi pre-check":          fmt.Errorf("vm 777 snapshot keep names it: %w", ErrMoveDiskSnapshotRefused),
		"gateway status":         answer(502, text),
		"ended context":          fmt.Errorf("%s: %w", text, context.DeadlineExceeded),
		"other snapshot answer":  answer(500, "you can't move a disk with snapshots and delete the source\n"),
		"lowercase answer":       answer(500, "can't move disk used by a snapshot to another vm\n"),
		"digest answer":          answer(500, "VM 777: detected modified configuration - file changed by other user? Try again.\n"),
	} {
		if IsMoveSnapshotRefusalAnswer(err) {
			t.Errorf("%s: classified as PVE's snapshot refusal: %v", name, err)
		}
	}
}

func TestIsMoveSnapshotRefusalTaskExitNamesOnlyTheWholeStatus(t *testing.T) {
	const upid = "UPID:n1:000573BE:03504636:6AA1786A:qmmove:777-unused0>90030-scsi0:root@pam:"
	sentence := "Can't move disk used by a snapshot to another VM"
	for name, err := range map[string]error{
		"fixed-interval verdict": WrapError(fmt.Errorf("task %s failed: exit status %q", upid, sentence)),
		"adaptive verdict":       classifyTaskExit(upid, sentence, false),
		"wrapped verdict":        cpierrors.Wrap(classifyTaskExit(upid, sentence, false), "move_disk task"),
	} {
		if !IsMoveSnapshotRefusalTaskExit(err) {
			t.Errorf("%s: not classified as the task's snapshot refusal: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"nil":              nil,
		"request answer":   sdkerrors.ParseAPIError(500, []byte(`{"message":"Can't move disk used by a snapshot to another VM\n"}`)),
		"longer status":    classifyTaskExit(upid, sentence+" (snapshot 'keep')", false),
		"prefixed status":  classifyTaskExit(upid, "VM 777: "+sentence, false),
		"lowercase status": classifyTaskExit(upid, "can't move disk used by a snapshot to another vm", false),
		"text only":        errors.New(sentence),
		"poll fault":       fmt.Errorf("AwaitTask %s poll failed: %s", upid, sentence),
		"unknown status":   pollTimeoutUnresolved(upid),
	} {
		if IsMoveSnapshotRefusalTaskExit(err) {
			t.Errorf("%s: classified as the task's snapshot refusal: %v", name, err)
		}
	}
}

func TestStopOnMoveSnapshotRefusalEndsOnlySnapshotRefusals(t *testing.T) {
	answer := func(message string) error {
		return sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`, message)))
	}
	refusal := answer("Can't move disk used by a snapshot to another VM\n")
	if !IsTransientTransport(refusal) {
		t.Fatalf("PVE's 500 no longer reads as transient, so this row proves nothing: %v", refusal)
	}
	stopped := stopOnMoveSnapshotRefusal(refusal)
	if IsTransientTransport(stopped) || IsPVEPushback(stopped) || IsStorageLockTimeout(stopped) || IsClusterNotQuorate(stopped) {
		t.Errorf("the loop would still retry %v", stopped)
	}
	if !IsMoveDiskSnapshotRefusal(stopped) || stopped.Error() != refusal.Error() {
		t.Errorf("the stop changed the refusal's text: %q, want %q", stopped, refusal)
	}
	for name, err := range map[string]error{
		"other server error": answer("storage migration failed: unable to rename volume\n"),
		"digest refusal":     answer("VM 777: detected modified configuration - file changed by other user? Try again.\n"),
	} {
		if stopped := stopOnMoveSnapshotRefusal(err); !errors.Is(stopped, err) || !IsTransientTransport(stopped) {
			t.Errorf("%s: the stop changed an error it should pass through: %v", name, stopped)
		}
	}
}

// TestMoveSnapshotRefusalMatchersDifferInStrictness pins how far each form of
// the refusal may stretch. The request form dies with the sentence and a
// newline at Qemu.pm 9.2.10:5060-5063 (9.0.0:4839-4842), and PVE's answer
// carries that message inside its own error text, so the request form accepts
// the sentence anywhere in an answered error, a prefixed or longer one
// included. The task form is the worker's die message, which fork_worker
// writes as "TASK ERROR: $err" (pve-common RESTEnvironment.pm 9.2.2:653-658)
// and read_status hands back unchanged (UPID.pm 9.2.2:78), so its exit status
// has to be the sentence and nothing else.
func TestMoveSnapshotRefusalMatchersDifferInStrictness(t *testing.T) {
	const upid = "UPID:n1:000573BE:03504636:6AA1786A:qmmove:777-unused0>90030-scsi0:root@pam:"
	sentence := "Can't move disk used by a snapshot to another VM"
	answer := func(message string) error {
		return sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`, message)))
	}
	for name, err := range map[string]error{
		"whole answer":    answer(sentence + "\n"),
		"prefixed answer": answer("VM 777: " + sentence + "\n"),
		"longer answer":   answer(sentence + " (snapshot 'keep')\n"),
	} {
		if !IsMoveSnapshotRefusalAnswer(err) {
			t.Errorf("request form, %s: not accepted: %v", name, err)
		}
	}
	if !IsMoveSnapshotRefusalTaskExit(classifyTaskExit(upid, sentence, false)) {
		t.Error("task form, whole status: not accepted")
	}
	for name, status := range map[string]string{
		"prefixed status": "VM 777: " + sentence,
		"longer status":   sentence + " (snapshot 'keep')",
	} {
		if err := classifyTaskExit(upid, status, false); IsMoveSnapshotRefusalTaskExit(err) {
			t.Errorf("task form, %s: accepted: %v", name, err)
		}
	}
}
