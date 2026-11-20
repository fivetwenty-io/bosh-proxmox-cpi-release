package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const haRebalanceUsage = "usage: pve-ops ha rebalance [--apply] [--config PATH] " +
	"[--request PATH] [--director-uuid UUID]\n" +
	"       [--max-moves N] [--settle-timeout DURATION] [--max-duration DURATION]\n" +
	"       [--start-threshold FRACTION] [--stop-threshold FRACTION] [--restore-only] [--json]"

// exitGate is the exit code of a rebalance that a safety gate refused or
// stopped with no move left part way.
const exitGate = 3

// haRebalanceFunc runs a rebalance for one Director against the cluster cfg
// names.
type haRebalanceFunc func(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner string,
	opts handlers.RebalanceOptions) (handlers.RebalanceReport, error)

// productionHARebalance runs handlers.RebalanceHA with the CPI's own PVE
// client, whose pool service the run lock, the VMID claims, and the HA rules
// lock take.
func productionHARebalance(ctx context.Context, cfg *config.CPIConfig, logger *log.Logger, owner string,
	opts handlers.RebalanceOptions,
) (handlers.RebalanceReport, error) {
	deps, err := haWidenDeps(cfg, logger, owner)
	if err != nil {
		return handlers.RebalanceReport{}, err
	}
	return handlers.RebalanceHA(ctx, deps, opts)
}

// haRebalanceFlags are the parsed flags of pve-ops ha rebalance.
type haRebalanceFlags struct {
	configPath, requestPath, directorUUID string
	asJSON                                bool
	opts                                  handlers.RebalanceOptions
}

// haRebalanceRefused are the flags of pve-ops ha that rebalance does not take.
var haRebalanceRefused = []string{"cpi-config", "widen-replicas"}

// parseHARebalanceFlags parses and checks the flags. ok is false when the
// command should exit with code, after the message it printed.
func parseHARebalanceFlags(args []string, stdout, stderr io.Writer) (haRebalanceFlags, int, bool) {
	var f haRebalanceFlags
	for _, a := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		for _, refused := range haRebalanceRefused {
			if strings.HasPrefix(a, "-") && name == refused {
				_, _ = fmt.Fprintf(stderr, "pve-ops ha rebalance: --%s does not go with rebalance\n", refused)
				_, _ = fmt.Fprintln(stderr, haRebalanceUsage)
				return f, exitUsage, false
			}
		}
	}
	fs := flag.NewFlagSet("ha rebalance", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.configPath, "config", "", "CPI configuration file")
	fs.StringVar(&f.requestPath, "request", "", "CPI request JSON file whose context names the Director and "+
		"selects a cpi_config entry")
	fs.StringVar(&f.directorUUID, "director-uuid", "", "the UUID of the Director whose VMs the run may move")
	fs.BoolVar(&f.asJSON, "json", false, "JSON output")
	fs.BoolVar(&f.opts.Apply, "apply", false, "make the moves (without it the command is a dry run)")
	fs.IntVar(&f.opts.MaxMoves, "max-moves", handlers.RebalanceDefaultMaxMoves, "the most moves one run makes")
	fs.DurationVar(&f.opts.SettleTimeout, "settle-timeout", handlers.RebalanceDefaultSettleTimeout,
		"how long to wait for each move, and the cluster, to settle")
	fs.DurationVar(&f.opts.MaxDuration, "max-duration", handlers.RebalanceDefaultMaxDuration,
		"how long the run may start moves")
	fs.Float64Var(&f.opts.StartThreshold, "start-threshold", handlers.RebalanceDefaultStart,
		"the memory spread at which memory evening starts")
	fs.Float64Var(&f.opts.StopThreshold, "stop-threshold", handlers.RebalanceDefaultStop,
		"the memory spread at which memory evening stops")
	fs.BoolVar(&f.opts.RestoreOnly, "restore-only", false, "run only the separation and AZ restore phases")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(stdout)
		_, _ = fmt.Fprintln(stdout, haRebalanceUsage)
		fs.PrintDefaults()
		return f, exitOK, false
	} else if err != nil || fs.NArg() != 0 {
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "pve-ops ha rebalance: %v\n", err)
		}
		_, _ = fmt.Fprintln(stderr, haRebalanceUsage)
		return f, exitUsage, false
	}
	if why := checkHARebalanceOptions(f.opts); why != "" {
		_, _ = fmt.Fprintf(stderr, "pve-ops ha rebalance: %s\n", why)
		_, _ = fmt.Fprintln(stderr, haRebalanceUsage)
		return f, exitUsage, false
	}
	return f, exitOK, true
}

