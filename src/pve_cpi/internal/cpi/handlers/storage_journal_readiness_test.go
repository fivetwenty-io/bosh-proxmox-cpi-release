package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// journalSetupGeneric is the line the Director reads for a journal-managed
// delete whose cause the CPI could not classify.
const journalSetupGeneric = "allocation decision could not verify or persist evidence"

// journalSetupCase breaks one half of the allocation journal's setup in a
// fixture whose namespace is enrolled and whose VM 777 the journal records.
type journalSetupCase struct {
	name      string
	configure func(t *testing.T, cfg *config.CPIConfig)
	want      []string
}

func journalSetupCases() []journalSetupCase {
	return []journalSetupCase{
		{
			name:      "journal directory without a namespace",
			configure: func(_ *testing.T, cfg *config.CPIConfig) { cfg.StoragePlacementNamespace = "" },
			want: []string{
				"storage_allocation_journal_dir is set but storage_placement_namespace is not",
				storageJournalProvisioningDoc,
			},
		},
		{
			name:      "namespace never enrolled",
			configure: func(_ *testing.T, cfg *config.CPIConfig) { cfg.StoragePlacementNamespace = "never-enrolled" },
			want: []string{
				`storage_placement_namespace "never-enrolled" is not enrolled in the allocation journal at `,
				"'cpi storage-journal audit-enrollment' and then 'cpi storage-journal initialize'",
				storageJournalEnrollmentDoc,
			},
		},
		{
			name: "journal directory never provisioned",
			configure: func(t *testing.T, cfg *config.CPIConfig) {
				cfg.StorageAllocationJournalDir = filepath.Join(t.TempDir(), "allocations")
			},
			want: []string{
				"/allocations does not exist; provision it with 'cpi provision-journal'",
				storageJournalProvisioningDoc,
			},
		},
	}
}

func assertJournalSetupText(t *testing.T, text string, want []string) {
	t.Helper()
	for _, fragment := range want {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text %q does not contain %q", text, fragment)
		}
	}
	if strings.Contains(text, journalSetupGeneric) {
		t.Fatalf("text %q is the generic evidence line", text)
	}
}

// TestDeleteVMNamesTheJournalSetupCause breaks the journal's setup in three
// ways and sends delete_vm for a VM the journal records through the
// production dispatcher. The Director reads a non-retriable CloudError that
// names the missing piece and the page that explains it, where it used to read
// the generic evidence line. The delete logs the same cause at Warn, and it
// never falls back to the legacy delete, so the VM is still there.
func TestDeleteVMNamesTheJournalSetupCause(t *testing.T) {
	for _, tc := range journalSetupCases() {
		t.Run(tc.name, func(t *testing.T) {
			deps, client, _, _, _ := lifecycleFlowFixture(t)
			tc.configure(t, deps.Config)
			var logged bytes.Buffer
			logger, err := log.NewLogger("debug", &logged)
			if err != nil {
				t.Fatal(err)
			}
			deps.Logger = logger
			d := cpi.NewDispatcher(log.NewNopLogger())
			if err := d.Register("delete_vm", HandleDeleteVM(deps)); err != nil {
				t.Fatal(err)
			}
			body := d.Handle(context.Background(), &jsonrpc.Request{
				Method: "delete_vm", Arguments: []json.RawMessage{planJSON(t, "777")},
			}).Error
			if body == nil || body.Type != string(cpierrors.TypeCloud) || body.OkToRetry {
				t.Fatalf("Director received %+v, want a non-retriable CloudError", body)
			}
			assertJournalSetupText(t, body.Message, tc.want)
			if _, ok := client.state.configs[777]; !ok {
				t.Fatal("the delete fell back to the legacy path and destroyed the journal's VM")
			}
			line := warnLine(t, logged.String(), "delete_vm: the journal-managed delete failed")
			if line["error"] != body.Message || line["vm_cid"] != "777" {
				t.Fatalf("Warn line %v does not carry the cause %q for vm_cid 777", line, body.Message)
			}
		})
	}
}

