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

// haWidenFunc runs the CPI's replica widening pass for one Director against
// the cluster cfg names. dryRun reports what the pass would claim and writes
// nothing.
type haWidenFunc func(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner string,
	dryRun bool) (handlers.ReplicaWidenReport, error)

// productionHAWiden runs handlers.WidenAwaitingReplicaPins, the pass the CPI
// runs after each call that takes the HA rules lock, with no caps, so one run
// widens every VM that is ready.
func productionHAWiden(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner string,
	dryRun bool,
) (handlers.ReplicaWidenReport, error) {
	deps, err := haWidenDeps(cfg, logger, owner)
	if err != nil {
		return handlers.ReplicaWidenReport{}, err
	}
	return handlers.WidenAwaitingReplicaPins(ctx, deps, handlers.ReplicaWidenOptions{DryRun: dryRun})
}

// haWidenDeps returns the deps the widening pass runs with: the config, the
// logger, the trimmed Director UUID, and the CPI's own PVE client, whose pool
// service the pass's VMID claims and the HA rules lock take.
func haWidenDeps(cfg *config.CPIConfig, logger *log.Logger, owner string) (handlers.Deps, error) {
	client, err := newPVEClient(cfg, logger)
	if err != nil {
		return handlers.Deps{}, fmt.Errorf("cannot reach the cluster: %w", err)
	}
	return handlers.Deps{Config: cfg, PVE: client, Logger: logger,
		RequestDirectorUUID: strings.TrimSpace(owner)}, nil
}

// haWidenOutput is the JSON form of a widening run.
type haWidenOutput struct {
	DirectorUUID string `json:"director_uuid"`
	Applied      bool   `json:"applied"`
	handlers.ReplicaWidenReport
}

// runHAWiden implements "pve-cid ha --widen-replicas". Without apply it is a
// dry run that reads the cluster and claims nothing.
func runHAWiden(ctx context.Context, eff *config.CPIConfig, logger *log.Logger, owner string, apply,
	asJSON bool, stdout, stderr io.Writer, deps haDeps,
) int {
	if owner == "" {
		_, _ = fmt.Fprintln(stderr, "pve-cid ha: --widen-replicas needs the Director's UUID from --request or "+
			"--director-uuid, because it widens only the pins that Director owns")
		_, _ = fmt.Fprintln(stderr, haUsage)
		return exitUsage
	}
	report, err := deps.widen(ctx, eff, logger, owner, !apply)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-cid ha: the replica widening pass did not run: %v\n", err)
		return exitError
	}
	if asJSON {
		out := haWidenOutput{DirectorUUID: owner, Applied: apply, ReplicaWidenReport: report}
		if out.Results == nil {
			out.Results = []handlers.ReplicaWidenResult{}
		}
		if code := writeJSON(stdout, stderr, out); code != exitOK {
			return code
		}
	} else {
		writeHAWidenText(stdout, owner, apply, report)
	}
	if report.Count(handlers.ReplicaWidenFailed) > 0 || report.Halted != "" {
		return exitError
	}
	return exitOK
}

func writeHAWidenText(w io.Writer, owner string, apply bool, report handlers.ReplicaWidenReport) {
	mode := "a dry run, so nothing was written; add --apply to widen"
	if apply {
		mode = "applied"
	}
	_, _ = fmt.Fprintf(w, "Replica pin widening for Director %s (%s)\n", owner, mode)
	if len(report.Results) == 0 {
		_, _ = fmt.Fprintln(w, "  No replicate VM of this Director has a pin that names its source node alone.")
	}
	for _, r := range report.Results {
		target := ""
		if r.Partner != "" {
			target = " to " + r.Partner
		}
		_, _ = fmt.Fprintf(w, "  VM %d on %s: %s%s, because %s\n", r.VMID, r.Node,
			haWidenOutcomeText(r.Outcome), target, r.Reason)
	}
	_, _ = fmt.Fprintf(w, "%d widened, %d would widen, %d skipped, %d failed, and %d deferred\n",
		report.Count(handlers.ReplicaWidened), report.Count(handlers.ReplicaWouldWiden),
		report.Count(handlers.ReplicaWidenSkipped), report.Count(handlers.ReplicaWidenFailed), report.Deferred)
	if report.Halted != "" {
		_, _ = fmt.Fprintf(w, "The pass stopped early: %s\n", report.Halted)
	}
}

func haWidenOutcomeText(o handlers.ReplicaWidenOutcome) string {
	switch o {
	case handlers.ReplicaWidened:
		return "widened"
	case handlers.ReplicaWouldWiden:
		return "would widen"
	case handlers.ReplicaWidenSkipped:
		return "skipped"
	case handlers.ReplicaWidenFailed:
		return "failed"
	}
	return string(o)
}
