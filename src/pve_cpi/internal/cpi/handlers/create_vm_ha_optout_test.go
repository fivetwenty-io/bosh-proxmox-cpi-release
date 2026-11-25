package handlers_test

import (
	"slices"
	"strings"
	"testing"
)

// TestCreateVM_OptedOutVMIsNotRefusedUnderManaged drives create_vm through
// its handler under pve.ha.mode managed, on a cluster whose HA profile misses
// a prerequisite and with a request that carries no director_uuid. Each of
// those refuses a VM that keeps HA before the clone. A VM whose
// cloud_properties set ha to false is created and tagged bosh-ha-off,
// because create_vm reads the property before the managed refusal runs.
func TestCreateVM_OptedOutVMIsNotRefusedUnderManaged(t *testing.T) {
	t.Parallel()
	for name, directorUUID := range map[string]string{"profile gap": "director-uuid-1", "no director_uuid": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			control := newManagedRefusalRig(t, refusingProfile(), nil, nil)
			if err := control.create(t, directorUUID, map[string]any{}); err == nil {
				t.Fatal("the rig must refuse a VM that keeps HA, or the test proves nothing")
			}
			control.wantNothingCreated(t)

			r := newManagedRefusalRig(t, refusingProfile(), nil, nil)
			if err := r.create(t, directorUUID, map[string]any{"ha": false}); err != nil {
				t.Fatalf("create_vm refused an opted-out VM under managed: %v", err)
			}
			if r.created.Load()+r.cloned.Load() == 0 {
				t.Fatal("create_vm sent no create or clone to PVE for an opted-out VM")
			}
			if tags := r.sentTags(); !slices.Contains(strings.Split(tags, ";"), "bosh-ha-off") {
				t.Fatalf("create_vm sent the tags %q; want bosh-ha-off on the opted-out VM", tags)
			}
		})
	}
}

// TestCreateVM_RejectsANonBooleanHAProperty drives create_vm through its
// handler with an ha cloud property that is not a boolean. The call fails
// with an error that names the property before anything reaches PVE.
func TestCreateVM_RejectsANonBooleanHAProperty(t *testing.T) {
	t.Parallel()
	r := newManagedRefusalRig(t, refusingProfile(), nil, nil)
	err := r.create(t, "director-uuid-1", map[string]any{"ha": "false"})
	if err == nil || !strings.Contains(err.Error(), "cloud property ha") {
		t.Fatalf("err = %v; want the error that names the ha cloud property", err)
	}
	r.wantNothingCreated(t)
}
