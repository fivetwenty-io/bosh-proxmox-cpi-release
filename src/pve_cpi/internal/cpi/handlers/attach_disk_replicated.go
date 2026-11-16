package handlers

import (
	"context"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A VM that PVE replicates takes a disk by reassignment only when the disk's
// storage can replicate. qemu-server's move_disk reassign check refuses any
// other volume once the target guest has a replication job, whatever that
// job's state ("Cannot move disk to a replicated VM. Storage does not support
// replication!"), and only a ZFS pool replicates. A config edit of a volume on
// shared storage is allowed, because PVE's update check exempts shared
// storage, and replication never copies a shared volume. The parked disk
// keeps its own name on that path, which is safe only when the name is not
// the parker's: a volume named for its parker belongs to the parker in PVE's
// eyes, and the parker's removal would free it.

// storageCanReplicate mirrors PVE's storage_can_replicate. Only the ZFS pool
// plugin answers yes, for raw and subvol volumes, and a ZFS pool holds no
// other format. Every other plugin answers no.
func storageCanReplicate(info pve.StorageInfo) bool {
	return strings.EqualFold(info.Type, pve.StorageTypeZFSPool)
}

// replicaReassignVerdict decides how a stable-ID disk that holder keeps
// enters targetVMID when the plan was a reassignment. jobs names the VM's
// replication jobs, such as "the replication job 100-0", and is empty when the
// VM has none. It returns true when the config-edit attach takes the disk
// instead, which is the case for a volume on shared storage that is named for
// neither the parker nor a mover. Every other disk gets a permanent refusal,
// because PVE refuses the reassignment for as long as the job exists and no
// config edit is safe. A storage that can replicate, or a VM with no job,
// keeps the reassignment.
func replicaReassignVerdict(
	op string, rd resolvedDisk, holder pve.DiskHolder, info pve.StorageInfo, targetVMID int, jobs string,
) (bool, error) {
	if jobs == "" || storageCanReplicate(info) {
		return false, nil
	}
	kind := info.Type
	if kind == "" {
		kind = "non-ZFS"
	}
	why := fmt.Sprintf("VM %d has %s, and PVE refuses to move a volume on %s storage %s into a replicated VM "+
		"because only a ZFS pool replicates", targetVMID, jobs, kind, info.Name)
	remedy := "Recreate the VM (bosh recreate): create_vm attaches the persistent disks it is given before it sets " +
		"up the replica, so the disk enters the VM before the job exists"
	if !info.IsShared() {
		return false, cpierrors.Cloud("%s: disk %s cannot enter VM %d: %s, and PVE refuses a node-local volume that "+
			"cannot replicate in a config edit too, unless the drive is excluded from replication, which the CPI "+
			"does not do, because the replica would then not cover the disk. %s, or keep persistent disks on "+
			"shared storage or on a ZFS pool", op, rd.diskCID, targetVMID, why, remedy)
	}
	embedded, named := pve.EmbeddedDiskVMID(rd.volid)
	if (named && embedded == holder.VMID) || pve.TagsMarkDiskMover(holder.Tags) {
		return false, cpierrors.Cloud("%s: disk %s cannot enter VM %d: %s, and the volume %q is named for parker VM "+
			"%d, so a config edit would leave it owned by the parker, whose removal would free it. %s", op,
			rd.diskCID, targetVMID, why, rd.volid, holder.VMID, remedy)
	}
	return true, nil
}

// describeReplicaJobs names the replication jobs ids for a refusal, and is
// empty when there are none.
func describeReplicaJobs(ids []string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return "the replication job " + ids[0]
	}
	return "the replication jobs " + strings.Join(ids, ", ")
}

// replicaJobIDs returns the IDs of jobs.
func replicaJobIDs(jobs []ha.ReplicaJobState) []string {
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	return ids
}

// diskStorageInfo returns the storage definition of volid from the backend
// resolver, and false when the resolver does not know the storage's type. A
// lookup that fails returns its error, which is retriable when the storage
// index could not be read, so a caller never takes a failed read for a
// storage of unknown type.
func diskStorageInfo(ctx context.Context, deps Deps, volid string) (pve.StorageInfo, bool, error) {
	storage, _, err := pve.ParseDiskCID(volid)
	if err != nil {
		return pve.StorageInfo{}, false, nil
	}
	backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
	if err != nil {
		return pve.StorageInfo{}, false, err
	}
	info, ok := pve.BackendStorageInfo(backend)
	if !ok || info.Type == "" {
		return pve.StorageInfo{}, false, nil
	}
	if info.Name == "" {
		info.Name = storage
	}
	return info, true, nil
}

