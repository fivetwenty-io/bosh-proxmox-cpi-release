package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// protectionWatchPVE is one request's cluster in the lifecycle flow fixture
// that records every protection write reaching the parker and ends the request
// as soon as a write that clears that protection lands.
type protectionWatchPVE struct {
	contendedFlowPVE
	parker int
	end    func()
	mu     *sync.Mutex
	writes *[]bool
}

func (c protectionWatchPVE) Nodes() nodes.Service {
	return protectionWatchNodes{Service: c.contendedFlowPVE.Nodes(), c: c}
}

type protectionWatchNodes struct {
	nodes.Service
	c protectionWatchPVE
}

func (n protectionWatchNodes) UpdateQemuConfig(ctx context.Context, node, vmidText string, p *nodes.UpdateQemuConfigParams) error {
	err := n.Service.UpdateQemuConfig(ctx, node, vmidText, p)
	vmid, _ := strconv.Atoi(vmidText)
	if err == nil && p.Protection != nil && vmid == n.c.parker {
		n.c.mu.Lock()
		*n.c.writes = append(*n.c.writes, *p.Protection)
		n.c.mu.Unlock()
		if !*p.Protection {
			n.c.end()
		}
	}
	return err
}

// TestManagedAttachEndedInsideTheWindowPutsProtectionBack ends the request
// right after the window clears the parker's protection and before the disk
// move is admitted. The guard refuses the move, and it still admits the restore
// that puts protection back on its own live context, so the parker ends
// protected, the restore's step is observed, and the error carries the move's
// refusal rather than a restore that was never sent.
func TestManagedAttachEndedInsideTheWindowPutsProtectionBack(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var writes []bool
	disk.deps.PVE = protectionWatchPVE{contendedFlowPVE: disk.deps.PVE.(contendedFlowPVE), parker: disk.parker, end: cancel, mu: &sync.Mutex{}, writes: &writes}

	err := disk.attach(ctx)
	if err == nil {
		t.Fatal("an attach whose request ended inside the window reported success")
	}
	if len(writes) == 0 || writes[0] {
		t.Fatalf("the window never cleared the parker's protection, so the test proves nothing: writes %v", writes)
	}
	if !writes[len(writes)-1] {
		t.Fatalf("the parker's protection was left off after the request ended inside the window: writes %v", writes)
	}
	record := disk.record(t)
	restored := false
	for _, step := range record.Steps {
		var parameters struct {
			Kind string `json:"kind"`
		}
		if step.Attempt != record.ActiveAttempt() || step.State != aj.Observed || json.Unmarshal(step.Parameters, &parameters) != nil {
			continue
		}
		if parameters.Kind == parkerProtectionOnKind {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("no observed %s step in the active attempt", parkerProtectionOnKind)
	}
	// The move's own refusal is a mutation that was not attempted, so only
	// what the error says about the restore counts here.
	var cutOff *pve.ProtectionRestoreCutOffError
	if errors.As(err, &cutOff) || strings.Contains(err.Error(), "was not attempted") {
		t.Fatalf("the error says the restore was not attempted: %v", err)
	}
}

// TestEndedRequestAdmitsOnlyAProtectionRestore pins what an ended request
// still sends. A write that only puts protection back, with or without a
// digest, is admitted while its own context is live. A write that clears
// protection, one that sets protection alongside any other field, and a
// restore whose own context has ended too are all refused as not attempted.
func TestEndedRequestAdmitsOnlyAProtectionRestore(t *testing.T) {
	t.Parallel()
	ended, end := context.WithCancel(t.Context())
	end()
	on, off := true, false
	digest, description, remove, tags := "abc", "x", "unused0", "bosh-parker"
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		params   *nodes.UpdateQemuConfigParams
		admitted bool
	}{
		{name: "protection on", ctx: t.Context(), params: &nodes.UpdateQemuConfigParams{Protection: &on}, admitted: true},
		{name: "protection on with a digest", ctx: t.Context(), params: &nodes.UpdateQemuConfigParams{Protection: &on, Digest: &digest}, admitted: true},
		{name: "protection on with the write's own context ended", ctx: ended, params: &nodes.UpdateQemuConfigParams{Protection: &on}},
		{name: "protection off", ctx: t.Context(), params: &nodes.UpdateQemuConfigParams{Protection: &off}},
		{name: "protection off with a digest", ctx: t.Context(), params: &nodes.UpdateQemuConfigParams{Protection: &off, Digest: &digest}},
		{name: "protection on with a description", ctx: t.Context(), params: &nodes.UpdateQemuConfigParams{Protection: &on, Description: &description}},
		{name: "protection on with a delete", ctx: t.Context(), params: &nodes.UpdateQemuConfigParams{Protection: &on, Delete: &remove}},
		{name: "protection on with tags and a digest", ctx: t.Context(), params: &nodes.UpdateQemuConfigParams{Protection: &on, Tags: &tags, Digest: &digest}},
		{name: "no protection field", ctx: t.Context(), params: &nodes.UpdateQemuConfigParams{Digest: &digest}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &managedDiskLifecycle{requestContext: ended}
			call := ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuConfig", Args: map[string]any{
				resourceTypeNode: "n1", metadataKeyVMID: "90000", managedArgumentParams: tc.params,
			}}
			err := m.requestEndedRefusal(tc.ctx, call)
			if tc.admitted {
				if err != nil {
					t.Fatalf("the restore was refused on an ended request: %v", err)
				}
				return
			}
			if !errors.Is(err, errManagedRequestEnded) {
				t.Fatalf("want the ended request's refusal, got %v", err)
			}
		})
	}
}