// checkHARebalanceOptions returns why the options are out of bounds, or "".
func checkHARebalanceOptions(o handlers.RebalanceOptions) string {
	switch {
	case o.MaxMoves < 1:
		return "--max-moves must be at least 1"
	case o.SettleTimeout < handlers.RebalanceMinSettleTimeout:
		return "--settle-timeout must be at least 1m"
	case o.MaxDuration <= 0:
		return "--max-duration must be positive"
	case math.IsNaN(o.StartThreshold) || o.StartThreshold <= 0 || o.StartThreshold > 1:
		return "--start-threshold must be above 0 and at most 1"
	case math.IsNaN(o.StopThreshold) || o.StopThreshold < 0:
		return "--stop-threshold must not be negative"
	case o.StopThreshold >= o.StartThreshold:
		return "--stop-threshold must be below --start-threshold"
	}
	return ""
}

// runHARebalance implements "pve-ops ha rebalance".
func runHARebalance(args []string, stdout, stderr io.Writer, deps haDeps) int {
	f, code, ok := parseHARebalanceFlags(args, stdout, stderr)
	if !ok {
		return code
	}
	owner := strings.TrimSpace(f.directorUUID)
	var extra map[string]any
	if f.requestPath != "" {
		reqCtx, err := readHARequestContext(f.requestPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "pve-ops ha rebalance: %s\n", err)
			return exitUsage
		}
		named := strings.TrimSpace(reqCtx.DirectorUUID)
		if owner != "" && named != "" && owner != named {
			_, _ = fmt.Fprintln(stderr, "pve-ops ha rebalance: --director-uuid names a different Director than "+
				"the request")
			return exitUsage
		}
		if owner == "" {
			owner = named
		}
		extra = reqCtx.Extra
	}
	if owner == "" {
		_, _ = fmt.Fprintln(stderr, "pve-ops ha rebalance: the Director's UUID must come from --request or "+
			"--director-uuid, because the run moves only the VMs that Director owns")
		_, _ = fmt.Fprintln(stderr, haRebalanceUsage)
		return exitUsage
	}
	configPath := resolveConfigPath(f.configPath)
	base, logger, err := deps.loadConfig(configPath, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	eff := base
	if extra != nil {
		if eff, _, _, err = config.ApplyContextOverrides(base, extra); err != nil {
			_, _ = fmt.Fprintf(stderr, "pve-ops ha rebalance: the request's cpi_config properties do not "+
				"apply: %s\n", err)
			return exitError
		}
	}
	ctx, release := commandContext()
	defer release()
	report, err := deps.rebalance(ctx, eff, logger, owner, f.opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-ops ha rebalance: the run did not start: %v\n", err)
		return exitError
	}
	out := haRebalanceOutput{DirectorUUID: owner, Token: maskedCredential(eff), Version: cliVersion,
		Config: configPath, RebalanceReport: report, tokenAuth: strings.TrimSpace(eff.APIToken) != ""}
	if f.asJSON {
		if code := writeJSON(stdout, stderr, out); code != exitOK {
			return code
		}
	} else {
		writeHARebalanceText(stdout, &out)
	}
	return report.ExitCode
}

// haRebalanceOutput is the JSON form of a rebalance run.
type haRebalanceOutput struct {
	DirectorUUID string `json:"director_uuid"`
	Token        string `json:"token"`
	Version      string `json:"version"`
	Config       string `json:"config"`
	handlers.RebalanceReport
	// tokenAuth says Token names an API token rather than a user.
	tokenAuth bool
}

// maskedCredential names the credential the run uses without its secret. A
// token reads as user@realm!****, and a user as user@realm.
func maskedCredential(cfg *config.CPIConfig) string {
	if cfg == nil {
		return ""
	}
	if token := strings.TrimSpace(cfg.APIToken); token != "" {
		user, _, found := strings.Cut(token, "!")
		if !found || user == "" {
			return "****"
		}
		return user + "!****"
	}
	user := strings.TrimSpace(cfg.User)
	if user == "" || strings.Contains(user, "@") {
		return user
	}
	realm := cfg.Realm
	if realm == "" {
		realm = "pam"
	}
	return user + "@" + realm
}

