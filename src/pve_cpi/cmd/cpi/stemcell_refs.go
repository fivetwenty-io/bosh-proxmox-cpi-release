package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const stemcellRefsUsage = "usage: cpi stemcell-refs report --config PATH [--json]\n" +
	"       cpi stemcell-refs prune --config PATH (--director REF [--allow-owned-vms] | --vmid N... [--untracked] | " +
	"--reconcile-tags) [--apply] [--json]\n" +
	"       cpi stemcell-refs adopt-owners --config PATH --director-uuid UUID --director-name NAME " +
	"--deployment NAME[,NAME]... [--vmid N]... [--apply] [--json]"

// stemcellRefsTimeout bounds one run. A scan reads every guest's config and
// an apply can destroy several templates, each awaited, so the budget is
// generous.
const stemcellRefsTimeout = 30 * time.Minute

// runStemcellRefs is the `cpi stemcell-refs` operator command. It runs
// outside the JSON-RPC path, like storage-journal, and keeps the logic in
// package handlers: ScanStemcellRefs, PlanStemcellRefsPrune,
// ApplyStemcellRefsPrune, and BuildStemcellRefsReport. Exit codes are 0 for
// success, 1 for runtime, config, or refusal errors and for any failed
// action, and 2 for usage errors.
func runStemcellRefs(args []string, stdout, stderr io.Writer, opts runOptions) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, stemcellRefsUsage)
		return 2
	}
	action := args[0]
	if action == "adopt-owners" {
		return runStemcellRefsAdopt(args[1:], stdout, stderr, opts)
	}
	if action != "report" && action != "prune" {
		fmt.Fprintf(stderr, "unknown stemcell-refs action %q\n", action)
		fmt.Fprintln(stderr, stemcellRefsUsage)
		return 2
	}
	fs := flag.NewFlagSet("cpi stemcell-refs "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "CPI JSON configuration (required)")
	jsonOut := fs.Bool("json", false, "print JSON instead of text")
	var director *string
	var vmids vmidListFlag
	var apply, allowOwned, reconcile, untracked *bool
	if action == "prune" {
		director = fs.String("director", "", "director ref to remove from every stemcell cache template")
		fs.Var(&vmids, "vmid", "VMID of an unreferenced stemcell cache template to destroy; repeat for more than one")
		untracked = fs.Bool("untracked", false,
			"with --vmid, destroy untracked templates (provenance JSON, no generation marker) that hold no ref")
		reconcile = fs.Bool("reconcile-tags", false,
			"remove every director-- tag that no description ref backs, on every stemcell cache template")
		apply = fs.Bool("apply", false, "make the changes; without it the command prints the plan and changes nothing")
		allowOwned = fs.Bool("allow-owned-vms", false, "prune the ref even though VMs it owns still exist; never overrides unknown ownership")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, stemcellRefsUsage)
			fs.SetOutput(stderr)
			fs.PrintDefaults()
			return 0
		}
		fmt.Fprintln(stderr, "invalid stemcell-refs flags: "+err.Error())
		fmt.Fprintln(stderr, stemcellRefsUsage)
		return 2
	}
	flags := stemcellRefsPruneFlags{director: director, allowOwned: allowOwned, reconcile: reconcile, untracked: untracked}
	if problem := stemcellRefsUsageProblem(action, fs, *configPath, flags); problem != "" {
		fmt.Fprintln(stderr, problem)
		fmt.Fprintln(stderr, stemcellRefsUsage)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), stemcellRefsTimeout)
	defer cancel()
	deps, scan, ok := openStemcellRefs(ctx, *configPath, stderr, opts)
	if !ok {
		return 1
	}
	if action == "report" {
		return writeStemcellRefsReport(stdout, stderr, handlers.BuildStemcellRefsReportFor(scan, *configPath), *jsonOut)
	}

	req := handlers.StemcellRefsPruneRequest{
		DirectorRef: strings.TrimSpace(*director), VMIDs: vmids, AllowOwnedVMs: *allowOwned, ReconcileTags: *reconcile,
		Untracked: *untracked, ConfigPath: *configPath,
	}
	plan, err := handlers.PlanStemcellRefsPrune(scan, req)
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: %s\n", err)
		return 2
	}
	warnStemcellRefsNoMatch(stderr, plan)
	if req.Untracked {
		// The dry run and the apply both show what each destroy frees.
		handlers.MeasureStemcellRefsPlan(ctx, deps, &plan)
	}
	if !*apply {
		return writeStemcellRefsPlan(stdout, stderr, plan, *jsonOut)
	}
	result, err := handlers.ApplyStemcellRefsPrune(ctx, deps, plan)
	if errors.Is(err, handlers.ErrStemcellRefsPruneRefused) {
		// A refused plan changes nothing. JSON output keeps the apply
		// result's shape, with refused set; text output prints the plan so
		// the operator sees why.
		if *jsonOut {
			encodeStemcellRefsJSON(stdout, stderr, result)
			fmt.Fprintln(stderr, "refused: "+plan.RefusalReason)
			return 1
		}
		writeStemcellRefsPlan(stdout, stderr, plan, false)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: %s\n", err)
		return 1
	}
	return writeStemcellRefsApply(stdout, stderr, result, *jsonOut)
}

