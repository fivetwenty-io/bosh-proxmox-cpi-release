package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// provisionTemp returns a fresh directory with its symlinks resolved. The name
// is short on purpose, because the journal's error text is cut at a fixed
// length and the tests check the end of it.
func provisionTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestProvisionJournalCommandIsExplicitAndOffline(t *testing.T) {
	base := provisionTemp(t)
	path := filepath.Join(base, "durable", "allocations")
	cfg := filepath.Join(base, "cpi.json")
	body, _ := json.Marshal(map[string]any{"storage_allocation_journal_dir": path, "password": "must-not-print", "host": "never-connect.invalid"})
	if err := os.WriteFile(cfg, body, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := runWithArgs([]string{"provision-journal", "--config", cfg}, strings.NewReader(""), &out, &stderr, runOptions{}); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	if strings.Contains(out.String()+stderr.String(), "must-not-print") {
		t.Fatal("secret emitted")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected enrollment: %v", err)
	}
	if code := runProvisionJournal([]string{"--directory", path}, &out, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
}
func TestProvisionJournalCommandNoConfiguredPathPreservesLegacy(t *testing.T) {
	base := provisionTemp(t)
	cfg := filepath.Join(base, "cpi.json")
	if err := os.WriteFile(cfg, []byte(`{"host":"not-used"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := runProvisionJournal([]string{"--config", cfg, "--owner", "vcap"}, &out, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	if out.Len() != 0 || stderr.Len() != 0 {
		t.Fatal("legacy command produced output")
	}
	entries, _ := os.ReadDir(base)
	if len(entries) != 1 {
		t.Fatal("legacy config created directory")
	}
}
func TestProvisionJournalCommandRejectsUsageAndUnsafeDirectory(t *testing.T) {
	for _, args := range [][]string{nil, {"--directory", "/"}, {"--directory", "relative"}, {"--directory", "/x", "--config", "/y"}, {"--config", "/missing"}, {"--directory", "/x", "extra"}} {
		var out, stderr bytes.Buffer
		if code := runProvisionJournal(args, &out, &stderr); code == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
}
func TestProvisionJournalCommandNamesItsFailures(t *testing.T) {
	base := provisionTemp(t)
	missing := filepath.Join(base, "absent.json")
	malformed := filepath.Join(base, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{"password":"must-not-print",`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--config", missing}, "journal provisioning: cannot open config: open " + missing + ": no such file or directory\n"},
		{[]string{"--config", malformed}, "journal provisioning: invalid config JSON: JSON syntax error at byte offset 29\n"},
		{[]string{"--directory", "/"}, "journal directory / could not be provisioned: journal provisioning: require a clean absolute non-root path and valid owner\n"},
	} {
		var out, stderr bytes.Buffer
		if code := runProvisionJournal(tc.args, &out, &stderr); code != 1 || stderr.String() != tc.want {
			t.Fatalf("%v reported as %d %q, want %q", tc.args, code, stderr.String(), tc.want)
		}
		if strings.Contains(stderr.String(), "must-not-print") {
			t.Fatal("provisioning printed config content")
		}
	}
}

func TestProvisionJournalPreStartLaunchResolution(t *testing.T) {
	base := provisionTemp(t)
	job := filepath.Join(base, "jobs", "pve_cpi")
	pkg := filepath.Join(base, "packages", "pve_cpi", "bin")
	for _, p := range []string{filepath.Join(job, "bin"), filepath.Join(job, "config"), pkg} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	template, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "jobs", "pve_cpi", "templates", "pre-start.erb"))
	if err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(job, "bin", "pre-start")
	if err := os.WriteFile(launch, template, 0700); err != nil {
		t.Fatal(err)
	}
	fake := []byte("#!/bin/bash\nprintf '%s\\n' \"$@\"\n")
	if err := os.WriteFile(filepath.Join(pkg, "cpi"), fake, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "bash", launch)
	command.Dir = base
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	expected := "provision-journal\n--config\n" + filepath.Join(job, "config", "cpi.json") + "\n--owner\nvcap\n"
	if string(out) != expected {
		t.Fatalf("wrong launch arguments: %q", out)
	}
	spec, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "jobs", "pve_cpi", "spec"))
	if err != nil || !bytes.Contains(spec, []byte("pre-start.erb: bin/pre-start")) {
		t.Fatal("pre-start absent from job spec")
	}
	wrapper, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "jobs", "pve_cpi", "templates", "cpi.erb"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrapper, []byte("provision")) {
		t.Fatal("per-invocation wrapper provisions journal")
	}
}

