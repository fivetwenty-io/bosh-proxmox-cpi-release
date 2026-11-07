package ha

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// This file holds the staging order that registers a VM with PVE HA. The CRM
// still fences and recovers a stopped service, so a resource registered as
// stopped could be moved away from its disks before its pin exists. The order
// therefore stages the resource as ignored, which the CRM drops from its state
// machine and a rule can still name:
//
//  1. Stage the resource as ignored, with the ownership marker in its comment,
//     failback off, and auto-rebalance off unless the VM is a DLB VM.
//  2. The caller finishes the VM's configuration.
//  3. The caller writes the VM's merged pin and its anti-affinity membership.
//  4. Verify re-reads every rule and checks that each confinement rule for the
//     VM exists, is enabled, and holds what the caller meant to write.
//  5. CheckWatchdog applies the watchdog check, and Activate requests started
//     and waits until the VM runs.
//  6. Activate checks the node the VM landed on against its hard admissible
//     set.
//
// Unstage removes a resource that this call staged and never activated, which
// is what auto does when step 4 or the watchdog check refuses the VM.

// VolumeRecordKey is the key of the comment token on a VM's pin that records
// the storages of every volume the VM held when the pin was written. The pin's
// node set is the reach of those volumes cut down by the VM's other hard
// constraints, and the token is what proves that: a pin without it was not
// written from an eligibility decision, so its nodes say nothing about where
// the volumes are. The dead-node delete completion reads it while the VM's
// node is down and its config cannot be read.
const VolumeRecordKey = "bosh-vols"

// Errors the staging order wraps, so a caller can tell them apart.
var (
	// ErrConfinementUnverified means a confinement rule of the VM is missing,
	// disabled, in error, or holds something other than what was written. Auto
	// removes the staged resource and the VM runs without HA, and managed fails
	// the call.
	ErrConfinementUnverified = errors.New("a confinement rule of the VM is not in effect")

	// ErrWatchdogNotArmed means the cluster is one failure from losing quorum
	// and the VM's node has no armed watchdog, so activating the VM would arm
	// one the operator never asked for. Only auto gets it, and it removes the
	// staged resource.
	ErrWatchdogNotArmed = errors.New("the node's HA watchdog is not armed while the cluster is one failure from " +
		"losing quorum")
)

// VolumeStorages returns the sorted, distinct storages of vols. A volume ID
// that is not storage-qualified is skipped, because DecideVM has already
// refused a VM that holds one.
func VolumeStorages(vols []VMVolume) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vols {
		storage, name, ok := strings.Cut(strings.TrimSpace(v.Volume), ":")
		storage = strings.TrimSpace(storage)
		if !ok || storage == "" || name == "" || seen[storage] {
			continue
		}
		seen[storage] = true
		out = append(out, storage)
	}
	sort.Strings(out)
	return out
}

// WithVolumeRecord returns comment with the volume record naming storages,
// replacing a record that names others. The rest of the comment, which starts
// with the ownership marker, keeps its order. An empty storages list drops the
// record.
func WithVolumeRecord(comment string, storages []string) string {
	fields := strings.Fields(comment)
	kept := fields[:0]
	for _, f := range fields {
		if key, _, ok := strings.Cut(f, "="); ok && key == VolumeRecordKey {
			continue
		}
		kept = append(kept, f)
	}
	clean := trimmed(storages)
	if len(clean) == 0 {
		return strings.Join(kept, " ")
	}
	return strings.Join(append(kept, VolumeRecordKey+"="+strings.Join(clean, ",")), " ")
}

// VolumeRecord returns the storages a comment's volume record names, sorted,
// and false when the comment carries no record or an empty one.
func VolumeRecord(comment string) ([]string, bool) {
	for _, f := range strings.Fields(comment) {
		key, val, ok := strings.Cut(f, "=")
		if !ok || key != VolumeRecordKey {
			continue
		}
		var out []string
		for s := range strings.SplitSeq(val, ",") {
			out = append(out, s)
		}
		out = trimmed(out)
		return out, len(out) > 0
	}
	return nil, false
}

