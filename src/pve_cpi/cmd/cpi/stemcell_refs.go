package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const stemcellRefsUsage = "usage: cpi stemcell-refs report --config PATH [--json]\n" +
	"       cpi stemcell-refs prune --config PATH (--director REF [--allow-owned-vms] | --vmid N) [--apply] [--json]"

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
	var vmid *int64
	var apply, allowOwned *bool
	if action == "prune" {
		director = fs.String("director", "", "director ref to remove from every stemcell cache template")
		vmid = fs.Int64("vmid", 0, "VMID of one unreferenced stemcell cache template to destroy")
		apply = fs.Bool("apply", false, "make the changes; without it the command prints the plan and changes nothing")
		allowOwned = fs.Bool("allow-owned-vms", false, "prune the ref even though non-template VMs still carry its owner tag")
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
	if problem := stemcellRefsUsageProblem(action, fs, *configPath, director, vmid, allowOwned); problem != "" {
		fmt.Fprintln(stderr, problem)
		fmt.Fprintln(stderr, stemcellRefsUsage)
		return 2
	}

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: config load failed: %s\n", err)
		return 1
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
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), stemcellRefsTimeout)
	defer cancel()
	deps := handlers.Deps{Config: cfg, PVE: client, Logger: logger}

	scan, err := handlers.ScanStemcellRefs(ctx, deps)
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: cluster scan failed: %s\n", err)
		return 1
	}
	if action == "report" {
		return writeStemcellRefsReport(stdout, stderr, handlers.BuildStemcellRefsReport(scan), *jsonOut)
	}

	req := handlers.StemcellRefsPruneRequest{
		DirectorRef: strings.TrimSpace(*director), VMID: *vmid, AllowOwnedVMs: *allowOwned,
	}
	plan, err := handlers.PlanStemcellRefsPrune(scan, req)
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: %s\n", err)
		return 2
	}
	warnStemcellRefsNoMatch(stderr, plan)
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

// warnStemcellRefsNoMatch warns on stderr when a --director prune matches no
// template, which usually means the ref was mistyped.
func warnStemcellRefsNoMatch(stderr io.Writer, plan handlers.StemcellRefsPlan) {
	if plan.Mode == "director" && len(plan.Actions) == 0 {
		fmt.Fprintf(stderr, "warning: no stemcell cache template holds the ref %q or carries the tag %s; "+
			"check the spelling against the report\n", plan.DirectorRef, plan.DirectorTag)
	}
}

// stemcellRefsUsageProblem returns the usage error in the parsed flags, or "".
func stemcellRefsUsageProblem(action string, fs *flag.FlagSet, configPath string, director *string, vmid *int64, allowOwned *bool) string {
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
	case directorSet && vmidSet:
		return "--director and --vmid are mutually exclusive"
	case !directorSet && !vmidSet:
		return "prune needs --director or --vmid"
	case directorSet && strings.TrimSpace(*director) == "":
		return "--director must name a non-empty ref"
	case vmidSet && *vmid <= 0:
		return "--vmid must be a positive integer"
	case vmidSet && *allowOwned:
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
			dashIDs(r.TagOnlyTemplates), ownedCount(r.OwnedVMCount), dashIDs(r.WouldFree), dashIDs(r.Blocked),
			dashIDs(kept), dash(r.Note))
	}
	fmt.Fprintln(tw, "WOULD FREE assumes --allow-owned-vms; without it a prune refuses while OWNED VMS is non-zero or unknown.")
	for i := range rep.Refs {
		for _, k := range rep.Refs[i].KeptReplicas {
			fmt.Fprintf(tw, "a prune of %s %s\n", refLabel(rep.Refs[i].Ref), keptReplicaText(k))
		}
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "UNREFERENCED (prune --vmid)")
	fmt.Fprintln(tw, "NODE\tVMID\tNAME\tFREEABLE\tNOTES")
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
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", u.Node, u.VMID, u.Name, yesNo(u.Freeable), dash(strings.Join(notes, " ")))
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
		for _, g := range plan.OwnedVMs {
			ids = append(ids, g.VMID)
		}
		fmt.Fprintf(stdout, "  owned VMs carrying %s: %s\n", plan.OwnerTag, dashIDs(ids))
	}
	if plan.OwnershipUnknown {
		fmt.Fprintf(stdout, "  owned VMs: unknown, because no owner tag exists for %s\n", plan.DirectorRef)
	}
	writeStemcellRefsActions(stdout, plan.Actions)
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
		line := fmt.Sprintf("  %s vmid %d (%s on %s)", a.Kind, a.VMID, a.Name, a.Node)
		if a.Replica {
			line += ", replica"
		}
		if a.DropTag != "" {
			line += ", removes tag " + a.DropTag
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
	}
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
	line := fmt.Sprintf("  %s vmid %d (%s on %s)", kind, r.Action.VMID, r.Action.Name, r.Action.Node)
	if r.Action.Replica {
		line += ", replica"
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
	if r.TagRemoved {
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
	return dashList(notes)
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
