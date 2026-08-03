package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// optFlag implements flag.Value to accept repeatable "--opt key=value" flags
// into a single map. Methods use a value receiver: optFlag is a map (a
// reference type), so mutation through Set is visible to the caller-held
// map without needing a pointer receiver.
type optFlag map[string]string

func (o optFlag) String() string {
	if len(o) == 0 {
		return ""
	}
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+o[k])
	}
	return strings.Join(parts, ",")
}

func (o optFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("--opt must be in the form key=value, got %q", s)
	}
	o[k] = v
	return nil
}

// encodeResult is the JSON shape emitted by "pve-cid encode --json".
type encodeResult struct {
	CID        string `json:"cid"`
	Length     int    `json:"length"`
	Compressed bool   `json:"compressed"`
	// OverTarget reports whether CID exceeds pve.DiskCIDLengthTarget, the
	// varchar(255) bound the BOSH Director enforces on a disk_cid column.
	// Advisory only: encode always succeeds and exits 0 even when true — the
	// Director's column bound is the actual enforcement point, not this
	// tool. EncodeDiskCIDCompressed (--compress) already tries to stay under
	// the target via gzip before returning a value where this can be true.
	OverTarget bool `json:"over_target"`
}

// runEncode implements:
//
//	pve-cid encode --volid <storage:path> [--pool P] [--node N] [--az Z]
//	               [--opt k=v ...] [--compress] [--json]
//
// Fully offline: builds the pvd-/pvz- envelope via internal/pve's own
// encoder, guaranteeing byte-for-byte parity with what the CPI itself emits.
func runEncode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pve-cid encode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	volid := fs.String("volid", "", "bare PVE volid to encode, format <storage>:<volume> (required)")
	pool := fs.String("pool", "", "PVE storage pool name to embed in the CID metadata")
	node := fs.String("node", "", "PVE node name to embed in the CID metadata")
	az := fs.String("az", "", "availability-zone label to embed in the CID metadata")
	compress := fs.Bool("compress", false,
		"allow the gzip-compressed pvz- envelope when the plain pvd- form would exceed the varchar(255) target")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON instead of plain text")
	opts := optFlag{}
	fs.Var(opts, "opt", "per-disk performance option key=value (repeatable, e.g. --opt iothread=1 --opt cache=writeback)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: pve-cid encode --volid <storage:path> [--pool P] [--node N] [--az Z] [--opt k=v ...] [--compress] [--json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "pve-cid encode: unexpected positional arguments: %v\n", fs.Args())
		fs.Usage()
		return exitUsage
	}
	if *volid == "" {
		_, _ = fmt.Fprintln(stderr, "pve-cid encode: --volid is required")
		fs.Usage()
		return exitUsage
	}

	if _, _, err := pve.ParseDiskCID(*volid); err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-cid encode: invalid --volid: %s\n", err)
		return exitError
	}

	var meta *pve.DiskCIDMeta
	if *pool != "" || *node != "" || *az != "" || len(opts) > 0 {
		m := pve.DiskCIDMeta{Pool: *pool, Node: *node, AZ: *az}
		if len(opts) > 0 {
			m.Opts = map[string]string(opts)
		}
		meta = &m
	}

	var cid string
	var err error
	if *compress {
		cid, err = pve.EncodeDiskCIDCompressed(*volid, meta)
	} else {
		cid, err = pve.EncodeDiskCID(*volid, meta)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-cid encode: %s\n", err)
		return exitError
	}

	overTarget := len(cid) > pve.DiskCIDLengthTarget

	if *jsonOut {
		return writeJSON(stdout, stderr, encodeResult{
			CID:        cid,
			Length:     len(cid),
			Compressed: strings.HasPrefix(cid, "pvz-"),
			OverTarget: overTarget,
		})
	}
	_, _ = fmt.Fprintf(stdout, "%s\nlength: %d\n", cid, len(cid))
	if overTarget {
		_, _ = fmt.Fprintf(stderr, "pve-cid encode: WARNING: CID length %d exceeds the varchar(255) target (%d); "+
			"the BOSH Director's disk_cid column will reject this value. Retry with --compress, "+
			"or use fewer --opt entries / a shorter --pool, --node, or --az value.\n",
			len(cid), pve.DiskCIDLengthTarget)
	}
	return exitOK
}
