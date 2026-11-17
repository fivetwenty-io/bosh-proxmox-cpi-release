package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
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
	case "CreateHaRules", "UpdateHaRules", "DeleteHaRules", "UpdateHaResources", "DeleteHaResources",
		"CreateHaResourcesMigrate":
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
// managedUnjournaledStep). A failed write leaves the guard usable. The
// operation it belongs to still fails, so the lifecycle's finish marks the
// disk's record for reconciliation as it does for any failed operation, and
// the next call that resolves the disk readmits it.
//
// The lifecycle serves one VM, the one an attach names (see haVMID), and a
// lifecycle that serves none refuses every HA write. A rule write passes only
// for that VM's pin, bosh-na-<vmid>, as a CPI rule of this Director: a create
// must name only the VM's resource, be absent, and carry this Director's
// marker, an update that sets the resources must name only the VM's resource,
// and an update or delete is judged the way a VM allocation judges it (see
// admitOwnedHARuleEdit). A resource write passes only for the VM's resource,
// and only when it carries this Director's marker now. An update may set the
// requested state, the comment, and the digest, and nothing else. The state
// must be started, stopped, or ignored, the states the barrier requests and
// restores. It may be disabled only when it is the barrier's restore of a
// resource that was disabled before the barrier held it. The resource must be
// ignored now and carry the barrier's token naming disabled, and the update
// must write a comment that drops the token. Any other request for disabled is
// refused, so the lifecycle never disables a VM. The update may delete no setting,
// and its delete list is read the way PVE reads it (see splitPVEList). A
// comment it writes must keep the marker. A removal must purge the resource
// from its rules, the only removal the CPI sends, and the removal of a
// resource that is already gone changes nothing and passes. A migrate request
// passes only for the VM's resource with a target node, and only when the
// resource carries this Director's marker now and requests started or stopped,
// which is the move an attach asks PVE HA for to bring a VM that drifted back
// into its node set. A disk lifecycle never creates an HA resource, because a registration also rewrites the VM's
// tags and description (see errAttachReaddGuarded).
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
	if m.haVMID <= 0 {
		return refuse(errors.New("the disk's lifecycle serves no VM whose HA it may change"))
	}
	sid, pin := haResourceSid(m.haVMID), naRuleNameFor(m.haVMID)
	switch call.Method {
	case "CreateHaRules":
		params, ok := call.Args[managedArgumentParams].(*cluster.CreateHaRulesParams)
		if !ok || params == nil {
			return refuse(errors.New("HA rule creation lacks its parameters"))
		}
		if params.Rule != pin {
			return refuse(fmt.Errorf("HA rule %q is not the pin %s of VM %d", params.Rule, pin, m.haVMID))
		}
		if !slices.Equal(splitPVEList(params.Resources), []string{sid}) {
			return refuse(fmt.Errorf("HA rule %q creation names resources %q, not only %s", params.Rule,
				params.Resources, sid))
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
		if err := lifecycleHARuleEditTargets(call, pin, sid); err != nil {
			return refuse(err)
		}
		if err := admitOwnedHARuleEdit(ctx, m.deps, call, readFailed("the HA rules")); err != nil {
			if errors.Is(err, errManagedAdmissionReadFailed) || errors.Is(err, errManagedRequestEnded) {
				return "", err
			}
			return refuse(err)
		}
	case "CreateHaResourcesMigrate":
		if err := g.admitLifecycleHAMigrate(ctx, call, sid, owner); err != nil {
			if errors.Is(err, errManagedAdmissionReadFailed) || errors.Is(err, errManagedRequestEnded) {
				return "", err
			}
			return refuse(err)
		}
	case "UpdateHaResources", "DeleteHaResources":
		if err := g.admitLifecycleHAResourceWrite(ctx, call, sid, owner); err != nil {
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

// admitLifecycleHAMigrate checks an HA migrate request inside a disk
// lifecycle against want, the resource of the lifecycle's VM, and against the
// resource as PVE holds it now (see admitLifecycleHAWrite).
func (g *managedDiskLifecycleGuard) admitLifecycleHAMigrate(
	ctx context.Context, call ManagedAllocationMutation, want, owner string,
) error {
	sid, _ := call.Args["sid"].(string)
	if sid != want {
		return fmt.Errorf("HA resource %q is not the resource %s of the lifecycle's VM", sid, want)
	}
	params, ok := call.Args[managedArgumentParams].(*cluster.CreateHaResourcesMigrateParams)
	if !ok || params == nil || strings.TrimSpace(params.Node) == "" {
		return fmt.Errorf("HA resource %s migrate request names no target node", sid)
	}
	got, err := g.lifecycle.deps.PVE.Cluster().GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return fmt.Errorf("HA resource %s was absent before its migrate request", sid)
	case err != nil:
		return g.admissionReadFailed(ctx, call, "the owner of HA resource "+sid, err)
	case got == nil:
		return g.admissionReadFailed(ctx, call, "the owner of HA resource "+sid,
			errors.New("the read returned nothing"))
	case got.Comment == nil || haOwnershipOf(*got.Comment, owner) != haOwnedByUs:
		return fmt.Errorf("HA resource %s does not carry this Director's ownership marker", sid)
	}
	if state := pve.HARequestedState(got.State); state != "started" && state != "stopped" {
		return fmt.Errorf("HA resource %s requests %s, and a disk lifecycle migrates only a resource that "+
			"requests started or stopped", sid, state)
	}
	return nil
}

// lifecycleHAStates lists the requested states a disk lifecycle may set on
// its VM's HA resource: ignored while the barrier holds the VM, and the
// started or stopped state the barrier puts back.
var lifecycleHAStates = []string{"started", "stopped", "ignored"}

// haStateDisabled is the requested state that stops a VM under PVE HA and keeps
// it stopped. A disk lifecycle requests it only as the barrier's restore of a
// resource that was disabled before.
const haStateDisabled = "disabled"

// lifecycleHAResourceUpdateFields lists the fields an HA resource update
// inside a disk lifecycle may carry.
var lifecycleHAResourceUpdateFields = []string{"state", "comment", pveConfigKeyDigest, "delete"}

// lifecycleHARuleEditTargets checks that an HA rule update or delete inside a
// disk lifecycle names the pin of the lifecycle's VM, and that an update that
// sets the rule's resources names only the VM's resource.
func lifecycleHARuleEditTargets(call ManagedAllocationMutation, pin, sid string) error {
	rule, _ := call.Args["rule"].(string)
	if rule != pin {
		return fmt.Errorf("HA rule %q is not the pin %s", rule, pin)
	}
	if call.Method != "UpdateHaRules" {
		return nil
	}
	params, ok := call.Args[managedArgumentParams].(*cluster.UpdateHaRulesParams)
	if ok && params != nil && params.Resources != nil && !slices.Equal(splitPVEList(*params.Resources), []string{sid}) {
		return fmt.Errorf("HA rule %q update names resources %q, not only %s", rule, *params.Resources, sid)
	}
	return nil
}

// admitLifecycleHAResourceWrite checks an HA resource update or removal
// against want, the resource of the lifecycle's VM, and against the resource
// as PVE holds it now (see admitLifecycleHAWrite).
func (g *managedDiskLifecycleGuard) admitLifecycleHAResourceWrite(
	ctx context.Context, call ManagedAllocationMutation, want, owner string,
) error {
	sid, _ := call.Args["sid"].(string)
	if sid != want {
		return fmt.Errorf("HA resource %q is not the resource %s of the lifecycle's VM", sid, want)
	}
	update := call.Method == "UpdateHaResources"
	restoresDisabled := false
	if update {
		var err error
		if restoresDisabled, err = lifecycleHAResourceUpdateAllowed(call, sid, owner); err != nil {
			return err
		}
	} else if err := lifecycleHAResourceRemovalAllowed(call, sid); err != nil {
		return err
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
	if restoresDisabled {
		from, marked := ha.NarrowMarkOf(got.Comment)
		if !marked || from != haStateDisabled || pve.HARequestedState(got.State) != haResourceStateIgnored {
			return fmt.Errorf("HA resource %s update requests disabled, and the barrier holds no disabled "+
				"resource to restore", sid)
		}
	}
	return nil
}

// lifecycleHAResourceUpdateAllowed checks the parameters of an HA resource
// update inside a disk lifecycle (see admitLifecycleHAWrite). It reports
// whether the update requests disabled as the barrier's restore, which the
// caller admits only once the resource shows the barrier holding a disabled
// resource.
func lifecycleHAResourceUpdateAllowed(call ManagedAllocationMutation, sid, owner string) (bool, error) {
	params, ok := call.Args[managedArgumentParams].(*cluster.UpdateHaResourcesParams)
	if !ok || params == nil {
		return false, fmt.Errorf("HA resource %s update lacks its parameters", sid)
	}
	fields, err := lifecycleMutationFields(params)
	if err != nil {
		return false, fmt.Errorf("HA resource %s update: %w", sid, err)
	}
	for key := range fields {
		if !slices.Contains(lifecycleHAResourceUpdateFields, key) {
			return false, fmt.Errorf("HA resource %s update sets %s, more than its requested state and comment",
				sid, key)
		}
	}
	restoresDisabled := false
	if params.State != nil {
		switch state := pve.HARequestedState(params.State); {
		case slices.Contains(lifecycleHAStates, state):
		case state == haStateDisabled && params.Comment != nil && !commentHasNarrowMark(params.Comment):
			restoresDisabled = true
		default:
			return false, fmt.Errorf("HA resource %s update requests state %q, which a disk lifecycle never "+
				"requests", sid, *params.State)
		}
	}
	if params.Delete != nil {
		if keys := splitPVEList(*params.Delete); len(keys) > 0 {
			return false, fmt.Errorf("HA resource %s update deletes %s, and a disk lifecycle deletes no setting",
				sid, strings.Join(keys, ","))
		}
	}
	if params.Comment != nil && haOwnershipOf(*params.Comment, owner) != haOwnedByUs {
		return false, fmt.Errorf("HA resource %s update does not carry this Director's ownership marker", sid)
	}
	return restoresDisabled, nil
}

// commentHasNarrowMark reports whether comment carries the barrier's token.
func commentHasNarrowMark(comment *string) bool {
	_, marked := ha.NarrowMarkOf(comment)
	return marked
}

// lifecycleHAResourceRemovalAllowed checks that an HA resource removal inside
// a disk lifecycle purges the resource from its rules and carries nothing
// else, which is the only removal the CPI sends.
func lifecycleHAResourceRemovalAllowed(call ManagedAllocationMutation, sid string) error {
	params, ok := call.Args[managedArgumentParams].(*cluster.DeleteHaResourcesParams)
	if !ok || params == nil || params.Purge == nil || !*params.Purge {
		return fmt.Errorf("HA resource %s removal does not purge it from its rules", sid)
	}
	return nil
}

// splitPVEList splits a list parameter the way PVE::Tools::split_list does,
// which is how PVE reads the delete parameter of a config or HA update. A
// list that holds a NUL byte splits on NUL bytes alone, and any other list
// splits on commas, semicolons, and whitespace. Empty items are dropped.
func splitPVEList(list string) []string {
	if strings.ContainsRune(list, 0) {
		return slices.DeleteFunc(strings.Split(list, "\x00"), func(item string) bool { return item == "" })
	}
	return strings.FieldsFunc(list, func(r rune) bool {
		switch r {
		case ',', ';', ' ', '\t', '\n', '\r', '\f', '\v':
			return true
		}
		return false
	})
}
