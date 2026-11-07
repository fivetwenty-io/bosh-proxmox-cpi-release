package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haTestDeps serves base as the loaded config and hands out the fakes in order
// of the clusters opened. opened records each effective config it was given.
type haTestDeps struct {
	deps   haDeps
	opened []*config.CPIConfig
}

func newHATestDeps(base *config.CPIConfig, readers ...HAReader) *haTestDeps {
	d := &haTestDeps{}
	d.deps = haDeps{
		loadConfig: func(string, io.Writer) (*config.CPIConfig, *log.Logger, error) { return base, log.NewNopLogger(), nil },
		openReader: func(cfg *config.CPIConfig, _ *log.Logger) (HAReader, error) {
			d.opened = append(d.opened, cfg)
			if len(readers) == 0 {
				return nil, errors.New("no reader")
			}
			r := readers[0]
			if len(readers) > 1 {
				readers = readers[1:]
			}
			if r == nil {
				return nil, errors.New("connection refused")
			}
			return r, nil
		},
		now: func() time.Time { return haNow },
	}
	return d
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runHATest(t *testing.T, d *haTestDeps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runHAWith(args, &stdout, &stderr, d.deps)
	return code, stdout.String(), stderr.String()
}

func TestRunHAHealthyTextAndJSON(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(haCfg("auto", "none"), haBaseFake())
	code, out, errOut := runHATest(t, d)
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	for _, frag := range []string{"ha: mode=auto effective=full", "headroom=ok(", "nodes:", "findings:\n  none", "result: 0 problem(s), 0 advice"} {
		if !strings.Contains(out, frag) {
			t.Errorf("output lacks %q:\n%s", frag, out)
		}
	}

	d = newHATestDeps(haCfg("auto", "none"), haBaseFake())
	code, out, _ = runHATest(t, d, "--json")
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	var report haReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if report.Mode != "auto" || len(report.Nodes) != 3 || report.Findings == nil || len(report.Findings) != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestRunHAProblemExitsOneAndAdviceDoesNot(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Advice = "add-qdevice: even"
	code, out, _ := runHATest(t, newHATestDeps(haCfg("auto", "none"), f))
	if code != exitOK || !strings.Contains(out, "[advice] qdevice_advice") {
		t.Fatalf("advice only: code = %d\n%s", code, out)
	}
	g := haBaseFake()
	g.rulesErr = errFake
	code, out, _ = runHATest(t, newHATestDeps(haCfg("auto", "none"), g))
	if code != exitError || !strings.Contains(out, "[problem] unchecked ha-rules") {
		t.Fatalf("problem: code = %d\n%s", code, out)
	}
	code, _, _ = runHATest(t, newHATestDeps(haCfg("auto", "none"), g), "--json")
	if code != exitError {
		t.Fatalf("json problem: code = %d", code)
	}
}

func TestRunHAOffModeStillProbes(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile = haHealthyProfile("off")
	f.profile.Effective = ha.EffectiveOff
	d := newHATestDeps(haCfg("off", "none"), f)
	code, out, _ := runHATest(t, d)
	if code != exitError || !strings.Contains(out, "off_mode_resource vm:100") || !strings.Contains(out, "ha: mode=off effective=off") {
		t.Fatalf("off mode: code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "nodes:\n  n1 online") {
		t.Fatalf("off mode output lacks the probed nodes:\n%s", out)
	}
}

func TestRunHAOffModeFailsOnUnreadableRules(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile = haHealthyProfile("off")
	f.profile.Effective = ha.EffectiveOff
	f.profile.RulesSupported = false
	f.resources = nil
	f.rulesErr = errFake
	code, out, _ := runHATest(t, newHATestDeps(haCfg("off", "none"), f))
	if code != exitError || !strings.Contains(out, "[problem] unchecked ha-rules") {
		t.Fatalf("off mode, unreadable rules: code = %d\n%s", code, out)
	}
}

func TestRunHAUsageErrors(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"unknown flag":       {"--bogus"},
		"positional":         {"extra"},
		"both input files":   {"--request", "a.json", "--cpi-config", "b.json"},
		"flag missing value": {"--config"},
	} {
		d := newHATestDeps(haCfg("auto", "none"), haBaseFake())
		code, _, errOut := runHATest(t, d, args...)
		if code != exitUsage || !strings.Contains(errOut, "usage: pve-cid ha") {
			t.Errorf("%s: code = %d, stderr = %q", name, code, errOut)
		}
	}
	code, _, errOut := runHATest(t, newHATestDeps(haCfg("auto", "none"), haBaseFake()), "--help")
	if code != exitOK || !strings.Contains(errOut, "--cpi-config") {
		t.Fatalf("help: code = %d, stderr = %q", code, errOut)
	}
}

func TestRunHALoadAndOpenFailures(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(haCfg("auto", "none"))
	d.deps.loadConfig = func(string, io.Writer) (*config.CPIConfig, *log.Logger, error) {
		return nil, nil, errors.New("pve-cid: config load failed (x)")
	}
	code, _, errOut := runHATest(t, d)
	if code != exitError || !strings.Contains(errOut, "config load failed") {
		t.Fatalf("load failure: code = %d, stderr = %q", code, errOut)
	}
	code, _, errOut = runHATest(t, newHATestDeps(haCfg("auto", "none"), nil))
	if code != exitError || !strings.Contains(errOut, "cannot reach the cluster: connection refused") {
		t.Fatalf("open failure: code = %d, stderr = %q", code, errOut)
	}
}

func TestRunHARequestAppliesContext(t *testing.T) {
	t.Parallel()
	request := `{"method":"create_vm","arguments":[],"context":{"director_uuid":"` + haDirector +
		`","pve_vmid_range_start":5000,"pve_vmid_range_end":5999,"pve_bogus_key":1}}`
	path := writeTemp(t, "request.json", request)
	f := haBaseFake()
	f.profile = haHealthyProfile("off")
	f.profile.Effective = ha.EffectiveOff
	f.guests[0].Tags = "bosh-cpi"
	f.resources = []HAResourceRow{{SID: "vm:7", State: "started", Comment: "bosh-cpi:v1 owner=other"}}
	d := newHATestDeps(haCfg("off", "none"), f)
	code, out, errOut := runHATest(t, d, "--request", path)
	if code != exitOK {
		t.Fatalf("code = %d\n%s\n%s", code, out, errOut)
	}
	if len(d.opened) != 1 || d.opened[0].VMIDRangeStart != 5000 || d.opened[0].VMIDRangeEnd != 5999 {
		t.Fatalf("opened config = %+v", d.opened)
	}
	// The request's Director owns nothing here, so the other Director's
	// resource is not reported, and the unknown property is noted.
	if strings.Contains(out, "off_mode_resource") || !strings.Contains(out, `"pve_bogus_key" is not one the CPI reads`) {
		t.Fatalf("output:\n%s", out)
	}

	bare := writeTemp(t, "bare.json", `{"director_uuid":"`+haDirector+`"}`)
	if code, _, _ = runHATest(t, newHATestDeps(haCfg("off", "none"), f), "--request", bare); code != exitOK {
		t.Fatalf("bare context: code = %d", code)
	}
}

func TestRunHARequestFailures(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"not json":       "nope",
		"array":          "[]",
		"context scalar": `{"context":"oops"}`,
	} {
		path := writeTemp(t, "r.json", content)
		code, _, errOut := runHATest(t, newHATestDeps(haCfg("auto", "none"), haBaseFake()), "--request", path)
		if code != exitUsage || !strings.Contains(errOut, "pve-cid ha:") {
			t.Errorf("%s: code = %d, stderr = %q", name, code, errOut)
		}
	}
	code, _, _ := runHATest(t, newHATestDeps(haCfg("auto", "none"), haBaseFake()), "--request", filepath.Join(t.TempDir(), "missing.json"))
	if code != exitUsage {
		t.Fatalf("missing file: code = %d", code)
	}
	bad := writeTemp(t, "bad.json", `{"context":{"pve_vmid_range_start":50}}`)
	code, _, errOut := runHATest(t, newHATestDeps(haCfg("auto", "none"), haBaseFake()), "--request", bad)
	if code != exitError || !strings.Contains(errOut, "do not apply") {
		t.Fatalf("bad overrides: code = %d, stderr = %q", code, errOut)
	}
}

func TestReadLimitedRefusesLargeFile(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "big.json", strings.Repeat("x", haInputLimit+1))
	if _, err := readLimited(path); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

// sharedNFS is a storage index with one export both clusters mount.
func sharedNFS() []pve.StorageInfo {
	return []pve.StorageInfo{{Name: "nfs1", Type: "nfs", Shared: true, Server: "filer", Export: "/vol/bosh", Content: "images"}}
}

const cpiConfigTwoClusters = `{"cpis":[
 {"name":"az1","type":"pve","properties":{}},
 {"name":"az2","type":"pve","properties":{
   "pve_vmid_range_start":110000,"pve_vmid_range_end":110999,
   "pve_disk_vmid_range_start":111000,"pve_disk_vmid_range_end":111999,
   "pve_stemcell_template_vmid_range_start":112000,"pve_stemcell_template_vmid_range_end":112999,
   "pve_parked_disk_vmid_range_start":113000,"pve_parked_disk_vmid_range_end":113999}}]}`

func sharedFake(next pve.VMIDBand) *fakeHA {
	f := haBaseFake()
	f.storages = sharedNFS()
	f.nextID = next
	return f
}

func TestRunHACPIConfigDisjointBandsExitZero(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cpi.json", cpiConfigTwoClusters)
	a := sharedFake(pve.VMIDBand{Start: 200000, End: 200999})
	b := sharedFake(pve.VMIDBand{Start: 210000, End: 210999})
	d := newHATestDeps(haCfg("auto", "none"), a, b)
	code, out, errOut := runHATest(t, d, "--cpi-config", path)
	if code != exitOK {
		t.Fatalf("code = %d\n%s\n%s", code, out, errOut)
	}
	for _, frag := range []string{"cluster az1", "cluster az2", "  ha: mode=auto", "shared exports across clusters:\n  none"} {
		if !strings.Contains(out, frag) {
			t.Errorf("output lacks %q:\n%s", frag, out)
		}
	}
	if len(d.opened) != 2 || d.opened[1].VMIDRangeStart != 110000 {
		t.Fatalf("opened = %+v", d.opened)
	}
}

func TestRunHACPIConfigOverlapExitsOne(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cpi.json", `{"cpis":[{"name":"az1","properties":{}},{"name":"az2","properties":{}}]}`)
	a := sharedFake(pve.VMIDBand{Start: 200000, End: 200999})
	b := sharedFake(pve.VMIDBand{Start: 210000, End: 210999})
	code, out, _ := runHATest(t, newHATestDeps(haCfg("auto", "none"), a, b), "--cpi-config", path)
	if code != exitError || !strings.Contains(out, "[problem] band_overlap [az1, az2]") {
		t.Fatalf("code = %d\n%s", code, out)
	}
	code, out, _ = runHATest(t, newHATestDeps(haCfg("auto", "none"), sharedFake(pve.VMIDBand{Start: 200000, End: 200999}),
		sharedFake(pve.VMIDBand{Start: 210000, End: 210999})), "--cpi-config", path, "--json")
	var multi haMultiReport
	if code != exitError || json.Unmarshal([]byte(out), &multi) != nil {
		t.Fatalf("json: code = %d\n%s", code, out)
	}
	if len(multi.Clusters) != 2 || multi.Clusters[0].Cluster != "az1" || len(multi.SharedExports) == 0 ||
		multi.SharedExports[0].Kind != "band_overlap" || !multi.SharedExports[0].Failing {
		t.Fatalf("multi = %+v", multi)
	}
}

func TestRunHACPIConfigUnreadableClusterIsUnchecked(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cpi.json", cpiConfigTwoClusters)
	a := sharedFake(pve.VMIDBand{Start: 200000, End: 200999})
	d := newHATestDeps(haCfg("auto", "none"), a, nil)
	code, out, _ := runHATest(t, d, "--cpi-config", path)
	if code != exitError {
		t.Fatalf("code = %d\n%s", code, out)
	}
	for _, frag := range []string{"[problem] unchecked cluster: cannot reach the cluster: connection refused",
		"[problem] unchecked [az2]: the cluster could not be read"} {
		if !strings.Contains(out, frag) {
			t.Errorf("output lacks %q:\n%s", frag, out)
		}
	}

	for name, mutate := range map[string]func(*fakeHA){
		"storages": func(f *fakeHA) { f.storagesErr = errFake },
		"next-id":  func(f *fakeHA) { f.nextIDErr = errFake },
	} {
		bad := sharedFake(pve.VMIDBand{Start: 210000, End: 210999})
		mutate(bad)
		code, out, _ = runHATest(t, newHATestDeps(haCfg("auto", "none"), a, bad), "--cpi-config", path)
		if code != exitError || !strings.Contains(out, "unchecked [az2]") {
			t.Errorf("%s: code = %d\n%s", name, code, out)
		}
	}
}

func TestRunHACPIConfigEntryThatDoesNotApplyIsUnchecked(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cpi.json", `{"cpis":[{"name":"az1","properties":{"pve_vmid_range_start":50}}]}`)
	d := newHATestDeps(haCfg("auto", "none"), haBaseFake())
	code, out, _ := runHATest(t, d, "--cpi-config", path)
	if code != exitError || !strings.Contains(out, "the entry's properties do not apply") || len(d.opened) != 0 {
		t.Fatalf("code = %d, opened = %d\n%s", code, len(d.opened), out)
	}
}

func TestRunHACPIConfigPossibleSharedBackingDoesNotFail(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cpi.json", `{"cpis":[{"name":"az1","properties":{}},{"name":"az2","properties":{}}]}`)
	a := sharedFake(pve.VMIDBand{Start: 200000, End: 200999})
	b := sharedFake(pve.VMIDBand{Start: 210000, End: 210999})
	a.storages = []pve.StorageInfo{{Name: "nfs1", Type: "nfs", Shared: true, Server: "filer-a", Export: "/vol/bosh", Content: "images"}}
	b.storages = []pve.StorageInfo{{Name: "nfs1", Type: "nfs", Shared: true, Server: "filer-b", Export: "/vol/bosh", Content: "images"}}
	code, out, _ := runHATest(t, newHATestDeps(haCfg("auto", "none"), a, b), "--cpi-config", path)
	if code != exitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "[advice] possible_shared_backing [az1, az2]") {
		t.Fatalf("different server names for one export path should be listed:\n%s", out)
	}
}