// openStemcellRefs loads the CPI config, builds the PVE client, and scans the
// cluster, which every stemcell-refs action starts with. It prints the reason
// on stderr and returns false when any step fails, so the caller exits 1.
func openStemcellRefs(
	ctx context.Context, configPath string, stderr io.Writer, opts runOptions,
) (handlers.Deps, handlers.StemcellRefsScan, bool) {
	cfg, err := config.LoadFile(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: config load failed: %s\n", err)
		return handlers.Deps{}, handlers.StemcellRefsScan{}, false
	}
	// The retry curves set the per-VMID lock's TTL and wait.
	applyRetryCurves(cfg)
	logger := log.NewNopLogger()
	factory := opts.ClientFactory
	if factory == nil {
		factory = pve.NewClientWithTracer
	}
	client, err := factory(cfg, logger, nil)
	if err != nil || client == nil {
		fmt.Fprintf(stderr, "stemcell-refs: PVE client unavailable: %v\n", err)
		return handlers.Deps{}, handlers.StemcellRefsScan{}, false
	}
	deps := handlers.Deps{Config: cfg, PVE: client, Logger: logger}
	scan, err := handlers.ScanStemcellRefs(ctx, deps)
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: cluster scan failed: %s\n", err)
		return handlers.Deps{}, handlers.StemcellRefsScan{}, false
	}
	return deps, scan, true
}

// warnStemcellRefsNoMatch warns on stderr when a --director prune matches no
// template, which usually means the ref was mistyped.
func warnStemcellRefsNoMatch(stderr io.Writer, plan handlers.StemcellRefsPlan) {
	if plan.Mode == "director" && len(plan.Actions) == 0 {
		fmt.Fprintf(stderr, "warning: no stemcell cache template holds the ref %q or carries the tag %s; "+
			"check the spelling against the report\n", plan.DirectorRef, plan.DirectorTag)
	}
}

// stemcellRefsPruneFlags holds the prune-only flags; each is nil for report.
type stemcellRefsPruneFlags struct {
	director                         *string
	allowOwned, reconcile, untracked *bool
}

// stemcellRefsUsageProblem returns the usage error in the parsed flags, or "".
// A --vmid that is not a positive integer already failed the parse.
func stemcellRefsUsageProblem(action string, fs *flag.FlagSet, configPath string, flags stemcellRefsPruneFlags) string {
	if fs.NArg() > 0 {
		return fmt.Sprintf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if configPath == "" {
		return "--config is required"
	}
	if action != "prune" {
		return ""
	}
	directorSet, vmidSet := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "director":
			directorSet = true
		case "vmid":
			vmidSet = true
		}
	})
	switch {
	case *flags.untracked && (directorSet || *flags.reconcile):
		return "--untracked applies only to --vmid, not to --director or --reconcile-tags"
	case *flags.untracked && !vmidSet:
		return "--untracked needs --vmid"
	case *flags.reconcile && (directorSet || vmidSet):
		return "--reconcile-tags is mutually exclusive with --director and --vmid"
	case *flags.reconcile && *flags.allowOwned:
		return "--allow-owned-vms applies only to --director"
	case *flags.reconcile:
		return ""
	case directorSet && vmidSet:
		return "--director and --vmid are mutually exclusive"
	case !directorSet && !vmidSet:
		return "prune needs --director, --vmid, or --reconcile-tags"
	case directorSet && strings.TrimSpace(*flags.director) == "":
		return "--director must name a non-empty ref"
	case vmidSet && *flags.allowOwned:
		return "--allow-owned-vms applies only to --director"
	}
	return ""
}

