package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// endpointCacheSiblingName is the directory beside the journal that the endpoint cache uses
// when it exists. The runtime never creates it, so this command does. It must match
// endpointcache.JournalSiblingName, which the runtime layer reads.
const endpointCacheSiblingName = "endpoint-cache"

// provisionDirectory is a seam so tests can see the path and owner each provisioning call receives.
var provisionDirectory = allocationjournal.ProvisionDirectory

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
			storageJournalFail(stderr, "journal provisioning: cannot open config", err)
			return 1
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, config.MaxConfigBytes+1))
		err = errors.Join(readErr, f.Close())
		if err == nil && int64(len(raw)) > config.MaxConfigBytes {
			err = storageJournalFixedError(fmt.Sprintf("config exceeds %d bytes", config.MaxConfigBytes))
		}
		if err != nil {
			storageJournalFail(stderr, "journal provisioning: cannot read bounded config", err)
			return 1
		}
		var local struct {
			Directory string `json:"storage_allocation_journal_dir"`
		}
		if err := json.Unmarshal(raw, &local); err != nil {
			storageJournalFail(stderr, "journal provisioning: invalid config JSON", err)
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
			storageJournalFail(stderr, "journal provisioning: runtime owner lookup failed", err)
			return 1
		}
		uid, err = strconv.Atoi(account.Uid)
		if err != nil {
			storageJournalFail(stderr, "journal provisioning: invalid runtime UID", err)
			return 1
		}
		gid, err = strconv.Atoi(account.Gid)
		if err != nil {
			storageJournalFail(stderr, "journal provisioning: invalid runtime GID", err)
			return 1
		}
	}
	if err := provisionDirectory(path, uid, gid); err != nil {
		storageJournalFail(stderr, storageJournalLine("journal directory "+path+" could not be provisioned"), err)
		return 1
	}
	provisionEndpointCacheSibling(path, uid, gid, stderr)
	if _, err := fmt.Fprintln(stdout, "Journal directory provisioned; authority enrollment is unchanged."); err != nil {
		storageJournalFail(stderr, "provision-journal output could not be written", err)
		return 1
	}
	return 0
}

// endpointCacheSibling returns the directory beside journal, and false when journal is itself named
// endpoint-cache, because the two names would collide. The comparison is exact on purpose. Production hosts
// are case-sensitive Linux filesystems, and a journal named Endpoint-Cache is a different directory there.
func endpointCacheSibling(journal string) (string, bool) {
	if filepath.Base(journal) == endpointCacheSiblingName {
		return "", false
	}
	return filepath.Join(filepath.Dir(journal), endpointCacheSiblingName), true
}

// provisionEndpointCacheSibling provisions the endpoint cache directory beside a journal directory that
// was just provisioned, with the same owner. Only a journal failure may fail the command, because pre-start
// runs it under set -e and a sibling made by hand with another owner or mode must not keep the Director from
// starting. A sibling failure therefore writes one warning line and leaves the exit status alone, and the
// runtime's private checks then give no cache for that directory. The path is flattened with the rest of
// the message, so a directory name holding a line break still gives one line.
func provisionEndpointCacheSibling(journal string, uid, gid int, stderr io.Writer) {
	sibling, ok := endpointCacheSibling(journal)
	if !ok {
		return
	}
	if err := provisionDirectory(sibling, uid, gid); err != nil {
		storageJournalFail(stderr,
			storageJournalLine("warning: endpoint cache directory "+sibling+" could not be provisioned"), err)
	}
}
