package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func provisionTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
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