func encodeStemcellRefsJSON(stdout, stderr io.Writer, v any) bool {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: output could not be written: %s\n", err)
		return false
	}
	return true
}

func writeStemcellRefsReport(stdout, stderr io.Writer, rep handlers.StemcellRefsReport, jsonOut bool) int {
	code := 0
	if !rep.CloneScanComplete {
		code = 1
	}
	if jsonOut {
		if !encodeStemcellRefsJSON(stdout, stderr, rep) {
			return 1
		}
		return code
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TEMPLATES")
	fmt.Fprintln(tw, "NODE\tVMID\tNAME\tSHA8\tREFS\tDIRECTOR TAGS\tDISAGREE\tLINKED CLONES\tNOTES")
	for i := range rep.Templates {
		t := &rep.Templates[i]
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Node, t.VMID, t.Name, dash(t.SHA8), dashList(t.DescriptionRefs), dashList(t.DirectorTags),
			yesNo(t.RefsTagsDisagree), dashIDs(t.LinkedClones), templateNotes(t))
	}
	if rep.DescriptionsNearLimit > 0 {
		fmt.Fprintf(tw, "%d template(s) have a description within 1024 bytes of PVE's 8192-byte limit; "+
			"prune their stale refs before a registration fails.\n", rep.DescriptionsNearLimit)
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "REFS")
	fmt.Fprintln(tw, "REF\tTEMPLATES\tTAG ONLY\tOWNED VMS\tWOULD FREE\tBLOCKED\tKEPT REPLICAS\tNOTES")
	for i := range rep.Refs {
		r := &rep.Refs[i]
		kept := make([]int64, 0, len(r.KeptReplicas))
		for _, k := range r.KeptReplicas {
			kept = append(kept, k.VMID)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", refLabel(r.Ref), dashIDs(r.DescriptionTemplates),
			dashIDs(r.TagOnlyTemplates), ownedCount(r.OwnedVMCount), wouldFree(r), dashIDs(r.Blocked),
			dashIDs(kept), dash(r.Note))
	}
	if rep.OwnershipUnknown {
		fmt.Fprintln(tw, "WOULD FREE reads blocked because ownership is unknown; every --director prune refuses, "+
			"with or without --allow-owned-vms, until every guest under OWNERSHIP UNKNOWN carries an owner tag.")
	} else {
		fmt.Fprintln(tw, "WOULD FREE assumes --allow-owned-vms; without it a prune refuses while OWNED VMS is non-zero or unknown.")
	}
	for i := range rep.Refs {
		for _, k := range rep.Refs[i].KeptReplicas {
			fmt.Fprintf(tw, "a prune of %s %s\n", refLabel(rep.Refs[i].Ref), keptReplicaText(k))
		}
	}
	fmt.Fprintln(tw)
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: output could not be written: %s\n", err)
		return 1
	}
	writeStemcellRefsUncovered(stdout, rep)
	fmt.Fprintln(tw, "PRUNE --vmid CANDIDATES (untracked ones need --untracked)")
	fmt.Fprintln(tw, "NODE\tVMID\tNAME\tFREEABLE\tFREE WITH\tNOTES")
	for _, u := range rep.Unreferenced {
		var notes []string
		if u.Replica {
			notes = append(notes, "replica")
		}
		if u.Legacy {
			notes = append(notes, "legacy")
		}
		if u.RefsUnknown {
			notes = append(notes, "refs unknown")
		}
		if u.Untracked {
			notes = append(notes, "untracked")
		}
		if u.Reason != "" {
			notes = append(notes, u.Reason)
		}
		freeWith := u.FreeCommand
		if freeWith == "" && len(u.BlockingClones) > 0 {
			freeWith = "blocked by linked clones " + dashIDs(u.BlockingClones)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", u.Node, u.VMID, u.Name, yesNo(u.Freeable), dash(freeWith),
			dash(strings.Join(notes, " ")))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: output could not be written: %s\n", err)
		return 1
	}
	for _, e := range rep.ScanErrors {
		fmt.Fprintln(stderr, "scan error: "+e)
	}
	if !rep.CloneScanComplete {
		fmt.Fprintln(stderr, "the linked-clone scan is incomplete; a prune will block every destroy until every guest config reads")
	}
	return code
}

