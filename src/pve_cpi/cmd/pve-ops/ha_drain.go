package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const haDrainUsage = "usage: pve-ops ha drain <node> [--apply] [--config PATH] [--request PATH] " +
	"[--director-uuid UUID]\n" +
	"       [--settle-timeout DURATION] [--json]"

const haUndrainUsage = "usage: pve-ops ha undrain <node> [--apply] [--config PATH] [--request PATH] " +
	"[--director-uuid UUID]\n" +
	"       [--settle-timeout DURATION] [--no-rebalance] [--max-moves N] [--max-duration DURATION]\n" +
	"       [--start-threshold FRACTION] [--stop-threshold FRACTION] [--restore-only] [--json]"

// haDrainFunc runs a drain for one Director against the cluster cfg names.
type haDrainFunc func(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner string,
	opts handlers.DrainOptions) (handlers.DrainReport, error)

// haUndrainFunc runs an undrain for one Director against the cluster cfg
// names.
type haUndrainFunc func(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner string,
	opts handlers.UndrainOptions) (handlers.DrainReport, error)

// productionHADrain runs handlers.DrainHA with the CPI's own PVE client.
func productionHADrain(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner string,
	opts handlers.DrainOptions,
) (handlers.DrainReport, error) {
	deps, err := haWidenDeps(cfg, logger, owner)
	if err != nil {
		return handlers.DrainReport{}, err
	}
	return handlers.DrainHA(ctx, deps, opts)
}

// productionHAUndrain runs handlers.UndrainHA with the CPI's own PVE client.
func productionHAUndrain(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner string,
	opts handlers.UndrainOptions,
) (handlers.DrainReport, error) {
	deps, err := haWidenDeps(cfg, logger, owner)
	if err != nil {
		return handlers.DrainReport{}, err
	}
	return handlers.UndrainHA(ctx, deps, opts)
}

// haDrainFlags are the parsed flags of pve-ops ha drain or undrain.
type haDrainFlags struct {
	configPath, requestPath, directorUUID string
	asJSON                                bool
	drain                                 handlers.DrainOptions
	undrain                               handlers.UndrainOptions
}

// parseHADrainFlags parses and checks the flags of the drain or the undrain
// that command names. The node may come before or after the flags. ok is
// false when the command should exit with code, after the message it printed.
func parseHADrainFlags(command string, args []string, stdout, stderr io.Writer) (haDrainFlags, int, bool) {
	var f haDrainFlags
	op, usage := "ha "+command, haDrainUsage
	if command == handlers.UndrainCommand {
		usage = haUndrainUsage
	}
	fail := func(why string) (haDrainFlags, int, bool) {
		if why != "" {
			_, _ = fmt.Fprintf(stderr, "pve-ops %s: %s\n", op, why)
		}
		_, _ = fmt.Fprintln(stderr, usage)
		return f, exitUsage, false
	}
	for _, a := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		for _, refused := range haRebalanceRefused {
			if strings.HasPrefix(a, "-") && name == refused {
				return fail("--" + refused + " does not go with " + command)
			}
		}
	}
	fs := flag.NewFlagSet(op, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.configPath, "config", "", "CPI configuration file")
	fs.StringVar(&f.requestPath, "request", "", "CPI request JSON file whose context names the Director and "+
		"selects a cpi_config entry")
	fs.StringVar(&f.directorUUID, "director-uuid", "", "the UUID of the Director whose VMs the run acts on")
	fs.BoolVar(&f.asJSON, "json", false, "JSON output")
	var apply bool
	var settle time.Duration
	if command == handlers.DrainCommand {
		fs.BoolVar(&apply, "apply", false, "stop the VMs that can't leave (without it the command is a dry run)")
		fs.DurationVar(&settle, "settle-timeout", handlers.DrainDefaultSettleTimeout,
			"how long to wait for PVE HA to move the VMs that can leave, and for the stops")
	} else {
		o := &f.undrain.Rebalance
		fs.BoolVar(&apply, "apply", false, "start the VMs the drain stopped and rebalance (without it the "+
			"command is a dry run)")
		fs.DurationVar(&settle, "settle-timeout", handlers.UndrainDefaultSettleTimeout,
			"how long to wait for the started VMs, and the rebalance's settle timeout")
		fs.BoolVar(&f.undrain.NoRebalance, "no-rebalance", false, "start the VMs and skip the rebalance")
		fs.IntVar(&o.MaxMoves, "max-moves", handlers.RebalanceDefaultMaxMoves, "the most moves the rebalance makes")
		fs.DurationVar(&o.MaxDuration, "max-duration", handlers.RebalanceDefaultMaxDuration,
			"how long the rebalance may start moves")
		fs.Float64Var(&o.StartThreshold, "start-threshold", handlers.RebalanceDefaultStart,
			"the memory spread at which memory evening starts")
		fs.Float64Var(&o.StopThreshold, "stop-threshold", handlers.RebalanceDefaultStop,
			"the memory spread at which memory evening stops")
		fs.BoolVar(&o.RestoreOnly, "restore-only", false, "run only the separation and AZ restore phases")
	}
	var nodes []string
	for rest := args; ; {
		if err := fs.Parse(rest); errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			_, _ = fmt.Fprintln(stdout, usage)
			fs.PrintDefaults()
			return f, exitOK, false
		} else if err != nil {
			return fail(err.Error())
		}
		if fs.NArg() == 0 {
			break
		}
		nodes = append(nodes, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(nodes) != 1 || strings.TrimSpace(nodes[0]) == "" {
		return fail("name exactly one node")
	}
	node := strings.TrimSpace(nodes[0])
	if settle < handlers.RebalanceMinSettleTimeout {
		return fail("--settle-timeout must be at least 1m")
	}
	f.drain = handlers.DrainOptions{Node: node, Apply: apply, SettleTimeout: settle}
	f.undrain.Node, f.undrain.Apply, f.undrain.SettleTimeout = node, apply, settle
	if command == handlers.UndrainCommand {
		check := f.undrain.Rebalance
		check.SettleTimeout = settle
		if why := checkHARebalanceOptions(check); why != "" {
			return fail(why)
		}
	}
	return f, exitOK, true
}

