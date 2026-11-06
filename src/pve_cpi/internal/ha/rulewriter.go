package ha

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// RuleAPI is the part of the cluster API the rule writer uses. The SDK's
// cluster service satisfies it.
type RuleAPI interface {
	ListHaRules(ctx context.Context, params *sdkcluster.ListHaRulesParams) (*sdkcluster.ListHaRulesResponse, error)
	CreateHaRules(ctx context.Context, params *sdkcluster.CreateHaRulesParams) error
	UpdateHaRules(ctx context.Context, rule string, params *sdkcluster.UpdateHaRulesParams) error
	DeleteHaRules(ctx context.Context, rule string) error
}

// OpKind is what a RuleOp does to one rule.
type OpKind string

// The rule operations. An update replaces the whole rule with a PUT that
// carries the digest of the read it was planned from.
const (
	OpCreate OpKind = "create"
	OpUpdate OpKind = "update"
	OpDelete OpKind = "delete"
)

// RuleOp is one planned rule write. For a delete only Rule.ID counts.
type RuleOp struct {
	Kind OpKind
	Rule Rule
}

// Planner returns the writes that take set to the state the caller wants. The
// writer calls it again after every write and every conflict, with a fresh
// read, so it must return no writes once set already holds that state.
type Planner func(set *RuleSet) ([]RuleOp, error)

// Errors the rule writer wraps, so a caller can tell them apart.
var (
	// ErrRuleInfeasible means the CPI's feasibility checks refused the write
	// before anything was sent, because it would make a rule infeasible.
	ErrRuleInfeasible = errors.New("HA rule write would make the rule set infeasible")
	// ErrRuleRefused means PVE answered a rule write with a refusal that is not
	// a feasibility error, such as a missing permission or a bad parameter. The
	// error carries the HTTP status, so the operator sees what to fix.
	ErrRuleRefused = errors.New("PVE refused the HA rule write")
	// ErrRuleNotInEffect means a CPI-owned rule is disabled or conflicting
	// after the write. A caller in managed mode fails the call, and one in auto
	// logs it.
	ErrRuleNotInEffect = errors.New("HA rule is not in effect")
)

// RuleWriterMaxAttempts caps digest conflicts, and separately held errors whose
// read shows the write did not land, before the writer gives up with a
// retriable error.
const RuleWriterMaxAttempts = 5

// ruleWriterMaxOps bounds the writes one Apply sends, so a planner that never
// converges cannot loop forever.
const ruleWriterMaxOps = 64

const (
	ruleBackoffBase = 250 * time.Millisecond
	ruleBackoffMax  = 4 * time.Second
)

// RepairDeps lets the writer remove an HA resource as the last step of the
// repair of a disabled confinement pin. Every field is optional. Without them
// the writer never removes a resource, and an unverified repair keeps the
// resource and returns a retriable error.
type RepairDeps struct {
	// StagedIgnored reports whether this call staged sid as ignored and never
	// activated it, in which case it may be removed directly.
	StagedIgnored func(sid string) bool
	// DecisionUID returns the CRM uid the call's decision read saw for sid.
	DecisionUID func(sid string) (uid string, ok bool)
	// ReadState reads the VM's normalized HA state right before a removal.
	ReadState func(ctx context.Context, vmid int) (*pve.HAServiceState, error)
	// RemoveResource removes the HA resource.
	RemoveResource func(ctx context.Context, sid string) error
}

// RuleWriter writes CPI rules under one cluster-scoped lock, with the read's
// digest, and checks the whole rule set before and after every write.
type RuleWriter struct {
	API RuleAPI
	// Lock takes the cluster-scoped rules lock and returns its release. nil
	// writes without a lock, and the digest still refuses a stale write.
	Lock func(ctx context.Context) (release func(), err error)
	// Owned reports whether a rule carries this Director's ownership marker.
	// Only owned rules are verified after a write and repaired.
	Owned func(r *Rule) bool
	// ClusterNodes is the configured node set the feasibility checks use.
	ClusterNodes []string
	// Managed makes a rule that is not in effect after a write fail the call.
	// In auto the writer logs it.
	Managed bool
	Logger  *log.Logger
	// Sleep waits between conflicts. nil sleeps on the context.
	Sleep  func(ctx context.Context, d time.Duration) error
	Repair RepairDeps
}

