package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// vmidListFlag collects a repeatable --vmid flag.
type vmidListFlag []int64

func (v *vmidListFlag) String() string {
	if v == nil {
		return ""
	}
	return dashIDs(*v)
}

func (v *vmidListFlag) Set(s string) error {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("--vmid %q is not a positive integer", s)
	}
	*v = append(*v, n)
	return nil
}

// runStemcellRefsAdopt is `cpi stemcell-refs adopt-owners`. It stamps the
// owner tag of one live director on that director's guests that predate
// owner tags, so a director prune can count them. Without --apply it prints
// the plan and changes nothing. Exit codes follow runStemcellRefs: 0 for
// success, 1 for runtime, config, or refusal errors and for any guest the
// apply failed on, and 2 for usage errors.
func runStemcellRefsAdopt(args []string, stdout, stderr io.Writer, opts runOptions) int {
	fs := flag.NewFlagSet("cpi stemcell-refs adopt-owners", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "CPI JSON configuration (required)")
	jsonOut := fs.Bool("json", false, "print JSON instead of text")
	uuid := fs.String("director-uuid", "", "UUID of the live director that owns the guests, from `bosh env`, "+
		"or the director_id in the state file of a create-env Director (required)")
	name := fs.String("director-name", "", "name of that director, as `bosh env` prints it (required)")
	deployment := fs.String("deployment", "", "consider only the guests of this deployment")
	var vmids vmidListFlag
	fs.Var(&vmids, "vmid", "consider only this guest; repeat for more than one")
	apply := fs.Bool("apply", false, "write the owner tags; without it the command prints the plan and changes nothing")
	if err := fs.Parse(args); err != nil {
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
	req := handlers.StemcellRefsAdoptRequest{
		DirectorUUID: strings.TrimSpace(*uuid), DirectorName: strings.TrimSpace(*name),
		Deployment: strings.TrimSpace(*deployment), VMIDs: vmids,
	}
	problem := ""
	switch {
	case fs.NArg() > 0:
		problem = "unexpected arguments: " + strings.Join(fs.Args(), " ")
	case *configPath == "":
		problem = "--config is required"
	case req.DirectorUUID == "":
		problem = "--director-uuid is required"
	case req.DirectorName == "":
		problem = "--director-name is required"
	default:
		if err := handlers.ValidateStemcellRefsAdoptRequest(req); err != nil {
			problem = err.Error()
		}
	}
	if problem != "" {
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
	plan, err := handlers.PlanStemcellRefsAdopt(scan, req)
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: %s\n", err)
		return 2
	}
	warnStemcellRefsAdopt(stderr, plan)
	if !*apply {
		return writeStemcellRefsAdoptPlan(stdout, stderr, plan, *jsonOut)
	}
	result, err := handlers.ApplyStemcellRefsAdopt(ctx, deps, plan)
	if errors.Is(err, handlers.ErrStemcellRefsAdoptRefused) {
		if *jsonOut {
			encodeStemcellRefsJSON(stdout, stderr, result)
			fmt.Fprintln(stderr, "refused: "+plan.RefusalReason)
			return 1
		}
		writeStemcellRefsAdoptPlan(stdout, stderr, plan, false)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "stemcell-refs: %s\n", err)
		return 1
	}
	return writeStemcellRefsAdoptApply(stdout, stderr, result, *jsonOut)
}

// warnStemcellRefsAdopt warns on stderr when the plan stamps nothing, which
// usually means the director name was mistyped, and when guests of the
// director are skipped, because those keep ownership unknown.
func warnStemcellRefsAdopt(stderr io.Writer, plan handlers.StemcellRefsAdoptPlan) {
	if plan.Refused {
		return
	}
	if len(plan.Targets) == 0 && len(plan.AlreadyOwned) == 0 && len(plan.Skipped) == 0 {
		fmt.Fprintf(stderr, "warning: no guest carries %s; check the director name against `bosh env` "+
			"and the report\n", plan.DirectorTag)
	}
	if len(plan.Skipped) > 0 {
		fmt.Fprintf(stderr, "warning: %d guest(s) tagged %s are skipped and keep ownership unknown until "+
			"they carry an owner tag or are removed\n", len(plan.Skipped), plan.DirectorTag)
	}
}

