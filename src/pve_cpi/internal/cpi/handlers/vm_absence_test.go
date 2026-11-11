package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// absenceStatus is a quorate /cluster/status answer listing each member
// online or offline as given.
func absenceStatus(members map[string]bool) *cluster.ListStatusResponse {
	resp := make(cluster.ListStatusResponse, 0, 1+len(members))
	resp = append(resp, json.RawMessage(`{"type":"cluster","name":"fixture","quorate":1}`))
	for name, online := range members {
		flag := 0
		if online {
			flag = 1
		}
		row, _ := json.Marshal(map[string]any{"type": "node", "name": name, "online": flag})
		resp = append(resp, row)
	}
	return &resp
}

// twoMemberDeps is testDepsFoundVM on a two-member cluster whose index still
// places vmid on pve-node1.
func twoMemberDeps(
	vmid int, qemuSvc *mockQEMUService, nodesSvc *mockNodesService, tasksSvc *mockTasksService, fastPath bool,
) handlers.Deps {
	deps := testDepsFoundVMWithStorage(vmid, qemuSvc, nodesSvc, tasksSvc, &mockAgentService{}, &mockStorageService{})
	clusterSvc := defaultClusterSvc(vmid, "pve-node1")
	clusterSvc.listConfigNodesFn = func(context.Context) (*cluster.ListConfigNodesResponse, error) {
		return handlers.AbsenceConfigNodes("pve-node1", "pve-node2"), nil
	}
	deps.PVE.(*mockPVEClient).clusterSvc = clusterSvc
	if fastPath {
		enabled := true
		deps.Config.FastPathDelete = &enabled
	}
	return deps
}

// movingVM holds a VM's config on pve-node1 until the call under test moves
// it, and on pve-node2 after that, the way a migration lands between two
// calls of one delete.
type movingVM struct{ moved atomic.Bool }

func (v *movingVM) move() { v.moved.Store(true) }

func (v *movingVM) config(_ context.Context, node string, _ int) (map[string]any, error) {
	onNode := "pve-node1"
	if v.moved.Load() {
		onNode = "pve-node2"
	}
	if node != onNode {
		return nil, notFoundAPIErr()
	}
	return map[string]any{}, nil
}

// recordingDestroy answers DeleteQemu with resp and err and counts the calls.
type recordingDestroy struct {
	mu    sync.Mutex
	nodes []string
}

func (r *recordingDestroy) fn(
	resp *nodes.DeleteQemuResponse, err error, before func(),
) func(context.Context, string, string, *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	return func(_ context.Context, node, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
		r.mu.Lock()
		r.nodes = append(r.nodes, node)
		r.mu.Unlock()
		if before != nil {
			before()
		}
		return resp, err
	}
}

func (r *recordingDestroy) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.nodes...)
}

