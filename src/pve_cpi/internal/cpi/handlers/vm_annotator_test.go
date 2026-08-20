package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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
	if annErr := a.AnnotateNotes(context.Background(), vmid, "created by bosh-pve-cpi"); annErr != nil {
		t.Fatalf("AnnotateNotes: unexpected error: %v", annErr)
	}

	if !strings.Contains(written, "created by bosh-pve-cpi") {
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
	if annErr := a.AnnotateNotes(context.Background(), vmid, "plain notes"); annErr != nil {
		t.Fatalf("AnnotateNotes: unexpected error: %v", annErr)
	}
	if written != "plain notes" {
		t.Errorf("expected plain notes on read failure, got %q", written)
	}
}