func writeHARebalanceText(w io.Writer, out *haRebalanceOutput) {
	r := &out.RebalanceReport
	mode := "a dry run, so nothing moved; add --apply to move"
	if r.Applied {
		mode = "applied"
	}
	cluster := r.Cluster
	if cluster == "" {
		cluster = "(unnamed)"
	}
	cred := "user " + out.Token
	if out.tokenAuth {
		cred = "token " + out.Token
	}
	_, _ = fmt.Fprintf(w, "HA rebalance for Director %s on cluster %s (%s), %s\n", out.DirectorUUID, cluster,
		cred, mode)
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
	if len(r.Nodes) > 0 {
		_, _ = fmt.Fprintln(w, "Nodes, memory committed now and after the plan")
		width := 0
		for _, n := range r.Nodes {
			width = max(width, len(n.Node))
		}
		for _, n := range r.Nodes {
			_, _ = fmt.Fprintf(w, "  %-*s  %3d%%  %3d%%\n", width, n.Node, haPercent(n.CommittedBefore),
				haPercent(n.CommittedAfter))
		}
	}
	if len(r.Resumed) > 0 {
		_, _ = fmt.Fprintln(w, "Moves of an earlier run")
		for _, m := range r.Resumed {
			line := fmt.Sprintf("  VM %d on %s, marked to move to %s: %s, %s", m.VMID, m.Node, m.Target, m.Action,
				haRebalanceOutcomeText(m.Outcome))
			if m.Error != "" {
				line += ", " + m.Error
			}
			_, _ = fmt.Fprintln(w, line)
		}
	}
	if r.Applied && len(r.Planned) > 0 {
		_, _ = fmt.Fprintln(w, "Plan")
		writeHARebalanceMoves(w, r.Planned, false)
	}
	_, _ = fmt.Fprintln(w, "Moves")
	if len(r.Moves) == 0 {
		_, _ = fmt.Fprintln(w, "  none")
	}
	writeHARebalanceMoves(w, r.Moves, r.Applied)
	if len(r.Skipped) > 0 {
		_, _ = fmt.Fprintln(w, "Skipped")
		for _, s := range r.Skipped {
			_, _ = fmt.Fprintf(w, "  VM %d%s on %s, because %s\n", s.VMID, haNameSuffix(s.Name), s.Node, s.Reason)
		}
	}
	writeHARebalanceSummary(w, r)
}

func writeHARebalanceMoves(w io.Writer, moves []handlers.RebalanceMove, applied bool) {
	for i := range moves {
		m := &moves[i]
		line := fmt.Sprintf("  %d. VM %d%s from %s to %s", i+1, m.VMID, haNameSuffix(m.Name), m.From, m.To)
		if applied {
			line += fmt.Sprintf(", %s in %.1fs", haRebalanceOutcomeText(m.Outcome), m.Seconds)
		}
		line += ", because " + m.Reason
		if m.Error != "" {
			line += " (" + m.Error + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

func writeHARebalanceSummary(w io.Writer, r *handlers.RebalanceReport) {
	planned := len(r.Moves)
	if r.Applied {
		planned = len(r.Planned)
	}
	made := r.Count(handlers.RebalanceMoved)
	_, _ = fmt.Fprintf(w, "%s planned, %d made, %s skipped, and %d deferred, and the spread goes from "+
		"%d points to %d\n", haCount(planned, "move", "moves"), made, haCount(len(r.Skipped), "VM", "VMs"),
		r.Deferred, haPercent(r.SpreadBefore), haPercent(r.SpreadAfter))
	if r.DiffersFromPlan {
		_, _ = fmt.Fprintln(w, "The moves made differ from the first plan, because the run planned again "+
			"after each move.")
	}
	switch {
	case r.Halted != "" && r.ExitCode == handlers.RebalanceExitGate:
		_, _ = fmt.Fprintf(w, "The run stopped, because the %s gate refused.\n", r.Halted)
	case r.Halted != "":
		_, _ = fmt.Fprintf(w, "The run stopped, because %s.\n", strings.TrimSuffix(r.Halted, "."))
	}
	if r.Applied && r.ExitCode == handlers.RebalanceExitError {
		_, _ = fmt.Fprintln(w, "Run the command again with --apply after the cause is fixed, and it finishes "+
			"any move left part way.")
	}
}

func haRebalanceOutcomeText(o handlers.RebalanceOutcome) string {
	if o == handlers.RebalanceLeftPending {
		return "left part way"
	}
	return string(o)
}

// haCount spells n with the singular or plural noun.
func haCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func haNameSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " (" + name + ")"
}

// haPercent turns a fraction into whole percent points.
func haPercent(f float64) int {
	return int(math.Round(f * 100))
}
