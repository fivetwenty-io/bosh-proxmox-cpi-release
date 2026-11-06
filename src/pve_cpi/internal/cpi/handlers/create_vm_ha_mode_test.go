package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// TestCreateVM_DLBSentinelAZ_ModeOff_FailsBeforeAnyPVECall verifies that naming
// the sentinel AZ under pve.ha.mode off, whether the mode is unset or explicit,
// fails with a non-retriable error that names the fix, and that nothing is
// created or even read on the cluster.
func TestCreateVM_DLBSentinelAZ_ModeOff_FailsBeforeAnyPVECall(t *testing.T) {
	t.Parallel()

	blocks := map[string]*config.HAConfig{
		"no ha block":     nil,
		"explicit off":    {Mode: haChoiceOf("off")},
		"off, dlb keys":   haModeOff(),
		"custom sentinel": nil,
	}
	for name, ha := range blocks {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			created := false
			q := &vmMockQEMU{
				createFn: func(context.Context, string, map[string]any) (string, error) {
					created = true
					return "UPID:pve:never:ok", nil
				},
			}
			panicStatus := func(context.Context) (*sdkcluster.ListStatusResponse, error) {
				panic("ListStatus must not be called when the sentinel check fails")
			}
			az := "dlb"
			deps := buildVMDepsPlacement(q, &vmMockNodes{}, panicStatus, emptyListResources, &vmMockAgent{},
				func(c *config.CPIConfig) {
					c.Placement = &config.PlacementConfig{AZMap: map[string][]string{"zone-a": {"pve"}}}
					c.HA = ha
					if name == "custom sentinel" {
						az = "balance"
						c.HA = &config.HAConfig{DLB: &config.HADLBConfig{AZName: stringPtrHelper("balance")}}
					}
					c.EnsureNoIPConflicts = placementDisabled
				})
			args := mkArgs("agent-sentinel-off", testStemcellCID,
				map[string]any{"availability_zone": az},
				map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
				[]string{}, map[string]any{})

			_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("sentinel-off"))
			if err == nil {
				t.Fatal("expected an error for the sentinel AZ under mode off, got nil")
			}
			var ce *cpierrors.Error
			if !errors.As(err, &ce) || ce.OkToRetry() {
				t.Errorf("want a non-retriable cloud error, got %T (%v)", err, err)
			}
			for _, want := range []string{az, "pve.ha.mode", "pve.ha.dlb.az_name"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must mention %q; got: %v", want, err)
				}
			}
			if created {
				t.Error("no VM may be created when the sentinel check fails")
			}
		})
	}
}

// TestCreateVM_NonSentinelAZ_ModeOff_StillCreates verifies mode off only
// refuses the sentinel: any other AZ, and a disabled sentinel, create normally.
func TestCreateVM_NonSentinelAZ_ModeOff_StillCreates(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		az string
		ha *config.HAConfig
	}{
		"real AZ, off":           {"zone-a", nil},
		"sentinel disabled, off": {"zone-a", &config.HAConfig{DLB: &config.HADLBConfig{AZName: stringPtrHelper("")}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			created := false
			q := &vmMockQEMU{
				createFn: func(context.Context, string, map[string]any) (string, error) {
					created = true
					return "UPID:pve:ok:ok", nil
				},
			}
			deps := buildVMDepsPlacement(q, &vmMockNodes{}, listStatusSingleNode(), emptyListResources, &vmMockAgent{},
				func(c *config.CPIConfig) {
					c.Node = "pve"
					c.Placement = &config.PlacementConfig{AZMap: map[string][]string{"zone-a": {"pve"}}}
					c.HA = tc.ha
					c.EnsureNoIPConflicts = placementDisabled
				})
			args := mkArgs("agent-nonsentinel-off", testStemcellCID,
				map[string]any{"availability_zone": tc.az},
				map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
				[]string{}, map[string]any{})
			if _, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("nonsentinel-off")); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !created {
				t.Error("expected the VM to be created")
			}
		})
	}
}

// TestWithRequestOverrides_RemovedKey_IsNonRetriable verifies a cpi-config
// entry that still carries a removed placement key fails the request with the
// replacement named, and that the Director is told not to retry it.
func TestWithRequestOverrides_RemovedKey_IsNonRetriable(t *testing.T) {
	t.Parallel()

	base := overrideTestBaseConfig()
	base.ApplyDefaults()
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}
	_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		RequestID: "r-removed",
		Extra: map[string]any{
			"pve": map[string]any{
				"host":      "az2-cluster.example",
				"api_token": overrideTestEntryToken,
				"placement": map[string]any{"exclude_maintenance_nodes": false, "dlb": map[string]any{"enabled": true}},
			},
		},
	})
	if err == nil {
		t.Fatal("expected the removed keys to be rejected")
	}
	var ce *cpierrors.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a *cpierrors.Error, got %T: %v", err, err)
	}
	if ce.Type() != cpierrors.TypeCloud || ce.OkToRetry() {
		t.Errorf("want a non-retriable cloud error, got type %s retry %t", ce.Type(), ce.OkToRetry())
	}
	for _, want := range []string{
		"pve.placement.exclude_maintenance_nodes", "the node health check always runs",
		"pve.placement.dlb.enabled", "use pve.ha.dlb.enabled with pve.ha.mode: auto",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q; got: %v", want, err)
		}
	}
}

func stringPtrHelper(s string) *string { return &s }