// TestJobWrappersResolveTheRealJobDirectory runs both job wrappers from
// data/jobs/<job>/<hash>, the path a caller that resolves symlinks hands them
// (sudo-rs does), as well as from the jobs/<job> link. Each must find the
// installation's packages either way.
func TestJobWrappersResolveTheRealJobDirectory(t *testing.T) {
	cases := []struct {
		template string
		bin      string
		args     func(job string) string
	}{
		{"cpi.erb", "cpi", func(job string) string {
			return "--config\n" + filepath.Join(job, "config", "cpi.json") + "\n"
		}},
		{"pre-start.erb", "pre-start", func(job string) string {
			return "provision-journal\n--config\n" + filepath.Join(job, "config", "cpi.json") + "\n--owner\nvcap\n"
		}},
	}
	for _, tc := range cases {
		for _, layout := range []string{filepath.Join("data", "jobs", "pve_cpi", "0123abcd"), filepath.Join("jobs", "pve_cpi")} {
			t.Run(tc.template+"/"+layout, func(t *testing.T) {
				base := provisionTemp(t)
				job := filepath.Join(base, layout)
				pkg := filepath.Join(base, "packages", "pve_cpi", "bin")
				for _, p := range []string{filepath.Join(job, "bin"), filepath.Join(job, "config"), pkg} {
					if err := os.MkdirAll(p, 0700); err != nil {
						t.Fatal(err)
					}
				}
				template, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "jobs", "pve_cpi", "templates", tc.template))
				if err != nil {
					t.Fatal(err)
				}
				launch := filepath.Join(job, "bin", tc.bin)
				if err := os.WriteFile(launch, template, 0700); err != nil {
					t.Fatal(err)
				}
				fake := []byte("#!/bin/bash\nprintf '%s\\n' \"$@\"\n")
				if err := os.WriteFile(filepath.Join(pkg, "cpi"), fake, 0700); err != nil {
					t.Fatal(err)
				}
				command := exec.CommandContext(t.Context(), "bash", launch)
				command.Dir = base
				command.Env = append(os.Environ(), "BOSH_PACKAGES_DIR=")
				out, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				if want := tc.args(job); string(out) != want {
					t.Fatalf("launch arguments = %q, want %q", out, want)
				}
			})
		}
	}
}

// requirePrivateOwnedDirectory fails unless path is a real directory with mode 0700 and the current user as
// owner, which is the owner the tests request because only root may name another.
func requirePrivateOwnedDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is %v, want a directory with mode 0700", path, info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid() {
		t.Fatalf("%s has the wrong owner", path)
	}
}