func writeStemcellRefsAdoptPlan(stdout, stderr io.Writer, plan handlers.StemcellRefsAdoptPlan, jsonOut bool) int {
	code := 0
	if plan.Refused {
		code = 1
	}
	if jsonOut {
		if !encodeStemcellRefsJSON(stdout, stderr, plan) {
			return 1
		}
	} else {
		fmt.Fprintln(stdout, "plan (dry run unless --apply):")
		writeStemcellRefsAdoptPlanBody(stdout, stderr, plan)
	}
	if plan.Refused {
		fmt.Fprintln(stderr, "refused: "+plan.RefusalReason)
	}
	return code
}

// writeStemcellRefsAdoptPlanBody prints the pair the plan stamps, the target
// guests grouped by deployment, and the guests it leaves alone and why. The
// dry run and the text apply both print it.
func writeStemcellRefsAdoptPlanBody(stdout, stderr io.Writer, plan handlers.StemcellRefsAdoptPlan) {
	fmt.Fprintf(stdout, "  stamps %s on guests tagged %s", plan.OwnerTag, plan.DirectorTag)
	if plan.DeploymentTag != "" {
		fmt.Fprintf(stdout, " and %s", plan.DeploymentTag)
	}
	if len(plan.VMIDs) > 0 {
		fmt.Fprintf(stdout, ", vmids %s", dashIDs(plan.VMIDs))
	}
	fmt.Fprintln(stdout)
	if len(plan.Targets) == 0 {
		fmt.Fprintln(stdout, "  nothing to stamp")
	}
	current := ""
	for i, g := range plan.Targets {
		if i == 0 || g.Deployment != current {
			current = g.Deployment
			fmt.Fprintf(stdout, "  deployment %s:\n", refLabel(current))
		}
		fmt.Fprintf(stdout, "    stamp vmid %d (%s on %s)\n", g.VMID, g.Name, g.Node)
	}
	for _, g := range plan.AlreadyOwned {
		fmt.Fprintf(stdout, "  already owned: vmid %d (%s on %s), deployment %s\n", g.VMID, g.Name, g.Node,
			refLabel(g.Deployment))
	}
	for _, g := range plan.Skipped {
		fmt.Fprintf(stdout, "  skipped: vmid %d (%s on %s), deployment %s: %s\n", g.VMID, g.Name, g.Node,
			refLabel(g.Deployment), g.Reason)
	}
	for _, g := range plan.Conflicts {
		fmt.Fprintf(stdout, "  conflict: vmid %d (%s on %s), deployment %s: %s\n", g.VMID, g.Name, g.Node,
			refLabel(g.Deployment), g.Reason)
	}
	for _, e := range plan.ScanErrors {
		fmt.Fprintln(stderr, "scan error: "+e)
	}
}

func writeStemcellRefsAdoptApply(stdout, stderr io.Writer, result handlers.StemcellRefsAdoptResult, jsonOut bool) int {
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
	writeStemcellRefsAdoptPlanBody(stdout, stderr, result.Plan)
	fmt.Fprintln(stdout, "applied:")
	if len(result.Results) == 0 {
		fmt.Fprintln(stdout, "  nothing to do")
	}
	for _, r := range result.Results {
		line := fmt.Sprintf("  vmid %d (%s on %s): %s", r.Guest.VMID, r.Guest.Name, r.Guest.Node, r.Outcome)
		if r.Error != "" {
			line += ": " + r.Error
		}
		fmt.Fprintln(stdout, line)
	}
	if result.Failures > 0 {
		fmt.Fprintf(stderr, "%d guest(s) were not stamped\n", result.Failures)
	}
	return code
}
