package handlers

import "testing"

func TestManagedHAResourceReadbackReadsAnAbsentFailbackAsOn(t *testing.T) {
	t.Parallel()
	stored := map[string]any{"sid": "vm:101", "state": "ignored", "comment": "marker"}
	on := map[string]any{"sid": "vm:101", "state": "ignored", "comment": "marker", "failback": true}
	if err := managedHAResourceFieldsMatch(stored, on); err != nil {
		t.Fatalf("PVE leaves out failback at its default of on, so the request is observed: %v", err)
	}
	off := map[string]any{"sid": "vm:101", "state": "ignored", "comment": "marker", "failback": false}
	if err := managedHAResourceFieldsMatch(stored, off); err == nil {
		t.Fatal("an absent failback reads as on and must not prove a request for off")
	}
	explicit := map[string]any{"sid": "vm:101", "state": "ignored", "comment": "marker", "failback": 0}
	if err := managedHAResourceFieldsMatch(explicit, on); err == nil {
		t.Fatal("a stored failback of 0 must not prove a request for on")
	}
	if err := managedHAResourceFieldsMatch(explicit, off); err != nil {
		t.Fatalf("a stored failback of 0 proves a request for off: %v", err)
	}
	if _, present := stored["failback"]; present {
		t.Fatal("the readback map must not be changed")
	}
}
