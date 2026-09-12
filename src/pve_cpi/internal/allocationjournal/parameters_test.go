package allocationjournal

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMutationParametersFreezeConcreteIdentity(t *testing.T) {
	j, _ := fixture(t)
	h := acquireVM(t, j)
	defer closeHandle(t, h)
	params, err := MutationParameters(map[string]any{"version": 1, "kind": "route", "vnet": "bosh-vnet", "subnet": "10.0.0.0/24", "cidr": "10.0.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	entry := step()
	entry.Parameters = params
	r.Steps = []Step{entry}
	save(t, h, r)
	params[0] = 'x'
	snapshot := h.Record()
	snapshot.Steps[0].Parameters[0] = 'x'
	if !json.Valid(h.Record().Steps[0].Parameters) {
		t.Fatal("record parameter aliases escaped")
	}
	changed := h.Record()
	changed.Steps[0].Parameters, err = MutationParameters(map[string]any{"version": 1, "kind": "route", "vnet": "different"})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Save(changed); err == nil {
		t.Fatal("planned infrastructure identity changed")
	}
	changed = h.Record()
	changed.Steps[0].Parameters = nil
	if err = h.Save(changed); err == nil {
		t.Fatal("planned parameters removed")
	}
}
func TestMutationParametersRejectUnboundedOrSecretContainers(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"kind":"route","password":"secret","version":1}`),
		json.RawMessage(`{"kind":"route","nodes":{"credentials":"secret"},"version":1}`),
		json.RawMessage(`{"kind":"route","version":2}`),
		json.RawMessage(`{"kind":"route","version":1,"version":1}`),
		json.RawMessage(`{"kind":"route","version":1} {}`),
		json.RawMessage(`{"kind":"route","version":1,"comment":"` + strings.Repeat("x", maxMutationParametersBytes) + `"}`),
	} {
		if err := validateMutationParameters(raw); err == nil {
			t.Fatalf("invalid parameter object accepted: %d bytes", len(raw))
		}
	}
	if _, err := MutationParameters([]string{"request", "secret"}); err == nil {
		t.Fatal("nonobject parameters accepted")
	}
}