func writeProvisionConfig(t *testing.T, dir string, journal string) string {
	t.Helper()
	body := map[string]any{"host": "never-connect.invalid"}
	if journal != "" {
		body["storage_allocation_journal_dir"] = journal
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cpi.json")
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProvisionJournalCreatesTheEndpointCacheBesideTheJournal(t *testing.T) {
	for _, mode := range []string{"config", "directory"} {
		t.Run(mode, func(t *testing.T) {
			base := provisionTemp(t)
			journal := filepath.Join(base, "durable", "allocations")
			sibling := filepath.Join(base, "durable", "endpoint-cache")
			args := []string{"--directory", journal}
			if mode == "config" {
				args = []string{"--config", writeProvisionConfig(t, base, journal)}
			}
			for run := 1; run <= 2; run++ {
				var out, stderr bytes.Buffer
				if code := runProvisionJournal(args, &out, &stderr); code != 0 || stderr.Len() != 0 {
					t.Fatalf("run %d: code %d: %s", run, code, stderr.String())
				}
				requirePrivateOwnedDirectory(t, journal)
				requirePrivateOwnedDirectory(t, sibling)
				if entries, err := os.ReadDir(journal); err != nil || len(entries) != 0 {
					t.Fatalf("run %d: journal holds %v, %v", run, entries, err)
				}
			}
		})
	}
}

func TestProvisionJournalSiblingSurvivesAnExistingJournalFromAnOlderRelease(t *testing.T) {
	base := provisionTemp(t)
	journal := filepath.Join(base, "allocations")
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := runProvisionJournal([]string{"--directory", journal}, &out, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	requirePrivateOwnedDirectory(t, filepath.Join(base, "endpoint-cache"))
}

func TestProvisionJournalWarnsAndExitsZeroWhenTheSiblingIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, sibling, base string)
	}{
		{"wrong mode", func(t *testing.T, sibling, _ string) {
			if err := os.Mkdir(sibling, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(sibling, 0o750); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, sibling, base string) {
			target := filepath.Join(base, "elsewhere")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, sibling); err != nil {
				t.Fatal(err)
			}
		}},
		{"regular file", func(t *testing.T, sibling, _ string) {
			if err := os.WriteFile(sibling, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, mode := range []string{"config", "directory"} {
			t.Run(tc.name+" with "+mode, func(t *testing.T) {
				base := provisionTemp(t)
				journal := filepath.Join(base, "allocations")
				sibling := filepath.Join(base, "endpoint-cache")
				tc.setup(t, sibling, base)
				before, err := os.Lstat(sibling)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"--directory", journal}
				if mode == "config" {
					args = []string{"--config", writeProvisionConfig(t, base, journal)}
				}
				var out, stderr bytes.Buffer
				if code := runProvisionJournal(args, &out, &stderr); code != 0 {
					t.Fatalf("code %d: %s", code, stderr.String())
				}
				requirePrivateOwnedDirectory(t, journal)
				if !strings.HasPrefix(out.String(), "Journal directory provisioned") {
					t.Fatalf("success line missing: %q", out.String())
				}
				warning := stderr.String()
				wantPrefix := "warning: endpoint cache directory " + sibling + " could not be provisioned: "
				if strings.Count(warning, "\n") != 1 || !strings.HasSuffix(warning, "\n") ||
					!strings.HasPrefix(warning, wantPrefix) {
					t.Fatalf("want one warning line naming %s, got %q", sibling, warning)
				}
				after, err := os.Lstat(sibling)
				if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatalf("sibling was changed: %v %v", after, err)
				}
			})
		}
	}
}

func TestProvisionJournalCreatesNoSiblingWithoutAJournal(t *testing.T) {
	base := provisionTemp(t)
	cfg := writeProvisionConfig(t, base, "")
	var out, stderr bytes.Buffer
	if code := runProvisionJournal([]string{"--config", cfg}, &out, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	if out.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("output %q %q", out.String(), stderr.String())
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %v, %v", entries, err)
	}
}

func TestProvisionJournalNamedLikeTheCacheGetsNoSibling(t *testing.T) {
	base := provisionTemp(t)
	journal := filepath.Join(base, "durable", "endpoint-cache")
	var out, stderr bytes.Buffer
	if code := runProvisionJournal([]string{"--directory", journal}, &out, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	requirePrivateOwnedDirectory(t, journal)
	entries, err := os.ReadDir(filepath.Join(base, "durable"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("durable holds %v, %v", entries, err)
	}
	if entries, err := os.ReadDir(journal); err != nil || len(entries) != 0 {
		t.Fatalf("journal holds %v, %v", entries, err)
	}
}

func TestProvisionJournalFailureStillExitsOneAndCreatesNoSibling(t *testing.T) {
	base := provisionTemp(t)
	journal := filepath.Join(base, "durable", "allocations")
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(journal, 0o750); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := runProvisionJournal([]string{"--directory", journal}, &out, &stderr); code != 1 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	if out.Len() != 0 || !strings.Contains(stderr.String(), "journal directory "+journal+" could not be provisioned") {
		t.Fatalf("output %q %q", out.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "endpoint cache") {
		t.Fatalf("sibling reported after a journal failure: %q", stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(base, "durable", "endpoint-cache")); !os.IsNotExist(err) {
		t.Fatalf("sibling exists after a journal failure: %v", err)
	}
}

func TestEndpointCacheSibling(t *testing.T) {
	for _, tc := range []struct {
		journal string
		want    string
		ok      bool
	}{
		{"/a/allocations", "/a/endpoint-cache", true},
		{"/a/endpoint-cache", "", false},
		{"/endpoint-cache", "", false},
		{"/a/endpoint-cache/x", "/a/endpoint-cache/endpoint-cache", true},
		{"/a/endpoint-cache2", "/a/endpoint-cache", true},
		{"/a/Endpoint-Cache", "/a/endpoint-cache", true},
	} {
		got, ok := endpointCacheSibling(tc.journal)
		if got != tc.want || ok != tc.ok {
			t.Errorf("endpointCacheSibling(%q) = %q, %v; want %q, %v", tc.journal, got, ok, tc.want, tc.ok)
		}
	}
}

type provisionCall struct {
	path     string
	uid, gid int
}

// recordProvisionDirectory swaps the provisioning seam for a recorder that returns result for every call.
func recordProvisionDirectory(t *testing.T, result error) *[]provisionCall {
	t.Helper()
	var calls []provisionCall
	saved := provisionDirectory
	provisionDirectory = func(path string, uid, gid int) error {
		calls = append(calls, provisionCall{path, uid, gid})
		return result
	}
	t.Cleanup(func() { provisionDirectory = saved })
	return &calls
}

func TestProvisionEndpointCacheSiblingPassesThePathAndOwner(t *testing.T) {
	calls := recordProvisionDirectory(t, nil)
	var stderr bytes.Buffer
	provisionEndpointCacheSibling("/x/allocations", 4242, 4343, &stderr)
	if want := []provisionCall{{"/x/endpoint-cache", 4242, 4343}}; len(*calls) != 1 || (*calls)[0] != want[0] {
		t.Fatalf("calls %v, want %v", *calls, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected output %q", stderr.String())
	}
	*calls = nil
	provisionEndpointCacheSibling("/x/endpoint-cache", 4242, 4343, &stderr)
	if len(*calls) != 0 {
		t.Fatalf("a journal named like the cache was given a sibling: %v", *calls)
	}
}

func TestProvisionEndpointCacheSiblingWarnsOnceWhenTheOwnerCannotBeSet(t *testing.T) {
	recordProvisionDirectory(t, &os.PathError{Op: "chown", Path: "/x/endpoint-cache", Err: syscall.EPERM})
	var stderr bytes.Buffer
	provisionEndpointCacheSibling("/x/allocations", 4242, 4343, &stderr)
	warning := stderr.String()
	if strings.Count(warning, "\n") != 1 ||
		!strings.HasPrefix(warning, "warning: endpoint cache directory /x/endpoint-cache could not be provisioned: ") {
		t.Fatalf("want one warning line, got %q", warning)
	}
}

func TestProvisionEndpointCacheSiblingWarningIsOneLineForAPathWithLineBreaks(t *testing.T) {
	recordProvisionDirectory(t, errors.New("refused"))
	var stderr bytes.Buffer
	provisionEndpointCacheSibling("/x\ny\x1b[31m/allocations", 1, 1, &stderr)
	warning := stderr.String()
	if strings.Count(warning, "\n") != 1 || strings.ContainsRune(warning, 0x1b) {
		t.Fatalf("want one clean warning line, got %q", warning)
	}
}

func TestProvisionJournalGivesTheSiblingTheJournalOwner(t *testing.T) {
	calls := recordProvisionDirectory(t, nil)
	var out, stderr bytes.Buffer
	if code := runProvisionJournal([]string{"--directory", "/x/allocations"}, &out, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	uid, gid := os.Geteuid(), os.Getegid()
	want := []provisionCall{{"/x/allocations", uid, gid}, {"/x/endpoint-cache", uid, gid}}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("calls %v, want %v", *calls, want)
	}
}

func TestProvisionJournalNamesTheOwnerOnTheSiblingAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("naming an owner requires root")
	}
	// A fake account whose ids differ from root's, so an ignored owner leaves uid 0 and fails the test on any machine.
	// Root can chown to ids that have no passwd entry, so the real chown still runs.
	account := &user.User{Uid: "4242", Gid: "4343", Username: "bosh"}
	savedLookup := lookupAccount
	lookupAccount = func(name string) (*user.User, error) {
		if name == account.Username {
			return account, nil
		}
		return nil, user.UnknownUserError(name)
	}
	t.Cleanup(func() { lookupAccount = savedLookup })
	wantUID, wantGID := 4242, 4343
	base := provisionTemp(t)
	journal := filepath.Join(base, "allocations")
	var out, stderr bytes.Buffer
	args := []string{"--directory", journal, "--owner", account.Username}
	if code := runProvisionJournal(args, &out, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	for _, path := range []string{journal, filepath.Join(base, "endpoint-cache")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != wantUID || int(st.Gid) != wantGID {
			t.Fatalf("%s is not owned by %s", path, account.Username)
		}
	}
}

// fakeRoot makes the command see root with the given accounts, so the --owner path runs for every user.
func fakeRoot(t *testing.T, accounts map[string]*user.User) {
	t.Helper()
	savedIDs, savedLookup := processIDs, lookupAccount
	processIDs = func() (int, int) { return 0, 0 }
	lookupAccount = func(name string) (*user.User, error) {
		if account, ok := accounts[name]; ok {
			return account, nil
		}
		return nil, user.UnknownUserError(name)
	}
	t.Cleanup(func() { processIDs, lookupAccount = savedIDs, savedLookup })
}

func TestProvisionJournalGivesBothDirectoriesTheNamedOwnerWithoutBeingRoot(t *testing.T) {
	fakeRoot(t, map[string]*user.User{"bosh": {Uid: "4242", Gid: "4343", Username: "bosh"}})
	calls := recordProvisionDirectory(t, nil)
	var out, stderr bytes.Buffer
	args := []string{"--directory", "/x/allocations", "--owner", "bosh"}
	if code := runProvisionJournal(args, &out, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	want := []provisionCall{{"/x/allocations", 4242, 4343}, {"/x/endpoint-cache", 4242, 4343}}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("calls %v, want %v", *calls, want)
	}
}

func TestProvisionJournalWithoutOwnerUsesTheProcessIDsForBothDirectories(t *testing.T) {
	fakeRoot(t, nil)
	processIDs = func() (int, int) { return 1001, 1002 }
	calls := recordProvisionDirectory(t, nil)
	var out, stderr bytes.Buffer
	if code := runProvisionJournal([]string{"--directory", "/x/allocations"}, &out, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	want := []provisionCall{{"/x/allocations", 1001, 1002}, {"/x/endpoint-cache", 1001, 1002}}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("calls %v, want %v", *calls, want)
	}
}

func TestProvisionJournalOwnerFailuresStopBeforeAnyProvisioning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ids      func() (int, int)
		accounts map[string]*user.User
		want     string
	}{
		{"not root", func() (int, int) { return 1000, 1000 }, nil, "--owner requires root"},
		{"unknown account", func() (int, int) { return 0, 0 }, nil, "runtime owner lookup failed"},
		{"bad uid", func() (int, int) { return 0, 0 },
			map[string]*user.User{"bosh": {Uid: "x", Gid: "1"}}, "invalid runtime UID"},
		{"bad gid", func() (int, int) { return 0, 0 },
			map[string]*user.User{"bosh": {Uid: "1", Gid: "x"}}, "invalid runtime GID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeRoot(t, tc.accounts)
			processIDs = tc.ids
			calls := recordProvisionDirectory(t, nil)
			var out, stderr bytes.Buffer
			args := []string{"--directory", "/x/allocations", "--owner", "bosh"}
			if code := runProvisionJournal(args, &out, &stderr); code != 1 {
				t.Fatalf("code %d: %s", code, stderr.String())
			}
			if len(*calls) != 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("calls %v, stderr %q, want %q", *calls, stderr.String(), tc.want)
			}
		})
	}
}

// ownerRecordingFake returns a provisioning fake that behaves like the real one for ownership. It creates a
// missing directory with the requested owner, accepts an existing one that has that owner, and refuses an
// existing one that has another owner without changing it.
func ownerRecordingFake(t *testing.T, existing map[string]provisionCall) map[string]provisionCall {
	t.Helper()
	saved := provisionDirectory
	provisionDirectory = func(path string, uid, gid int) error {
		if have, ok := existing[path]; ok {
			if have.uid != uid || have.gid != gid {
				return fmt.Errorf("%s is owned by %d:%d, want %d:%d", path, have.uid, have.gid, uid, gid)
			}
			return nil
		}
		existing[path] = provisionCall{path, uid, gid}
		return nil
	}
	t.Cleanup(func() { provisionDirectory = saved })
	return existing
}

func TestProvisionJournalWarnsWhenTheSiblingHasAnotherOwnerOnEveryMachine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uid, gid int
	}{
		{"another group", 1001, 9999},
		{"another user", 9999, 1002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeRoot(t, nil)
			processIDs = func() (int, int) { return 1001, 1002 }
			owners := ownerRecordingFake(t, map[string]provisionCall{
				"/x/endpoint-cache": {"/x/endpoint-cache", tc.uid, tc.gid},
			})
			var out, stderr bytes.Buffer
			if code := runProvisionJournal([]string{"--directory", "/x/allocations"}, &out, &stderr); code != 0 {
				t.Fatalf("code %d: %s", code, stderr.String())
			}
			wantPrefix := "warning: endpoint cache directory /x/endpoint-cache could not be provisioned: "
			if !strings.HasPrefix(stderr.String(), wantPrefix) || strings.Count(stderr.String(), "\n") != 1 {
				t.Fatalf("want one warning line, got %q", stderr.String())
			}
			if got := owners["/x/endpoint-cache"]; got.uid != tc.uid || got.gid != tc.gid {
				t.Fatalf("the sibling's owner was changed to %v", got)
			}
			if got := owners["/x/allocations"]; got.uid != 1001 || got.gid != 1002 {
				t.Fatalf("the journal owner is %v", got)
			}
		})
	}
}

