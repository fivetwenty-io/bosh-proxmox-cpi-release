package allocationjournal

import "testing"

func TestVolumeBirthParametersOnlyAdmitConcreteAbsence(t *testing.T) {
	for _, kind := range []string{"ephemeral_birth", "persistent_birth"} {
		if _, err := MutationParameters(map[string]any{"version": 1, "kind": kind, "absence_verified": true}); err != nil {
			t.Fatal(err)
		}
		for _, fields := range []map[string]any{
			{"version": 1, "kind": kind},
			{"version": 1, "kind": kind, "absence_verified": false},
			{"version": 1, "kind": kind, "absence_verified": "true"},
			{"version": 1, "kind": kind, "absence_verified": true, "digest": "unbound"},
		} {
			if _, err := MutationParameters(fields); err == nil {
				t.Fatal("incomplete absence accepted")
			}
		}
	}
	if _, err := MutationParameters(map[string]any{"version": 1, "kind": "ha_rule", "absence_verified": true}); err == nil {
		t.Fatal("birth proof accepted for other mutation")
	}
}
