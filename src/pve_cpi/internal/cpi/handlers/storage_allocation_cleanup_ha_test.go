package handlers

import (
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"reflect"
	"testing"
)

type unknownHACleanupClient struct {
	*deleteManagedClient
	ha *infrastructureHACluster
}

func (c *unknownHACleanupClient) Cluster() cluster.Service { return c.ha }
func TestCleanupUnknownHAWritePreservesOtherMembers(t *testing.T) {
	for _, kind := range []string{"vm.Cluster.CreateHaResources", "vm.Cluster.UpdateHaResources", "vm.Cluster.CreateHaRules"} {
		t.Run(kind, func(t *testing.T) {
			deps, j, base, record := deleteManagedFixture(t)
			h, err := j.Acquire(t.Context(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			pending := h.Record()
			pending.State = aj.ReconciliationRequired
			pending.Reason = "HA response lost"
			if err = h.Save(pending); err != nil {
				t.Fatal(err)
			}
			var parameters []byte
			if kind == "vm.Cluster.CreateHaRules" {
				parameters = []byte(`{"version":1,"kind":"ha_rule","rule":"shared","type":"resource-affinity","resources":"vm:123,vm:124","affinity":"negative"}`)
			}
			step, err := storageMutationIntent(h, kind, aj.Target{Node: "pve1", VMID: 123}, nil, parameters)
			if err != nil {
				t.Fatal(err)
			}
			before := h.Record()
			if err = h.Close(); err != nil {
				t.Fatal(err)
			}
			ha := &infrastructureHACluster{Service: base.Cluster(), rows: []map[string]any{{"rule": "shared", "type": "resource-affinity", "resources": "vm:123,vm:124", "affinity": "negative"}}}
			deps.PVE = &cleanupTaskClient{Client: &unknownHACleanupClient{deleteManagedClient: base, ha: ha}}
			result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, cleanupAttestedDecision(record.ID))
			if err != nil {
				t.Fatalf("%v: %v", err, unwrapCleanupTest(err))
			}
			if result.State != aj.Cleaned || base.destroyCount != 1 || ha.deletes != 1 || len(ha.rows) != 1 || ha.rows[0]["resources"] != "vm:124" {
				t.Fatal("cleanup did not preserve other HA members")
			}
			for i, old := range before.Steps {
				if old.ID == step && !reflect.DeepEqual(result.Steps[i], old) {
					t.Fatal("unknown HA history rewritten")
				}
			}
		})
	}
}

func TestCleanupUnknownHAPurgeRequiresObservedEffectWithoutReplay(t *testing.T) {
	for _, remains := range []bool{false, true} {
		t.Run(fmt.Sprint(remains), func(t *testing.T) {
			checkCleanupUnknownHAPurgeRequiresObservedEffectWithoutReplay(t, remains)
		})
	}
}

func checkCleanupUnknownHAPurgeRequiresObservedEffectWithoutReplay(t *testing.T, remains bool) {
	t.Helper()
	deps, j, base, record := deleteManagedFixture(t)
	h, err := j.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	current := h.Record()
	current.State = aj.ReconciliationRequired
	current.Reason = "HA response lost"
	if err = h.Save(current); err != nil {
		t.Fatal(err)
	}
	if _, err = storageMutationIntent(h, "vm.Cluster.CreateHaResources", aj.Target{Node: "pve1", VMID: 123}, nil); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	ha := &infrastructureHACluster{Service: base.Cluster(), unknown: true, rows: []map[string]any{{"rule": "shared", "type": "resource-affinity", "resources": "vm:123,vm:124", "affinity": "negative"}}}
	if !remains {
		ha.beforeDelete = func() {
			ha.removed = true
			ha.rows = []map[string]any{{"rule": "shared", "type": "resource-affinity", "resources": "vm:124", "affinity": "negative"}}
		}
	}
	deps.PVE = &cleanupTaskClient{Client: &unknownHACleanupClient{deleteManagedClient: base, ha: ha}}
	decision := cleanupAttestedDecision(record.ID)
	if _, err = CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
		t.Fatal("unknown purge hidden")
	}
	prior, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision.DecisionID += "-second"
	result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
	if remains {
		if err == nil {
			t.Fatal("remaining HA resource admitted")
		}
	} else if err != nil || result.State != aj.Cleaned {
		t.Fatalf("observed purge not reconciled: %v (%v)", err, unwrapCleanupTest(err))
	}
	if ha.deletes != 1 {
		t.Fatal("unknown purge replayed")
	}
	after, e := j.Inspect(record.ID)
	if e != nil {
		t.Fatal(e)
	}
	for i := range prior.Steps {
		if !reflect.DeepEqual(prior.Steps[i], after.Steps[i]) {
			t.Fatal("unknown history changed")
		}
	}
}