func TestProvisionJournalJournalFailureIsOneLineForAPathWithLineBreaks(t *testing.T) {
	calls := recordProvisionDirectory(t, errors.New("refused"))
	var out, stderr bytes.Buffer
	if code := runProvisionJournal([]string{"--directory", "/x\ny/allocations"}, &out, &stderr); code != 1 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	if len(*calls) != 1 || out.Len() != 0 {
		t.Fatalf("calls %v, stdout %q", *calls, out.String())
	}
	line := stderr.String()
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") ||
		!strings.HasPrefix(line, "journal directory /x y/allocations could not be provisioned: ") {
		t.Fatalf("want one flattened line, got %q", line)
	}
}

func TestProvisionJournalWarnsWhenTheSiblingHasAnotherGroup(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Skip(err)
	}
	other := -1
	for _, g := range groups {
		if g != os.Getegid() {
			other = g
			break
		}
	}
	if other < 0 && os.Geteuid() == 0 {
		other = 4242
	}
	if other < 0 {
		t.Skip("the user belongs to one group only")
	}
	base := provisionTemp(t)
	sibling := filepath.Join(base, "endpoint-cache")
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(sibling, -1, other); err != nil {
		t.Skipf("cannot give the directory another group: %v", err)
	}
	var out, stderr bytes.Buffer
	args := []string{"--directory", filepath.Join(base, "allocations")}
	if code := runProvisionJournal(args, &out, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "warning: endpoint cache directory "+sibling+" could not be provisioned: ") {
		t.Fatalf("want a warning, got %q", stderr.String())
	}
	info, err := os.Lstat(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Gid) != other {
		t.Fatalf("the sibling's group was changed")
	}
}

func TestProvisionJournalCreatesTheSiblingWhateverTheDiscoveryModeSays(t *testing.T) {
	for _, mode := range []string{"off", "on", "auto", ""} {
		t.Run("discovery "+mode, func(t *testing.T) {
			base := provisionTemp(t)
			journal := filepath.Join(base, "allocations")
			raw, err := json.Marshal(map[string]any{
				"host":                           "never-connect.invalid",
				"storage_allocation_journal_dir": journal,
				"api_endpoint_discovery":         mode,
			})
			if err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(base, "cpi.json")
			if err := os.WriteFile(cfg, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			if code := runProvisionJournal([]string{"--config", cfg}, &out, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("code %d: %s", code, stderr.String())
			}
			requirePrivateOwnedDirectory(t, filepath.Join(base, "endpoint-cache"))
		})
	}
}
