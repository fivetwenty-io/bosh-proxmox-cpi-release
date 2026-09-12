package handlers

import (
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestManagedConfigDescriptionClearReadback(t *testing.T) {
	empty := ""
	params := &sdknodes.UpdateQemuConfigParams{Description: &empty}
	for _, tc := range []struct {
		name   string
		config map[string]any
		reject bool
	}{
		{name: "PVE omits cleared description", config: map[string]any{"protection": 1}},
		{name: "explicit empty description", config: map[string]any{"description": ""}},
		{name: "old provenance remains", config: map[string]any{"description": "parked disk provenance"}, reject: true},
		{name: "null is not absence", config: map[string]any{"description": nil}, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := managedConfigFieldsMatch(tc.config, params); (err != nil) != tc.reject {
				t.Fatalf("unexpected description clear verification: %v", err)
			}
		})
	}
}

func TestManagedConfigDescriptionOmissionDoesNotHideRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{
		{name: "nonempty provenance", fields: map[string]any{"description": "owned allocation"}},
		{name: "null request", fields: map[string]any{"description": nil}},
		{name: "disk identity", fields: map[string]any{"description": "", "scsi0": "nfs-persistent-1:20768/owned.qcow2"}},
		{name: "protection", fields: map[string]any{"description": "", "protection": 1}},
		{name: "other empty property", fields: map[string]any{"name": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := managedConfigFieldsMatch(map[string]any{}, tc.fields); err == nil {
				t.Fatal("missing required config field was accepted")
			}
		})
	}
}

func TestManagedDiskLifecycleObservesLastProvenanceClear(t *testing.T) {
	const volume = "nfs-persistent-1:20768/owned.qcow2"
	guard := managedDiskLifecycleGuard{lifecycle: &managedDiskLifecycle{disk: resolvedDisk{volid: volume}}}
	volumes, err := guard.observeConfigFields(managedDiskMutationObservation{
		fields: map[string]any{"description": ""},
	}, map[string]any{"protection": 1})
	if err != nil {
		t.Fatalf("successful PVE description clear was not observed: %v", err)
	}
	if len(volumes) != 1 || volumes[0] != volume {
		t.Fatalf("clear lost allocation volume evidence: %v", volumes)
	}
}
