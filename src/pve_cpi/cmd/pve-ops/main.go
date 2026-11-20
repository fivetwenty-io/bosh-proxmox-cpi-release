// Package main implements pve-ops, an operator CLI for inspecting
// BOSH Proxmox CPI CIDs (cloud IDs) and correlating them against live PVE
// cluster state.
//
// pve-ops reads and does not change PVE. Every subcommand either decodes or
// encodes a CID offline, or makes read-only API calls to locate and inventory
// CPI-managed resources. There are two exceptions. "pve-ops ha
// --widen-replicas --apply" runs the CPI's own replica widening pass and
// writes the node-affinity pins it widens, "pve-ops ha rebalance --apply"
// live-migrates this Director's HA VMs back into a good spread, and
// "pve-ops ha drain --apply" and "pve-ops ha undrain --apply" stop and start
// through PVE HA the VMs that can't leave a node in maintenance. Without
// --apply every one of these commands is a dry run.
//
// # CID families
//
// The CPI emits and accepts five CID shapes (see internal/pve/stemcell_volume.go
// for the authoritative grammar table):
//
//	Stemcell  :light:<storage>:import/<file>   operator-managed qcow2
//	Stemcell  :heavy:<storage>:import/<file>   CPI-uploaded qcow2
//	Disk      pvd-<base64url(json)>            persistent-disk envelope
//	Disk      pvz-<base64url(gzip(json))>      compressed persistent-disk envelope
//	VM        <vmid>                           bare integer
//	Snapshot  <vmid>:<name>                    VMID plus PVE snapshot name
//
// # Subcommands
//
//	pve-ops decode <cid> [--json]
//	    Decode any CID family offline; no PVE API calls. Prints family,
//	    decoded fields, and (for disk CIDs) the encoded length against the
//	    varchar(255) Director column target.
//
//	pve-ops encode --volid <storage:path> [--pool P] [--node N] [--az Z]
//	                [--opt k=v ...] [--compress] [--json]
//	    Build a pvd-/pvz- disk CID envelope offline. --compress allows the
//	    gzip-compressed pvz- form when the plain form would exceed the
//	    varchar(255) target (mirrors create_disk's automatic fallback).
//
//	pve-ops locate <cid|volid> [--config PATH] [--json]
//	    Online. Accepts a disk CID (pvd-/pvz-), a raw PVE volid
//	    ("<storage>:<volume>"), or a stemcell path CID (":light:"/":heavy:").
//	    For disks: scans every cluster VM's active bus slots (scsi/virtio/
//	    ide/sata) for the holder, and separately checks every VM's
//	    bosh_attached_disks/bosh_parked_disks description sentinels (a
//	    sentinel hit with no bus-slot holder flags a stale sentinel). For
//	    stemcells: resolves the sha8 from the CID's filename, looks up
//	    per-cluster cache templates by sha tag, prints each template's
//	    director references, and reports whether the backing qcow2 still
//	    exists on the named storage.
//
//	pve-ops stemcells [--storage ID] [--node N] [--orphans] [--json]
//	                  [--config PATH]
//	    Online. Cluster-wide stemcell inventory: every bosh-stemcell-tagged
//	    cache template correlated against every bosh-stemcell-*.qcow2 file on
//	    the configured (or --storage) stemcell storage, grouped by sha8.
//	    --orphans filters to entries with zero director references, files
//	    with no correlated template (kind unknown), or templates whose
//	    backing file is gone — the "what is safe to delete" view.
//
//	pve-ops ha [--config PATH] [--request PATH | --cpi-config PATH] [--json]
//	    Online. The HA doctor. Probes the cluster even when pve.ha.mode is
//	    off, then prints the HA profile (nodes, quorum, CPU migration
//	    domains, N+1 memory headroom) and everything the CPI left that
//	    needs an operator: rule and resource problems, local-disk VMs
//	    running unprotected, replication state, stale tags, and objects that
//	    outlive an off mode. --request applies the cpi_config properties of a
//	    CPI request's context to the config first. --cpi-config reads a BOSH
//	    cpi-config converted to JSON and runs the same checks against every
//	    cpis entry of type pve (it is not the rendered cpi.json, which --config
//	    takes), then checks that clusters mounting one shared export do not overlap on
//	    VMID bands or next-id ranges. It exits 1 when any problem is found.
//
//	pve-ops ha --widen-replicas [--apply] [--config PATH] [--request PATH | --director-uuid UUID] [--json]
//	    Online. Runs the CPI's replica widening pass for one Director. The
//	    pass finds each replicate VM of that Director whose pin still names
//	    its source node alone and checks it the way reboot_vm and
//	    set_vm_metadata do, under the same locks. Without --apply it is a
//	    dry run that claims and writes nothing. With --apply it widens each
//	    pin a verified sync covers. It prints every VM it considered with
//	    the outcome and the reason, and exits 1 when a VM failed or the pass
//	    stopped early.
//
//	pve-ops ha rebalance [--apply] [--config PATH] [--request PATH] [--director-uuid UUID]
//	                     [--max-moves N] [--settle-timeout D] [--max-duration D]
//	                     [--start-threshold F] [--stop-threshold F] [--restore-only] [--json]
//	    Online. Plans the live migrations that return one Director's HA VMs
//	    to their AZs, separate anti-affinity members that share a node, and
//	    even out committed memory, and prints each move with its reason.
//	    Without --apply it is a dry run that takes no lock and writes
//	    nothing. With --apply it makes one move at a time through PVE HA and
//	    waits for each to settle. It exits 1 when a read or a move failed or
//	    a move was left part way, and 3 when a safety gate refused the run.
//
//	pve-ops ha drain <node> [--apply] [--config PATH] [--request PATH] [--director-uuid UUID]
//	                 [--settle-timeout D] [--json]
//	    Online. Decides, from PVE's HA rules and the cluster's placement
//	    facts, which of one Director's VMs on a node in HA maintenance can
//	    leave it. With --apply it waits for PVE HA to move those, then stops
//	    each VM that can't leave by setting its HA resource's state to
//	    stopped, and records the stop with a drain token on the resource. It
//	    names the deployments whose resurrection to turn off. It exits 1 when
//	    a read or a stop failed or a VM that could leave stayed, and 3 when a
//	    safety gate refused the run, such as a node not yet in maintenance.
//
//	pve-ops ha undrain <node> [--apply] [--config PATH] [--request PATH] [--director-uuid UUID]
//	                   [--settle-timeout D] [--no-rebalance] [rebalance flags] [--json]
//	    Online. Once the node's maintenance ends, starts through PVE HA each
//	    VM that carries the node's drain token, drops the token, waits for
//	    the VMs, and then runs "pve-ops ha rebalance --apply" unless
//	    --no-rebalance is given. Its exit codes are the drain's, and the
//	    rebalance's when that ended with one.
//
// # Config resolution (locate, stemcells, ha)
//
// The online subcommands load the same cpi.json the pve_cpi job runs
// against, resolved in this order: --config flag, then the PVE_CPI_CONFIG
// environment variable, then the release job's default install path
// ("/var/vcap/jobs/pve_cpi/config/cpi.json").
//
// # Exit codes
//
//	0  success
//	1  runtime error (parse failure, PVE API error, config load failure)
//	2  usage error (bad flags/arguments)
//	3  ha rebalance, drain, and undrain only: a safety gate refused or stopped the run
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
)