// RecordPin is the pin of a registered VM whose decision asks for no node
// constraint and no preference. It names every admissible node at one
// priority and is not strict, so it changes nothing about where the CRM may
// run the VM, and it carries the volume record that every pin needs.
func RecordPin(id, sid, comment string, admissible []string) *Rule {
	nodes := trimmed(admissible)
	if len(nodes) == 0 {
		return nil
	}
	pin := &Rule{
		ID:        id,
		Type:      RuleTypeNodeAffinity,
		Affinity:  AffinityPositive,
		Comment:   comment,
		Resources: []string{sid},
		Nodes:     make(map[string]int, len(nodes)),
	}
	for _, n := range nodes {
		pin.Nodes[n] = PinPriorityRest
	}
	return pin
}

// DecisionPin returns the merged pin of a registered VM: the pin BuildPin makes
// from the decision, or the record pin when the decision asks for no pin. The
// comment carries the volume record of storages. singlePriority flattens the
// priorities of a VM that is in a resource-affinity rule.
func DecisionPin(
	d *VMDecision, id, marker string, storages, configured []string, rebalanceOnStart, singlePriority bool,
) (*Rule, error) {
	switch {
	case !d.Registers():
		return nil, fmt.Errorf("pin %s: the VM's decision does not register it", id)
	case len(trimmed(storages)) == 0:
		return nil, fmt.Errorf("pin %s: the VM holds no volume to record", id)
	}
	comment := WithVolumeRecord(marker, storages)
	spec := d.PinSpec(id, comment, configured, rebalanceOnStart)
	spec.SinglePriority = singlePriority
	pin, err := BuildPin(spec)
	if err != nil {
		return nil, err
	}
	if pin == nil {
		pin = RecordPin(id, d.SID, comment, d.Admissible)
	}
	if pin == nil {
		return nil, fmt.Errorf("pin %s: the VM's decision names no admissible node", id)
	}
	return pin, nil
}

// Stager runs the staging order for one VM.
type Stager struct {
	// Resources reads and writes the VM's HA resource.
	Resources ResourceAPI

	// Owned reports whether a resource comment carries this Director's
	// ownership marker.
	Owned func(comment string) bool

	// ReadRules reads the HA rules, for the verification of step 4.
	ReadRules func(ctx context.Context) (*RuleSet, error)

	// Watchdog reads the watchdog state of a node for step 5.
	Watchdog func(ctx context.Context, node string) (pve.WatchdogState, error)

	// SetStarted waits until the VM whose resource requests started runs,
	// which is pve.SetHAState with the started target in production.
	SetStarted func(ctx context.Context, vmid int, node string) (*pve.SetHAStateResult, error)

	// Managed is true in managed mode, and FragileQuorum is true when the
	// cluster is one failure from losing quorum, which is when the watchdog
	// check applies.
	Managed       bool
	FragileQuorum bool

	Logger *log.Logger
}

func (s *Stager) logger() *log.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return log.NewNopLogger()
}

// Stage registers the VM's resource as ignored with marker as its comment,
// failback off, and auto-rebalance on only for a DLB VM, and marks the
// decision staged. A resource that already exists counts as staged only when
// it carries this Director's marker and requests ignored, and its comment and
// flags are then set to what the stage writes. Any other resource fails the
// call with a retriable error and is left alone, because it may be an
// operator's or a resource the CRM already manages. A held error re-reads the
// resource and counts the stage as done only when the read shows it.
func (s *Stager) Stage(ctx context.Context, d *VMDecision, marker string) error {
	switch {
	case !d.Registers():
		return cpierrors.Cloud("ha stage: the decision for VM %d does not register it", d.VMID)
	case s.Resources == nil || s.Owned == nil:
		return cpierrors.Cloud("ha stage: the resource client and the ownership check must be set")
	case !s.Owned(marker):
		return cpierrors.Cloud("ha stage: the comment of %s does not carry this Director's ownership marker", d.SID)
	}
	ignored, failback, rebalance := haStateIgnored, false, d.DLB
	err := s.Resources.CreateHaResources(ctx, &sdkcluster.CreateHaResourcesParams{
		Sid: d.SID, State: &ignored, Comment: &marker, Failback: &failback, AutoRebalance: &rebalance,
	})
	if err == nil {
		d.MarkStaged()
		s.logger().Info("ha: staged the HA resource as ignored", log.String("sid", d.SID),
			log.Bool("auto_rebalance", rebalance))
		return nil
	}
	got, readErr := s.Resources.GetHaResources(ctx, d.SID)
	if readErr != nil || got == nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha stage: register %s as ignored", d.SID))
	}
	if blocker := s.stagedBlocker(got); blocker != "" {
		return cpierrors.Retriable("ha stage: %s already exists and %s, so the CPI leaves it alone and does not "+
			"register VM %d; remove the resource or let the earlier call finish, and retry", d.SID, blocker, d.VMID)
	}
	if got.Comment == nil || *got.Comment != marker || !pveBoolIs(got.Failback, false) ||
		!pveBoolIs(got.AutoRebalance, rebalance) {
		if updErr := s.Resources.UpdateHaResources(ctx, d.SID, &sdkcluster.UpdateHaResourcesParams{
			Comment: &marker, Failback: &failback, AutoRebalance: &rebalance,
		}); updErr != nil {
			return cpierrors.WrapAs(updErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ha stage: set the staged flags of %s", d.SID))
		}
	}
	d.MarkStaged()
	s.logger().Info("ha: found the HA resource already staged as ignored", log.String("sid", d.SID))
	return nil
}

