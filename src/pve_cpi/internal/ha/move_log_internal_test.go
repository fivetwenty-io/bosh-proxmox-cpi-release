package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestMoveIntoSetLogsTheMovingVMBesideTheCallsVM pins the key that names the
// moved VM on every "ha move:" line. A call can move a VM other than its own,
// and its logger already carries vmid for its own VM, so the move names the
// VM it moves moving_vmid. The call's vmid stays as it was, and no key
// repeats under a numbered name such as vmid_2.
func TestMoveIntoSetLogsTheMovingVMBesideTheCallsVM(t *testing.T) {
	states := func() []*pve.HAServiceState {
		return []*pve.HAServiceState{
			managedOn("started", "b", 100, 90),
			managedOn("started", "b", 100, 90),
			managedOn("started", "b", 200, 190),
			managedOn("started", "a", 220, 215),
		}
	}
	var buf bytes.Buffer
	logger, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatal(err)
	}
	logger = logger.With(log.Int("vmid", 100))

	// One move whose request PVE never answered, and one whose answer names a
	// comigrated partner, so every line of the move is written.
	unanswered := &moveRig{settleOn: []string{"b", "a"}, migrateErr: errors.New("read: connection reset by peer"),
		states: states()}
	comigrated := &moveRig{settleOn: []string{"b", "a"}, comigrated: []json.RawMessage{json.RawMessage(`"vm:102"`)},
		states: states()}
	for _, r := range []*moveRig{unanswered, comigrated} {
		deps := r.deps()
		deps.Logger = logger
		in := moveInput()
		in.AwaitUnanswered = true
		if _, err := MoveIntoSet(context.Background(), deps, in); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		"ha move: PVE never answered the migrate request",
		"ha move: PVE HA moves other resources with the VM",
		"ha move: asked PVE HA to migrate the VM into its node set",
		"ha move: the VM runs inside its node set",
	}
	var seen []string
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		msg, _ := fields["msg"].(string)
		if !strings.HasPrefix(msg, "ha move:") {
			continue
		}
		for _, w := range want {
			if strings.HasPrefix(msg, w) && !slices.Contains(seen, w) {
				seen = append(seen, w)
			}
		}
		if fields["vmid"] != float64(100) || fields["moving_vmid"] != float64(101) {
			t.Errorf("line %q: vmid %v moving_vmid %v; want the call's VM 100 and the moving VM 101", msg,
				fields["vmid"], fields["moving_vmid"])
		}
		for k := range fields {
			if strings.HasSuffix(k, "_2") {
				t.Errorf("line %q: key %s; want no repeated key", msg, k)
			}
		}
	}
	for _, w := range want {
		if !slices.Contains(seen, w) {
			t.Errorf("no line starts %q; got %s", w, buf.String())
		}
	}
}