// runHADrain implements "pve-ops ha drain" and "pve-ops ha undrain".
func runHADrain(command string, args []string, stdout, stderr io.Writer, deps haDeps) int {
	f, code, ok := parseHADrainFlags(command, args, stdout, stderr)
	if !ok {
		return code
	}
	usage := haDrainUsage
	if command == handlers.UndrainCommand {
		usage = haUndrainUsage
	}
	run, code, ok := resolveHAWriteRun("ha "+command, usage, f.configPath, f.requestPath, f.directorUUID, stderr,
		deps)
	if !ok {
		return code
	}
	ctx, release := commandContext()
	defer release()
	var report handlers.DrainReport
	var err error
	if command == handlers.DrainCommand {
		report, err = deps.drain(ctx, run.cfg, run.logger, run.owner, f.drain)
	} else {
		report, err = deps.undrain(ctx, run.cfg, run.logger, run.owner, f.undrain)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-ops ha %s: the run did not start: %v\n", command, err)
		return exitError
	}
	out := haDrainOutput{DirectorUUID: run.owner, Token: maskedCredential(run.cfg), Version: cliVersion,
		Config: run.configPath, DrainReport: report, tokenAuth: strings.TrimSpace(run.cfg.APIToken) != ""}
	if f.asJSON {
		if code := writeJSON(stdout, stderr, out); code != exitOK {
			return code
		}
	} else {
		writeHADrainText(stdout, &out)
	}
	return report.ExitCode
}

// haDrainOutput is the JSON form of a drain or an undrain.
type haDrainOutput struct {
	DirectorUUID string `json:"director_uuid"`
	Token        string `json:"token"`
	Version      string `json:"version"`
	Config       string `json:"config"`
	handlers.DrainReport
	// tokenAuth says Token names an API token rather than a user.
	tokenAuth bool
}

