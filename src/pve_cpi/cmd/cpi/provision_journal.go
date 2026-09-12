package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// runProvisionJournal is an explicit local command, dispatched before logging,
// telemetry, credential validation, PVE client construction, or CPI requests.
func runProvisionJournal(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cpi provision-journal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "CPI JSON config containing the journal directory")
	directory := fs.String("directory", "", "explicit durable host journal directory")
	owner := fs.String("owner", "", "runtime user (root provisioning only; default current user)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || (*configPath == "") == (*directory == "") {
		fmt.Fprintln(stderr, "specify exactly one of --config or --directory, with no positional arguments")
		return 2
	}
	path := *directory
	if *configPath != "" {
		f, err := os.Open(*configPath)
		if err != nil {
			fmt.Fprintln(stderr, "journal provisioning: cannot open config")
			return 1
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, config.MaxConfigBytes+1))
		err = errors.Join(readErr, f.Close())
		if err != nil || int64(len(raw)) > config.MaxConfigBytes {
			fmt.Fprintln(stderr, "journal provisioning: cannot read bounded config")
			return 1
		}
		var local struct {
			Directory string `json:"storage_allocation_journal_dir"`
		}
		if err := json.Unmarshal(raw, &local); err != nil {
			fmt.Fprintln(stderr, "journal provisioning: invalid config JSON")
			return 1
		}
		path = local.Directory
		if path == "" {
			return 0
		}
	}
	uid, gid := os.Geteuid(), os.Getegid()
	if *owner != "" {
		if uid != 0 {
			fmt.Fprintln(stderr, "journal provisioning: --owner requires root")
			return 1
		}
		account, err := user.Lookup(*owner)
		if err != nil {
			fmt.Fprintln(stderr, "journal provisioning: runtime owner lookup failed")
			return 1
		}
		uid, err = strconv.Atoi(account.Uid)
		if err != nil {
			fmt.Fprintln(stderr, "journal provisioning: invalid runtime UID")
			return 1
		}
		gid, err = strconv.Atoi(account.Gid)
		if err != nil {
			fmt.Fprintln(stderr, "journal provisioning: invalid runtime GID")
			return 1
		}
	}
	if err := allocationjournal.ProvisionDirectory(path, uid, gid); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, "Journal directory provisioned; authority enrollment is unchanged."); err != nil {
		return 1
	}
	return 0
}