// stagedBlocker says why an existing resource is not one this call may treat
// as its own staged resource, or "" when it is.
func (s *Stager) stagedBlocker(got *sdkcluster.GetHaResourcesResponse) string {
	switch {
	case got.Comment == nil || !s.Owned(*got.Comment):
		return "does not carry this Director's ownership marker"
	case got.State == nil || *got.State != haStateIgnored:
		state := "no state"
		if got.State != nil {
			state = *got.State
		}
		return "requests " + state + " rather than ignored"
	}
	return ""
}

// pveBoolIs reports whether a PVE flag reads as want, with an unset flag
// reading as false, which is PVE's default for failback and auto-rebalance on a
// resource the CPI wrote.
func pveBoolIs(v *sdkclient.PVEBool, want bool) bool {
	if v == nil {
		return !want
	}
	return v.Bool() == want
}

// Verify re-reads every rule and checks the VM's confinement. The pin must
// exist, be enabled, carry no error, and match want, or the same pin with its
// priorities flattened, which the anti-affinity writer does for a member of a
// resource-affinity rule, or its confinement-only form, which the rule
// writer's repair writes. When antiAffinity names the VM's anti-affinity
// rule, that rule must exist, hold the VM, be enabled, and carry no error,
// because it keeps the VM off its group's other nodes. Every other
// confinement rule that names the VM must be enabled and carry no error.
//
// A confinement failure returns an error that wraps ErrConfinementUnverified.
// A failure of a pin that confines nothing, which is a non-strict pin, is
// logged in auto, because the VM may run on every node anyway, and fails the
// call in managed with an error that wraps ErrRuleNotInEffect.
func (s *Stager) Verify(ctx context.Context, d *VMDecision, want *Rule, antiAffinity string) error {
	switch {
	case want == nil:
		return cpierrors.Cloud("ha verify: no pin was given for VM %d", d.VMID)
	case s.ReadRules == nil:
		return cpierrors.Cloud("ha verify: the rules reader must be set")
	}
	// A rules read that fails or returns nothing proves no confinement, so
	// its error wraps ErrConfinementUnverified like any other failed check.
	set, err := s.ReadRules(ctx)
	if err != nil {
		return cpierrors.WrapAs(fmt.Errorf("%w: the HA rules could not be re-read: %w", ErrConfinementUnverified, err),
			cpierrors.TypeRetriableCloud, fmt.Sprintf("ha verify: re-read the HA rules for %s", d.SID))
	}
	if set == nil {
		return cpierrors.WrapAs(fmt.Errorf("%w: the re-read of the HA rules returned nothing", ErrConfinementUnverified),
			cpierrors.TypeRetriableCloud, fmt.Sprintf("ha verify: re-read the HA rules for %s", d.SID))
	}
	var problems []string
	pinProblem := pinMismatch(set.Find(want.ID), want)
	if pinProblem != "" && want.Strict {
		problems = append(problems, pinProblem)
	}
	if id := strings.TrimSpace(antiAffinity); id != "" {
		switch aa := set.Find(id); {
		case aa == nil:
			problems = append(problems, fmt.Sprintf("anti-affinity rule %q is missing", id))
		case aa.Type != RuleTypeResourceAffinity || aa.Affinity != AffinityNegative:
			problems = append(problems, fmt.Sprintf("rule %q is not an anti-affinity rule", id))
		case !aa.HasResource(d.SID):
			problems = append(problems, fmt.Sprintf("anti-affinity rule %q does not hold %s", id, d.SID))
		case aa.Disabled:
			problems = append(problems, fmt.Sprintf("anti-affinity rule %q is disabled", id))
		case len(aa.Errors) > 0:
			problems = append(problems, fmt.Sprintf("anti-affinity rule %q has errors: %s", id,
				strings.Join(aa.Errors, "; ")))
		}
	}
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.ID == want.ID || !IsConfinement(r) || !r.HasResource(d.SID) {
			continue
		}
		switch {
		case r.Disabled:
			problems = append(problems, fmt.Sprintf("confinement rule %q is disabled", r.ID))
		case len(r.Errors) > 0:
			problems = append(problems, fmt.Sprintf("confinement rule %q has errors: %s", r.ID,
				strings.Join(r.Errors, "; ")))
		}
	}
	if len(problems) > 0 {
		return cpierrors.WrapAs(fmt.Errorf("%w: %s", ErrConfinementUnverified, strings.Join(problems, "; ")),
			cpierrors.TypeRetriableCloud, fmt.Sprintf("ha verify: %s", d.SID))
	}
	if pinProblem == "" {
		return nil
	}
	if s.Managed {
		return cpierrors.WrapAs(fmt.Errorf("%w: %s", ErrRuleNotInEffect, pinProblem), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha verify: %s", d.SID))
	}
	s.logger().Warn("ha: the VM's non-strict pin is not in effect; it confines nothing, so the VM stays registered",
		log.String("sid", d.SID), log.String("problem", pinProblem))
	return nil
}