func requireAbsenceRetriable(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("want a retriable error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable error, got %v", err)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestDeleteVM_NotFoundWithConfigStillPresentIsRetriable pins every delete
// step that can answer not found while the VM's config still exists. The step
// must not finish as success, and no later destroy may run on its answer.
func TestDeleteVM_NotFoundWithConfigStillPresentIsRetriable(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmdestroy:610:root@pam:"
	present := func(context.Context, string, int) (map[string]any, error) { return map[string]any{}, nil }

	cases := []struct {
		name        string
		fastPath    bool
		stopErr     error
		destroyErr  error
		awaitErr    error
		wantDestroy int
	}{
		{name: "stop", stopErr: notFoundAPIErr(), wantDestroy: 0},
		{name: "destroy", destroyErr: notFoundAPIErr(), wantDestroy: 1},
		{name: "destroy await", awaitErr: notFoundAPIErr(), wantDestroy: 1},
		{name: "fast-path stop", fastPath: true, stopErr: notFoundAPIErr(), wantDestroy: 0},
		{name: "fast-path destroy", fastPath: true, destroyErr: notFoundAPIErr(), wantDestroy: 1},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vmid := 610 + i
			qemuSvc := &mockQEMUService{
				stopFn:   func(context.Context, string, int) (string, error) { return "", tc.stopErr },
				configFn: present,
			}
			resp := nodes.DeleteQemuResponse(`"` + upid + `"`)
			destroy := &recordingDestroy{}
			var destroyResp *nodes.DeleteQemuResponse
			if tc.destroyErr == nil {
				destroyResp = &resp
			}
			nodesSvc := &mockNodesService{
				deleteQemuFn: destroy.fn(destroyResp, tc.destroyErr, nil),
				updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
					return nil
				},
			}
			tasksSvc := &mockTasksService{
				waitFn: func(context.Context, string, string, *tasks.WaitOptions) (*tasks.Status, error) {
					if tc.awaitErr != nil {
						return nil, tc.awaitErr
					}
					return &tasks.Status{ExitStatus: "OK"}, nil
				},
			}
			deps := testDepsFoundVMWithStorage(vmid, qemuSvc, nodesSvc, tasksSvc, &mockAgentService{}, &mockStorageService{})
			if tc.fastPath {
				enabled := true
				deps.Config.FastPathDelete = &enabled
			}

			_, err := handlers.HandleDeleteVM(
				deps,
			).Handle(quickTaskPollCtx(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})

			requireAbsenceRetriable(t, err, "holds its config now")
			if got := len(destroy.calls()); got != tc.wantDestroy {
				t.Errorf("destroy calls = %d, want %d", got, tc.wantDestroy)
			}
		})
	}
}

// TestDeleteVM_MigrationDuringDeleteIsRetriable moves the VM to pve-node2 in
// the middle of each delete step, so the step's not-found answer comes from
// the member the stale index still names. The delete must answer retriable,
// name the member that holds the VM now, and never destroy on pve-node1.
func TestDeleteVM_MigrationDuringDeleteIsRetriable(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve-node1:00AABBCC:00112233:6789ABCD:qmdestroy:620:root@pam:"

	cases := []struct {
		name     string
		fastPath bool
		at       string
	}{
		{name: "stop", at: "stop"},
		{name: "after stop, before the disk reads", at: "after-stop"},
		{name: "destroy", at: "destroy"},
		{name: "destroy await", at: "await"},
		{name: "fast-path stop", fastPath: true, at: "stop"},
		{name: "fast-path destroy", fastPath: true, at: "destroy"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vmid := 620 + i
			vm := &movingVM{}
			qemuSvc := &mockQEMUService{
				stopFn: func(context.Context, string, int) (string, error) {
					switch tc.at {
					case "stop":
						vm.move()
						return "", notFoundAPIErr()
					case "after-stop":
						vm.move()
					}
					return "", nil
				},
				configFn: vm.config,
			}
			resp := nodes.DeleteQemuResponse(`"` + upid + `"`)
			destroy := &recordingDestroy{}
			deleteFn := destroy.fn(&resp, nil, nil)
			if tc.at == "destroy" {
				deleteFn = destroy.fn(nil, notFoundAPIErr(), vm.move)
			}
			nodesSvc := &mockNodesService{
				deleteQemuFn: deleteFn,
				updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
					return nil
				},
			}
			tasksSvc := &mockTasksService{
				waitFn: func(_ context.Context, _, got string, _ *tasks.WaitOptions) (*tasks.Status, error) {
					if tc.at == "await" && got == upid {
						vm.move()
						return nil, notFoundAPIErr()
					}
					return &tasks.Status{ExitStatus: "OK"}, nil
				},
			}
			deps := twoMemberDeps(vmid, qemuSvc, nodesSvc, tasksSvc, tc.fastPath)

			_, err := handlers.HandleDeleteVM(
				deps,
			).Handle(quickTaskPollCtx(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})

			requireAbsenceRetriable(t, err, "pve-node2")
			for _, node := range destroy.calls() {
				if node != "pve-node1" {
					t.Errorf("destroy ran on %s; the retry must locate the VM afresh first", node)
				}
			}
			if tc.at == "stop" || tc.at == "after-stop" {
				if got := destroy.calls(); len(got) != 0 {
					t.Errorf("destroy calls = %v, want none once the VM moved before the destroy", got)
				}
			}
		})
	}
}

