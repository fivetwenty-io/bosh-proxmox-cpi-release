package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// errManagedHAWriteRefused marks an HA rule or resource write that a disk
// lifecycle's guard refused before it called PVE, because the write is not
// this Director's. Nothing was sent and no step was recorded, so begin hands
// the refusal back as not attempted and the guard stays usable.
var errManagedHAWriteRefused = errors.New("managed disk lifecycle refused the HA write")

// isLifecycleHAWrite reports whether a lifecycle mutation is a write of a PVE
// HA rule or resource.
func isLifecycleHAWrite(call ManagedAllocationMutation) bool {
	if call.Service != managedServiceCluster {
		return false
	}
	switch call.Method {
	case "CreateHaRules", "UpdateHaRules", "DeleteHaRules", "UpdateHaResources", "DeleteHaResources":
		return true
	}
	return false
}

// admitLifecycleHAWrite judges an HA rule or resource write made inside a
// disk's lifecycle, such as the pin an attach writes after the disk lands, or
// the requested state that holds a VM in place while its pin narrows. Such a
// write names a rule or a resource and no VM, so it gets none of the holder
// checks a write to a VM gets. It names no volume either, and its writer reads
// its outcome back, so it is admitted without a journal step (see
// managedUnjournaledStep), and a failed write leaves the disk's record as it
// was.
//
// A rule write passes only when the rule is a CPI rule of this Director: a
// create must name a bosh-na- or bosh-aa- rule that is absent and carry this
// Director's marker, and an update or delete is judged the way a VM
// allocation judges it (see admitOwnedHARuleEdit). A resource write passes
// only for a VM resource that carries this Director's marker now. An update
// may set the requested state and the comment, and a comment it writes must
// keep the marker. The removal of a resource that is already gone changes
// nothing and passes. A disk lifecycle never creates an HA resource, because
// a registration also rewrites the VM's tags and description (see
// errAttachReaddGuarded).
//
// A refusal wraps errManagedHAWriteRefused and a read that fails wraps
// errManagedAdmissionReadFailed, so neither locks the guard.
func (g *managedDiskLifecycleGuard) admitLifecycleHAWrite(ctx context.Context, call ManagedAllocationMutation) (string, error) {
	m := g.lifecycle
	refuse := func(err error) (string, error) {
		return "", cpierrors.WrapAs(errors.Join(err, errManagedHAWriteRefused), cpierrors.TypeCloud,
			"managed disk lifecycle: "+call.Service+"."+call.Method+" was not sent")
	}
	readFailed := func(what string) func(error) error {
		return func(err error) error { return g.admissionReadFailed(ctx, call, what, err) }
	}
	owner := m.deps.RequestDirectorUUID
	if owner == "" {
		return refuse(errors.New("the HA write lacks a Director to own it"))
	}
	switch call.Method {
	case "CreateHaRules":
		params, ok := call.Args[managedArgumentParams].(*cluster.CreateHaRulesParams)
		if !ok || params == nil {
			return refuse(errors.New("HA rule creation lacks its parameters"))
		}
		if !strings.HasPrefix(params.Rule, naRuleNamePrefix) && !strings.HasPrefix(params.Rule, haRuleNamePrefix) {
			return refuse(fmt.Errorf("HA rule %q is not a CPI rule", params.Rule))
		}
		if params.Comment == nil || haOwnershipOf(*params.Comment, owner) != haOwnedByUs {
			return refuse(fmt.Errorf("HA rule %q creation does not carry this Director's ownership marker",
				params.Rule))
		}
		existing, err := readManagedVMHARule(ctx, m.deps, params.Rule)
		if err != nil {
			return "", readFailed("the HA rules")(err)
		}
		if existing != nil {
			return refuse(fmt.Errorf("HA rule %q was not absent before its creation", params.Rule))
		}
	case "UpdateHaRules", "DeleteHaRules":
		if err := admitOwnedHARuleEdit(ctx, m.deps, call, readFailed("the HA rules")); err != nil {
			if errors.Is(err, errManagedAdmissionReadFailed) || errors.Is(err, errManagedRequestEnded) {
				return "", err
			}
			return refuse(err)
		}
	case "UpdateHaResources", "DeleteHaResources":
		if err := g.admitLifecycleHAResourceWrite(ctx, call, owner); err != nil {
			if errors.Is(err, errManagedAdmissionReadFailed) || errors.Is(err, errManagedRequestEnded) {
				return "", err
			}
			return refuse(err)
		}
	default:
		return refuse(fmt.Errorf("unexpected HA write %s", call.Method))
	}
	return managedUnjournaledStep, nil
}

// admitLifecycleHAResourceWrite checks an HA resource update or removal
// against the resource as PVE holds it now (see admitLifecycleHAWrite).
func (g *managedDiskLifecycleGuard) admitLifecycleHAResourceWrite(
	ctx context.Context, call ManagedAllocationMutation, owner string,
) error {
	sid, _ := call.Args["sid"].(string)
	if !managedHAResourceSID(sid) || !strings.HasPrefix(sid, "vm:") {
		return fmt.Errorf("HA resource %q is not a VM resource", sid)
	}
	update := call.Method == "UpdateHaResources"
	if update {
		params, ok := call.Args[managedArgumentParams].(*cluster.UpdateHaResourcesParams)
		if !ok || params == nil {
			return fmt.Errorf("HA resource %s update lacks its parameters", sid)
		}
		if params.Group != nil || params.MaxRelocate != nil || params.MaxRestart != nil ||
			params.Failback != nil || params.AutoRebalance != nil {
			return fmt.Errorf("HA resource %s update sets more than its requested state and comment", sid)
		}
		if params.Delete != nil && slices.Contains(strings.Split(*params.Delete, ","), "comment") {
			return fmt.Errorf("HA resource %s update removes its ownership marker", sid)
		}
		if params.Comment != nil && haOwnershipOf(*params.Comment, owner) != haOwnedByUs {
			return fmt.Errorf("HA resource %s update does not carry this Director's ownership marker", sid)
		}
	}
	got, err := g.lifecycle.deps.PVE.Cluster().GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		if update {
			return fmt.Errorf("HA resource %s was absent before its update", sid)
		}
		return nil
	case err != nil:
		return g.admissionReadFailed(ctx, call, "the owner of HA resource "+sid, err)
	case got == nil:
		return g.admissionReadFailed(ctx, call, "the owner of HA resource "+sid,
			errors.New("the read returned nothing"))
	case got.Comment == nil || haOwnershipOf(*got.Comment, owner) != haOwnedByUs:
		return fmt.Errorf("HA resource %s does not carry this Director's ownership marker", sid)
	}
	return nil
}