// pinMismatch says how got fails to be want, or "" when it is want in one of
// the forms Verify accepts.
func pinMismatch(got, want *Rule) string {
	switch {
	case got == nil:
		return fmt.Sprintf("pin %q is missing", want.ID)
	case got.Disabled:
		return fmt.Sprintf("pin %q is disabled", want.ID)
	case len(got.Errors) > 0:
		return fmt.Sprintf("pin %q has errors: %s", want.ID, strings.Join(got.Errors, "; "))
	}
	forms := []*Rule{want, Flattened(want)}
	if want.Strict {
		forms = append(forms, ConfinementOnly(want))
	}
	for _, f := range forms {
		if SameRule(f, got) {
			return ""
		}
	}
	return fmt.Sprintf("pin %q differs in %s", want.ID, strings.Join(RuleDiff(want, got), ","))
}

// CheckWatchdog applies the watchdog check to node and to every node of the
// VM's hard admissible set, because PVE HA may start the VM on any of them.
// It passes when the cluster is not one failure from losing quorum, or when
// every one of those nodes has its watchdog armed, because one more service
// changes nothing there. Otherwise auto gets an error that wraps
// ErrWatchdogNotArmed and names the nodes, and managed passes with a warning.
// A failed read counts as an unknown watchdog.
func (s *Stager) CheckWatchdog(ctx context.Context, d *VMDecision, node string) error {
	if !s.FragileQuorum {
		return nil
	}
	nodes := []string{node}
	for _, n := range d.Admissible {
		if n != "" && !slices.Contains(nodes, n) {
			nodes = append(nodes, n)
		}
	}
	var unarmed []string
	for _, n := range nodes {
		state := pve.WatchdogUnknown
		if s.Watchdog != nil {
			got, err := s.Watchdog(ctx, n)
			if err != nil {
				s.logger().Warn("ha: could not read the node's watchdog state; it counts as unknown",
					log.String("sid", d.SID), log.String("node", n), log.Err(err))
			} else {
				state = got
			}
		}
		if state != pve.WatchdogArmed {
			unarmed = append(unarmed, fmt.Sprintf("node %s is %s", n, state))
		}
	}
	if len(unarmed) == 0 {
		return nil
	}
	if s.Managed {
		s.logger().Warn("ha: activating the VM arms the HA watchdog of the node it starts on while the cluster is "+
			"one failure from losing quorum; that node reboots itself if it loses quorum",
			log.String("sid", d.SID), log.String("watchdog", strings.Join(unarmed, "; ")))
		return nil
	}
	return fmt.Errorf("%w: %s: %s", ErrWatchdogNotArmed, d.SID, strings.Join(unarmed, "; "))
}