// ApplyResult is what one Apply did.
type ApplyResult struct {
	// Set is the last read of the rules.
	Set *RuleSet
	// Wrote lists the writes that landed, in order.
	Wrote []RuleOp
}

// Apply runs plan to completion under the rules lock. Each round reads the
// rules, plans, checks the candidate set, and sends the first planned write.
// A digest conflict backs off with jitter and retries, up to
// RuleWriterMaxAttempts times. A held error, which may hide a write that
// landed, re-reads and compares the whole rule with what was meant: a match
// counts as landed, and a mismatch goes round again, which checks feasibility
// against the fresh read. After the last write it verifies every CPI-owned rule
// and repairs a disabled confinement pin.
func (w *RuleWriter) Apply(ctx context.Context, label string, plan Planner) (*ApplyResult, error) {
	if w.Lock != nil {
		release, err := w.Lock(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	res := &ApplyResult{}
	var before *RuleSet
	conflicts, misses := 0, 0
	for {
		set, err := w.read(ctx)
		if err != nil {
			return res, fmt.Errorf("%s: read HA rules: %w", label, err)
		}
		res.Set = set
		if before == nil {
			before = set
		}
		ops, err := plan(set)
		if err != nil {
			return res, err
		}
		if len(ops) == 0 {
			break
		}
		if len(res.Wrote) >= ruleWriterMaxOps {
			return res, cpierrors.Retriable("%s: HA rule plan did not converge after %d writes", label, len(res.Wrote))
		}
		op := normalizeOp(ops[0])
		if bad := w.introduced(set.Rules, candidateRules(set, op), op.Rule.ID); len(bad) > 0 {
			return res, cpierrors.WrapAs(ErrRuleInfeasible, cpierrors.TypeCloud,
				fmt.Sprintf("%s: %s rule %q refused before sending it: %s", label, op.Kind, op.Rule.ID,
					describeConflicts(bad)))
		}
		sendErr := w.send(ctx, set, op)
		switch classifyRuleWrite(op, sendErr) {
		case writeLanded:
			res.Wrote = append(res.Wrote, op)
			continue
		case writeConflict:
			conflicts++
			if conflicts >= RuleWriterMaxAttempts {
				return res, cpierrors.WrapAs(sendErr, cpierrors.TypeRetriableCloud,
					fmt.Sprintf("%s: HA rules kept changing under %d writes of rule %q", label, conflicts, op.Rule.ID))
			}
			w.logger().Info("HA rule write hit a digest conflict; backing off",
				log.String("rule", op.Rule.ID), log.Int("attempt", conflicts), log.String("op", label))
			if err := w.sleep(ctx, ruleBackoff(conflicts)); err != nil {
				return res, err
			}
			continue
		case writeRejected:
			return res, rejectedRuleWrite(label, op, sendErr)
		}
		// Held: read again and compare the whole rule.
		fresh, err := w.read(ctx)
		if err != nil {
			return res, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: rule %q write held (%v) and the rules could not be read again", label, op.Rule.ID, sendErr))
		}
		if landed(op, fresh) {
			res.Wrote = append(res.Wrote, op)
			continue
		}
		misses++
		w.logger().Warn("HA rule write held and the rule does not match what was meant; planning again",
			log.String("rule", op.Rule.ID), log.String("differs", strings.Join(RuleDiff(&op.Rule,
				fresh.Find(op.Rule.ID)), ",")), log.Err(sendErr))
		if misses >= RuleWriterMaxAttempts {
			return res, cpierrors.WrapAs(sendErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: rule %q write did not land after %d held attempts", label, op.Rule.ID, misses))
		}
	}
	if len(res.Wrote) == 0 {
		return res, nil
	}
	return res, w.verify(ctx, label, before, res)
}

// check runs the CPI's feasibility checks, or none when the configured nodes
// are unknown. Without them every negative rule would read as oversized, so
// only PVE's own errors count, which only a removal accepts.
func (w *RuleWriter) check(rules []Rule) map[string][]string {
	if w.ClusterNodes == nil {
		return nil
	}
	return CheckFeasibility(rules, w.ClusterNodes)
}

func (w *RuleWriter) introduced(before, candidate []Rule, id string) map[string][]string {
	if w.ClusterNodes == nil {
		return nil
	}
	return IntroducedConflicts(before, candidate, []string{id}, w.ClusterNodes)
}

// read lists and normalizes the rules.
func (w *RuleWriter) read(ctx context.Context) (*RuleSet, error) {
	resp, err := w.API.ListHaRules(ctx, &sdkcluster.ListHaRulesParams{})
	if err != nil {
		return nil, err
	}
	return ParseRuleList(resp)
}

func (w *RuleWriter) logger() *log.Logger {
	if w.Logger == nil {
		return log.NewNopLogger()
	}
	return w.Logger
}

func (w *RuleWriter) sleep(ctx context.Context, d time.Duration) error {
	if w.Sleep != nil {
		return w.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ruleBackoff is the wait before conflict attempt n: exponential from
// ruleBackoffBase, capped at ruleBackoffMax, plus up to half again of jitter so
// two writers that collided do not collide again.
func ruleBackoff(n int) time.Duration {
	d := ruleBackoffBase << max(n-1, 0)
	if d <= 0 || d > ruleBackoffMax {
		d = ruleBackoffMax
	}
	return d + time.Duration(rand.Int64N(int64(d)/2+1))
}

// normalizeOp puts the rule in the shape a read returns, so a held write
// compares equal to the rule it landed as.
func normalizeOp(op RuleOp) RuleOp {
	op.Rule = op.Rule.Clone()
	op.Rule.Resources = trimmed(op.Rule.Resources)
	op.Rule.Errors = nil
	if op.Rule.Type == RuleTypeNodeAffinity && op.Rule.Affinity == "" {
		op.Rule.Affinity = AffinityPositive
	}
	if op.Rule.Type != RuleTypeNodeAffinity {
		op.Rule.Strict = false
		op.Rule.Nodes = nil
	}
	return op
}

// candidateRules returns set's rules with op applied.
func candidateRules(set *RuleSet, op RuleOp) []Rule {
	out := make([]Rule, 0, len(set.Rules)+1)
	replaced := false
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.ID != op.Rule.ID {
			out = append(out, *r)
			continue
		}
		replaced = true
		if op.Kind != OpDelete {
			out = append(out, op.Rule)
		}
	}
	if !replaced && op.Kind != OpDelete {
		out = append(out, op.Rule)
	}
	return out
}

func describeConflicts(bad map[string][]string) string {
	ids := make([]string, 0, len(bad))
	for id := range bad {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s (%s)", id, strings.Join(bad[id], "; ")))
	}
	return strings.Join(parts, ", ")
}

// send writes op. An update carries the read's digest and replaces every field
// the rule's type has, so the rule after it is exactly op.Rule.
func (w *RuleWriter) send(ctx context.Context, set *RuleSet, op RuleOp) error {
	r := op.Rule
	switch op.Kind {
	case OpDelete:
		return w.API.DeleteHaRules(ctx, r.ID)
	case OpCreate:
		p := &sdkcluster.CreateHaRulesParams{Rule: r.ID, Type: r.Type, Resources: r.ResourcesParam()}
		if r.Comment != "" {
			p.Comment = strRef(r.Comment)
		}
		if r.Disabled {
			p.Disable = boolRef(true)
		}
		if r.Type == RuleTypeNodeAffinity {
			nodes := r.NodesParam()
			p.Nodes = &nodes
			p.Strict = boolRef(r.Strict)
			if r.Affinity == AffinityNegative {
				p.Affinity = strRef(AffinityNegative)
			}
		} else if r.Affinity != "" {
			p.Affinity = strRef(r.Affinity)
		}
		return w.API.CreateHaRules(ctx, p)
	}
	prev := set.Find(r.ID)
	resources := r.ResourcesParam()
	p := &sdkcluster.UpdateHaRulesParams{Type: r.Type, Resources: &resources}
	if set.Digest != "" {
		p.Digest = strRef(set.Digest)
	}
	var del []string
	if r.Comment != "" {
		p.Comment = strRef(r.Comment)
	} else if prev != nil && prev.Comment != "" {
		del = append(del, "comment")
	}
	switch {
	case r.Disabled:
		p.Disable = boolRef(true)
	case prev != nil && prev.Disabled:
		del = append(del, "disable")
	}
	if r.Type == RuleTypeNodeAffinity {
		nodes := r.NodesParam()
		p.Nodes = &nodes
		p.Strict = boolRef(r.Strict)
		if r.Affinity == AffinityNegative || prev != nil && prev.Affinity == AffinityNegative {
			p.Affinity = strRef(r.Affinity)
		}
	} else if r.Affinity != "" {
		p.Affinity = strRef(r.Affinity)
	}
	if len(del) > 0 {
		p.Delete = strRef(strings.Join(del, ","))
	}
	return w.API.UpdateHaRules(ctx, r.ID, p)
}

func strRef(s string) *string { return &s }
func boolRef(b bool) *bool    { return &b }

type writeOutcome int

const (
	writeLanded writeOutcome = iota
	writeConflict
	writeRejected
	writeHeld
)

// rejectedRuleWrite wraps a write PVE refused. Only a refusal whose text is
// one of PVE's feasibility errors carries ErrRuleInfeasible, the way the CPI's
// own checks do. Any other refusal, such as a 403 for a missing privilege,
// carries ErrRuleRefused and its HTTP status.
func rejectedRuleWrite(label string, op RuleOp, sendErr error) error {
	msg := fmt.Sprintf("%s: PVE refused to %s rule %q", label, op.Kind, op.Rule.ID)
	if IsFeasibilityRefusal(sendErr) {
		return cpierrors.WrapAs(fmt.Errorf("%w: %w", ErrRuleInfeasible, sendErr), cpierrors.TypeCloud, msg)
	}
	status, _ := pve.AnsweredStatus(sendErr)
	return cpierrors.WrapAs(fmt.Errorf("%w with HTTP %d: %w", ErrRuleRefused, status, sendErr), cpierrors.TypeCloud,
		msg)
}

// classifyRuleWrite sorts a write's error. A digest mismatch is a conflict. A
// create that finds the rule already there, or any error PVE did not answer
// with a 4xx, may hide a write that landed, so it is held and settled by a read.
// A delete of a missing rule has already landed. An update of a rule that is
// gone was planned from a stale read, so it plans again like a conflict.
func classifyRuleWrite(op RuleOp, err error) writeOutcome {
	switch {
	case err == nil:
		return writeLanded
	case IsDigestConflict(err):
		return writeConflict
	case op.Kind == OpDelete && isRuleNotFound(err):
		return writeLanded
	case op.Kind == OpUpdate && pve.IsNotFound(err):
		return writeConflict
	case op.Kind == OpCreate && isRuleAlreadyDefined(err):
		return writeHeld
	case pve.IsAnsweredConfigRefusal(err) && !pve.IsVMConfigLocked(err):
		return writeRejected
	}
	return writeHeld
}

// IsDigestConflict reports whether err is PVE's refusal of a write whose digest
// no longer matches the rules file.
func IsDigestConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "detected modified configuration") ||
		strings.Contains(msg, "file changed by other user")
}

func isRuleNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return pve.IsNotFound(err) || strings.Contains(msg, "no such") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "not defined")
}

