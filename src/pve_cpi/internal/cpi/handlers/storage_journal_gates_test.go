package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// TestCreateDiskNamesTheJournalSetupCause runs a set-managed create_disk with a
// journal directory and no namespace, and then with a namespace that was never
// enrolled. Each refuses with the typed setup text in place of the generic
// managed-allocation line, and neither creates a volume. The configuration
// check runs before the node listing, so the first case refuses without
// listing the cluster's nodes, while enrollment is checked only when the
// journal opens after that listing.
func TestCreateDiskNamesTheJournalSetupCause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(cfg *config.CPIConfig)
		want      []string
		listed    bool
	}{
		{"journal directory without a namespace", func(cfg *config.CPIConfig) { cfg.StoragePlacementNamespace = "" },
			[]string{"pve.storage_allocation_journal_dir is set but no namespace is", storageJournalEnrollmentDoc},
			false},
		{"namespace never enrolled", func(*config.CPIConfig) {},
			[]string{`namespace "namespace" is not enrolled in the allocation journal at `,
				storageJournalEnrollmentDoc}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, selection, resolver, client, _ := managedDiskSiblingFixture(t, "spread")
			tc.configure(deps.Config)
			args := []json.RawMessage{json.RawMessage(`1025`), json.RawMessage(`{}`)}
			_, err := createManagedDisk(t.Context(), deps, args, selection, 1025, createDiskCloudProperties{}, "",
				resolver)
			if !cpierrors.IsType(err, cpierrors.TypeCloud) || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("create_disk error = %v, want a non-retriable CloudError", err)
			}
			assertJournalSetupText(t, err.Error(), tc.want)
			if strings.Contains(err.Error(), "create_disk: managed allocation failed") {
				t.Fatalf("create_disk error %q is the generic managed-allocation line", err.Error())
			}
			if len(client.state.volumes) != 0 {
				t.Fatalf("create_disk created volumes: %v", client.state.volumes)
			}
			if listed := client.firstCall("list") >= 0; listed != tc.listed {
				t.Fatalf("create_disk listed the cluster's nodes: %t, want %t", listed, tc.listed)
			}
		})
	}
}

// TestDeadNodeDeleteLogsTheJournalSetupCauseWithTheCID breaks the journal's
// setup and runs the delete a dead-node move finishes with. The logged cause
// carries the VM CID the Director sent, so the line can be matched to its
// task, and the VM is never handed to the legacy delete.
func TestDeadNodeDeleteLogsTheJournalSetupCauseWithTheCID(t *testing.T) {
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	deps.Config.StoragePlacementNamespace = "never-enrolled"
	var logged bytes.Buffer
	logger, err := log.NewLogger("debug", &logged)
	if err != nil {
		t.Fatal(err)
	}
	deps.Logger = logger
	const stamped = "777-0a1b2c3d-4e5f6a7b"
	cid, err := vmcid.Parse(stamped)
	if err != nil {
		t.Fatal(err)
	}
	_, err = deleteAfterDeadNodeMove(context.Background(), deps, stamped, cid)
	if !cpierrors.IsType(err, cpierrors.TypeCloud) ||
		!strings.Contains(err.Error(), `"never-enrolled" is not enrolled`) {
		t.Fatalf("dead-node delete error = %v, want the not-enrolled refusal", err)
	}
	line := logLine(t, logged.String(), "DEBUG", "delete_vm: the journal-managed delete failed")
	if line["vm_cid"] != stamped || line["error"] != err.Error() {
		t.Fatalf("logged line %v does not carry vm_cid %s and the cause", line, stamped)
	}
	if _, ok := client.state.configs[777]; !ok {
		t.Fatal("the dead-node delete fell back to the legacy path and destroyed the journal's VM")
	}
}

// TestManagedDiskCallsNameTheJournalSetupCause breaks the journal's setup and
// attaches a journal-managed disk. The Director reads the setup cause rather
// than the audit text the disk's identity check gives when it cannot open the
// journal or match its namespace, and nothing is attached.
func TestManagedDiskCallsNameTheJournalSetupCause(t *testing.T) {
	for _, tc := range journalSetupCases() {
		t.Run(tc.name, func(t *testing.T) {
			deps, client, _, _, cid := lifecycleFlowFixture(t)
			tc.configure(t, deps.Config)
			delete(client.state.configs[777], "scsi1")
			args := []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}
			_, err := HandleAttachDisk(deps).Handle(context.Background(), args, jsonrpc.Context{})
			if !cpierrors.IsType(err, cpierrors.TypeCloud) || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("attach_disk error = %v, want a non-retriable CloudError", err)
			}
			assertJournalSetupText(t, err.Error(), tc.want)
			if strings.Contains(err.Error(), "audit required") {
				t.Fatalf("attach_disk error %q is the audit text", err.Error())
			}
			if _, attached := client.state.configs[777]["scsi1"]; attached {
				t.Fatal("attach_disk attached the disk with a broken journal setup")
			}
		})
	}
}

// TestStampedDeleteVMNamesTheJournalSetupCauseFirst sends delete_vm with a
// stamped CID whose token the VM does not carry. The ownership check would
// answer that with its own verdict, so the setup refusal must come first for
// the Director to read the cause.
func TestStampedDeleteVMNamesTheJournalSetupCauseFirst(t *testing.T) {
	for _, tc := range journalSetupCases() {
		t.Run(tc.name, func(t *testing.T) {
			deps, client, _, _, _ := lifecycleFlowFixture(t)
			tc.configure(t, deps.Config)
			d := cpi.NewDispatcher(log.NewNopLogger())
			if err := d.Register("delete_vm", HandleDeleteVM(deps)); err != nil {
				t.Fatal(err)
			}
			body := d.Handle(context.Background(), &jsonrpc.Request{
				Method: "delete_vm", Arguments: []json.RawMessage{planJSON(t, "777-0a1b2c3d-4e5f6a7b")},
			}).Error
			if body == nil || body.Type != string(cpierrors.TypeCloud) || body.OkToRetry {
				t.Fatalf("Director received %+v, want a non-retriable CloudError", body)
			}
			assertJournalSetupText(t, body.Message, tc.want)
			if _, ok := client.state.configs[777]; !ok {
				t.Fatal("the stamped delete destroyed the journal's VM")
			}
		})
	}
}
