package handlers

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// repeatedLineKeys returns the top-level keys a JSON log line carries more
// than once, which a map decode would hide.
func repeatedLineKeys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if _, err := dec.Token(); err != nil {
		t.Fatalf("line %q: %v", line, err)
	}
	seen := map[string]int{}
	var out []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		key, _ := tok.(string)
		seen[key]++
		if seen[key] == 2 {
			out = append(out, key)
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
	}
	return out
}

// TestReplicaTouchLogsEachKeyOnce runs the widen a touch call makes, with the
// handler's logger already naming the VM, and checks that no line repeats a
// key, as the widen line once repeated vmid three times and node twice.
func TestReplicaTouchLogsEachKeyOnce(t *testing.T) {
	var buf bytes.Buffer
	base, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatal(err)
	}
	logger := base.With(log.Int(metadataKeyVMID, 100), log.String("node", "pve01"))
	r := newReplicaRig(config.HAModeAuto)
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.ownJob(1)
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	loc := pve.VMLocation{Node: "pve01", Found: true, Tags: "bosh-cpi;" + haReplicaTag}
	if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc, logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.pinNodes() != "pve01,pve02" {
		t.Fatalf("pin nodes %q, want the partner added", r.pinNodes())
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	widened := false
	for _, line := range lines {
		if rep := repeatedLineKeys(t, line); len(rep) != 0 {
			t.Errorf("line %s repeats %v", line, rep)
		}
		widened = widened || strings.Contains(line, "the pin names the partner now")
	}
	if !widened {
		t.Fatalf("no widen line in %q", buf.String())
	}
}