func TestParseCPIConfigShapesAndTypes(t *testing.T) {
	t.Parallel()
	object, err := parseCPIConfig([]byte(`{"cpis":[{"name":"a","type":"pve"},{"name":" b "},{"name":"c","type":"PVE"}]}`))
	if err != nil || len(object) != 3 || object[1].Name != "b" {
		t.Fatalf("object form = %+v, %v", object, err)
	}
	bare, err := parseCPIConfig([]byte(` [{"name":"a","type":"pve"}] `))
	if err != nil || len(bare) != 1 || bare[0].Name != "a" {
		t.Fatalf("bare list = %+v, %v", bare, err)
	}
	mixed, err := parseCPIConfig([]byte(`{"cpis":[{"name":"a","type":"pve"},{"name":"v","type":"vsphere"}]}`))
	if err != nil || len(mixed) != 1 || mixed[0].Name != "a" {
		t.Fatalf("a non-pve entry must be dropped: %+v, %v", mixed, err)
	}
	// A dropped entry still holds its name, so a duplicate stays an error.
	if _, err := parseCPIConfig([]byte(`{"cpis":[{"name":"a","type":"pve"},{"name":"a","type":"vsphere"}]}`)); err == nil {
		t.Fatal("a duplicate name across types should fail")
	}
}

