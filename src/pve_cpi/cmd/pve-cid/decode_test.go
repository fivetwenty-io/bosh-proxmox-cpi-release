package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunDecode_Table(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDecode([]string{":light:nfs:import/bosh-stemcell-x-1-cafebabe.qcow2"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"family: stemcell-light", "storage: nfs", "sha8: cafebabe"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRunDecode_JSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDecode([]string{"--json", ":light:nfs:import/bosh-stemcell-x-1-cafebabe.qcow2"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	var d DecodedCID
	if err := json.Unmarshal(stdout.Bytes(), &d); err != nil {
		t.Fatalf("json unmarshal: %v; got:\n%s", err, stdout.String())
	}
	if d.Family != FamilyStemcellLight {
		t.Errorf("Family = %q, want %q", d.Family, FamilyStemcellLight)
	}
	if d.SHA8 != "cafebabe" {
		t.Errorf("SHA8 = %q, want %q", d.SHA8, "cafebabe")
	}
}

// TestRunDecode_FlagAfterPositional verifies the usage-text form
// "pve-cid decode <cid> --json" works: stdlib flag stops parsing at the
// first non-flag argument, so parseWithPositionals must resume flag
// parsing after extracting the CID.
func TestRunDecode_FlagAfterPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDecode([]string{":light:nfs:import/bosh-stemcell-x-1-cafebabe.qcow2", "--json"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	var d DecodedCID
	if err := json.Unmarshal(stdout.Bytes(), &d); err != nil {
		t.Fatalf("json unmarshal: %v; got:\n%s", err, stdout.String())
	}
	if d.Family != FamilyStemcellLight {
		t.Errorf("Family = %q, want %q", d.Family, FamilyStemcellLight)
	}
}

func TestRunDecode_GarbageExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDecode([]string{"not-a-real-cid"}, &stdout, &stderr)
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if stderr.Len() == 0 {
		t.Error("expected an error message on stderr")
	}
}

func TestRunDecode_UsageErrors(t *testing.T) {
	cases := [][]string{
		{},                    // missing CID
		{"a", "b"},            // too many args
		{"--bogus-flag", "x"}, // unknown flag
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := runDecode(args, &stdout, &stderr)
		if code != exitUsage {
			t.Errorf("runDecode(%v) exit code = %d, want %d", args, code, exitUsage)
		}
	}
}
