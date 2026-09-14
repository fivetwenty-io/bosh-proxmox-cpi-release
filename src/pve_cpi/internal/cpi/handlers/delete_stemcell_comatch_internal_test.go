// Package handlers internal tests for delete_stemcell's co-match sweep gate.
package handlers

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// coMatchDeps builds Deps whose QEMU.Config answers the co-match's own
// provenance read with cfg/cfgErr.
func coMatchDeps(cfg map[string]any, cfgErr error) Deps {
	return Deps{
		Config: &config.CPIConfig{Node: "pve1", VMStorage: "local"},
		PVE: &templateGapPVE{
			nodes:   &templateGapNodesSvc{},
			cluster: &templateGapClusterSvc{},
			qemu: &etQEMU{
				configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
					return cfg, cfgErr
				},
			},
		},
		Logger: log.NewNopLogger(),
	}
}

// coMatchProvDesc renders a provenance description whose DirectorRefs are
// exactly refs.
func coMatchProvDesc(refs ...string) map[string]any {
	prov := stemcellProvenance{Name: "ubuntu-jammy", Version: "1.0", SHA8: "abcd1234", DirectorRefs: refs}
	b, _ := json.Marshal(prov)
	return map[string]any{pveConfigKeyDescription: string(b)}
}

// TestCoMatchSafeToSweep pins the gate that decides whether a co-matching
// template may be destroyed in the anchor's last-ref sweep. The empty-UUID
// rows are the create-env case: every ref writer substitutes the
// unknown-director sentinel for an empty director UUID, and the gate must
// resolve its own side the same way or this call's replicas read as foreign
// and leak permanently.
// coMatchProvDescSHA renders a provenance description for sha8 whose
// DirectorRefs are exactly refs.
func coMatchProvDescSHA(sha8 string, refs ...string) map[string]any {
	prov := stemcellProvenance{Name: "ubuntu-jammy", Version: "1.0", SHA8: sha8, DirectorRefs: refs}
	b, _ := json.Marshal(prov)
	return map[string]any{pveConfigKeyDescription: string(b)}
}

func TestCoMatchSafeToSweep(t *testing.T) {
	t.Parallel()
	notFound := sdkerrors.ParseAPIError(404, []byte(`{"message":"Configuration file 'nodes/pve2/qemu-server/8102.conf' does not exist"}`))

	cases := []struct {
		name   string
		cfg    map[string]any
		cfgErr error
		uuid   string
		tags   string
		want   bool
	}{
		{"empty refs allow", coMatchProvDesc(), nil, "dir-a", "", true},
		{"sole own ref allows", coMatchProvDesc("dir-a"), nil, "dir-a", "", true},
		{"foreign ref preserves", coMatchProvDesc("dir-b"), nil, "dir-a", "", false},
		{"own plus foreign preserves", coMatchProvDesc("dir-a", "dir-b"), nil, "dir-a", "", false},
		{"empty uuid matches sentinel refs", coMatchProvDesc("unknown-director"), nil, "", "", true},
		{"empty uuid against real ref preserves", coMatchProvDesc("dir-a"), nil, "", "", false},
		{"already gone reports false", nil, notFound, "dir-a", "", false},
		{"unreadable config preserves", nil, stderrors.New("pmxcfs timeout"), "dir-a", "", false},
		{"unparseable description preserves", map[string]any{pveConfigKeyDescription: "not json"}, nil, "dir-a", "", false},
		{"storage replica with foreign ref is swept on last-ref", coMatchProvDesc("dir-b"), nil, "dir-a", "bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2", true},
		{"node replica with foreign ref is swept on last-ref", coMatchProvDesc("dir-b"), nil, "dir-a", "bosh-stemcell-sha-abcd1234;bosh-stemcell-node-pve2", true},
		{"replica for a different sha8 is preserved", coMatchProvDescSHA("ffff0000", "dir-b"), nil, "dir-a", "bosh-stemcell-sha-ffff0000;bosh-stemcell-storage-ns-2", false},
		{"replica already gone reports false", nil, notFound, "dir-a", "bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2", false},
		{"non-replica with foreign ref is preserved", coMatchProvDesc("dir-b"), nil, "dir-a", "bosh-stemcell-sha-abcd1234", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref := pve.TemplateRef{VMID: 8102, Node: "pve2", Tags: tc.tags}
			got := coMatchSafeToSweep(context.Background(), coMatchDeps(tc.cfg, tc.cfgErr), ref, tc.uuid, "abcd1234", log.NewNopLogger())
			if got != tc.want {
				t.Errorf("coMatchSafeToSweep(uuid=%q tags=%q) = %v; want %v", tc.uuid, tc.tags, got, tc.want)
			}
		})
	}
}