func TestRunHACPIConfigSkipsNonPVEEntries(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cpi.json", `{"cpis":[{"name":"az1","type":"pve","properties":{}},`+
		`{"name":"other","type":"vsphere","properties":{"pve_vmid_range_start":110000}}]}`)
	d := newHATestDeps(haCfg("auto", "none"), sharedFake(pve.VMIDBand{Start: 200000, End: 200999}))
	code, out, errOut := runHATest(t, d, "--cpi-config", path)
	if code != exitOK || len(d.opened) != 1 || strings.Contains(out, "cluster other") {
		t.Fatalf("code = %d, opened = %d\n%s\n%s", code, len(d.opened), out, errOut)
	}
}

func TestRunHACPIConfigInputFailures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ content, hint string }{
		"yaml":       {"cpis:\n- name: az1\n", "yq -o=json"},
		"no entries": {`{"cpis":[]}`, "no cpis entries"},
		"empty list": {`[]`, "no cpis entries"},
		"no name":    {`{"cpis":[{"properties":{}}]}`, "has no name"},
		"duplicate":  {`{"cpis":[{"name":"a"},{"name":"a"}]}`, "more than once"},
		"not pve":    {`{"cpis":[{"name":"a","type":"vsphere"}]}`, "no cpis entry of type pve"},
		"bad list":   {`{"cpis":{"name":"a"}}`, "not a list of entries"},
		"bad array":  {`[1]`, "yq -o=json"},
		"rendered":   {`{"host":"pve","node":"n1","vm_storage":"local-lvm"}`, "/var/vcap/jobs/pve_cpi/config/cpi.json"},
		"no cpis":    {`{"other":1}`, "no cpis entries"},
	} {
		path := writeTemp(t, "cpi.json", tc.content)
		code, _, errOut := runHATest(t, newHATestDeps(haCfg("auto", "none"), haBaseFake()), "--cpi-config", path)
		if code != exitUsage || !strings.Contains(errOut, "pve-cid ha:") || !strings.Contains(errOut, tc.hint) {
			t.Errorf("%s: code = %d, stderr = %q", name, code, errOut)
		}
	}
	code, _, _ := runHATest(t, newHATestDeps(haCfg("auto", "none"), haBaseFake()), "--cpi-config", filepath.Join(t.TempDir(), "none.json"))
	if code != exitUsage {
		t.Fatalf("missing file: code = %d", code)
	}
}