// TestDeleteVM_StaleIndexRelocatesToTheMemberThatHoldsTheVM covers the index
// that still names the old owner after a migration: the first config read on
// pve-node1 answers not found, the sweep finds the VM on pve-node2, and the
// delete runs there instead of reporting the VM gone.
func TestDeleteVM_StaleIndexRelocatesToTheMemberThatHoldsTheVM(t *testing.T) {
	t.Parallel()

	const vmid = 640
	const upid = "UPID:pve-node2:00AABBCC:00112233:6789ABCD:qmdestroy:640:root@pam:"
	vm := &movingVM{}
	vm.move()
	var stopped []string
	var stopMu sync.Mutex
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, node string, _ int) (string, error) {
			stopMu.Lock()
			stopped = append(stopped, node)
			stopMu.Unlock()
			return "", nil
		},
		configFn: vm.config,
	}
	resp := nodes.DeleteQemuResponse(`"` + upid + `"`)
	destroy := &recordingDestroy{}
	nodesSvc := &mockNodesService{deleteQemuFn: destroy.fn(&resp, nil, nil)}
	tasksSvc := &mockTasksService{
		waitFn: func(context.Context, string, string, *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	deps := twoMemberDeps(vmid, qemuSvc, nodesSvc, tasksSvc, false)

	_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("delete of a VM the stale index misplaced: %v", err)
	}
	if got := destroy.calls(); len(got) != 1 || got[0] != "pve-node2" {
		t.Fatalf("destroy calls = %v, want one on pve-node2", got)
	}
	stopMu.Lock()
	defer stopMu.Unlock()
	for _, node := range stopped {
		if node != "pve-node2" {
			t.Errorf("stop ran on %s, want pve-node2", node)
		}
	}
}

// TestDeleteVM_ConfigMissingFromANodeThatCannotAnswerIsRetriable pins the
// proxy-target rule: a config-missing answer routed through a member the
// cluster does not report able to answer, or through a cluster whose status
// cannot be read, never proves the VM gone. The HA gate reads the cluster
// status before any config read, so it is the gate's retriable deferral that
// answers here, and nothing is destroyed.
func TestDeleteVM_ConfigMissingFromANodeThatCannotAnswerIsRetriable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status func(context.Context) (*cluster.ListStatusResponse, error)
		want   string
	}{
		{
			name: "owner offline",
			status: func(context.Context) (*cluster.ListStatusResponse, error) {
				return absenceStatus(map[string]bool{"pve-node1": false, "pve-node2": true}), nil
			},
			want: "deferred",
		},
		{
			name: "status unreadable",
			status: func(context.Context) (*cluster.ListStatusResponse, error) {
				return nil, errors.New("cluster status unavailable")
			},
			want: "cluster status unavailable",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vmid := 650 + i
			qemuSvc := &mockQEMUService{
				stopFn: func(context.Context, string, int) (string, error) { return "", nil },
				configFn: func(context.Context, string, int) (map[string]any, error) {
					return nil, notFoundAPIErr()
				},
			}
			destroy := &recordingDestroy{}
			nodesSvc := &mockNodesService{deleteQemuFn: destroy.fn(nil, nil, nil)}
			deps := twoMemberDeps(vmid, qemuSvc, nodesSvc, &mockTasksService{}, false)
			deps.PVE.(*mockPVEClient).clusterSvc.(*mockClusterSvc).listStatusFn = tc.status

			_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})

			requireAbsenceRetriable(t, err, tc.want)
			if got := destroy.calls(); len(got) != 0 {
				t.Errorf("destroy calls = %v, want none", got)
			}
		})
	}
}
