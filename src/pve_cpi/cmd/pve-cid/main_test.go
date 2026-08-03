package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_NoArgsPrintsUsageAndExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "pve-cid") {
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
		if !strings.HasPrefix(stdout.String(), "pve-cid ") {
			t.Errorf("run(%q) stdout = %q, want prefix %q", arg, stdout.String(), "pve-cid ")
		}
	}
}

// TestRun_DecodeSmoke exercises the full offline dispatch path end to end
// (run -> runDecode -> DecodeCID) the way an operator invokes the binary:
// "pve-cid decode ':light:nfs:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2'".
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