func writeStemcellRefsPlan(stdout, stderr io.Writer, plan handlers.StemcellRefsPlan, jsonOut bool) int {
	code := 0
	if plan.HasFailures() {
		code = 1
	}
	if jsonOut {
		if !encodeStemcellRefsJSON(stdout, stderr, plan) {
			return 1
		}
		if plan.Refused {
			fmt.Fprintln(stderr, "refused: "+plan.RefusalReason)
		}
		return code
	}
	fmt.Fprintln(stdout, "plan (dry run unless --apply):")
	writeStemcellRefsPlanBody(stdout, stderr, plan)
	if plan.Refused {
		fmt.Fprintln(stderr, "refused: "+plan.RefusalReason)
	}
	return code
}

// writeStemcellRefsPlanBody prints the owned VMs, the actions, and the scan
// errors of plan. The dry run and the text apply both print it, so the apply
// shows the plan it executed before its results.
func writeStemcellRefsPlanBody(stdout, stderr io.Writer, plan handlers.StemcellRefsPlan) {
	if len(plan.OwnedVMs) > 0 {
		ids := make([]int64, 0, len(plan.OwnedVMs))
		for i := range plan.OwnedVMs {
			ids = append(ids, plan.OwnedVMs[i].VMID)
		}
		fmt.Fprintf(stdout, "  owned VMs carrying %s: %s\n", plan.OwnerTag, dashIDs(ids))
	}
	switch {
	case plan.CoverageIncomplete:
		ids := uncoveredIDs(plan.Uncovered)
		fmt.Fprintf(stdout, "  owned VMs: unknown, because %d guest(s) carry no owner tag or could not be read "+
			"(%s); the report says how to deal with each\n", len(ids), dashIDs(ids))
	case plan.OwnershipUnknown:
		fmt.Fprintf(stdout, "  owned VMs: unknown, because no owner tag exists for %s\n", plan.DirectorRef)
	}
	writeStemcellRefsActions(stdout, plan.Actions)
	if plan.Mode == "untracked" {
		fmt.Fprintf(stdout, "  total provisioned size: %s\n", stemcellRefsTotal(plan.TotalBytes, plan.SizesUnknown))
	}
	for _, e := range plan.ScanErrors {
		fmt.Fprintln(stderr, "scan error: "+e)
	}
	if !plan.CloneScanComplete {
		fmt.Fprintln(stderr, "the linked-clone scan is incomplete, so every destroy is blocked")
	}
}