func TestParseNextID(t *testing.T) {
	t.Parallel()
	cases := map[string]pve.VMIDBand{
		`{"lower":5000,"upper":6000}`:     {Start: 5000, End: 5999},
		`"lower=5000,upper=6000"`:         {Start: 5000, End: 5999},
		`{"lower":"5000","upper":"6000"}`: {Start: 5000, End: 5999},
		`{"lower":7000}`:                  pve.NextIDRange(7000, 0),
		`null`:                            pve.NextIDRange(0, 0),
		`{}`:                              pve.NextIDRange(0, 0),
		`""`:                              pve.NextIDRange(0, 0),
	}
	for raw, want := range cases {
		got, err := parseNextID(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("parseNextID(%s) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	for _, raw := range []string{`{"lower":"x"}`, `"lower=abc"`, `[1]`, `{"lower":1.5}`, `12`} {
		if _, err := parseNextID(json.RawMessage(raw)); err == nil {
			t.Errorf("parseNextID(%s) succeeded, want an error", raw)
		}
	}
}

func TestMainDispatchesHA(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"ha", "--bogus"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: pve-cid ha") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	stdout.Reset()
	printUsage(&stdout)
	if !strings.Contains(stdout.String(), "pve-cid ha [--config PATH]") {
		t.Fatalf("usage lacks the ha line:\n%s", stdout.String())
	}
}