// replicaJobsLeaving reports whether every job of jobs is marked for removal,
// which PVE's runner carries out on its next run. PVE still refuses a volume
// that cannot replicate while such a job is listed, and the refusal passes
// once the runner has removed it, so a refusal that rests on it alone is
// retriable.
func replicaJobsLeaving(jobs []ha.ReplicaJobState) bool {
	if len(jobs) == 0 {
		return false
	}
	for _, j := range jobs {
		if j.RemoveJob == "" {
			return false
		}
	}
	return true
}

// errReplicaJobsLeaving is the retriable refusal of a disk that cannot enter
// targetVMID while the VM's jobs, all marked for removal, are still listed.
func errReplicaJobsLeaving(op, diskCID string, targetVMID int, jobs string) error {
	return cpierrors.Retriable("%s: disk %s cannot enter VM %d while %s, which PVE has marked for removal, is "+
		"still configured; the attach goes on once pvesr has removed it", op, diskCID, targetVMID, jobs)
}

// replicatedTargetRoute checks, before a reassignment from holder into
// targetVMID, whether PVE would refuse it because the VM is replicated, and
// returns true when the config-edit attach takes the disk instead. It looks up
// the VM's replication jobs only for a disk whose storage is known not to
// replicate. A storage the resolver cannot classify, or a job list that
// cannot be read, keeps the reassignment, and PVE's own refusal then reaches
// the fallback in attachDiskViaTransfer, which reaches the same answer. A
// storage lookup that fails is retriable, and so is a VM whose only jobs are
// marked for removal.
func replicatedTargetRoute(
	ctx context.Context, deps Deps, op string, rd resolvedDisk, holder pve.DiskHolder, targetVMID int,
) (bool, error) {
	info, known, err := diskStorageInfo(ctx, deps, rd.volid)
	if err != nil {
		return false, retriableUnlessPermanent(err, fmt.Sprintf("%s: classify the storage of disk %s before it "+
			"enters VM %d", op, rd.diskCID, targetVMID))
	}
	if !known || storageCanReplicate(info) {
		return false, nil
	}
	all, err := replicationAPIFor(deps).ListJobs(ctx)
	var jobs []ha.ReplicaJobState
	if err == nil {
		jobs, err = ha.GuestJobs(all, targetVMID)
	}
	if err != nil {
		deps.Log(ctx).Warn(op+": the replication jobs could not be read before the reassignment; PVE's own answer "+
			"decides whether the VM takes the disk that way",
			log.Int(metadataKeyVMID, targetVMID), log.String("disk_cid", rd.diskCID), log.Err(err))
		return false, nil
	}
	described := describeReplicaJobs(replicaJobIDs(jobs))
	configEdit, err := replicaReassignVerdict(op, rd, holder, info, targetVMID, described)
	if err != nil && replicaJobsLeaving(jobs) {
		return false, errReplicaJobsLeaving(op, rd.diskCID, targetVMID, described)
	}
	if configEdit {
		deps.Log(ctx).Warn(op+": the VM is replicated and the disk's storage cannot replicate, so PVE refuses the "+
			"reassignment; using the config-edit attach, which keeps the volume's name (named for neither the "+
			"parker nor the VM, on shared storage, safe)",
			log.Int(metadataKeyVMID, targetVMID), log.String("disk_cid", rd.diskCID),
			log.Int("parker_vmid", holder.VMID), log.String("volid", rd.volid))
	}
	return configEdit, err
}