// TestManagedDeleteVMErrorLogsTheScrubbedCause hands managedDeleteVMError a
// cause it cannot classify, which carries a PVE token. The Director still
// reads only the generic evidence line, and the Warn line carries the cause
// with the token scrubbed, as every logged error is.
func TestManagedDeleteVMErrorLogsTheScrubbedCause(t *testing.T) {
	var logged bytes.Buffer
	logger, err := log.NewLogger("debug", &logged)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "bosh@pve!cpi=0f3c2b1a-1111-2222-3333-444455556666"
	cause := fmt.Errorf("lstat namespace: request sent Authorization PVEAPIToken=%s: %w", secret, os.ErrPermission)
	got := managedDeleteVMError(logger, cause)
	var generic *storageDecisionObservationError
	if !errors.As(got, &generic) || got.Error() != journalSetupGeneric {
		t.Fatalf("Director error = %v, want the generic evidence line", got)
	}
	line := warnLine(t, logged.String(), "delete_vm: the journal-managed delete failed")
	text, _ := line["error"].(string)
	if !strings.Contains(text, "lstat namespace: request sent Authorization PVEAPIToken=") ||
		!strings.Contains(text, "permission denied") {
		t.Fatalf("Warn line error %q does not carry the cause", text)
	}
	if strings.Contains(logged.String(), "0f3c2b1a-1111-2222-3333-444455556666") {
		t.Fatalf("Warn line carries the token unscrubbed: %s", logged.String())
	}
	logged.Reset()
	if managedDeleteVMError(logger, nil) != nil || logged.Len() != 0 {
		t.Fatalf("a delete that succeeded reported or logged an error: %s", logged.String())
	}
}

// TestStorageJournalReadinessErrors pins each refusal's text and checks that
// an enrolled namespace, and a configuration whose enrollment cannot be told,
// pass through to the journal's own open.
func TestStorageJournalReadinessErrors(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		namespace, journal string
		want               string
	}{
		{"neither half", "", "", "set-enabled allocation needs the allocation journal, which needs " +
			"storage_placement_namespace, storage_allocation_journal_dir, and an enrolled namespace; set both " +
			"and enroll the namespace as docs/storage-journal-provisioning.md describes"},
		{"journal directory only", "", directory, "storage_allocation_journal_dir is set but " +
			"storage_placement_namespace is not, and the allocation journal needs both and an enrolled " +
			"namespace before the CPI can create or delete VMs and disks; set storage_placement_namespace and " +
			"enroll it as docs/storage-journal-provisioning.md describes"},
		{"namespace only", "director", "", "storage_placement_namespace is set but storage_allocation_journal_dir " +
			"is not an absolute directory, and the allocation journal needs both and an enrolled namespace " +
			"before the CPI can create or delete VMs and disks; set storage_allocation_journal_dir and enroll " +
			"the namespace as docs/storage-journal-provisioning.md describes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.CPIConfig{StoragePlacementNamespace: tc.namespace, StorageAllocationJournalDir: tc.journal}
			err := storageJournalConfigError(cfg)
			if !cpierrors.IsType(err, cpierrors.TypeCloud) || err.Error() != tc.want {
				t.Fatalf("error = %v, want CloudError %q", err, tc.want)
			}
		})
	}
	cfg := &config.CPIConfig{StoragePlacementNamespace: "never-enrolled", StorageAllocationJournalDir: directory}
	if err := storageJournalConfigError(cfg); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	want := `storage_placement_namespace "never-enrolled" is not enrolled in the allocation journal at ` + directory +
		"; enroll it with 'cpi storage-journal audit-enrollment' and then 'cpi storage-journal initialize' as " +
		"docs/storage-journal-operations.md#enroll-a-new-namespace describes"
	if err := storageJournalEnrollmentError(cfg); !cpierrors.IsType(err, cpierrors.TypeCloud) || err.Error() != want {
		t.Fatalf("not enrolled: error = %v, want %q", err, want)
	}
	absent := filepath.Join(directory, "absent")
	cfg.StorageAllocationJournalDir = absent
	want = "allocation journal directory " + absent + " does not exist; provision it with 'cpi provision-journal' " +
		"as docs/storage-journal-provisioning.md describes, then enroll storage_placement_namespace " +
		`"never-enrolled" with 'cpi storage-journal audit-enrollment' and 'cpi storage-journal initialize'`
	if err := storageJournalEnrollmentError(cfg); !cpierrors.IsType(err, cpierrors.TypeCloud) || err.Error() != want {
		t.Fatalf("not provisioned: error = %v, want %q", err, want)
	}
	deps, _, _, _, _ := lifecycleFlowFixture(t)
	if err := storageJournalEnrollmentError(deps.Config); err != nil {
		t.Fatalf("an enrolled namespace was refused: %v", err)
	}
	cfg.StoragePlacementNamespace = "../escape"
	if err := storageJournalEnrollmentError(cfg); err != nil {
		t.Fatalf("an enrollment it cannot tell about was refused here, not by the journal's open: %v", err)
	}
}

// warnLine returns the first Warn line in logged whose message is msg.
func warnLine(t *testing.T, logged, msg string) map[string]any {
	t.Helper()
	for raw := range strings.SplitSeq(strings.TrimSpace(logged), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) != nil {
			continue
		}
		if line["msg"] == msg && line["level"] == "WARN" {
			return line
		}
	}
	t.Fatalf("no Warn line %q in:\n%s", msg, logged)
	return nil
}