// cliVersion is pve-ops's own version string, independent of the cpi
// binary's internal/version package (a different compiled artifact from the
// same module). Overridable at build time via
// -ldflags "-X main.cliVersion=...".
var cliVersion = "dev"

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(runAs(filepath.Base(os.Args[0]), os.Args[1:], os.Stdout, os.Stderr))
}

// deprecatedAliasName is the former name of this command. The package still
// installs it as a symlink to pve-ops for one release.
const deprecatedAliasName = "pve-cid"

// runAs runs the CLI under the name it was invoked as. Under the deprecated
// alias it prints one notice line to stderr before anything else, then runs
// normally, so stdout and the exit codes stay the same.
func runAs(prog string, args []string, stdout, stderr io.Writer) int {
	if prog == deprecatedAliasName {
		_, _ = fmt.Fprintln(stderr, deprecatedAliasName+" is deprecated and will be removed in 0.13.0; run pve-ops instead.")
	}
	return run(args, stdout, stderr)
}

// run dispatches to the requested subcommand. Accepting args/stdout/stderr as
// parameters (rather than reading os.Args/os.Stdout/os.Stderr directly) makes
// every code path testable in-process.
//
// Every subcommand that builds a PVE client can start background cache work, a
// down mark written when a request fails over or a discovery refresh. So run
// opens the endpoint cache's worker registry and drains it before it returns,
// with the same bounded wait and signal rule as the cpi binary, and a mark
// written just before the command ends survives the exit. A subcommand that
// starts no worker never waits.
func run(args []string, stdout, stderr io.Writer) int {
	endpointcache.Open()
	defer endpointcache.DrainAtExit("pve-ops", stderr, 0)
	if len(args) == 0 {
		printUsage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "-h", "--help", "help":
		printUsage(stdout)
		return exitOK
	case "-version", "--version", "version":
		_, _ = fmt.Fprintln(stdout, "pve-ops "+cliVersion)
		return exitOK
	case "decode":
		return runDecode(args[1:], stdout, stderr)
	case "encode":
		return runEncode(args[1:], stdout, stderr)
	case "storage-plan":
		return runStoragePlan(args[1:], stdout, stderr)
	case "locate":
		return runLocate(args[1:], stdout, stderr)
	case "stemcells":
		return runStemcells(args[1:], stdout, stderr)
	case "ha":
		return runHA(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "pve-ops: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return exitUsage
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `pve-ops, the operator CLI for BOSH Proxmox CPI CIDs

Usage:
  pve-ops decode <cid> [--json]
  pve-ops encode --volid <storage:path> [--pool P] [--node N] [--az Z] [--opt k=v ...] [--compress] [--json]
  pve-ops locate <cid|volid> [--config PATH] [--json]
  pve-ops storage-plan --request PATH [--config PATH] [--json]
  pve-ops stemcells [--storage ID] [--node N] [--orphans] [--config PATH] [--json]
  pve-ops ha [--config PATH] [--request PATH | --cpi-config PATH] [--json]
  pve-ops ha --widen-replicas [--apply] [--config PATH] [--request PATH | --director-uuid UUID] [--json]
  pve-ops ha rebalance [--apply] [--config PATH] [--request PATH] [--director-uuid UUID] [--max-moves N]
                       [--settle-timeout D] [--max-duration D] [--start-threshold F] [--stop-threshold F]
                       [--restore-only] [--json]
  pve-ops ha drain <node> [--apply] [--config PATH] [--request PATH] [--director-uuid UUID]
                   [--settle-timeout D] [--json]
  pve-ops ha undrain <node> [--apply] [--config PATH] [--request PATH] [--director-uuid UUID]
                     [--settle-timeout D] [--no-rebalance] [--max-moves N] [--max-duration D]
                     [--start-threshold F] [--stop-threshold F] [--restore-only] [--json]
  pve-ops version
  pve-ops help

decode and encode are fully offline (no PVE API calls). locate, stemcells,
and ha are read-only online commands: they load the CPI's cpi.json (--config flag,
then $PVE_CPI_CONFIG, then /var/vcap/jobs/pve_cpi/config/cpi.json) and query
the configured PVE cluster. pve-ops changes nothing in PVE, except that
"ha --widen-replicas --apply" widens the replica pins the CPI's own check
finds ready, "ha rebalance --apply" live-migrates this Director's HA VMs, and
"ha drain --apply" and "ha undrain --apply" stop and start through PVE HA the
VMs that can't leave a node in maintenance.

Run "pve-ops <subcommand> --help" for subcommand-specific flags.
`)
}

// parseWithPositionals parses args with fs, allowing flags and positional
// arguments in any order (stdlib flag stops at the first non-flag argument,
// which would reject the "pve-ops decode <cid> --json" form the usage text
// advertises). It returns the positional arguments in their original order.
// Flag values are never misread as positionals because each fs.Parse round
// consumes value-taking flags itself.
func parseWithPositionals(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

// writeJSON encodes v as indented JSON to w. On encode failure it reports the
// error to errw and returns exitError; callers propagate the returned code.
func writeJSON(w, errw io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_, _ = fmt.Fprintf(errw, "pve-ops: json encode: %s\n", err)
		return exitError
	}
	return exitOK
}
