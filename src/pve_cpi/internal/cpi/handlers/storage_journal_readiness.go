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
// and storageJournalEnrollmentError. The journal-managed gates of create_vm,
// delete_vm, and the managed-disk calls run it as soon as they decide the
// journal is in use, so a journal that cannot be opened is refused, with its
// cause, before the gate lists nodes or reads a VM from PVE.
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
// than falling back, and this text is the refusal.
//
// Config load and the job template reject a namespace without a journal
// directory in the job, because no cpi-config entry can supply the directory.
// They do not check an entry's effective configuration, so an entry that adds
// pve_storage_placement_namespace to a job with no journal directory still
// applies, and its calls that answer from PVE alone keep working. That entry
// is the only way a namespace without a directory reaches this function at
// run time, so its text names the entry's namespace and the job. They cannot reject a
// journal directory without a namespace either, because a cpi-config entry
// may supply the namespace while the installed job supplies only the
// process-level journal directory, so that text names both places the
// namespace is set.
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
				"pve.storage_placement_namespace, pve.storage_allocation_journal_dir, and an enrolled "+
				"namespace; set both, provision the directory as %s describes, and enroll the namespace as "+
				"%s describes", storageJournalProvisioningDoc, storageJournalEnrollmentDoc)
		}
		return cpierrors.Cloud("pve.storage_allocation_journal_dir is set but no namespace is, and the "+
			"allocation journal needs a namespace that is enrolled before the CPI can create or delete VMs "+
			"and disks; set pve.storage_placement_namespace in the job or pve_storage_placement_namespace "+
			"in the cpi-config entry, and enroll it as %s describes", storageJournalEnrollmentDoc)
	}
	if !hasDirectory {
		return cpierrors.Cloud("pve_storage_placement_namespace %q is set by the cpi-config entry, but the job "+
			"sets no pve.storage_allocation_journal_dir, which only the job can set, and the allocation journal needs "+
			"both and an enrolled namespace before the CPI can create or delete VMs and disks; set "+
			"pve.storage_allocation_journal_dir in the job, provision it as %s describes, and enroll the "+
			"namespace as %s describes, or remove pve_storage_placement_namespace from the cpi-config entry",
			cfg.StoragePlacementNamespace, storageJournalProvisioningDoc, storageJournalEnrollmentDoc)
	}
	if !filepath.IsAbs(directory) {
		return cpierrors.Cloud("pve.storage_placement_namespace is set but pve.storage_allocation_journal_dir "+
			"is not an absolute directory, and the allocation journal needs both and an enrolled namespace "+
			"before the CPI can create or delete VMs and disks; set pve.storage_allocation_journal_dir, "+
			"provision it as %s describes, and enroll the namespace as %s describes",
			storageJournalProvisioningDoc, storageJournalEnrollmentDoc)
	}
	return nil
}

// storageJournalEnrollmentError explains a journal that the configuration
// names completely but that cannot be opened because the directory is
// missing or its namespace was never enrolled. It returns nil when the
// enrollment file is present, and also when it cannot tell, so that the
// journal's own open decides and still refuses. It reads only whether the
// directory and the enrollment files exist, never their content.
//
// A directory that pre-start provisioned on the host but that a BPM worker
// was never given looks the same from inside the worker as a directory that
// was never provisioned, so the missing-directory text names both causes and
// both remedies. Other namespaces enrolled in the same directory are normal
// when each cpi-config entry has its own namespace, but they also appear when
// the namespace was changed, and then enrolling the new one would start a
// fresh authority and leave the old records behind. The text counts them and
// says to set the namespace back if it replaced one of them.
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
		return cpierrors.Cloud("allocation journal directory %s does not exist or this process cannot see it; "+
			"if it was never provisioned, provision it with 'cpi provision-journal' as %s describes, and if "+
			"the Director's workers run under BPM, list it in director.cpi_additional_volumes with "+
			"writable: true and mount_only: true; then enroll namespace %q as %s describes",
			directory, storageJournalProvisioningDoc, namespace, storageJournalEnrollmentDoc)
	}
	others, otherErr := aj.OtherEnrollmentsRecorded(directory, namespace)
	if otherErr == nil && others > 0 {
		return cpierrors.Cloud("namespace %q is not enrolled in the allocation journal at %s, which holds %d "+
			"other enrolled namespace(s), such as other cpi-config entries' namespaces; if this namespace "+
			"replaced one of them, set it back rather than enrolling a new one, and otherwise enroll it with "+
			"'cpi storage-journal audit-enrollment' and then 'cpi storage-journal initialize' as %s describes",
			namespace, directory, others, storageJournalEnrollmentDoc)
	}
	return cpierrors.Cloud("namespace %q is not enrolled in the allocation journal at %s; enroll it with "+
		"'cpi storage-journal audit-enrollment' and then 'cpi storage-journal initialize' as %s describes",
		namespace, directory, storageJournalEnrollmentDoc)
}