// replicaRefusalVerdict answers PVE's refusal to reassign the disk into a
// replicated VM, which proves that the VM had a job and that the disk's
// storage cannot replicate. It returns nil when the config-edit attach may
// take the disk, and the permanent refusal otherwise. A storage the resolver
// cannot classify counts as shared only when its backend is not node-local.
// A storage lookup that fails is retriable. So is a refusal that a retry
// would not meet, because the VM's jobs are gone since or are all marked for
// removal, and a refusal whose job list cannot be read to tell.
func replicaRefusalVerdict(
	ctx context.Context, deps Deps, op string, rd resolvedDisk, volid string, holder pve.DiskHolder, targetVMID int,
	refusal error,
) error {
	rd.volid = volid
	storage, _, _ := pve.ParseDiskCID(volid)
	unclassified := func(err error) error {
		return retriableUnlessPermanent(err, fmt.Sprintf("%s: PVE refused to move disk %s into replicated VM %d "+
			"(%v), and the disk's storage could not be classified", op, rd.diskCID, targetVMID, refusal))
	}
	info, known, err := diskStorageInfo(ctx, deps, volid)
	if err != nil {
		return unclassified(err)
	}
	if !known {
		local, err := diskBackendIsLocal(ctx, deps, op, volid)
		if err != nil {
			return unclassified(err)
		}
		info = pve.StorageInfo{Name: storage, Shared: !local}
	}
	info.Type = strings.TrimSpace(info.Type)
	if storageCanReplicate(info) {
		// PVE's answer outranks the storage index, which may be stale.
		info.Type = ""
	}
	_, verdict := replicaReassignVerdict(op, rd, holder, info, targetVMID, "a replication job")
	if verdict == nil {
		return nil
	}
	all, err := replicationAPIFor(deps).ListJobs(ctx)
	var jobs []ha.ReplicaJobState
	if err == nil {
		jobs, err = ha.GuestJobs(all, targetVMID)
	}
	switch {
	case err != nil:
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf("%s: PVE refused "+
			"to move disk %s into replicated VM %d (%v), and the VM's replication jobs could not be read to tell "+
			"whether the refusal still holds", op, rd.diskCID, targetVMID, refusal))
	case len(jobs) == 0:
		return cpierrors.Retriable("%s: PVE refused to move disk %s into VM %d for a replication job (%v), and the "+
			"VM has no replication job now; the retry moves it", op, rd.diskCID, targetVMID, refusal)
	case replicaJobsLeaving(jobs):
		return errReplicaJobsLeaving(op, rd.diskCID, targetVMID, describeReplicaJobs(replicaJobIDs(jobs)))
	}
	return cpierrors.Wrap(refusal, verdict.Error())
}

// replicatedLocalAddText is the refusal of a disk on node-local storage that
// cannot replicate, which the CPI does not attach to a VM that has a
// replication job. PVE's move_disk refuses the reassignment, and update_vm
// refuses the config edit ("cannot add non-replicatable volume to a
// replicated VM"), for as long as the job exists, unless the drive is marked
// replicate=0. The CPI never sets that flag, because the replica would then
// not cover the disk, and a failover would start the VM without it. jobs
// names the VM's jobs, and is empty when only PVE's answer proves one exists.
// storage names the disk's storage.
func replicatedLocalAddText(op, diskCID, vmCID, jobs, storage string) string {
	if jobs == "" {
		jobs = "a replication job"
	}
	return fmt.Sprintf("%s: disk %s cannot enter VM %s: the VM has %s, and the disk is on node-local storage %s, "+
		"which cannot replicate, because only a ZFS pool replicates. PVE refuses to move such a volume into a "+
		"replicated VM, and refuses it in a config edit unless the drive is excluded from replication, which the "+
		"CPI does not do, because the replica would then not cover the disk. Keep the persistent disks of a VM under pve.ha.local_policy replicate on shared "+
		"storage or on a ZFS pool its replica partner also has, or remove the VM's replication job if it is not "+
		"this Director's", op, diskCID, vmCID, jobs, storage)
}

// replicatedLocalAddRefusal is replicatedLocalAddText as a permanent Cloud
// error.
func replicatedLocalAddRefusal(op, diskCID, vmCID, jobs string, info pve.StorageInfo) error {
	return cpierrors.Cloud("%s", replicatedLocalAddText(op, diskCID, vmCID, jobs, info.Name))
}

// profileReplicaJobs returns the IDs of the replication jobs the profile read
// for guest, and false when the profile could not read them.
func profileReplicaJobs(profile *ha.Profile, guest int) ([]string, bool) {
	if profile == nil || profile.ReplicationErr != "" {
		return nil, false
	}
	var ids []string
	for _, j := range profile.Replication {
		if int(j.Guest) == guest {
			ids = append(ids, j.ID)
		}
	}
	return ids, true
}

// profileStorage returns the definition of storage the profile read, and
// false when the profile holds none for it.
func profileStorage(profile *ha.Profile, storage string) (pve.StorageInfo, bool) {
	if profile == nil {
		return pve.StorageInfo{}, false
	}
	for i := range profile.Storages {
		if s := profile.Storages[i]; s.Name == storage && s.Type != "" {
			return s, true
		}
	}
	return pve.StorageInfo{}, false
}
