package pve

import (
	"errors"
	"fmt"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// replicatedAddRefusalText is the sentence qemu-server's update_vm dies with
// when a config edit adds a node-local volume that cannot replicate to a VM
// that has a replication job (API2/Qemu.pm, check_replication).
const replicatedAddRefusalText = "cannot add non-replicatable volume to a replicated VM\n"

// TestReplicatedVolumeAddRefusalIsPermanent checks that PVE's refusal of a
// config edit that adds a node-local volume to a replicated VM is classified
// by its text, is never transient, and maps to a CloudError that is not
// retried, whether PVE answered with a status or not.
func TestReplicatedVolumeAddRefusalIsPermanent(t *testing.T) {
	answered := sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`, replicatedAddRefusalText)))
	uncoded := errors.New(`nodes.UpdateQemuConfig: API request failed: ` + replicatedAddRefusalText + ` (code: 0)`)
	for name, err := range map[string]error{"answered": answered, "without code": uncoded} {
		if !IsReplicatedVolumeAddRefusal(err) {
			t.Errorf("%s: not classified as the replicated add refusal: %v", name, err)
		}
		if IsTransientTransport(err) || IsRetryableOrLockFault(err) {
			t.Errorf("%s: the refusal is retried: %v", name, err)
		}
		wrapped := WrapError(err)
		var typed *cpierrors.Error
		if !errors.As(wrapped, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
			t.Errorf("%s: WrapError gives %v, want a CloudError that is not retried", name, wrapped)
		}
	}
	for name, err := range map[string]error{
		"nil":          nil,
		"server error": sdkerrors.ParseAPIError(500, []byte(`{"message":"got timeout\n"}`)),
		"pass-through": errors.New("cannot add non-managed/pass-through volume to a replicated VM"),
	} {
		if IsReplicatedVolumeAddRefusal(err) {
			t.Errorf("%s: classified as the replicated add refusal: %v", name, err)
		}
	}
}

// TestMoveReplicationRefusalWrapsAsCloud checks that WrapError gives the
// move refusal the same permanent answer the move itself returns.
func TestMoveReplicationRefusalWrapsAsCloud(t *testing.T) {
	err := sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`,
		"Cannot move disk to a replicated VM. Storage does not support replication!\n")))
	var typed *cpierrors.Error
	if wrapped := WrapError(err); !errors.As(wrapped, &typed) || typed.Type() != cpierrors.TypeCloud {
		t.Fatalf("WrapError gives %v, want a CloudError", wrapped)
	}
}