// Activate takes the resource out of staging and starts the VM with one
// request for started, then waits until the VM runs and checks the node it
// landed on against its hard admissible set. The decision is marked activated
// before the request goes out, because from then on the CRM may act on the
// resource and a failed repair may no longer remove it directly. A held error
// on the request re-reads the resource and goes on only when it requests
// started. A landing outside the hard admissible set is a retriable error.
// It returns the landing node.
func (s *Stager) Activate(ctx context.Context, d *VMDecision, node string) (string, error) {
	switch {
	case !d.Registers():
		return "", cpierrors.Cloud("ha activate: the decision for VM %d does not register it", d.VMID)
	case s.Resources == nil || s.SetStarted == nil:
		return "", cpierrors.Cloud("ha activate: the resource client and the state wait must be set")
	}
	d.MarkActivated()
	started := haStateStarted
	if err := s.Resources.UpdateHaResources(ctx, d.SID, &sdkcluster.UpdateHaResourcesParams{State: &started}); err != nil {
		got, readErr := s.Resources.GetHaResources(ctx, d.SID)
		if readErr != nil || got == nil || got.State == nil || *got.State != haStateStarted {
			return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ha activate: request started for %s", d.SID))
		}
	}
	res, err := s.SetStarted(ctx, d.VMID, node)
	if err != nil {
		return "", err
	}
	switch {
	case res == nil:
		return "", cpierrors.Retriable("ha activate: the state wait for %s returned no result", d.SID)
	case res.Outcome == pve.HAStateNeedsReregistration:
		return "", res.ReregistrationError()
	case res.Outcome != pve.HAStateReached:
		return "", cpierrors.Retriable("ha activate: %s did not reach started (%s)", d.SID, res.Outcome)
	}
	landing := strings.TrimSpace(res.Node)
	if !slices.Contains(d.Admissible, landing) {
		return landing, cpierrors.Retriable("ha activate: PVE HA started %s on node %s, which lies outside its hard "+
			"admissible set %s; the VM's volumes or hardware may not be reachable there", d.SID, orDash(landing),
			strings.Join(d.Admissible, ","))
	}
	s.logger().Info("ha: the VM runs under HA", log.String("sid", d.SID), log.String("node", landing))
	return landing, nil
}

