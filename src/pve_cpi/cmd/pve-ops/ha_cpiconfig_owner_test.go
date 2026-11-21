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
