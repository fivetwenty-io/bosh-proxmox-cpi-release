package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRun_NoArgsPrintsUsageAndExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "pve-ops") {
		t.Errorf("usage not printed to stderr: %q", stderr.String())
	}
}

func TestRun_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "bogus"`) {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRun_Help(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{arg}, &stdout, &stderr)
		if code != exitOK {
			t.Errorf("run(%q) exit code = %d, want %d", arg, code, exitOK)
		}
		if !strings.Contains(stdout.String(), "Usage:") {
			t.Errorf("run(%q) stdout missing usage text: %q", arg, stdout.String())
		}
	}
}

func TestRun_Version(t *testing.T) {
	for _, arg := range []string{"-version", "--version", "version"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{arg}, &stdout, &stderr)
		if code != exitOK {
			t.Errorf("run(%q) exit code = %d, want %d", arg, code, exitOK)
		}
		if !strings.HasPrefix(stdout.String(), "pve-ops ") {
			t.Errorf("run(%q) stdout = %q, want prefix %q", arg, stdout.String(), "pve-ops ")
		}
	}
}

// TestRun_DecodeSmoke exercises the full offline dispatch path end to end
// (run -> runDecode -> DecodeCID) the way an operator invokes the binary:
// "pve-ops decode ':light:nfs:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2'".
func TestRun_DecodeSmoke(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"decode", ":light:nfs:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"family: stemcell-light",
		"storage: nfs",
		"filename: bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2",
		"sha8: cafebabe",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("decode smoke output missing %q; got:\n%s", want, out)
		}
	}
}

// TestRun_EncodeSmoke exercises the full offline dispatch path for encode.
func TestRun_EncodeSmoke(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"encode", "--volid", "local-lvm:vm-100-disk-0"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "pvd-") {
		t.Errorf("encode smoke output missing pvd- prefix: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "length:") {
		t.Errorf("encode smoke output missing length line: %q", stdout.String())
	}
}

// TestRun_LocateAndStemcellsRequireConfig proves the online subcommands fail
// fast with a clear error when no CPI config can be resolved (no --config,
// no $PVE_CPI_CONFIG, and the release default path does not exist in a test
// environment), rather than hanging or panicking.
func TestRun_LocateAndStemcellsRequireConfig(t *testing.T) {
	t.Setenv("PVE_CPI_CONFIG", "")
	for _, args := range [][]string{
		{"locate", "--config", "/nonexistent/cpi.json", "5042"},
		// Positional before flags — parseWithPositionals must accept the
		// usage-text argument order too (getting past arg validation to the
		// config-load failure proves the flags were parsed).
		{"locate", "5042", "--config", "/nonexistent/cpi.json"},
		{"stemcells", "--config", "/nonexistent/cpi.json"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != exitError {
			t.Errorf("run(%v) exit code = %d, want %d; stderr=%s", args, code, exitError, stderr.String())
		}
		if !strings.Contains(stderr.String(), "config load failed") {
			t.Errorf("run(%v) stderr = %q, want a config-load-failed message", args, stderr.String())
		}
	}
}

func TestPrintUsage_ListsAllSubcommands(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	out := buf.String()
	for _, sub := range []string{"decode", "encode", "locate", "stemcells"} {
		if !strings.Contains(out, sub) {
			t.Errorf("usage text missing subcommand %q", sub)
		}
	}
}

func TestRunAs_DeprecatedAliasNoticeOnStderrOnly(t *testing.T) {
	const notice = "pve-cid is deprecated and will be removed in 0.13.0; run pve-ops instead.\n"
	args := []string{"encode", "--volid", "local-lvm:vm-100-disk-0"}

	var newOut, newErr bytes.Buffer
	newCode := runAs("pve-ops", args, &newOut, &newErr)
	if newCode != exitOK {
		t.Fatalf("pve-ops exit code = %d, want %d; stderr=%s", newCode, exitOK, newErr.String())
	}
	if strings.Contains(newErr.String(), "deprecated") {
		t.Errorf("pve-ops stderr = %q, want no deprecation notice", newErr.String())
	}

	var oldOut, oldErr bytes.Buffer
	oldCode := runAs("pve-cid", args, &oldOut, &oldErr)
	if oldCode != newCode {
		t.Errorf("pve-cid exit code = %d, want %d", oldCode, newCode)
	}
	if !strings.HasPrefix(oldErr.String(), notice) {
		t.Errorf("pve-cid stderr = %q, want prefix %q", oldErr.String(), notice)
	}
	if strings.Count(oldErr.String(), "deprecated") != 1 {
		t.Errorf("pve-cid stderr = %q, want exactly one notice", oldErr.String())
	}
	if oldOut.String() != newOut.String() {
		t.Errorf("pve-cid stdout = %q, want the pve-ops stdout %q", oldOut.String(), newOut.String())
	}
	if strings.Contains(oldOut.String(), "deprecated") {
		t.Errorf("pve-cid stdout carries the notice: %q", oldOut.String())
	}
}

func TestRunAs_DeprecatedAliasKeepsJSONStdoutClean(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAs("pve-cid", []string{"decode", "--json", ":light:nfs:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\n%s", err, stdout.String())
	}
}

func TestRunAs_DeprecatedAliasKeepsExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAs("pve-cid", nil, &stdout, &stderr); code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
}
