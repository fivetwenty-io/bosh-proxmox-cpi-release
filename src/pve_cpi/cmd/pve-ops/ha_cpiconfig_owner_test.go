package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestRunHACPIConfig_ScopesEachEntryToItsDirector runs the doctor over a
// cpi-config entry whose band holds a local-disk VM that Director B created.
// With A's UUID the VM is B's to judge, so the run stays silent about it. With
// B's UUID, or with no UUID at all, the run judges it, and the run without a
// UUID says that only the band scoped it.
func TestRunHACPIConfig_ScopesEachEntryToItsDirector(t *testing.T) {
	t.Parallel()
	const entry = `{"name":"a","type":"pve","properties":{"pve_vmid_range_start":100,"pve_vmid_range_end":199`
	const (
		dirA = "aaaaaaaa-0000"
		dirB = "bbbbbbbb-0000"
	)
	cases := []struct {
		name     string
		doc      string
		flag     string
		judged   bool
		advises  bool
		wantNote string
	}{
		{name: "document names another Director", doc: `{"director_uuid":"` + dirA + `","cpis":[` + entry + `}}]}`},
		{name: "document names the owner", doc: `{"director_uuid":"` + dirB + `","cpis":[` + entry + `}}]}`, judged: true},
		{name: "entry wins over the document", doc: `{"director_uuid":"` + dirB + `","cpis":[` + entry +
			`},"director_uuid":"` + dirA + `"}]}`},
		{name: "flag names another Director", doc: `[` + entry + `}}]`, flag: dirA},
		{name: "no owner available", doc: `[` + entry + `}}]`, judged: true, advises: true, wantNote: "director_uuid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			localVM(f, "n1", "bosh-cpi;bosh-owner--"+dirB)
			deps := haDeps{
				openReader: func(*config.CPIConfig, *log.Logger) (HAReader, error) { return f, nil },
				now:        func() time.Time { return haNow },
			}
			path := filepath.Join(t.TempDir(), "cpi-config.json")
			if err := os.WriteFile(path, []byte(tc.doc), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			runHACPIConfig(context.Background(), path, haCfg("auto", "hold"), nil, tc.flag, true, &stdout, &stderr, deps)
			var out haMultiReport
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out.Clusters) != 1 {
				t.Fatalf("output %q (%v), stderr %q", stdout.String(), err, stderr.String())
			}
			got := findings(out.Clusters[0], haKindLocalUnprotected)
			if judged := len(got) > 0; judged != tc.judged {
				t.Errorf("local_unprotected for the other Director's VM = %+v, want judged %v", got, tc.judged)
			}
			notes := findings(out.Clusters[0], haKindAdvice)
			if advises := len(notes) > 0 && strings.Contains(notes[0].Detail, tc.wantNote); advises != tc.advises {
				t.Errorf("advice = %+v, want one naming %q: %v", notes, tc.wantNote, tc.advises)
			}
		})
	}
}

// TestRunHACPIConfig_SaysOnceThatNoDirectorUUIDIsKnown pins that a run whose
// entries name no Director UUID gives the advice once, on the first such
// entry, and names every entry it applies to. An entry that names its own
// UUID is left out of the list, and the other entries carry no copy.
func TestRunHACPIConfig_SaysOnceThatNoDirectorUUIDIsKnown(t *testing.T) {
	t.Parallel()
	doc := `[{"name":"a","type":"pve","properties":{"pve_vmid_range_start":100,"pve_vmid_range_end":199}},` +
		`{"name":"b","type":"pve","director_uuid":"aaaaaaaa-0000",` +
		`"properties":{"pve_vmid_range_start":200,"pve_vmid_range_end":299}},` +
		`{"name":"c","type":"pve","properties":{"pve_vmid_range_start":300,"pve_vmid_range_end":399}}]`
	f := haBaseFake()
	deps := haDeps{
		openReader: func(*config.CPIConfig, *log.Logger) (HAReader, error) { return f, nil },
		now:        func() time.Time { return haNow },
	}
	path := filepath.Join(t.TempDir(), "cpi-config.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runHACPIConfig(context.Background(), path, haCfg("auto", "hold"), nil, "", true, &stdout, &stderr, deps)
	var out haMultiReport
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out.Clusters) != 3 {
		t.Fatalf("output %q (%v), stderr %q", stdout.String(), err, stderr.String())
	}
	var notes []string
	for _, c := range out.Clusters {
		for _, n := range findings(c, haKindAdvice) {
			if strings.Contains(n.Detail, "no Director UUID is known") {
				notes = append(notes, c.Cluster+": "+n.Detail)
			}
		}
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "a: ") || !strings.Contains(notes[0], "the entries a and c, so") {
		t.Fatalf("advice = %q; want it once, on entry a, naming a and c", notes)
	}
}

// TestRunHACPIConfig_PutsTheNoDirectorUUIDAdviceOnAnEntryThatWasRead pins
// that the once-per-run advice lands on the first entry without a UUID whose
// cluster the doctor read, not on an entry whose cluster it couldn't reach and
// so judged nothing. When no such entry was read, the advice stays on the
// first entry without a UUID, so it is never dropped.
func TestRunHACPIConfig_PutsTheNoDirectorUUIDAdviceOnAnEntryThatWasRead(t *testing.T) {
	t.Parallel()
	doc := `[{"name":"a","type":"pve","properties":{"pve_host":"down","pve_vmid_range_start":100,"pve_vmid_range_end":199}},` +
		`{"name":"b","type":"pve","properties":{"pve_vmid_range_start":200,"pve_vmid_range_end":299}}]`
	run := func(t *testing.T, down func(host string) bool) map[string]bool {
		t.Helper()
		deps := haDeps{
			openReader: func(cfg *config.CPIConfig, _ *log.Logger) (HAReader, error) {
				if down(cfg.Host) {
					return nil, errFake
				}
				return haBaseFake(), nil
			},
			now: func() time.Time { return haNow },
		}
		path := filepath.Join(t.TempDir(), "cpi-config.json")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		runHACPIConfig(context.Background(), path, haCfg("auto", "hold"), nil, "", true, &stdout, &stderr, deps)
		var out haMultiReport
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out.Clusters) != 2 {
			t.Fatalf("output %q (%v), stderr %q", stdout.String(), err, stderr.String())
		}
		got := map[string]bool{}
		for _, c := range out.Clusters {
			for _, n := range findings(c, haKindAdvice) {
				if strings.Contains(n.Detail, "no Director UUID is known") {
					got[c.Cluster] = true
				}
			}
		}
		return got
	}
	if got := run(t, func(host string) bool { return host == "down" }); len(got) != 1 || !got["b"] {
		t.Errorf("advice on %v, want it once, on entry b, whose cluster was read", got)
	}
	if got := run(t, func(string) bool { return true }); len(got) != 1 || !got["a"] {
		t.Errorf("advice on %v, want it once, on entry a, when no entry was read", got)
	}
}
