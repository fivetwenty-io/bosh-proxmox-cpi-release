package handlers

import (
	"context"
	"fmt"
	"slices"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The failback flag of a CPI HA resource follows the VM's volumes. create_vm
// writes the HA decision's flag when it stages the resource. attach_disk turns
// failback off before a disk that leaves the VM without it lands, and turns it
// on after a disk that allows it has landed. detach_disk and every call that
// reconciles a touched VM set the flag the decision gives for the volumes the
// VM has now, so a VM that loses its last node-local disk gets failback back.

// vmFailbackWant is what the failback reconcile found for a VM.
type vmFailbackWant struct {
	// decided is false when the facts did not settle the flag, and the
	// reconcile then writes nothing.
	decided bool
	want    bool
	reason  string
}

// reconcileVMFailback sets the failback flag of the HA resource of VM vmid to
// what its volumes and policy call for, and writes only when the flag reads
// otherwise. It runs for a VM with the HA-managed or the replicate tag, in mode
// auto or managed, outside a managed allocation guard, once the profile lets
// the CPI write HA state. A resource
// that is missing or not this Director's is left alone. Failback is off when
// pve.ha.failback is false, when the VM replicates, and when the HA decision
// over the VM's volumes gives it a policy other than shared or no HA at all;
// it is on when the decision registers the VM under the shared policy. A
// decision that refuses HA for a reason that may pass leaves the flag as it is.
//
// Every failure is logged and never fails the call. The flag is never a safety
// fault the reconcile alone repairs, because create_vm writes failback off for
// every VM that may not have it, and attach_disk turns it off before such a
// disk lands or fails the attach.
func reconcileVMFailback(ctx context.Context, deps Deps, op string, vmid int, loc pve.VMLocation,
	logger *log.Logger,
) {
	if deps.Config == nil || deps.PVE == nil || deps.Config.HAModeValue() == config.HAModeOff ||
		loc.Node == "" || deps.RequestDirectorUUID == "" {
		return
	}
	if !hasReplicaTag(loc.Tags) && !slices.Contains(parseTagsField(loc.Tags), haManagedTag) {
		return
	}
	if _, wrapped := deps.PVE.(guardWrappedClient); wrapped {
		// A managed allocation guard admits only the writes of its own
		// lifecycle, and the next call outside one sets the flag.
		return
	}
	logger = logger.With(log.Int(metadataKeyVMID, vmid), log.String("node", loc.Node))
	w, err := wantVMFailback(ctx, deps, vmid, loc)
	if err != nil {
		logger.Warn(op+": the failback flag of the VM's HA resource could not be checked; a later call checks "+
			"again", log.Err(err))
		return
	}
	if !w.decided {
		logger.Debug(op+": the failback flag of the VM's HA resource stays as it is", log.String("reason", w.reason))
		return
	}
	changed, err := setVMFailback(ctx, deps.PVE.Cluster(), deps.RequestDirectorUUID, vmid, w.want)
	switch {
	case err != nil:
		logger.Warn(op+": the failback flag of the VM's HA resource could not be set; a later call sets it",
			log.Bool("failback", w.want), log.String("reason", w.reason), log.Err(err))
	case changed:
		logger.Info(op+": set the failback flag of the VM's HA resource", log.Bool("failback", w.want),
			log.String("reason", w.reason))
	}
}

// setVMFailback sets the failback flag of the HA resource of VM vmid that
// Director owner owns to want when it reads otherwise, through api (see
// ha.ReconcileFailback).
func setVMFailback(ctx context.Context, api ha.FailbackAPI, owner string, vmid int, want bool) (bool, error) {
	return ha.ReconcileFailback(ctx, api, haResourceSid(vmid), want, func(comment string) bool {
		return owner != "" && haOwnershipOf(comment, owner) == haOwnedByUs
	})
}

// wantVMFailback finds the failback flag VM vmid's HA resource should carry.
func wantVMFailback(ctx context.Context, deps Deps, vmid int, loc pve.VMLocation) (vmFailbackWant, error) {
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return vmFailbackWant{}, fmt.Errorf("resolve the HA profile: %w", err)
	}
	if !profile.WritesAllowed() {
		return vmFailbackWant{reason: "the HA profile does not let the CPI write HA state"}, nil
	}
	off := func(reason string) (vmFailbackWant, error) {
		return vmFailbackWant{decided: true, reason: reason}, nil
	}
	if !deps.Config.HAFailbackEnabled() {
		return off("pve.ha.failback is false")
	}
	if hasReplicaTag(loc.Tags) {
		return off("the VM replicates its volumes to a partner node")
	}
	svc := deps.PVE.Cluster()
	res, err := svc.GetHaResources(ctx, haResourceSid(vmid))
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return vmFailbackWant{reason: "the VM has no HA resource"}, nil
	case err != nil:
		return vmFailbackWant{}, fmt.Errorf("read the HA resource: %w", pve.WrapError(err))
	case res == nil:
		return vmFailbackWant{}, cpierrors.Retriable("the read of the HA resource of VM %d returned nothing", vmid)
	case res.Comment == nil || haOwnershipOf(*res.Comment, deps.RequestDirectorUUID) != haOwnedByUs:
		return vmFailbackWant{reason: "the VM's HA resource is not this Director's"}, nil
	}
	set, err := readHARuleSet(ctx, svc)
	if err != nil {
		return vmFailbackWant{}, fmt.Errorf("read the HA rules: %w", err)
	}
	pin := set.Find(naRuleNameFor(vmid))
	if pin != nil && replicaPinComment(pin.Comment) {
		return off("the VM's pin carries a token of the replicate policy")
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, loc.Node, vmid)
	if err != nil {
		return vmFailbackWant{}, fmt.Errorf("read the VM config: %w", pve.WrapError(err))
	}
	if configHasReplicaTag(cfg) {
		return off("the VM replicates its volumes to a partner node")
	}
	in := pinWidenInput(deps, vmid, loc.Node, cfg, res)
	if pin != nil && pin.Strict && !pin.Disabled && haOwnershipOf(pin.Comment, deps.RequestDirectorUUID) == haOwnedByUs {
		in.Constraints = append(in.Constraints, ownPinConstraint(vmid, pin.NodeNames()))
	}
	d, err := profile.DecideVM(in)
	if err != nil {
		return vmFailbackWant{}, fmt.Errorf("decide HA for the VM: %w", err)
	}
	switch {
	case d.Registers():
		return vmFailbackWant{decided: true, want: d.Failback, reason: d.Reason}, nil
	case d.Transient || d.PlaceableShortfall:
		return vmFailbackWant{reason: "the HA decision refused HA for a reason that may pass: " + d.Reason}, nil
	}
	return off("the HA decision gives the VM no HA: " + d.Reason)
}
