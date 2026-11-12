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

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
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
				"pve.storage_allocation_journal_dir is set but no namespace is",
				"set pve.storage_placement_namespace in the job or pve_storage_placement_namespace in the " +
					"cpi-config entry",
				storageJournalEnrollmentDoc,
			},
		},
		{
			name:      "namespace never enrolled",
			configure: func(_ *testing.T, cfg *config.CPIConfig) { cfg.StoragePlacementNamespace = "never-enrolled" },
			want: []string{
				`namespace "never-enrolled" is not enrolled in the allocation journal at `,
				"which holds 1 other enrolled namespace(s); if the storage placement namespace was changed, " +
					"set it back rather than enrolling a new one",
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
				"/allocations does not exist or this process cannot see it",
				"provision it with 'cpi provision-journal' as " + storageJournalProvisioningDoc,
				"list it in director.cpi_additional_volumes with writable: true and mount_only: true",
				storageJournalEnrollmentDoc,
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
// the generic evidence line. The delete logs the same cause at Debug, because
// the Director already reads it, and it never falls back to the legacy delete,
// so the VM is still there.
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
			line := logLine(t, logged.String(), "DEBUG", "delete_vm: the journal-managed delete failed")
			if line["error"] != body.Message || line["vm_cid"] != "777" {
				t.Fatalf("Debug line %v does not carry the cause %q for vm_cid 777", line, body.Message)
			}
			if hasLogLine(logged.String(), "WARN", "delete_vm: the journal-managed delete failed") {
				t.Fatalf("a cause the Director reads was also logged at Warn:\n%s", logged.String())
			}
		})
	}
}

// TestManagedDeleteVMErrorLogsTheScrubbedCause hands managedDeleteVMError a
// cause it cannot classify, which carries a PVE token. The Director still
// reads only the generic evidence line, and the Warn line carries the cause
// with the token scrubbed, as every logged error is. A typed cause keeps its
// own text and is logged at Debug only.
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
	typed := cpierrors.Cloud("audit gate refused")
	if got := managedDeleteVMError(logger, typed); !errors.Is(got, typed) || got.Error() != typed.Error() {
		t.Fatalf("a typed cause became %v", got)
	}
	if hasLogLine(logged.String(), "WARN", "delete_vm: the journal-managed delete failed") ||
		!hasLogLine(logged.String(), "DEBUG", "delete_vm: the journal-managed delete failed") {
		t.Fatalf("a typed cause was not logged at Debug alone:\n%s", logged.String())
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
			"pve.storage_placement_namespace, pve.storage_allocation_journal_dir, and an enrolled namespace; " +
			"set both, provision the directory as docs/storage-journal-provisioning.md describes, and enroll " +
			"the namespace as docs/storage-journal-operations.md#enroll-a-new-namespace describes"},
		{"journal directory only", "", directory, "pve.storage_allocation_journal_dir is set but no namespace " +
			"is, and the allocation journal needs a namespace that is enrolled before the CPI can create or " +
			"delete VMs and disks; set pve.storage_placement_namespace in the job or " +
			"pve_storage_placement_namespace in the cpi-config entry, and enroll it as " +
			"docs/storage-journal-operations.md#enroll-a-new-namespace describes"},
		{"namespace only", "director", "", "pve.storage_placement_namespace is set but " +
			"pve.storage_allocation_journal_dir is not an absolute directory, and the allocation journal needs " +
			"both and an enrolled namespace before the CPI can create or delete VMs and disks; set " +
			"pve.storage_allocation_journal_dir, provision it as docs/storage-journal-provisioning.md " +
			"describes, and enroll the namespace as docs/storage-journal-operations.md#enroll-a-new-namespace " +
			"describes"},
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
	want := `namespace "never-enrolled" is not enrolled in the allocation journal at ` + directory +
		"; enroll it with 'cpi storage-journal audit-enrollment' and then 'cpi storage-journal initialize' as " +
		"docs/storage-journal-operations.md#enroll-a-new-namespace describes"
	if err := storageJournalEnrollmentError(cfg); !cpierrors.IsType(err, cpierrors.TypeCloud) || err.Error() != want {
		t.Fatalf("not enrolled: error = %v, want %q", err, want)
	}
	enrolled, err := aj.Initialize(context.Background(), directory, "previous", aj.Enrollment{
		ClusterID: "cluster", AuthorityID: "authority", AuditID: "audit",
		PreviousWriterFenced: true, CompleteHistoricalAudit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := enrolled.Close(); err != nil {
		t.Fatal(err)
	}
	want = `namespace "never-enrolled" is not enrolled in the allocation journal at ` + directory +
		", which holds 1 other enrolled namespace(s); if the storage placement namespace was changed, set it " +
		"back rather than enrolling a new one, and otherwise enroll it with 'cpi storage-journal " +
		"audit-enrollment' and then 'cpi storage-journal initialize' as " +
		"docs/storage-journal-operations.md#enroll-a-new-namespace describes"
	if err := storageJournalEnrollmentError(cfg); !cpierrors.IsType(err, cpierrors.TypeCloud) || err.Error() != want {
		t.Fatalf("not enrolled beside another namespace: error = %v, want %q", err, want)
	}
	absent := filepath.Join(directory, "absent")
	cfg.StorageAllocationJournalDir = absent
	want = "allocation journal directory " + absent + " does not exist or this process cannot see it; if it " +
		"was never provisioned, provision it with 'cpi provision-journal' as docs/storage-journal-provisioning.md " +
		"describes, and if the Director's workers run under BPM, list it in director.cpi_additional_volumes " +
		`with writable: true and mount_only: true; then enroll namespace "never-enrolled" as ` +
		"docs/storage-journal-operations.md#enroll-a-new-namespace describes"
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
	return logLine(t, logged, "WARN", msg)
}

// logLine returns the first line in logged at level whose message is msg.
func logLine(t *testing.T, logged, level, msg string) map[string]any {
	t.Helper()
	if line := findLogLine(logged, level, msg); line != nil {
		return line
	}
	t.Fatalf("no %s line %q in:\n%s", level, msg, logged)
	return nil
}

// hasLogLine reports whether logged holds a line at level whose message is msg.
func hasLogLine(logged, level, msg string) bool {
	return findLogLine(logged, level, msg) != nil
}

func findLogLine(logged, level, msg string) map[string]any {
	for raw := range strings.SplitSeq(strings.TrimSpace(logged), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) != nil {
			continue
		}
		if line["msg"] == msg && line["level"] == level {
			return line
		}
	}
	return nil
}
