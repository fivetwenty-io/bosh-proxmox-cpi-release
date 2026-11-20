package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// haInputLimit caps the size of a request or cpi-config file the doctor reads.
const haInputLimit = 1 << 20

const haUsage = "usage: pve-ops ha [--config PATH] [--request PATH | --cpi-config PATH] [--json]\n" +
	"       pve-ops ha --widen-replicas [--apply] [--config PATH] [--request PATH | --director-uuid UUID] [--json]"

// haDeps are the pieces of the ha subcommand a test replaces: how the base CPI
// config is loaded, how a reader is opened for an effective config, and the
// clock every age is measured on, and the replica widening pass.
type haDeps struct {
	loadConfig func(path string, stderr io.Writer) (*config.CPIConfig, *log.Logger, error)
	openReader func(cfg *config.CPIConfig, logger *log.Logger) (HAReader, error)
	now        func() time.Time
	widen      haWidenFunc
}

// productionHADeps wires the ha subcommand to the real config loader and PVE
// client.
func productionHADeps() haDeps {
	return haDeps{
		loadConfig: loadConfigAndLogger,
		openReader: func(cfg *config.CPIConfig, logger *log.Logger) (HAReader, error) {
			client, err := newPVEClient(cfg, logger)
			if err != nil {
				return nil, err
			}
			return newPVEHAReader(client, cfg, logger), nil
		},
		now:   time.Now,
		widen: productionHAWiden,
	}
}

// runHA implements "pve-ops ha".
func runHA(args []string, stdout, stderr io.Writer) int {
	return runHAWith(args, stdout, stderr, productionHADeps())
}

// runHAWith runs the doctor against the pieces in deps. The doctor reads PVE
// and nothing else, because no reader method writes. The one exception is
// --widen-replicas --apply, which runs the CPI's own replica widening pass and
// writes the node-affinity pins that pass widens.
func runHAWith(args []string, stdout, stderr io.Writer, deps haDeps) int {
	fs := flag.NewFlagSet("ha", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "CPI configuration file")
	requestPath := fs.String("request", "", "CPI request JSON file whose context selects a cpi_config entry")
	cpiConfigPath := fs.String("cpi-config", "", "BOSH cpi-config as JSON (not the rendered cpi.json), used to check every cpis entry of type pve and the exports they share")
	asJSON := fs.Bool("json", false, "JSON output")
	widenReplicas := fs.Bool("widen-replicas", false, "widen the pin of each replicate VM whose replica a verified sync covers, using the check the CPI runs (a dry run unless --apply is given)")
	apply := fs.Bool("apply", false, "with --widen-replicas, write the widened pins")
	directorUUID := fs.String("director-uuid", "", "with --widen-replicas, the UUID of the Director whose VMs the pass may widen, when no --request names it")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(stderr)
		_, _ = fmt.Fprintln(stderr, haUsage)
		fs.PrintDefaults()
		return exitOK
	} else if err != nil || fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, haUsage)
		return exitUsage
	}
	if *requestPath != "" && *cpiConfigPath != "" {
		_, _ = fmt.Fprintln(stderr, "pve-ops ha: --request and --cpi-config cannot be combined")
		_, _ = fmt.Fprintln(stderr, haUsage)
		return exitUsage
	}
	if code, ok := checkHAWidenFlags(*widenReplicas, *apply, *cpiConfigPath, *directorUUID, stderr); !ok {
		return code
	}

	base, logger, err := deps.loadConfig(*configPath, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	ctx, release := commandContext()
	defer release()
	if *cpiConfigPath != "" {
		return runHACPIConfig(ctx, *cpiConfigPath, base, logger, *asJSON, stdout, stderr, deps)
	}

	eff, owner := base, ""
	var notes []string
	if *requestPath != "" {
		reqCtx, err := readHARequestContext(*requestPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "pve-ops ha: %s\n", err)
			return exitUsage
		}
		owner = strings.TrimSpace(reqCtx.DirectorUUID)
		eff, _, notes, err = config.ApplyContextOverrides(base, reqCtx.Extra)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "pve-ops ha: the request's cpi_config properties do not apply: %s\n", err)
			return exitError
		}
	}
	if *widenReplicas {
		if named := strings.TrimSpace(*directorUUID); named != "" {
			if owner != "" && owner != named {
				_, _ = fmt.Fprintln(stderr, "pve-ops ha: --director-uuid names a different Director than the request")
				return exitUsage
			}
			owner = named
		}
		return runHAWiden(ctx, eff, logger, owner, *apply, *asJSON, stdout, stderr, deps)
	}
	reader, err := deps.openReader(eff, logger)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-ops ha: cannot reach the cluster: %v\n", err)
		return exitError
	}
	report := collectHA(ctx, newLedgerHAReader(reader, newHANodeLedger()), eff, owner, deps.now())
	for _, key := range notes {
		report.advise(haKindAdvice, key, "the request's cpi_config property %q is not one the CPI reads, so it was ignored", key)
	}
	if *asJSON {
		if code := writeJSON(stdout, stderr, report); code != exitOK {
			return code
		}
	} else {
		report.writeText(stdout, "")
	}
	if report.Problems() > 0 {
		return exitError
	}
	return exitOK
}

// checkHAWidenFlags refuses the flag combinations --widen-replicas does not
// take, and the widening flags without it.
func checkHAWidenFlags(widen, apply bool, cpiConfigPath, directorUUID string, stderr io.Writer) (int, bool) {
	switch {
	case !widen && (apply || directorUUID != ""):
		_, _ = fmt.Fprintln(stderr, "pve-ops ha: --apply and --director-uuid go with --widen-replicas")
	case widen && cpiConfigPath != "":
		_, _ = fmt.Fprintln(stderr, "pve-ops ha: --widen-replicas and --cpi-config cannot be combined; "+
			"run it once per cpis entry with that entry's --request")
	default:
		return exitOK, true
	}
	_, _ = fmt.Fprintln(stderr, haUsage)
	return exitUsage, false
}

// readHARequestContext reads the context of a CPI request file. The file is
// either a full request with a top-level context object, or a bare context
// object, so an operator can paste either what the Director sent or just the
// cpi_config properties with a director UUID.
func readHARequestContext(path string) (jsonrpc.Context, error) {
	var zero jsonrpc.Context
	data, err := readLimited(path)
	if err != nil {
		return zero, fmt.Errorf("request file: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return zero, fmt.Errorf("request file is not a JSON object: %w", err)
	}
	raw := json.RawMessage(data)
	if inner, ok := top["context"]; ok {
		raw = inner
	}
	var out jsonrpc.Context
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("request context is not a JSON object: %w", err)
	}
	return out, nil
}

// readLimited reads a file the doctor was pointed at, refusing one larger than
// haInputLimit.
func readLimited(path string) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- the operator names this file on the command line
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, haInputLimit+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > haInputLimit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, haInputLimit)
	}
	return data, nil
}
