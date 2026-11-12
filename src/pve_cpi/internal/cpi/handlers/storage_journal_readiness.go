package handlers

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// storageJournalProvisioningDoc and storageJournalEnrollmentDoc are the pages
// a journal setup refusal points the operator at.
const (
	storageJournalProvisioningDoc = "docs/storage-journal-provisioning.md"
	storageJournalEnrollmentDoc   = "docs/storage-journal-operations.md#enroll-a-new-namespace"
)

// storageJournalReadinessError returns the first of storageJournalConfigError
// and storageJournalEnrollmentError. The journal-managed gates of create_vm and
// delete_vm call it as soon as they decide the journal is in use, so a
// journal that cannot be opened is refused, with its cause, before the gate
// makes a PVE request of its own.
func storageJournalReadinessError(cfg *config.CPIConfig) error {
	if err := storageJournalConfigError(cfg); err != nil {
		return err
	}
	return storageJournalEnrollmentError(cfg)
}

// storageJournalConfigError explains why the configuration cannot name a
// usable allocation journal, or returns nil when it names both halves.
//
// Every journal-managed gate opens the journal when either the namespace or
// the journal directory is set, because a journal directory may already hold
// records for VMs and disks that the legacy paths would orphan or allocate a
// second time. Such a gate therefore refuses a half-configured journal rather
// than falling back, and this text is the refusal. The configuration cannot
// reject the half on its own at load time, because a cpi-config entry may
// supply pve_storage_placement_namespace while the installed job supplies
// only the process-level journal directory.
//
// The errors are typed CloudErrors, so the Director reads this text and not
// the generic evidence line. They carry only property names, the configured
// directory, and the configured namespace, which the operator wrote and which
// are not secret.
func storageJournalConfigError(cfg *config.CPIConfig) error {
	if cfg == nil {
		return nil
	}
	directory := cfg.StorageAllocationJournalDir
	hasDirectory := strings.TrimSpace(directory) != ""
	if strings.TrimSpace(cfg.StoragePlacementNamespace) == "" {
		if !hasDirectory {
			return cpierrors.Cloud("set-enabled allocation needs the allocation journal, which needs "+
				"storage_placement_namespace, storage_allocation_journal_dir, and an enrolled namespace; set "+
				"both and enroll the namespace as %s describes", storageJournalProvisioningDoc)
		}
		return cpierrors.Cloud("storage_allocation_journal_dir is set but storage_placement_namespace is not, "+
			"and the allocation journal needs both and an enrolled namespace before the CPI can create or "+
			"delete VMs and disks; set storage_placement_namespace and enroll it as %s describes",
			storageJournalProvisioningDoc)
	}
	if !hasDirectory || !filepath.IsAbs(directory) {
		return cpierrors.Cloud("storage_placement_namespace is set but storage_allocation_journal_dir is not an "+
			"absolute directory, and the allocation journal needs both and an enrolled namespace before the CPI "+
			"can create or delete VMs and disks; set storage_allocation_journal_dir and enroll the namespace as "+
			"%s describes", storageJournalProvisioningDoc)
	}
	return nil
}

// storageJournalEnrollmentError explains a journal that the configuration
// names completely but that cannot be opened because it was never provisioned
// or its namespace was never enrolled. It returns nil when the enrollment
// file is present, and also when it cannot tell, so that the journal's own
// open decides and still refuses. It reads only whether the directory and the
// enrollment file exist, never their content.
func storageJournalEnrollmentError(cfg *config.CPIConfig) error {
	if cfg == nil {
		return nil
	}
	directory, namespace := cfg.StorageAllocationJournalDir, cfg.StoragePlacementNamespace
	recorded, err := aj.EnrollmentRecorded(directory, namespace)
	if err != nil || recorded {
		return nil
	}
	// #nosec G703 -- operator-configured journal directory; only its existence is read
	if _, statErr := os.Lstat(directory); errors.Is(statErr, fs.ErrNotExist) {
		return cpierrors.Cloud("allocation journal directory %s does not exist; provision it with "+
			"'cpi provision-journal' as %s describes, then enroll storage_placement_namespace %q with "+
			"'cpi storage-journal audit-enrollment' and 'cpi storage-journal initialize'",
			directory, storageJournalProvisioningDoc, namespace)
	}
	return cpierrors.Cloud("storage_placement_namespace %q is not enrolled in the allocation journal at %s; "+
		"enroll it with 'cpi storage-journal audit-enrollment' and then 'cpi storage-journal initialize' as "+
		"%s describes", namespace, directory, storageJournalEnrollmentDoc)
}
