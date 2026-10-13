package pve_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestListParkersForNode_WarnsAboutBandAndPrefixStrays lists a node holding
// our own parker, a parker of another prefix inside our band, another
// Director's parker inside our band, and a parker with our director tag and
// prefix outside it. The park-target answer is unchanged, each of the three
// warnings is logged once, and the only config read is the
// one the existing evidence rule already makes for an untagged in-band row.
func TestListParkersForNode_WarnsAboutBandAndPrefixStrays(t *testing.T) {
	t.Parallel()
	node := "pve1"
	director := "prod-director"
	ours := "bosh-cpi;bosh-parker;" + parkerPrefixTagFor("az1") + ";director--" + director
	configReads := 0
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			configReads++
			if vmid != 90003 {
				t.Errorf("unexpected config read for vmid %d", vmid)
			}
			return map[string]any{"tags": "bosh-cpi", "name": "operator-vm"}, nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(90001), "node": node, "type": "qemu", "name": "az1-parker-90001", "tags": ours},
			map[string]any{"vmid": int64(90002), "node": node, "type": "qemu", "name": "az2-parker-90002",
				"tags": "bosh-cpi;bosh-parker;" + parkerPrefixTagFor("az2")},
			// An untagged in-band row: the evidence rule reads its config,
			// as it did before the warnings existed.
			map[string]any{"vmid": int64(90003), "node": node, "type": "qemu"},
			map[string]any{"vmid": int64(91500), "node": node, "type": "qemu", "name": "az1-parker-91500", "tags": ours},
			// Another Director's parker in the shared default band.
			map[string]any{"vmid": int64(90004), "node": node, "type": "qemu", "name": "bosh-parker-90004",
				"tags": "bosh-cpi;bosh-parker;" + parkerPrefixTagFor("bosh") + ";director--other-director"},
			// Out-of-band rows with no tags are never read for evidence.
			map[string]any{"vmid": int64(101), "node": node, "type": "qemu"},
		), nil
	})
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	ctx := log.IntoContext(context.Background(), logger)
	cfg := parkerTestCfgWithPrefix("az1")
	cfg.DirectorID = director

	parkers, err := pve.ListParkersForNode(ctx, c, node, cfg)
	if err != nil {
		t.Fatalf("ListParkersForNode: %v", err)
	}
	if len(parkers) != 1 || parkers[0] != 90001 {
		t.Fatalf("parkers = %v, want [90001]", parkers)
	}
	if configReads != 1 {
		t.Fatalf("config reads = %d, want only the untagged in-band row's", configReads)
	}
	var outside, other, foreign []log.Entry
	for _, e := range obs.All() {
		switch {
		case strings.Contains(e.Message, "outside the configured parker band"):
			outside = append(outside, e)
		case strings.Contains(e.Message, "inside this entry's parker band"):
			other = append(other, e)
		case strings.Contains(e.Message, "share this entry's parker band"):
			foreign = append(foreign, e)
		default:
			t.Errorf("unexpected warning %q", e.Message)
		}
	}
	if len(outside) != 1 || outside[0].Attrs["parkers"] != "pve1/91500" || outside[0].Attrs["listing"] != "list_parkers_for_node" {
		t.Fatalf("outside-band warnings = %+v", outside)
	}
	if len(other) != 1 || other[0].Attrs["parkers"] != "pve1/90002(az2)" {
		t.Fatalf("other-prefix warnings = %+v", other)
	}
	if len(foreign) != 1 || foreign[0].Attrs["parkers"] != "pve1/90004(bosh)" {
		t.Fatalf("other-director warnings = %+v", foreign)
	}
}

// TestListParkersForNode_OwnBandIsQuiet is the steady state: our own parkers
// and unrelated guests produce no warning.
func TestListParkersForNode_OwnBandIsQuiet(t *testing.T) {
	t.Parallel()
	node := "pve1"
	c := buildParkerClient(&parkerQEMU{}, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(90001), "node": node, "type": "qemu", "name": "az1-parker-90001",
				"tags": "bosh-cpi;bosh-parker;" + parkerPrefixTagFor("az1")},
			map[string]any{"vmid": int64(90002), "node": node, "type": "qemu", "name": "az1-mover-90002",
				"tags": "bosh-cpi;bosh-parker;bosh-disk-mover;" + parkerPrefixTagFor("az1")},
			map[string]any{"vmid": int64(101), "node": node, "type": "qemu", "name": "vm-1", "tags": "bosh-cpi"},
		), nil
	})
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	ctx := log.IntoContext(context.Background(), logger)
	if _, err := pve.ListParkersForNode(ctx, c, node, parkerTestCfgWithPrefix("az1")); err != nil {
		t.Fatalf("ListParkersForNode: %v", err)
	}
	if obs.Len() != 0 {
		t.Fatalf("clean node warned: %+v", obs.All())
	}
}