func writeStemcellRefsActions(w io.Writer, actions []handlers.StemcellRefsAction) {
	if len(actions) == 0 {
		fmt.Fprintln(w, "  nothing to do")
		return
	}
	for i := range actions {
		a := &actions[i]
		line := "  " + a.Kind + " " + stemcellRefsTarget(a.VMID, a.Name, a.Node)
		if a.Replica {
			line += ", replica"
		}
		if a.ResumesPending {
			line += ", resumes the pending destroy"
		}
		if a.DropTag != "" {
			line += ", removes tag " + a.DropTag
		}
		if len(a.DropTags) > 0 {
			line += ", removes tags " + strings.Join(a.DropTags, ", ")
		}
		if len(a.KeptTags) > 0 {
			line += ", keeps tags " + strings.Join(a.KeptTags, ", ")
		}
		if a.AddsCacheMarker {
			line += ", adds tag bosh-stemcell-cache"
		}
		if len(a.RemainingRefs) > 0 {
			line += ", leaves refs " + strings.Join(a.RemainingRefs, ", ")
		}
		if len(a.SweepReplicas) > 0 {
			ids := make([]string, 0, len(a.SweepReplicas))
			for _, r := range a.SweepReplicas {
				ids = append(ids, fmt.Sprintf("%d", r.VMID))
			}
			line += ", then replicas " + strings.Join(ids, ", ")
		}
		if a.Reason != "" {
			line += ": " + a.Reason
		}
		fmt.Fprintln(w, line)
		for _, k := range a.KeptReplicas {
			fmt.Fprintln(w, "    "+keptReplicaText(k))
		}
		for _, ref := range a.LegacyRefs {
			fmt.Fprintf(w, "    legacy ref %s (retired ref count, self-reference that no released CPI resolves)\n", ref)
		}
		if a.Frees != nil {
			for _, v := range a.Frees.Volumes {
				size := "provisioned size unknown"
				switch {
				case v.SizeBytes != nil:
					size = "provisioned size " + humanBytes(*v.SizeBytes)
				case v.SizeError != "":
					size = "provisioned size unknown: " + v.SizeError
				}
				fmt.Fprintf(w, "    frees %s %s on %s, %s\n", v.Slot, v.Volume, v.Storage, size)
			}
			fmt.Fprintf(w, "    provisioned size in all: %s\n",
				stemcellRefsTotal(a.Frees.TotalBytes, a.Frees.SizesUnknown))
		}
	}
}

// stemcellRefsTarget renders "vmid N (name on node)", leaving out whichever of
// name and node is empty, and the parenthetical when both are, as for a VMID
// that names no guest.
func stemcellRefsTarget(vmid int64, name, node string) string {
	target := fmt.Sprintf("vmid %d", vmid)
	switch {
	case name != "" && node != "":
		target += fmt.Sprintf(" (%s on %s)", name, node)
	case name != "":
		target += " (" + name + ")"
	case node != "":
		target += " (on " + node + ")"
	}
	return target
}

// stemcellRefsTotal renders a total of provisioned sizes, noting the volumes
// whose size could not be read. A destroy can give back less than this on
// thin or copy-on-write storage, where a volume uses less than its size.
func stemcellRefsTotal(total int64, unknown int) string {
	if unknown > 0 {
		return fmt.Sprintf("at least %s (%d volume size(s) unknown)", humanBytes(total), unknown)
	}
	return humanBytes(total)
}

// humanBytes renders n bytes in binary units with the exact count.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB (%d bytes)", float64(n)/float64(div), "KMGTP"[exp], n)
}

// keptReplicaText describes a replica a prune leaves in place.
func keptReplicaText(k handlers.StemcellRefsKeptReplica) string {
	freeable := "not freeable by --vmid"
	if k.FreeableByVMID {
		freeable = "freeable by --vmid afterwards"
	}
	return fmt.Sprintf("keeps replica %d (%s on %s), %s: %s", k.VMID, k.Name, k.Node, freeable, k.Reason)
}

func writeStemcellRefsApply(stdout, stderr io.Writer, result handlers.StemcellRefsApplyResult, jsonOut bool) int {
	code := 0
	if result.Failures > 0 {
		code = 1
	}
	if jsonOut {
		if !encodeStemcellRefsJSON(stdout, stderr, result) {
			return 1
		}
		return code
	}
	fmt.Fprintln(stdout, "plan:")
	writeStemcellRefsPlanBody(stdout, stderr, result.Plan)
	fmt.Fprintln(stdout, "applied:")
	if len(result.Results) == 0 {
		fmt.Fprintln(stdout, "  nothing to do")
	}
	for i := range result.Results {
		fmt.Fprintln(stdout, stemcellRefsResultLine(&result.Results[i]))
		for _, n := range result.Results[i].Notes {
			fmt.Fprintln(stdout, "    "+n)
		}
	}
	if result.Failures > 0 {
		fmt.Fprintf(stderr, "%d action(s) failed or were skipped\n", result.Failures)
	}
	return code
}