// Unstage removes the resource this call staged and never activated, so the
// VM goes on without HA, and marks the decision as one that registers
// nothing, with reason as its reason. It removes only a resource that carries
// this Director's marker and still requests ignored. A resource that is
// already gone counts as removed. Anything else returns a retriable error and
// stays, because the CRM may already act on it.
func (s *Stager) Unstage(ctx context.Context, d *VMDecision, reason string) error {
	if s.Resources == nil || s.Owned == nil {
		return cpierrors.Cloud("ha unstage: the resource client and the ownership check must be set")
	}
	if !d.StagedIgnored {
		return cpierrors.Retriable("ha unstage: %s was not staged by this call or was activated, so it stays", d.SID)
	}
	got, err := s.Resources.GetHaResources(ctx, d.SID)
	switch {
	case err != nil && isResourceNotFound(err):
	case err != nil:
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha unstage: read %s before its removal", d.SID))
	case got == nil:
		return cpierrors.Retriable("ha unstage: the read of %s returned nothing", d.SID)
	default:
		if blocker := s.stagedBlocker(got); blocker != "" {
			return cpierrors.Retriable("ha unstage: %s %s, so it stays", d.SID, blocker)
		}
		purge := true
		if delErr := s.Resources.DeleteHaResources(ctx, d.SID, &sdkcluster.DeleteHaResourcesParams{Purge: &purge}); delErr != nil &&
			!isResourceNotFound(delErr) {
			return cpierrors.WrapAs(delErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ha unstage: remove the staged %s", d.SID))
		}
	}
	d.StagedIgnored = false
	d.Outcome, d.Policy, d.Reason = OutcomeSkip, PolicyNone, reason
	s.logger().Warn("ha: removed the staged HA resource; the VM runs without HA", log.String("sid", d.SID),
		log.String("reason", reason))
	return nil
}

// LeaveHA removes the active HA resource of a VM that may no longer run under
// HA, and waits until the CRM has let go of it, so nothing moves the VM while
// the caller changes it. It removes only a resource for which owned holds, and
// a resource that is already gone counts as removed, because a crash after an
// earlier removal leaves exactly that. The wait is the narrowing barrier's
// drain: a CRM round newer than the removal no longer lists the service, the
// VM's node has an LRM report newer than that round, and the VM holds no lock.
// It returns where the VM is. Every failure is retriable, and a resource in the
// error state stays, because the operator must clear it.
func LeaveHA(
	ctx context.Context, deps NarrowDeps, vmid int, node string, budget time.Duration, owned func(string) bool,
) (pve.VMLocation, error) {
	sid := pve.HAServiceSID(vmid)
	switch {
	case vmid <= 0:
		return pve.VMLocation{}, cpierrors.Cloud("ha leave: VM ID must be positive, got %d", vmid)
	case deps.Resources == nil || deps.ReadState == nil || deps.Settle == nil || owned == nil:
		return pve.VMLocation{}, cpierrors.Cloud("ha leave: the resource client, the state read, the settle wait, " +
			"and the ownership check must be set")
	}
	got, err := deps.Resources.GetHaResources(ctx, sid)
	switch {
	case err != nil && isResourceNotFound(err):
	case err != nil:
		return pve.VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha leave: read %s before its removal", sid))
	case got == nil:
		return pve.VMLocation{}, cpierrors.Retriable("ha leave: the read of %s returned nothing", sid)
	default:
		comment := ""
		if got.Comment != nil {
			comment = *got.Comment
		}
		if !owned(comment) {
			return pve.VMLocation{}, cpierrors.Retriable("ha leave: %s does not carry this Director's ownership "+
				"marker, so the CPI leaves it in place", sid)
		}
		if got.State != nil && *got.State == haStateError {
			return pve.VMLocation{}, cpierrors.Retriable("ha leave: %s is in the error state; the operator must "+
				"clear it", sid)
		}
		purge := true
		if delErr := deps.Resources.DeleteHaResources(ctx, sid, &sdkcluster.DeleteHaResourcesParams{Purge: &purge}); delErr != nil &&
			!isResourceNotFound(delErr) {
			return pve.VMLocation{}, cpierrors.WrapAs(delErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ha leave: remove %s", sid))
		}
	}
	// The barrier with nothing to restore: restore is ignored and no token is
	// on the comment, so a failed drain writes nothing back.
	b := &barrier{deps: deps, in: NarrowInput{VMID: vmid, Node: node, Budget: budget}, sid: sid,
		restore: haStateIgnored}
	b.defaults()
	after, err := b.readState(ctx, node)
	if err != nil {
		return pve.VMLocation{}, err
	}
	loc, err := b.drain(ctx, after.EntryTime, after.Node)
	if err != nil {
		return loc, err
	}
	b.deps.Logger.Info("ha: removed the VM's HA resource and the CRM let go of it", log.String("sid", sid),
		log.String("node", loc.Node))
	return loc, nil
}

// NarrowPending reports whether a resource with the requested state and the
// comment given is ignored and carries the narrowing barrier's token, which is
// what a barrier that stopped part way leaves. A caller that finds one runs the
// barrier again, which finishes it and puts back the state the token names.
func NarrowPending(state, comment *string) bool {
	_, marked := narrowMarkOf(comment)
	return marked && state != nil && *state == haStateIgnored
}

// isResourceNotFound reports whether err says the HA resource does not exist.
func isResourceNotFound(err error) bool {
	if err == nil {
		return false
	}
	if pve.IsNotFound(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "not defined")
}