func writeHADrainText(w io.Writer, out *haDrainOutput) {
	r := &out.DrainReport
	drain := r.Command == handlers.DrainCommand
	mode := "applied"
	switch {
	case !r.Applied && drain:
		mode = "a dry run, so nothing stopped; add --apply to stop the VMs that can't leave"
	case !r.Applied:
		mode = "a dry run, so nothing started; add --apply to start the VMs and rebalance"
	}
	cluster := r.Cluster
	if cluster == "" {
		cluster = "(unnamed)"
	}
	cred := "user " + out.Token
	if out.tokenAuth {
		cred = "token " + out.Token
	}
	_, _ = fmt.Fprintf(w, "HA %s of node %s for Director %s on cluster %s (%s), %s\n", r.Command, r.Node,
		out.DirectorUUID, cluster, cred, mode)
	_, _ = fmt.Fprintf(w, "pve-ops %s with the configuration %s\n", out.Version, out.Config)
	_, _ = fmt.Fprintln(w, "Gates")
	for _, g := range r.Gates {
		state := ""
		if !g.OK {
			state = "REFUSED: "
		}
		_, _ = fmt.Fprintf(w, "  %s%s\n", state, g.Detail)
	}
	for _, warning := range r.Warnings {
		_, _ = fmt.Fprintf(w, "Warning: %s\n", warning)
	}
	if drain {
		_, _ = fmt.Fprintf(w, "VMs on %s\n", r.Node)
	} else {
		_, _ = fmt.Fprintf(w, "VMs the drain of %s stopped\n", r.Node)
	}
	if len(r.VMs) == 0 {
		_, _ = fmt.Fprintln(w, "  none")
	}
	for i := range r.VMs {
		_, _ = fmt.Fprintln(w, "  "+haDrainVMText(&r.VMs[i], drain))
	}
	writeHADrainSummary(w, r)
	if r.Reminder != "" {
		_, _ = fmt.Fprintln(w, r.Reminder)
	}
	if r.Rebalance != nil {
		_, _ = fmt.Fprintln(w)
		writeHARebalanceText(w, &haRebalanceOutput{DirectorUUID: out.DirectorUUID, Token: out.Token,
			Version: out.Version, Config: out.Config, RebalanceReport: *r.Rebalance, tokenAuth: out.tokenAuth})
	}
	switch {
	case r.Halted != "" && r.ExitCode == handlers.RebalanceExitGate:
		_, _ = fmt.Fprintf(w, "The run stopped, because the %s gate refused.\n", r.Halted)
	case r.Halted != "":
		_, _ = fmt.Fprintf(w, "The run stopped, because %s.\n", strings.TrimSuffix(r.Halted, "."))
	}
	if r.Applied && r.ExitCode == handlers.RebalanceExitError {
		_, _ = fmt.Fprintf(w, "Run the command again with --apply after the cause is fixed, and it goes on from "+
			"the drain tokens.\n")
	}
}

// haDrainVMText is one VM's line.
func haDrainVMText(vm *handlers.DrainVM, drain bool) string {
	line := fmt.Sprintf("VM %d%s", vm.VMID, haNameSuffix(vm.Name))
	if !drain {
		line += " on " + vm.Node + ", " + haDrainOutcomeText(vm.Outcome, false)
	} else {
		switch vm.Fate {
		case handlers.DrainLeaves:
			line += ", leaves for " + strings.Join(vm.Targets, " or ")
		case handlers.DrainStays:
			line += ", can't leave, because " + vm.Reason
		case handlers.DrainNotHA, handlers.DrainNotStarted:
			line += ", stays, because " + vm.Reason
		}
		if vm.Outcome != "" {
			line += ": " + haDrainOutcomeText(vm.Outcome, true)
		}
	}
	if vm.Error != "" {
		line += " (" + vm.Error + ")"
	}
	return line
}

func haDrainOutcomeText(o handlers.DrainOutcome, drain bool) string {
	switch o {
	case handlers.DrainOutcomePlanned:
		if drain {
			return "stop planned"
		}
		return "start planned"
	case handlers.DrainOutcomeStoppedEarlier:
		return "stopped by an earlier drain"
	case handlers.DrainOutcomeLeftPending:
		return "left part way"
	}
	return string(o)
}

func writeHADrainSummary(w io.Writer, r *handlers.DrainReport) {
	if r.Command == handlers.UndrainCommand {
		_, _ = fmt.Fprintf(w, "%s to start, and %d started\n", haCount(len(r.VMs), "VM", "VMs"),
			r.Count(handlers.DrainOutcomeStarted))
		return
	}
	leaves, stays := 0, 0
	for i := range r.VMs {
		switch r.VMs[i].Fate {
		case handlers.DrainLeaves:
			leaves++
		case handlers.DrainStays:
			stays++
		}
	}
	_, _ = fmt.Fprintf(w, "%s can leave, %s can't, and %d stopped\n", haCount(leaves, "VM", "VMs"),
		haCount(stays, "VM", "VMs"), r.Count(handlers.DrainOutcomeStopped)+r.Count(handlers.DrainOutcomeStoppedEarlier))
}
