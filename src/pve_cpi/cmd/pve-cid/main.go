// Package main implements pve-cid, a read-only operator CLI for inspecting
// BOSH Proxmox CPI CIDs (cloud IDs) and correlating them against live PVE
// cluster state.
//
// pve-cid never mutates PVE: every subcommand either decodes/encodes a CID
// offline, or performs read-only API calls (GET-only) to locate and
// inventory CPI-managed resources.
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
//	pve-cid decode <cid> [--json]
//	    Decode any CID family offline; no PVE API calls. Prints family,
//	    decoded fields, and (for disk CIDs) the encoded length against the
//	    varchar(255) Director column target.
//
//	pve-cid encode --volid <storage:path> [--pool P] [--node N] [--az Z]
//	                [--opt k=v ...] [--compress] [--json]
//	    Build a pvd-/pvz- disk CID envelope offline. --compress allows the
//	    gzip-compressed pvz- form when the plain form would exceed the
//	    varchar(255) target (mirrors create_disk's automatic fallback).
//
//	pve-cid locate <cid|volid> [--config PATH] [--json]
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
//	pve-cid stemcells [--storage ID] [--node N] [--orphans] [--json]
//	                  [--config PATH]
//	    Online. Cluster-wide stemcell inventory: every bosh-stemcell-tagged
//	    cache template correlated against every bosh-stemcell-*.qcow2 file on
//	    the configured (or --storage) stemcell storage, grouped by sha8.
//	    --orphans filters to entries with zero director references, files
//	    with no correlated template (kind unknown), or templates whose
//	    backing file is gone — the "what is safe to delete" view.
//
// # Config resolution (locate, stemcells)
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
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

// cliVersion is pve-cid's own version string, independent of the cpi
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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to the requested subcommand. Accepting args/stdout/stderr as
// parameters (rather than reading os.Args/os.Stdout/os.Stderr directly) makes
// every code path testable in-process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "-h", "--help", "help":
		printUsage(stdout)
		return exitOK
	case "-version", "--version", "version":
		_, _ = fmt.Fprintln(stdout, "pve-cid "+cliVersion)
		return exitOK
	case "decode":
		return runDecode(args[1:], stdout, stderr)
	case "encode":
		return runEncode(args[1:], stdout, stderr)
	case "locate":
		return runLocate(args[1:], stdout, stderr)
	case "stemcells":
		return runStemcells(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "pve-cid: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return exitUsage
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `pve-cid — read-only operator CLI for BOSH Proxmox CPI CIDs

Usage:
  pve-cid decode <cid> [--json]
  pve-cid encode --volid <storage:path> [--pool P] [--node N] [--az Z] [--opt k=v ...] [--compress] [--json]
  pve-cid locate <cid|volid> [--config PATH] [--json]
  pve-cid stemcells [--storage ID] [--node N] [--orphans] [--config PATH] [--json]
  pve-cid version
  pve-cid help

decode and encode are fully offline (no PVE API calls). locate and stemcells
are read-only online commands: they load the CPI's cpi.json (--config flag,
then $PVE_CPI_CONFIG, then /var/vcap/jobs/pve_cpi/config/cpi.json) and query
the configured PVE cluster; pve-cid never mutates PVE.

Run "pve-cid <subcommand> --help" for subcommand-specific flags.
`)
}

// parseWithPositionals parses args with fs, allowing flags and positional
// arguments in any order (stdlib flag stops at the first non-flag argument,
// which would reject the "pve-cid decode <cid> --json" form the usage text
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
		_, _ = fmt.Fprintf(errw, "pve-cid: json encode: %s\n", err)
		return exitError
	}
	return exitOK
}