func isRuleAlreadyDefined(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already defined") || strings.Contains(msg, "already exists")
}

// landed reports whether fresh shows op's result.
func landed(op RuleOp, fresh *RuleSet) bool {
	got := fresh.Find(op.Rule.ID)
	if op.Kind == OpDelete {
		return got == nil
	}
	return got != nil && SameRule(&op.Rule, got)
}

// inEffect reports whether a rule is enabled and has no feasibility error,
// either PVE's or the CPI's.
func inEffect(r *Rule, ours map[string][]string) bool {
	return r != nil && !r.Disabled && len(r.Errors) == 0 && len(ours[r.ID]) == 0
}

// IsConfinement reports whether a rule confines its resources to its nodes.
func IsConfinement(r *Rule) bool {
	return r != nil && r.Type == RuleTypeNodeAffinity && r.Strict && r.Affinity != AffinityNegative
}

// failingOwned returns the CPI-owned rules of after that are not in effect and
// that the write is answerable for: a rule it wrote, or one that was in effect
// before it. A rule that was already failing is only logged.
func (w *RuleWriter) failingOwned(before, after *RuleSet, wrote []RuleOp) []Rule {
	oursBefore := w.check(before.Rules)
	oursAfter := w.check(after.Rules)
	touched := map[string]bool{}
	for i := range wrote {
		touched[wrote[i].Rule.ID] = true
	}
	var out []Rule
	for i := range after.Rules {
		r := &after.Rules[i]
		if w.Owned == nil || !w.Owned(r) || inEffect(r, oursAfter) {
			continue
		}
		prev := before.Find(r.ID)
		if touched[r.ID] || inEffect(prev, oursBefore) {
			out = append(out, r.Clone())
			continue
		}
		w.logger().Warn("CPI-owned HA rule was already not in effect before this write; leaving it to the operator",
			log.String("rule", r.ID), log.String("errors", strings.Join(append(slicesClone(r.Errors),
				oursAfter[r.ID]...), "; ")), log.Bool("disabled", r.Disabled))
	}
	return out
}

