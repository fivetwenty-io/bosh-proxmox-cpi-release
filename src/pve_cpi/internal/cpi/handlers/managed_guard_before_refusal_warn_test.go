package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// blockedWarnings returns the WARN lines in which the guard says it locked
// itself because its checks before a lifecycle mutation refused it.
func blockedWarnings(logged string) []string {
	var lines []string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, `"level":"WARN"`) &&
			strings.Contains(line, "requires reconciliation because the guard's checks before it refused it") {
			lines = append(lines, log.ScrubMessage(line))
		}
	}
	return lines
}

// TestManagedDiskBeforeRefusalWarnsWithTheReason runs the adopted mover's
// attach and moves the mover off the source out of band just before the
// migrate's admission, so the guard's read of the mover's config on the
// source answers that it is gone. That answer locks the guard before the
// migrate is sent. The guard logs one WARN with the reason, the call, the
// operation, and the allocation, in the shape of the WARN for an outcome it
// could not settle.
func TestManagedDiskBeforeRefusalWarnsWithTheReason(t *testing.T) {
	c := newAdoptedMoverCase(t)
	client, mover := c.client, c.mover
	moved := false
	client.onCertificateRead = func(string) error {
		if moved || len(client.protectionWrites) == 0 {
			return nil
		}
		last := client.protectionWrites[len(client.protectionWrites)-1]
		if last.vmid == mover && !last.on && last.node == "n2" {
			moved = true
			client.vmNodes[mover] = "n1"
		}
		return nil
	}

	_, attachErr := HandleAttachDisk(c.deps).Handle(c.ctx,
		[]json.RawMessage{planJSON(t, "777"), planJSON(t, c.cid)}, jsonrpc.Context{})
	if !moved {
		t.Fatalf("the attach never reached the migrate's admission; attach_disk = %v", attachErr)
	}
	if attachErr == nil {
		t.Fatal("attach_disk succeeded, want the locked guard's error")
	}
	if client.migrations != 0 {
		t.Fatalf("the attach sent %d migrations, want none", client.migrations)
	}
	lines := blockedWarnings(c.logged.String())
	if len(lines) != 1 {
		t.Fatalf("the guard logged %d WARNs that its checks refused a mutation, want one: %v", len(lines), lines)
	}
	for _, want := range []string{
		`"call":"Nodes.CreateQemuMigrate"`,
		`"operation":"attach_disk"`,
		`"allocation_id":"` + c.id + `"`,
		"cannot verify lifecycle holder before mutation",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the guard's WARN lacks %s: %s", want, lines[0])
		}
	}
}

// TestManagedGuardBlockedHookRunsOutsideTheGuardLock gives the guard a
// Blocked hook that reads the guard back, which a hook run under the guard's
// lock could never finish. The hook sees the guard already locked by the
// refusal, the call returns as not attempted without reaching PVE, and the
// guard stays locked for the next call, which doesn't run the hook again.
func TestManagedGuardBlockedHookRunsOutsideTheGuardLock(t *testing.T) {
	refusal := errors.New("the holder's config could not be read")
	client := managedDiskTestPVE{state: &managedDiskTestState{}}
	var guard *ManagedAllocationGuard
	var hooked []error
	guard, err := NewManagedAllocationGuard(client, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "", refusal },
		After:  func(context.Context, ManagedAllocationMutation, string, any) error { return nil },
		Failed: func(_ context.Context, _ ManagedAllocationMutation, _ string, err error) error { return err },
		Blocked: func(_ context.Context, call ManagedAllocationMutation, err error) {
			if !errors.Is(err, refusal) || call.Service+"."+call.Method != "Nodes.CreateQemuMigrate" {
				t.Errorf("Blocked got %s.%s with %v, want the migrate and Before's refusal",
					call.Service, call.Method, err)
			}
			hooked = append(hooked, guard.Err())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	migrate := func() error {
		done := make(chan error, 1)
		go func() {
			_, err := guard.Client().Nodes().CreateQemuMigrate(t.Context(), "n2", "90144",
				&nodes.CreateQemuMigrateParams{Target: "n1"})
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("the guarded call never returned: the Blocked hook deadlocked on the guard's lock")
			return nil
		}
	}

	if err := migrate(); err == nil || !errors.Is(err, pve.ErrMutationNotAttempted) {
		t.Fatalf("the refused migrate = %v, want it not attempted", err)
	}
	if len(hooked) != 1 || hooked[0] == nil {
		t.Fatalf("Blocked read the guard back as %v, want one read that finds it locked", hooked)
	}
	if err := migrate(); err == nil || !errors.Is(err, pve.ErrMutationNotAttempted) || len(hooked) != 1 {
		t.Fatalf("the migrate after the lock = %v with %d hook calls, want it not attempted and no new call",
			err, len(hooked))
	}
	if !errors.Is(guard.Err(), hooked[0]) {
		t.Fatalf("guard.Err() = %v, want the lock the hook saw (%v)", guard.Err(), hooked[0])
	}
}
