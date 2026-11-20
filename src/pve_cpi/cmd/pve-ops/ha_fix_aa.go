package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// haFixAAFunc plans the anti-affinity groups of one Director again against
// the cluster cfg names, and writes the groups whose rules differ from the
// plan unless dryRun is set. rule limits the run to one group, or "" runs
// every group of the Director.
type haFixAAFunc func(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner, rule string,
	dryRun bool) ([]handlers.AntiAffinityFixResult, error)

// productionHAFixAA runs handlers.FixAntiAffinityGroups with the CPI's own PVE
// client, whose pool service the HA rules lock takes.
func productionHAFixAA(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner, rule string,
	dryRun bool,
) ([]handlers.AntiAffinityFixResult, error) {
	deps, err := haWidenDeps(cfg, logger, owner)
	if err != nil {
		return nil, err
	}
	return handlers.FixAntiAffinityGroups(ctx, deps, handlers.AntiAffinityFixOptions{DryRun: dryRun, Rule: rule})
}

// haFixAAOutput is the JSON form of a fix run.
type haFixAAOutput struct {
	DirectorUUID string                           `json:"director_uuid"`
	Applied      bool                             `json:"applied"`
	Groups       []handlers.AntiAffinityFixResult `json:"groups"`
}

// runHAFixAA implements "pve-ops ha --fix-anti-affinity". Without apply it is
// a dry run that reads the cluster and writes nothing.
func runHAFixAA(ctx context.Context, eff *config.CPIConfig, logger *log.Logger, owner, rule string, apply,
	asJSON bool, stdout, stderr io.Writer, deps haDeps,
) int {
	if owner == "" {
		_, _ = fmt.Fprintln(stderr, "pve-ops ha: --fix-anti-affinity needs the Director's UUID from --request or "+
			"--director-uuid, because it writes only the rules that Director owns")
		_, _ = fmt.Fprintln(stderr, haUsage)
		return exitUsage
	}
	results, err := deps.fixAA(ctx, eff, logger, owner, rule, !apply)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-ops ha: the anti-affinity fix did not run: %v\n", err)
		return exitError
	}
	if asJSON {
		out := haFixAAOutput{DirectorUUID: owner, Applied: apply, Groups: results}
		if out.Groups == nil {
			out.Groups = []handlers.AntiAffinityFixResult{}
		}
		if code := writeJSON(stdout, stderr, out); code != exitOK {
			return code
		}
	} else {
		writeHAFixAAText(stdout, owner, rule, apply, results)
	}
	for i := range results {
		if o := results[i].Outcome; o == handlers.AntiAffinityFailed || o == handlers.AntiAffinityProblem {
			return exitError
		}
	}
	return exitOK
}

func writeHAFixAAText(w io.Writer, owner, rule string, apply bool, results []handlers.AntiAffinityFixResult) {
	mode := "a dry run, so nothing was written; add --apply to write"
	if apply {
		mode = "applied"
	}
	_, _ = fmt.Fprintf(w, "Anti-affinity layout for Director %s (%s)\n", owner, mode)
	if len(results) == 0 {
		if rule != "" {
			_, _ = fmt.Fprintf(w, "  No anti-affinity group of this Director has the rule name %s.\n", rule)
		} else {
			_, _ = fmt.Fprintln(w, "  This Director has no anti-affinity group with a marked rule or HA resource.")
		}
	}
	counts := map[string]int{}
	for i := range results {
		r := &results[i]
		counts[r.Outcome]++
		_, _ = fmt.Fprintf(w, "  Group %s of deployment %s (%s), %d members, pattern %s: %s\n", r.Group,
			r.Deployment, r.Rule, r.Members, r.Pattern, haFixAAOutcomeText(r.Outcome))
		for _, d := range r.Drift {
			_, _ = fmt.Fprintf(w, "    %s\n", d)
		}
		if r.Problem != "" {
			_, _ = fmt.Fprintf(w, "    The group could not be planned: %s\n", r.Problem)
		}
		if r.Error != "" {
			_, _ = fmt.Fprintf(w, "    The write failed: %s\n", r.Error)
		}
	}
	_, _ = fmt.Fprintf(w, "%d in layout, %d would be fixed, %d fixed, %d failed, and %d not planned\n",
		counts[handlers.AntiAffinityInLayout], counts[handlers.AntiAffinityWouldFix],
		counts[handlers.AntiAffinityFixed], counts[handlers.AntiAffinityFailed], counts[handlers.AntiAffinityProblem])
}

func haFixAAOutcomeText(o string) string {
	switch o {
	case handlers.AntiAffinityInLayout:
		return "the rules hold the planned layout"
	case handlers.AntiAffinityWouldFix:
		return "the rules differ from the planned layout, and the fix would write these changes"
	case handlers.AntiAffinityFixed:
		return "the planned layout was written"
	case handlers.AntiAffinityFailed:
		return "the planned layout could not be written"
	case handlers.AntiAffinityProblem:
		return "the group could not be planned"
	}
	return strings.ReplaceAll(o, "_", " ")
}
