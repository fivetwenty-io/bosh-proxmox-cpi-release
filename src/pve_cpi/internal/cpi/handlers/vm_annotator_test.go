package handlers_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// Tests for VMAnnotator.AnnotateNotes' sentinel preservation: the notes_audit
// hook fires after create_vm has written the bosh_pool provenance sentinel,
// so the annotation write must merge-preserve the <!--BOSH:{...}--> block
// instead of plain-setting the description over it.

func TestVMAnnotator_PreservesExistingSentinel(t *testing.T) {
	t.Parallel()

	const vmid = 101
	seed, err := pve.SetPoolMembershipOnDescription("", &pve.PoolMembership{
		Name: "bosh-d1-dep1", Layer: pve.PoolLayerTemplate, Director: "d1", Deployment: "dep1",
	})
	if err != nil {
		t.Fatalf("SetPoolMembershipOnDescription: %v", err)
	}

	var written string
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": seed}, nil
		},
	}
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params != nil && params.Description != nil {
				written = *params.Description
			}
			return nil
		},
	}
	deps := testDepsFoundVM(vmid, qemuSvc, nodesSvc, nil, &mockAgentService{})

	a := handlers.NewVMAnnotator(deps)
	if annErr := a.AnnotateNotes(context.Background(), strconv.Itoa(vmid), "created by bosh-proxmox-cpi"); annErr != nil {
		t.Fatalf("AnnotateNotes: unexpected error: %v", annErr)
	}

	if !strings.Contains(written, "created by bosh-proxmox-cpi") {
		t.Errorf("notes text missing from written description: %q", written)
	}
	pm, ok := pve.GetPoolMembership(written)
	if !ok || pm.Name != "bosh-d1-dep1" {
		t.Errorf("bosh_pool sentinel lost by annotation write: got %+v ok=%v (desc %q)", pm, ok, written)
	}
}

func TestVMAnnotator_ConfigReadFailure_FallsBackToPlainNotes(t *testing.T) {
	t.Parallel()

	const vmid = 101
	var written string
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return nil, errors.New("simulated config read failure")
		},
	}
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params != nil && params.Description != nil {
				written = *params.Description
			}
			return nil
		},
	}
	deps := testDepsFoundVM(vmid, qemuSvc, nodesSvc, nil, &mockAgentService{})

	a := handlers.NewVMAnnotator(deps)
	if annErr := a.AnnotateNotes(context.Background(), strconv.Itoa(vmid), "plain notes"); annErr != nil {
		t.Fatalf("AnnotateNotes: unexpected error: %v", annErr)
	}
	if written != "plain notes" {
		t.Errorf("expected plain notes on read failure, got %q", written)
	}
}

// TestVMAnnotator_StampedCIDWritesOnlyItsOwnVM pins that a stamped CID's Notes
// go only to the VM whose tags carry its token, and that the write is skipped
// with an error, never sent, when that cannot be shown.
func TestVMAnnotator_StampedCIDWritesOnlyItsOwnVM(t *testing.T) {
	t.Parallel()
	const cid = "101-0a1b2c3d-4e5f6a7b"
	cases := []struct {
		name      string
		cfg       map[string]any
		cfgErr    error
		wantWrite bool
		wantErr   string
	}{
		{name: "tags agree", cfg: map[string]any{"tags": "bosh-cpi;bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b"}, wantWrite: true},
		{name: "token tag only", cfg: map[string]any{"tags": "bosh-cpi;bosh-cid--4e5f6a7b"}, wantWrite: true},
		{name: "another token", cfg: map[string]any{"tags": "bosh-cpi;bosh-cluster--0a1b2c3d;bosh-cid--deadbeef"},
			wantErr: "carries token deadbeef"},
		{name: "no token", cfg: map[string]any{"tags": "bosh-cpi;bosh-cluster--0a1b2c3d"}, wantErr: "carries token none"},
		{name: "two tokens", cfg: map[string]any{"tags": "bosh-cid--4e5f6a7b;bosh-cid--deadbeef"}, wantErr: "carries token"},
		{name: "another cluster", cfg: map[string]any{"tags": "bosh-cluster--99aabbcc;bosh-cid--4e5f6a7b"},
			wantErr: "records cluster 99aabbcc"},
		{name: "config read fails", cfgErr: errors.New("simulated config read failure"), wantErr: "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wrote := false
			qemuSvc := &mockQEMUService{
				configFn: func(context.Context, string, int) (map[string]any, error) { return tc.cfg, tc.cfgErr },
			}
			nodesSvc := &mockNodesService{
				updateQemuConfigFn: func(context.Context, string, string, *sdknodes.UpdateQemuConfigParams) error {
					wrote = true
					return nil
				},
			}
			a := handlers.NewVMAnnotator(testDepsFoundVM(101, qemuSvc, nodesSvc, nil, &mockAgentService{}))
			err := a.AnnotateNotes(context.Background(), cid, "notes")
			if wrote != tc.wantWrite {
				t.Errorf("wrote = %v, want %v", wrote, tc.wantWrite)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("AnnotateNotes: unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("AnnotateNotes error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}

	if err := handlers.NewVMAnnotator(testDepsFoundVM(101, nil, nil, nil, &mockAgentService{})).
		AnnotateNotes(context.Background(), "101-0A1B2C3D-4e5f6a7b", "notes"); err == nil {
		t.Error("AnnotateNotes accepted a malformed CID")
	}
}