func slicesClone(in []string) []string { return append([]string(nil), in...) }

// verify re-reads every rule and handles each CPI-owned rule the write left
// out of effect. A preference rule is logged in auto and fails the call in
// managed. A confinement pin goes through the repair, one VM at a time.
func (w *RuleWriter) verify(ctx context.Context, label string, before *RuleSet, res *ApplyResult) error {
	after, err := w.read(ctx)
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, label+": re-read HA rules after the write")
	}
	res.Set = after
	failing := w.failingOwned(before, after, res.Wrote)
	if len(failing) == 0 {
		return nil
	}
	var notes []string
	for i := range failing {
		r := &failing[i]
		if IsConfinement(r) {
			continue
		}
		notes = append(notes, fmt.Sprintf("preference rule %q is not in effect", r.ID))
	}
	reverted := false
	for i := range failing {
		r := &failing[i]
		if !IsConfinement(r) {
			continue
		}
		note, err := w.repairPin(ctx, label, before, res, r.ID, &reverted)
		if err != nil {
			return err
		}
		notes = append(notes, note)
	}
	msg := fmt.Sprintf("%s: %s", label, strings.Join(notes, "; "))
	if w.Managed {
		return cpierrors.WrapAs(ErrRuleNotInEffect, cpierrors.TypeCloud, msg)
	}
	w.logger().Warn("HA rules not fully in effect after the write (auto mode logs and carries on)",
		log.String("detail", msg))
	return nil
}

