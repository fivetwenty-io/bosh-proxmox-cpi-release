package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
)

// runDecode implements "pve-cid decode <cid> [--json]". Fully offline: no
// PVE API calls, no config load.
func runDecode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pve-cid decode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON instead of a table")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: pve-cid decode <cid> [--json]")
		fs.PrintDefaults()
	}
	positionals, err := parseWithPositionals(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(positionals) != 1 {
		_, _ = fmt.Fprintf(stderr, "pve-cid decode: expected exactly one CID argument, got %d\n", len(positionals))
		fs.Usage()
		return exitUsage
	}

	decoded, err := DecodeCID(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	if *jsonOut {
		return writeJSON(stdout, stderr, decoded)
	}
	printDecodedTable(stdout, decoded)
	return exitOK
}

// printDecodedTable renders d as plain-text "key: value" lines, showing only
// the fields relevant to d.Family.
func printDecodedTable(w io.Writer, d *DecodedCID) {
	_, _ = fmt.Fprintf(w, "family: %s\n", d.Family)
	_, _ = fmt.Fprintf(w, "raw: %s\n", d.Raw)

	switch d.Family {
	case FamilyStemcellLight, FamilyStemcellHeavy:
		_, _ = fmt.Fprintf(w, "storage: %s\n", d.Storage)
		_, _ = fmt.Fprintf(w, "volume_path: %s\n", d.VolumePath)
		_, _ = fmt.Fprintf(w, "filename: %s\n", d.Filename)
		if d.SHA8 != "" {
			_, _ = fmt.Fprintf(w, "sha8: %s\n", d.SHA8)
		} else {
			_, _ = fmt.Fprintln(w, "sha8: (could not be extracted from filename)")
		}

	case FamilyDiskPVD, FamilyDiskPVZ:
		_, _ = fmt.Fprintf(w, "volid: %s\n", d.Volid)
		_, _ = fmt.Fprintf(w, "disk_storage: %s\n", d.DiskStorage)
		_, _ = fmt.Fprintf(w, "disk_volume: %s\n", d.DiskVolume)
		if d.Pool != "" {
			_, _ = fmt.Fprintf(w, "pool: %s\n", d.Pool)
		}
		if d.Node != "" {
			_, _ = fmt.Fprintf(w, "node: %s\n", d.Node)
		}
		if d.AZ != "" {
			_, _ = fmt.Fprintf(w, "az: %s\n", d.AZ)
		}
		if d.Anchor {
			_, _ = fmt.Fprintln(w, "anchor: true")
		}
		if d.Format != "" {
			_, _ = fmt.Fprintf(w, "format: %s\n", d.Format)
		}
		if d.StableID != "" {
			_, _ = fmt.Fprintf(w, "stable_id: %s\n", d.StableID)
		}
		optKeys := make([]string, 0, len(d.Opts))
		for k := range d.Opts {
			optKeys = append(optKeys, k)
		}
		sort.Strings(optKeys)
		for _, k := range optKeys {
			_, _ = fmt.Fprintf(w, "opt.%s: %s\n", k, d.Opts[k])
		}
		_, _ = fmt.Fprintf(w, "encoded_length: %d\n", d.EncodedLength)
		_, _ = fmt.Fprintf(w, "length_target: %d\n", d.LengthTarget)
		if d.OverTarget {
			_, _ = fmt.Fprintf(w, "over_target: true (%d bytes over the varchar(255) Director column target)\n",
				d.EncodedLength-d.LengthTarget)
		} else {
			_, _ = fmt.Fprintln(w, "over_target: false")
		}

	case FamilyVM:
		_, _ = fmt.Fprintf(w, "vmid: %d\n", d.VMID)

	case FamilySnapshot:
		_, _ = fmt.Fprintf(w, "vm_cid: %s\n", d.VMCID)
		_, _ = fmt.Fprintf(w, "vmid: %d\n", d.VMID)
		_, _ = fmt.Fprintf(w, "snapshot_name: %s\n", d.SnapshotName)
	}
}
