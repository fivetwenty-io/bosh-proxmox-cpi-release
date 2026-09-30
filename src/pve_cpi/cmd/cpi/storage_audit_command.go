package main

import (
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// storageAuditCommand renders the exact command that prints the full
// allocation audit on this host, for audit-gated refusals to end with. On a
// Director that is "sudo -u vcap <package>/bin/cpi storage-journal audit
// --summary --config <job>/config/cpi.json"; under create-env the CPI runs as
// the operator who owns the journal, so the sudo prefix drops away. It returns
// "" when there is no journal, or when the journal owner cannot be named, and
// the refusal then describes the command instead.
func storageAuditCommand(configPath, journalDir string) string {
	if journalDir == "" || configPath == "" {
		return ""
	}
	executable, err := storageAuditExecutable()
	if err != nil {
		return ""
	}
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return ""
	}
	info, err := os.Stat(journalDir)
	if err != nil {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	owner := strconv.FormatUint(uint64(stat.Uid), 10)
	return renderStorageAuditCommand(executable, absoluteConfig, owner, strconv.Itoa(os.Geteuid()), user.LookupId)
}

// storageAuditExecutable prefers the absolute path the job wrapper invoked,
// which stays valid across package upgrades, over the resolved binary path.
func storageAuditExecutable() (string, error) {
	if len(os.Args) > 0 && filepath.IsAbs(os.Args[0]) {
		return os.Args[0], nil
	}
	return os.Executable()
}

// renderStorageAuditCommand is the pure half of storageAuditCommand. owner and
// euid are decimal UIDs, and lookup names the owner when the two differ.
func renderStorageAuditCommand(executable, configPath, owner, euid string, lookup func(string) (*user.User, error)) string {
	if executable == "" || configPath == "" {
		return ""
	}
	command := shellQuoteArg(executable) + " storage-journal audit --summary --config " + shellQuoteArg(configPath)
	if owner == euid {
		return command
	}
	account, err := lookup(owner)
	if err != nil || account == nil || account.Username == "" {
		return ""
	}
	return "sudo -u " + shellQuoteArg(account.Username) + " " + command
}

var shellSafeArg = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// shellQuoteArg quotes s for a POSIX shell when it holds anything beyond
// the characters a path or user name usually needs.
func shellQuoteArg(s string) string {
	if shellSafeArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
