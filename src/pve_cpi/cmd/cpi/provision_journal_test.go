package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
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
				if strings.Count(warning, "\n") != 1 || !strings.HasSuffix(warning, "\n") ||
					!strings.HasPrefix(warning, "warning: endpoint cache directory "+sibling+" could not be provisioned: ") {
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