// stemcellRefsResultLine renders one apply result. It names the kind the
// apply ran, so a replica whose planned destroy became a keep reads as kept.
func stemcellRefsResultLine(r *handlers.StemcellRefsActionResult) string {
	kind := r.AppliedKind
	if kind == "" {
		kind = r.Action.Kind
	}
	line := "  " + kind + " " + stemcellRefsTarget(r.Action.VMID, r.Action.Name, r.Action.Node)
	if r.Action.Replica {
		line += ", replica"
	}
	if r.Action.ResumesPending {
		line += ", resumes the pending destroy"
	}
	line += ": " + r.Outcome
	if kind != r.Action.Kind {
		line += " (planned " + r.Action.Kind + ")"
	}
	if r.KeptReason != "" {
		line += ", kept: " + r.KeptReason
	}
	if len(r.Destroyed) > 0 {
		line += ", destroyed " + dashIDs(r.Destroyed)
	}
	switch {
	case r.TagRemoved && len(r.Action.DropTags) > 0:
		line += ", removed tags " + strings.Join(r.Action.DropTags, ", ")
	case r.TagRemoved:
		line += ", removed tag " + r.Action.DropTag
	}
	if r.CacheMarkerAdded {
		line += ", added tag bosh-stemcell-cache"
	}
	if r.Action.Reason != "" {
		line += ": " + r.Action.Reason
	}
	if r.Error != "" {
		line += ": " + r.Error
	}
	return line
}

func templateNotes(t *handlers.StemcellRefsTemplate) string {
	var notes []string
	if t.Replica {
		notes = append(notes, "replica")
	}
	if t.Legacy {
		notes = append(notes, "legacy")
	}
	if t.DestroyPending {
		notes = append(notes, "destroy-pending")
	}
	if t.DescriptionState != "provenance" {
		notes = append(notes, "description "+t.DescriptionState)
	}
	if t.RefsUnknown {
		notes = append(notes, "refs unknown")
	}
	if t.Untracked {
		notes = append(notes, "untracked")
	}
	if t.DescriptionNearLimit {
		notes = append(notes, fmt.Sprintf("description %d bytes, near the 8192-byte limit", t.DescriptionBytes))
	}
	return dashList(notes)
}

// wouldFree renders a ref's WOULD FREE column, which reads blocked while
// ownership is unknown across the cluster.
func wouldFree(r *handlers.StemcellRefsRefSummary) string {
	if r.PruneBlocked {
		return "blocked"
	}
	return dashIDs(r.WouldFree)
}

// uncoveredIDs returns every VMID in groups, sorted.
func uncoveredIDs(groups []handlers.StemcellRefsUncoveredGroup) []int64 {
	var ids []int64
	for _, g := range groups {
		ids = append(ids, g.VMIDs...)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

// writeStemcellRefsUncovered prints the OWNERSHIP UNKNOWN section of the text
// report: why ownership is unknown, then each group of BOSH guests without an
// owner tag, with the adopt-owners commands that cover it and where the
// director UUID comes from. It prints nothing while every guest is covered.
func writeStemcellRefsUncovered(w io.Writer, rep handlers.StemcellRefsReport) {
	if len(rep.Uncovered) == 0 {
		return
	}
	fmt.Fprintln(w, "OWNERSHIP UNKNOWN")
	fmt.Fprintln(w, rep.OwnershipUnknownReason)
	for _, g := range rep.Uncovered {
		fmt.Fprintf(w, "  director %s, deployment %s: vmids %s\n", refLabel(g.DirectorName), refLabel(g.Deployment),
			dashIDs(g.VMIDs))
		for _, c := range g.Commands {
			fmt.Fprintln(w, "    "+c)
		}
		fmt.Fprintln(w, "    "+g.UUIDSource)
	}
	fmt.Fprintln(w)
}

// refLabel shows a blank ref visibly in the text report.
func refLabel(ref string) string {
	if ref == "" {
		return `""`
	}
	return ref
}

// ownedCount renders an owned VM count, which is nil when it is unknown.
func ownedCount(n *int) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *n)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dashList(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ",")
}

func dashIDs(ids []int64) string {
	if len(ids) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	return strings.Join(parts, ",")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