// repairPin runs the three-step repair for the confinement pin id. It returns a
// note on what fixed it, or a retriable error when nothing could be verified
// and the resource had to stay.
func (w *RuleWriter) repairPin(
	ctx context.Context, label string, before *RuleSet, res *ApplyResult, id string, reverted *bool,
) (string, error) {
	// 1. Revert this call's writes when the pin was in effect before them.
	if prev := before.Find(id); !*reverted && inEffect(prev, w.check(before.Rules)) {
		*reverted = true
		if err := w.revert(ctx, before, res.Wrote); err != nil {
			return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: revert the write that disabled confinement pin %q", label, id))
		}
		ok, err := w.pinInEffect(ctx, res, id)
		if err != nil {
			return "", err
		}
		if ok {
			return fmt.Sprintf("confinement pin %q was disabled by this write, which was reverted", id), nil
		}
	}

	// 2. Rewrite the pin as confinement-only, and drop the VM from the
	// anti-affinity rule the checks name as the cause.
	current := res.Set.Find(id)
	if current == nil {
		return "", cpierrors.Retriable("%s: confinement pin %q vanished during its repair", label, id)
	}
	if err := w.confinementOnly(ctx, res, current); err != nil {
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: rewrite confinement pin %q", label, id))
	}
	ok, err := w.pinInEffect(ctx, res, id)
	if err != nil {
		return "", err
	}
	if ok {
		return fmt.Sprintf("confinement pin %q was rewritten as confinement-only", id), nil
	}

	// 3. Remove the HA resource only when that is proven safe.
	return w.removeUnrepairable(ctx, label, res.Set.Find(id))
}

