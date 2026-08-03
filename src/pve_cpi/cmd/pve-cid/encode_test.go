package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

func TestRunEncode_PlainRoundTrip(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runEncode([]string{
		"--volid", "local-lvm:vm-100-disk-0",
		"--pool", "local-lvm",
		"--node", "pve1",
		"--az", "az1",
		"--opt", "iothread=1",
		"--opt", "cache=writeback",
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	lines := strings.SplitN(strings.TrimSpace(stdout.String()), "\n", 2)
	if len(lines) != 2 {
		t.Fatalf("expected 2 output lines, got %d: %q", len(lines), stdout.String())
	}
	cid := lines[0]
	if !strings.HasPrefix(cid, "pvd-") {
		t.Fatalf("expected a pvd- envelope, got %q", cid)
	}

	// Round trip through the same decoder DecodeCID/pve.ParseEncodedDiskCID uses.
	bareCID, meta, err := pve.ParseEncodedDiskCID(cid)
	if err != nil {
		t.Fatalf("ParseEncodedDiskCID: %v", err)
	}
	if bareCID != "local-lvm:vm-100-disk-0" {
		t.Errorf("bareCID = %q, want %q", bareCID, "local-lvm:vm-100-disk-0")
	}
	if meta == nil || meta.Pool != "local-lvm" || meta.Node != "pve1" || meta.AZ != "az1" {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Opts["iothread"] != "1" || meta.Opts["cache"] != "writeback" {
		t.Errorf("Opts = %v", meta.Opts)
	}
}

func TestRunEncode_JSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runEncode([]string{"--volid", "local:vm-1-disk-0", "--json"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	var res encodeResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("json unmarshal: %v; got:\n%s", err, stdout.String())
	}
	if !strings.HasPrefix(res.CID, "pvd-") {
		t.Errorf("CID = %q, want pvd- prefix", res.CID)
	}
	if res.Length != len(res.CID) {
		t.Errorf("Length = %d, want %d", res.Length, len(res.CID))
	}
	if res.Compressed {
		t.Errorf("Compressed = true for a short CID")
	}
	if res.OverTarget {
		t.Errorf("OverTarget = true for a short CID")
	}
}

// TestRunEncode_OverTargetWarnsOnStderr covers a plain pvd- CID that exceeds
// the varchar(255) target: it must print a prominent warning to stderr
// (table mode) even though the command still exits 0 — enforcement is the
// Director column bound, not this tool.
func TestRunEncode_OverTargetWarnsOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	longVolid := "ceph-rbd-nvme-tier1:" + repeatString("x", 400)
	code := runEncode([]string{"--volid", longVolid}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (advisory only); stderr=%s", code, exitOK, stderr.String())
	}
	cid := strings.SplitN(stdout.String(), "\n", 2)[0]
	if len(cid) <= pve.DiskCIDLengthTarget {
		t.Fatalf("test setup: expected an over-target CID, got length %d", len(cid))
	}
	if !strings.Contains(stderr.String(), "WARNING") || !strings.Contains(stderr.String(), "255") {
		t.Errorf("expected an over-target warning on stderr, got:\n%s", stderr.String())
	}
}

// TestRunEncode_OverTargetJSON covers the --json contract: over_target is
// true for a CID exceeding the length target and false otherwise.
func TestRunEncode_OverTargetJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	longVolid := "ceph-rbd-nvme-tier1:" + repeatString("x", 400)
	code := runEncode([]string{"--volid", longVolid, "--json"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	var res encodeResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("json unmarshal: %v; got:\n%s", err, stdout.String())
	}
	if !res.OverTarget {
		t.Errorf("OverTarget = false, want true for a %d-byte CID (target %d)", res.Length, pve.DiskCIDLengthTarget)
	}
	// --json mode carries the signal in the struct; stderr stays clean so
	// scripted/machine consumers do not have to filter warning text out of
	// their own stderr stream.
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output in --json mode, got:\n%s", stderr.String())
	}
}

func TestRunEncode_CompressForcesOversized(t *testing.T) {
	var stdout, stderr bytes.Buffer
	longVolid := "ceph-rbd-nvme-tier1:" + repeatString("x", 400)
	code := runEncode([]string{"--volid", longVolid, "--compress"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "pvz-") {
		t.Errorf("expected a pvz- envelope for an oversized volid with --compress, got:\n%s", stdout.String())
	}
}

func TestRunEncode_MissingVolidIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runEncode(nil, &stdout, &stderr)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
}

func TestRunEncode_InvalidVolidIsError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runEncode([]string{"--volid", "no-colon-here"}, &stdout, &stderr)
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
}

func TestOptFlag_SetAndString(t *testing.T) {
	o := optFlag{}
	if err := o.Set("iothread=1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := o.Set("badentry"); err == nil {
		t.Error("expected error for entry without '='")
	}
	if o["iothread"] != "1" {
		t.Errorf("o[\"iothread\"] = %q, want %q", o["iothread"], "1")
	}
	if s := o.String(); s != "iothread=1" {
		t.Errorf("String() = %q, want %q", s, "iothread=1")
	}
}
