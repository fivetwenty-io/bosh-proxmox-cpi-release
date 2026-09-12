package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the certification parser with records emitted by the real planner,
// journal and guarded root/E/ISO execution path, rather than a parallel schema.
func TestStorageTransitionPythonReadsProductionLedger(t *testing.T) {
	for _, clone := range []bool{false, true} {
		t.Run(map[bool]string{false: "import", true: "clone"}[clone], func(t *testing.T) {
			m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{ephemeral: true, iso: true, clone: clone})
			guarded := m.deps
			guarded.PVE = m.guard.Client()
			if err := createManagedVMRoot(context.Background(), guarded, m.parsed, m.shape, m.prepared.plan.Targets[0], 101, m.marker); err != nil {
				t.Fatal(err)
			}
			assertManagedVMRootGrowth(t, m, fixture, guarded)
			before := m.handle.Record()
			if _, err := m.execute(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]any{"before": before, "after": m.handle.Record()})
			if err != nil {
				t.Fatal(err)
			}
			scripts, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "..", "scripts"))
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "python3", "-c", `
import json, sys
sys.path.insert(0, sys.argv[1])
from _storage_placement_transition import transition_ledger
payload = json.load(sys.stdin)
before = transition_ledger(payload['before'])
after = transition_ledger(payload['after'])
assert before['acquired_bytes'] > 0
assert before['remaining_bytes'] > 0
assert before['planned_bytes'] == after['planned_bytes']
assert after['remaining_bytes'] == 0
assert after['acquired_bytes'] == after['planned_bytes']
assert payload['before']['id'] == payload['after']['id']
`, scripts)
			command.Stdin = bytes.NewReader(payload)
			if output, runErr := command.CombinedOutput(); runErr != nil {
				t.Fatalf("production ledger Python seam: %v: %s", runErr, output)
			}
		})
	}
}