// revert undoes wrote, newest first, each under the digest of a fresh read.
func (w *RuleWriter) revert(ctx context.Context, before *RuleSet, wrote []RuleOp) error {
	for i := len(wrote) - 1; i >= 0; i-- {
		op := wrote[i]
		prev := before.Find(op.Rule.ID)
		var undo RuleOp
		switch {
		case prev == nil:
			undo = RuleOp{Kind: OpDelete, Rule: Rule{ID: op.Rule.ID}}
		case op.Kind == OpDelete:
			undo = RuleOp{Kind: OpCreate, Rule: prev.Clone()}
		default:
			undo = RuleOp{Kind: OpUpdate, Rule: prev.Clone()}
		}
		if err := w.writeOnce(ctx, normalizeOp(undo)); err != nil {
			return err
		}
	}
	return nil
}

// writeOnce sends one write under a fresh digest and settles a held error with
// a read. It retries digest conflicts like Apply does.
func (w *RuleWriter) writeOnce(ctx context.Context, op RuleOp) error {
	for attempt := 1; ; attempt++ {
		set, err := w.read(ctx)
		if err != nil {
			return err
		}
		if landed(op, set) {
			return nil
		}
		sendErr := w.send(ctx, set, op)
		switch classifyRuleWrite(op, sendErr) {
		case writeLanded:
			return nil
		case writeRejected:
			return sendErr
		case writeConflict:
			if attempt >= RuleWriterMaxAttempts {
				return sendErr
			}
			if err := w.sleep(ctx, ruleBackoff(attempt)); err != nil {
				return err
			}
			continue
		}
		fresh, err := w.read(ctx)
		if err != nil {
			return err
		}
		if landed(op, fresh) {
			return nil
		}
		if attempt >= RuleWriterMaxAttempts {
			return sendErr
		}
	}
}

// pinInEffect re-reads the rules and reports whether the pin is in effect.
func (w *RuleWriter) pinInEffect(ctx context.Context, res *ApplyResult, id string) (bool, error) {
	set, err := w.read(ctx)
	if err != nil {
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "re-read HA rules during a pin repair")
	}
	res.Set = set
	return inEffect(set.Find(id), w.check(set.Rules)), nil
}

// confinementOnly rewrites pin as strict with no priorities, and removes its
// resources from every CPI-owned resource-affinity rule the checks name.
func (w *RuleWriter) confinementOnly(ctx context.Context, res *ApplyResult, pin *Rule) error {
	ours := w.check(res.Set.Rules)
	for i := range res.Set.Rules {
		ra := &res.Set.Rules[i]
		if ra.Type != RuleTypeResourceAffinity || len(ours[ra.ID]) == 0 || w.Owned == nil || !w.Owned(ra) {
			continue
		}
		var keep []string
		for _, sid := range ra.Resources {
			if !pin.HasResource(sid) {
				keep = append(keep, sid)
			}
		}
		if len(keep) == len(ra.Resources) {
			continue
		}
		op := RuleOp{Kind: OpDelete, Rule: Rule{ID: ra.ID}}
		if len(keep) >= 2 {
			next := ra.Clone()
			next.Resources = keep
			op = RuleOp{Kind: OpUpdate, Rule: next}
		}
		if err := w.writeOnce(ctx, normalizeOp(op)); err != nil {
			return err
		}
	}
	target := ConfinementOnly(pin)
	target.Errors = nil
	if SameRule(target, pin) {
		return nil
	}
	return w.writeOnce(ctx, normalizeOp(RuleOp{Kind: OpUpdate, Rule: *target}))
}

