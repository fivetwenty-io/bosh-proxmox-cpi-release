package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haScanCluster is the detach rig's cluster with HA resource and rule lists
// that answer from fixed SIDs or fail with a fixed error.
type haScanCluster struct {
	*mockClusterSvc
	resourceSIDs []string
	resourcesErr error
	rulesErr     error
}

func (c *haScanCluster) ListHaResources(
	context.Context, *cluster.ListHaResourcesParams,
) (*cluster.ListHaResourcesResponse, error) {
	if c.resourcesErr != nil {
		return nil, c.resourcesErr
	}
	out := cluster.ListHaResourcesResponse{}
	for _, sid := range c.resourceSIDs {
		out = append(out, []byte(fmt.Sprintf(`{"sid":%q,"state":"stopped"}`, sid)))
	}
	return &out, nil
}

func (c *haScanCluster) ListHaRules(context.Context, *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	if c.rulesErr != nil {
		return nil, c.rulesErr
	}
	out := cluster.ListHaRulesResponse{}
	return &out, nil
}

// pveStatus builds the error the SDK returns for an HTTP status with body.
func pveStatus(code int, body string) error {
	return sdkerrors.ParseAPIError(code, []byte(body))
}

// detachIntoNewParker runs a parked detach under pve.ha.mode mode on a node
// with no parker, so the park creates one in the band [90000, bandEnd], and
// returns the VMID it created at and the handler's error.
func detachIntoNewParker(t *testing.T, mode string, bandEnd int, hac *haScanCluster) (int, error) {
	t.Helper()
	const parkedVolid = "local-lvm:vm-9001-disk-0"
	created := 0
	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: 90000,
		sourceCfg:       map[string]any{diskSlot: parkedVolid},
		parkerCfg:       map[string]any{"tags": "bosh-parker"},
		attachDiskFn: func(context.Context, string, int, string, string, *qemu.AttachOpts) (string, error) {
			return "", nil
		},
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			created, _ = params["vmid"].(int)
			return "", nil
		},
	}
	hac.mockClusterSvc = parkerEmptyClusterSvc()
	deps := detachDepsParked(qemuSvc, hac)
	deps.Config.ParkedDiskVMIDRangeEnd = bandEnd
	choice := config.HAChoice(mode)
	deps.Config.HA = &config.HAConfig{Mode: &choice}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	_, err := handlers.HandleDetachDisk(deps).Handle(ctx, detachArgs(t, "100", parkedVolid), jsonrpc.Context{})
	return created, err
}

// retriable reports whether err tells the Director to retry.
func retriable(err error) bool {
	var retry interface{ OkToRetry() bool }
	return errors.As(err, &retry) && retry.OkToRetry()
}

// TestDetachDisk_ParkerAllocationWithUnreadableHAState proves the HA state
// scan of a new parker's VMID allocation keeps every promise the docs make
// when PVE does not answer it. A refusal for a missing grant parks the disk
// in auto and off mode, as a park did before the scan existed. A rules
// endpoint that PVE does not implement, as on PVE 8 or on an entry node that
// a cluster upgrade has not reached, leaves the resources scanned and the park
// going. A server error that outlasts the retries fails the park retriably in
// auto mode and parks the disk in off mode, where the scan is best effort.
func TestDetachDisk_ParkerAllocationWithUnreadableHAState(t *testing.T) {
	forbidden := pveStatus(403, "Permission check failed (/, Sys.Audit)")
	notImplemented := pveStatus(501, "Method 'GET /cluster/ha/rules' not implemented")
	unavailable := pveStatus(503, "service unavailable")

	cases := []struct {
		name      string
		mode      string
		hac       *haScanCluster
		wantErr   bool
		wantRetry bool
	}{
		{name: "auto mode without Sys.Audit", mode: config.HAModeAuto,
			hac: &haScanCluster{resourcesErr: forbidden, rulesErr: forbidden}},
		{name: "off mode without Sys.Audit", mode: config.HAModeOff,
			hac: &haScanCluster{resourcesErr: forbidden, rulesErr: forbidden}},
		{name: "auto mode on a cluster without the rules endpoint", mode: config.HAModeAuto,
			hac: &haScanCluster{rulesErr: notImplemented}},
		{name: "off mode on a cluster without the rules endpoint", mode: config.HAModeOff,
			hac: &haScanCluster{rulesErr: notImplemented}},
		{name: "auto mode when the HA reads keep failing", mode: config.HAModeAuto,
			hac: &haScanCluster{resourcesErr: unavailable, rulesErr: unavailable}, wantErr: true, wantRetry: true},
		{name: "off mode when the HA reads keep failing", mode: config.HAModeOff,
			hac: &haScanCluster{resourcesErr: unavailable, rulesErr: unavailable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, err := detachIntoNewParker(t, tc.mode, 90999, tc.hac)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("detach parked the disk in parker %d, want the failed HA read's error", created)
				}
				if retriable(err) != tc.wantRetry {
					t.Fatalf("retriable = %v, want %v: %v", retriable(err), tc.wantRetry, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("detach failed on an HA read the park must outlive: %v", err)
			}
			if created == 0 {
				t.Fatal("the detach created no parker")
			}
		})
	}
}

// TestDetachDisk_NewParkerSkipsALeftoverHAResource proves a new parker never
// lands on a VMID a leftover HA resource still names, in off mode as in auto
// mode, and on a cluster whose rules endpoint PVE does not implement. The band
// holds 100 VMIDs and the resources name all but the last.
func TestDetachDisk_NewParkerSkipsALeftoverHAResource(t *testing.T) {
	const bandEnd = 90099
	var sids []string
	for vmid := 90000; vmid < bandEnd; vmid++ {
		sids = append(sids, fmt.Sprintf("vm:%d", vmid))
	}
	for _, mode := range []string{config.HAModeOff, config.HAModeAuto} {
		for _, rulesErr := range []error{nil, pveStatus(501, "Method 'GET /cluster/ha/rules' not implemented")} {
			name := mode + " mode"
			if rulesErr != nil {
				name += " without the rules endpoint"
			}
			t.Run(name, func(t *testing.T) {
				created, err := detachIntoNewParker(t, mode, bandEnd, &haScanCluster{resourceSIDs: sids, rulesErr: rulesErr})
				if err != nil {
					t.Fatalf("detach: %v", err)
				}
				if created != bandEnd {
					t.Fatalf("the new parker is at %d, want %d, the one VMID no HA resource names", created, bandEnd)
				}
			})
		}
	}
}
