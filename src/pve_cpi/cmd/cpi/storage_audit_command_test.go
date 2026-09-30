package main

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRenderStorageAuditCommand(t *testing.T) {
	lookup := func(uid string) (*user.User, error) {
		if uid == "1000" {
			return &user.User{Uid: uid, Username: "vcap"}, nil
		}
		return nil, errors.New("unknown user")
	}
	const executable = "/var/vcap/packages/pve_cpi/bin/cpi"
	const configPath = "/var/vcap/jobs/pve_cpi/config/cpi.json"
	for _, tc := range []struct {
		name, executable, configPath, owner, euid, want string
	}{
		{"journal owned by another user", executable, configPath, "1000", "0", "sudo -u vcap /var/vcap/packages/pve_cpi/bin/cpi storage-journal audit --summary --config /var/vcap/jobs/pve_cpi/config/cpi.json"},
		{"journal owned by this user", executable, configPath, "1000", "1000", "/var/vcap/packages/pve_cpi/bin/cpi storage-journal audit --summary --config /var/vcap/jobs/pve_cpi/config/cpi.json"},
		{"owner without an account", executable, configPath, "4242", "0", ""},
		{"paths that need quoting", "/home/op/bosh cpi/bin/cpi", "/home/op/it's/cpi.json", "1000", "1000", `'/home/op/bosh cpi/bin/cpi' storage-journal audit --summary --config '/home/op/it'\''s/cpi.json'`},
		{"no executable", "", configPath, "1000", "1000", ""},
		{"no config", executable, "", "1000", "1000", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStorageAuditCommand(tc.executable, tc.configPath, tc.owner, tc.euid, lookup); got != tc.want {
				t.Fatalf("command = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStorageAuditCommandFromHost(t *testing.T) {
	journal := t.TempDir()
	config := filepath.Join(t.TempDir(), "cpi.json")
	got := storageAuditCommand(config, journal)
	if got == "" {
		t.Fatal("no command rendered for a journal this process owns")
	}
	executable, err := storageAuditExecutable()
	if err != nil {
		t.Fatal(err)
	}
	// The test process owns its temp directory, so no sudo prefix applies.
	want := renderStorageAuditCommand(executable, config, strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Geteuid()), user.LookupId)
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	if storageAuditCommand(config, "") != "" {
		t.Fatal("a CPI without a journal directory rendered a command")
	}
	if storageAuditCommand(config, filepath.Join(journal, "missing")) != "" {
		t.Fatal("an unprovisioned journal rendered a command without a known owner")
	}
	absolute, err := filepath.Abs("cpi.json")
	if err != nil {
		t.Fatal(err)
	}
	if relative := storageAuditCommand("cpi.json", journal); !strings.HasSuffix(relative, "--config "+absolute) {
		t.Fatalf("relative config path was not made absolute: %q", relative)
	}
}
