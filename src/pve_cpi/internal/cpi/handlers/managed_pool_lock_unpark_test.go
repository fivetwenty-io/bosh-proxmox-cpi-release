package handlers

import "testing"

// createdManagedVM is a VM allocation whose root exists, ready for the
// post-create steps.
func createdManagedVM(t *testing.T) *managedVMAllocation {
	t.Helper()
	m, _, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{})
	guarded := m.deps
	guarded.PVE = m.guard.Client()
	if err := createManagedVMRoot(t.Context(), guarded, m.parsed, m.shape, m.prepared.plan.Targets[0], 101, m.marker); err != nil {
		t.Fatal(err)
	}
	return m
}