// removeUnrepairable removes the pin's HA resource only when the repair
// dependencies prove it safe, and otherwise keeps it and returns a retriable
// error that names the VM and the rule.
func (w *RuleWriter) removeUnrepairable(ctx context.Context, label string, pin *Rule) (string, error) {
	if pin == nil || len(pin.Resources) == 0 {
		return "confinement pin left without resources after its repair", nil
	}
	sid := pin.Resources[0]
	keep := func(why string) (string, error) {
		return "", cpierrors.Retriable("%s: confinement pin %q of %s is not in effect and could not be repaired; "+
			"the HA resource stays in place because %s", label, pin.ID, sid, why)
	}
	rd := w.Repair
	if rd.RemoveResource == nil {
		return keep("no removal path is available")
	}
	if rd.StagedIgnored != nil && rd.StagedIgnored(sid) {
		if err := rd.RemoveResource(ctx, sid); err != nil {
			return keep(fmt.Sprintf("removing the staged resource failed: %v", err))
		}
		return fmt.Sprintf("confinement pin %q could not be repaired, so the staged resource %s was removed",
			pin.ID, sid), nil
	}
	vmid, ok := sidVMID(sid)
	if !ok {
		return keep("its service ID names no VM")
	}
	if rd.DecisionUID == nil || rd.ReadState == nil {
		return keep("no decision read is known for it")
	}
	uid, ok := rd.DecisionUID(sid)
	if !ok || uid == "" {
		return keep("no decision read is known for it")
	}
	st, err := rd.ReadState(ctx, vmid)
	if err != nil {
		return keep(fmt.Sprintf("its HA state could not be read: %v", err))
	}
	if why := removalBlocker(st, sid, uid); why != "" {
		return keep(why)
	}
	if err := rd.RemoveResource(ctx, sid); err != nil {
		return keep(fmt.Sprintf("the removal failed: %v", err))
	}
	return fmt.Sprintf("confinement pin %q could not be repaired, so the resource %s was removed", pin.ID, sid), nil
}

// removalBlocker returns why the fresh state forbids removing the resource, or
// "" when it shows the same episode, a settled raw state, and an online node
// with a fresh LRM.
func removalBlocker(st *pve.HAServiceState, sid, uid string) string {
	switch {
	case st == nil || !st.Known:
		reason := "no state was read"
		if st != nil {
			reason = st.Reason
		}
		return "its HA state is unknown: " + reason
	case st.SID != sid || !st.InCRM || st.UID != uid:
		return fmt.Sprintf("its CRM episode changed since the decision read (uid %q, now %q)", uid, st.UID)
	case st.Raw != haStateStarted && st.Raw != haStateStopped:
		return fmt.Sprintf("its raw state is %q, not started or stopped", st.Raw)
	case st.NodeHealth.Health != pve.NodeHealthOnline || !st.NodeHealth.ProxyTarget || st.NodeHealth.DeadOrFenced:
		return fmt.Sprintf("node %s is not online (%s)", st.Node, st.NodeHealth.Health)
	case !st.LRMFresh:
		return fmt.Sprintf("the LRM timestamp of node %s is not fresh", st.Node)
	}
	return ""
}

// The CRM raw states the repair and the barrier read.
const (
	haStateStarted = "started"
	haStateStopped = "stopped"
	haStateError   = "error"
	haStateIgnored = "ignored"
)

// sidVMID parses vm:<vmid>.
func sidVMID(sid string) (int, bool) {
	rest, ok := strings.CutPrefix(sid, "vm:")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 0
}
