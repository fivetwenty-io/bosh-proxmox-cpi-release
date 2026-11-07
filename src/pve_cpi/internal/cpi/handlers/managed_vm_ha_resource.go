package handlers

import (
	"context"
	"errors"
	"fmt"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

const (
	managedVMCallDeleteHAResource = "Cluster.DeleteHaResources"
	managedVMCallUpdateHAResource = "Cluster.UpdateHaResources"
)

// errManagedHAResourceDeleteRefused marks an HA resource removal that a VM
// allocation's guard refused before it wrote any intent or called PVE. The
// refusal leaves nothing uncertain, so begin hands it back as not attempted
// and the guard stays usable for the calls that follow.
var errManagedHAResourceDeleteRefused = errors.New("managed VM allocation refused the HA resource removal")

// admitHAResourceDelete lets the removal of an HA resource through a VM
// allocation's guard only when it undoes this call's own staging. The resource
// must be the allocated VM's, this call must have created it, and the resource
// as read now must carry this Director's ownership marker, the same check the
// guard makes for an HA rule edit (see admitManagedVMHARuleEdit). A resource
// that is already gone passes, because its removal changes nothing.
//
// Every refusal wraps errManagedHAResourceDeleteRefused, and a read that fails
// wraps errManagedAdmissionReadFailed, so neither locks the guard.
func (m *managedVMAllocation) admitHAResourceDelete(ctx context.Context, call ManagedAllocationMutation) error {
	sid, _ := call.Args["sid"].(string)
	want := haResourceSid(m.vmid)
	refuse := func(format string, args ...any) error {
		return cpierrors.WrapAs(fmt.Errorf("%w: %s", errManagedHAResourceDeleteRefused, fmt.Sprintf(format, args...)),
			cpierrors.TypeCloud, "managed VM allocation: "+call.Service+"."+call.Method+" was not sent")
	}
	switch {
	case sid != want:
		return refuse("HA resource %q is not the allocated VM's resource %s", sid, want)
	case !m.haResourceStaged:
		return refuse("HA resource %s was not staged by this call", sid)
	case m.deps.RequestDirectorUUID == "":
		return refuse("HA resource %s removal lacks a Director to own it", sid)
	}
	got, err := m.deps.PVE.Cluster().GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return nil
	case err != nil:
		return cpierrors.WrapAs(errors.Join(fmt.Errorf("cannot read the owner of %s: %v", sid, err), //nolint:errorlint // The read's cause stays as text, so its status can't read as PVE refusing the removal.
			errManagedAdmissionReadFailed), cpierrors.TypeRetriableCloud,
			"managed VM allocation could not check "+call.Service+"."+call.Method+" before sending it, so nothing was sent")
	case got == nil:
		return cpierrors.WrapAs(errors.Join(fmt.Errorf("the read of %s returned nothing", sid),
			errManagedAdmissionReadFailed), cpierrors.TypeRetriableCloud,
			"managed VM allocation could not check "+call.Service+"."+call.Method+" before sending it, so nothing was sent")
	case got.Comment == nil || haOwnershipOf(*got.Comment, m.deps.RequestDirectorUUID) != haOwnedByUs:
		return refuse("HA resource %s does not carry this Director's ownership marker", sid)
	}
	return nil
}

// noteStagedHAResource records that this call created the allocated VM's HA
// resource, as requested ignored, so the guard can admit the removal that
// undoes the staging (see admitHAResourceDelete). It runs once the creation's
// readback has been proved.
func (m *managedVMAllocation) noteStagedHAResource(call ManagedAllocationMutation) {
	params, ok := call.Args[managedArgumentParams].(*cluster.CreateHaResourcesParams)
	if !ok || params == nil || params.Sid != haResourceSid(m.vmid) {
		return
	}
	if params.State == nil || *params.State != "ignored" {
		return
	}
	m.haResourceStaged = true
}

// noteHAResourceActivation ends the staged admission once this call sends a
// request that moves the allocated VM's HA resource out of ignored, such as
// the started request that activates it. From then on the resource is no
// longer only staged, so admitHAResourceDelete refuses its removal. An update
// of another resource, one that keeps the resource ignored, and one that sets
// no state leave the admission as it was.
func (m *managedVMAllocation) noteHAResourceActivation(call ManagedAllocationMutation) {
	if call.Service+"."+call.Method != managedVMCallUpdateHAResource {
		return
	}
	sid, _ := call.Args["sid"].(string)
	params, ok := call.Args[managedArgumentParams].(*cluster.UpdateHaResourcesParams)
	if sid != haResourceSid(m.vmid) || !ok || params == nil || params.State == nil || *params.State == "ignored" {
		return
	}
	m.haResourceStaged = false
}

// observeDeletedHAResource proves that the allocated VM's HA resource is gone
// after its removal. Only a read that reports the resource missing proves it.
func (m *managedVMAllocation) observeDeletedHAResource(ctx context.Context, call ManagedAllocationMutation) error {
	sid, _ := call.Args["sid"].(string)
	if sid != haResourceSid(m.vmid) {
		return fmt.Errorf("HA resource differs from planned VM")
	}
	_, err := m.deps.PVE.Cluster().GetHaResources(ctx, sid)
	switch {
	case err == nil:
		return fmt.Errorf("HA resource %s still present after its removal", sid)
	case isHaNotFound(err) || pve.IsNotFound(err):
		return nil
	default:
		return err
	}
}

// settleFailedWrite is the guard's SettleFailedWrite hook. An HA resource
// removal that PVE answered with not-found is settled when a fresh read proves
// the resource gone, because a removal of nothing changed nothing. Every other
// failed write goes to settleFailedConfigDelete.
func (m *managedVMAllocation) settleFailedWrite(
	ctx context.Context, call ManagedAllocationMutation, step string, err error,
) bool {
	if call.Service+"."+call.Method != managedVMCallDeleteHAResource {
		return m.settleFailedConfigDelete(ctx, call, step, err)
	}
	if !isHaNotFound(err) && !pve.IsNotFound(err) {
		return false
	}
	if m.observeDeletedHAResource(ctx, call) != nil {
		return false
	}
	return storageMutationObserved(m.handle, step, nil, false) == nil
}
