package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestRunHACPIConfig_SharesOneLedgerAndKeepsEachEntrysScope drives two entries
// that reach one cluster and share a host and VM ID band. The first entry's
// read of n2 fails with a transport fault, so the run's one ledger makes the
// second entry skip n2 and name why. The second entry is the non-leader of a
// band whose entries disagree on anti-affinity rules, so it stays silent about
// that disagreement while the leader reports it once. A later edit that built
// the ledger per entry, or that reordered the scope, would break one of these.
func TestRunHACPIConfig_SharesOneLedgerAndKeepsEachEntrysScope(t *testing.T) {
	t.Parallel()
	group := "bosh-aa-0123456789ab"
	newFake := func() *countingHA {
		f := replicaFake()
		f.guests[0].Tags += ";" + group
		f.guests[1].Tags += ";" + group
		return newCountingHA(f)
	}
	first, second := newFake(), newFake()
	first.volumesErr = map[string]error{"n2/local-zfs": lgUnreachable()}
	readers := []*countingHA{first, second}
	next := 0
	deps := haDeps{
		openReader: func(*config.CPIConfig, *log.Logger) (HAReader, error) {
			r := readers[next]
			next++
			return r, nil
		},
		now: func() time.Time { return haNow },
	}
	path := filepath.Join(t.TempDir(), "cpi-config.json")
	doc := `{"cpis":[` +
		`{"name":"a","type":"pve","properties":{"pve_ha":{"anti_affinity":"on"}}},` +
		`{"name":"b","type":"pve","properties":{"pve_ha":{"anti_affinity":"off"}}}]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	base := haCfgWith("auto", "replicate", "", `,"placement":{"anti_affinity":{"enabled":true}}`)
	var stdout, stderr bytes.Buffer
	code := runHACPIConfig(context.Background(), path, base, nil, "", true, &stdout, &stderr, deps)
	if code != exitError {
		t.Fatalf("exit code = %d, want %d; stderr %q", code, exitError, stderr.String())
	}
	if first.volumeReads["n2"] == 0 {
		t.Fatal("the first entry must have asked n2")
	}
	if second.volumeReads["n2"] != 0 {
		t.Errorf("the second entry asked n2 %d times, want 0 from the shared ledger", second.volumeReads["n2"])
	}
	out := stdout.String()
	if !strings.Contains(out, "an earlier read of it in this run failed") {
		t.Errorf("the second entry must name the first entry's failure: %s", out)
	}
	if got := strings.Count(out, haKindAntiAffinityAmbiguous); got != 1 {
		t.Errorf("anti_affinity_ambiguous appears %d times, want once from the leader: %s", got, out)
	}
}
