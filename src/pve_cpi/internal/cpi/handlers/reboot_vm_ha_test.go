package handlers_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// These tests drive reboot_vm through the handler with the PVE view served by
// the HA scene. They pin that the reboot waits out a migration and follows the
// VM to its new node, that it refuses a node that cannot answer, and that an
// HA-managed VM is stopped and started through PVE HA rather than by a direct
// call that the CRM would undo.

// settleTestCtx is a context whose settle waits run on a fake clock, so a
// ten-minute migration takes no real time.
func settleTestCtx() context.Context {
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return pve.WithVMSettleClockForTest(context.Background(),
		func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
			return ctx.Err()
		})
}

// statusQEMU answers the status read from a function and everything else from
// the scene.
type statusQEMU struct {
	*handlers.HASceneQEMU
	status func() (map[string]any, error)
}

func (q *statusQEMU) Status(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.status != nil {
		return q.status()
	}
	return q.HASceneQEMU.Status(ctx, node, vmid)
}

// haReboot is a handler-level reboot of VM 101 against scene.
type haReboot struct {
	scene *handlers.HADeleteScene
	deps  handlers.Deps
	mu    sync.Mutex
	// rebootNodes and resetNodes are the nodes the soft reboot and the hard
	// reset were sent to.
	rebootNodes []string
	resetNodes  []string
}

func newHAReboot(t *testing.T, scene *handlers.HADeleteScene, mode string) *haReboot {
	t.Helper()
	scene.IndexTags = "bosh-cpi"
	r := &haReboot{scene: scene}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi"}, nil
		},
		resetFn: func(_ context.Context, node string, _ int) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.resetNodes = append(r.resetNodes, node)
			return "", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, node, _ string, _ *nodes.CreateQemuStatusRebootParams) (
			*nodes.CreateQemuStatusRebootResponse, error,
		) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.rebootNodes = append(r.rebootNodes, node)
			return rebootRawEmpty(), nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	cfg := testConfig()
	cfg.RebootMode = mode
	haMode := config.HAChoice(config.HAModeAuto)
	cfg.HA = &config.HAConfig{Mode: &haMode}
	r.deps = handlers.Deps{
		Config: cfg,
		Agent:  &mockAgentService{},
		Logger: log.NewNopLogger(),
		PVE: &handlers.HAScenePermsClient{Scene: scene, Client: &mockPVEClient{
			qemuSvc:    &handlers.HASceneQEMU{Service: qemuSvc, Scene: scene},
			nodesSvc:   &handlers.HASceneNodes{Service: nodesSvc, Scene: scene},
			tasksSvc:   tasksSvc,
			storageSvc: &mockStorageService{},
			clusterSvc: &handlers.HASceneCluster{Service: &mockClusterSvc{}, Scene: scene, UseIndex: true},
			poolsSvc:   &noopPoolService{},
		}},
	}
	return r
}

func (r *haReboot) run(ctx context.Context) error {
	_, err := handlers.HandleRebootVM(r.deps).Handle(ctx, marshalArgs("101"), jsonrpc.Context{})
	return err
}

// withStatus replaces the VM status read.
func (r *haReboot) withStatus(f func() (map[string]any, error)) {
	mc := r.deps.PVE.(*handlers.HAScenePermsClient).Client.(*mockPVEClient)
	mc.qemuSvc = &statusQEMU{HASceneQEMU: mc.qemuSvc.(*handlers.HASceneQEMU), status: f}
}

func noHAWrites(t *testing.T, events []string) {
	t.Helper()
	for _, ev := range events {
		if strings.HasPrefix(ev, "UpdateHaResources:") || strings.HasPrefix(ev, "DeleteHaResources:") ||
			strings.HasPrefix(ev, "Stop:") || strings.HasPrefix(ev, "Start:") {
			t.Fatalf("expected no HA write or direct power call, got %v", events)
		}
	}
}

// TestHandleRebootVM_WaitsOutMigrationAndRebootsOnNewNode holds the VM under a
// migrate lock on node1 for several polls, then lands it on node2. The reboot
// goes to node2.
func TestHandleRebootVM_WaitsOutMigrationAndRebootsOnNewNode(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Place(101, "pve-node1")
	scene.Do(func(s *handlers.HADeleteScene) { s.IndexLock[101] = "migrate" })
	scene.OnIndexRead = func(s *handlers.HADeleteScene, reads int) {
		if reads >= 4 {
			s.IndexNode[101] = "pve-node2"
			delete(s.IndexLock, 101)
		}
	}
	r := newHAReboot(t, scene, "soft")

	if err := r.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.rebootNodes) != 1 || r.rebootNodes[0] != "pve-node2" {
		t.Fatalf("soft reboot nodes = %v, want [pve-node2]", r.rebootNodes)
	}
	if len(r.resetNodes) != 0 {
		t.Fatalf("unexpected hard reset on %v", r.resetNodes)
	}
}

// TestHandleRebootVM_MigrationAfterTheWaitIsRetriableNotGone starts a migration
// after the wait returned: the status read on the old node answers config
// missing and the VM is on node2. The reboot answers a retriable error, never
// VMNotFound, and sends no reset or reboot.
func TestHandleRebootVM_MigrationAfterTheWaitIsRetriableNotGone(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Place(101, "pve-node1")
	r := newHAReboot(t, scene, "hard")
	r.withStatus(func() (map[string]any, error) {
		scene.Place(101, "pve-node2")
		return nil, notFoundAPIErr()
	})

	err := r.run(settleTestCtx())
	requireHARetriable(t, err, "answered that the VM is missing")
	var typed *cpierrors.Error
	if errors.As(err, &typed) && typed.Type() == cpierrors.TypeVMNotFound {
		t.Fatalf("a VM found on another node must not be reported gone: %v", err)
	}
	if len(r.resetNodes) != 0 || len(r.rebootNodes) != 0 {
		t.Fatalf("no reboot or reset may be sent, got reset=%v reboot=%v", r.resetNodes, r.rebootNodes)
	}
}

// TestHandleRebootVM_DeadNodeDefersWithoutCalls leaves the VM on a node the
// cluster reports offline. The reboot is a retriable error naming the node,
// and nothing is sent to it.
func TestHandleRebootVM_DeadNodeDefersWithoutCalls(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Place(101, "pve-node2")
	scene.MarkNodeDead("pve-node2")
	r := newHAReboot(t, scene, "soft")
	statusCalls := 0
	r.withStatus(func() (map[string]any, error) {
		statusCalls++
		return map[string]any{"status": "running"}, nil
	})

	requireHARetriable(t, r.run(settleTestCtx()), "pve-node2")
	if statusCalls != 0 || len(r.rebootNodes) != 0 || len(r.resetNodes) != 0 {
		t.Fatalf("no call may reach the dead node: status=%d reboot=%v reset=%v",
			statusCalls, r.rebootNodes, r.resetNodes)
	}
	noHAWrites(t, scene.EventsSnapshot())
}

// TestHandleRebootVM_HAManagedRunningThroughTheResource stops and then starts
// an HA-managed running VM through its HA resource when the token holds
// Sys.Console. No direct power call is made, in either reboot mode.
func TestHandleRebootVM_HAManagedRunningThroughTheResource(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"soft", "hard"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			scene := handlers.NewHADeleteScene()
			scene.Manage(101, "pve-node1", "started", "started")
			r := newHAReboot(t, scene, mode)

			if err := r.run(settleTestCtx()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			events := scene.EventsSnapshot()
			stop := eventIndex(events, "UpdateHaResources:vm:101=stopped")
			start := eventIndex(events, "UpdateHaResources:vm:101=started")
			if stop < 0 || start < 0 || stop > start {
				t.Fatalf("want a stopped request and then a started request, got %v", events)
			}
			if eventIndex(events, "Stop:") >= 0 || eventIndex(events, "Start:") >= 0 {
				t.Fatalf("a direct power call must not be made through Sys.Console, got %v", events)
			}
			if len(r.rebootNodes) != 0 || len(r.resetNodes) != 0 {
				t.Fatalf("no direct reboot or reset may be sent, got %v %v", r.rebootNodes, r.resetNodes)
			}
		})
	}
}

// TestHandleRebootVM_HAManagedWithoutSysConsole stops and starts the VM
// directly on the CRM's node when the token lacks Sys.Console, and writes no
// HA resource.
func TestHandleRebootVM_HAManagedWithoutSysConsole(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.Do(func(s *handlers.HADeleteScene) { delete(s.Perms, "Sys.Console") })
	r := newHAReboot(t, scene, "soft")

	if err := r.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := scene.EventsSnapshot()
	stop, start := eventIndex(events, "Stop:pve-node1/101"), eventIndex(events, "Start:pve-node1/101")
	if stop < 0 || start < 0 || stop > start {
		t.Fatalf("want a direct stop and then a direct start, got %v", events)
	}
	if eventIndex(events, "UpdateHaResources:") >= 0 {
		t.Fatalf("no HA resource may be written without Sys.Console, got %v", events)
	}
}

// TestHandleRebootVM_HAManagedStoppedOnlyStarts starts a stopped HA-managed VM
// and does not stop it first.
func TestHandleRebootVM_HAManagedStoppedOnlyStarts(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "stopped", "stopped")
	r := newHAReboot(t, scene, "soft")

	if err := r.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := scene.EventsSnapshot()
	if len(events) != 1 || events[0] != "UpdateHaResources:vm:101=started" {
		t.Fatalf("want one started request, got %v", events)
	}
}

// TestHandleRebootVM_HAErrorStates covers a resource in error. A running guest
// is never sent disabled, and the reboot returns the retriable error that names
// the ha-manager commands. A stopped guest is cleared with disabled and then
// started.
func TestHandleRebootVM_HAErrorStates(t *testing.T) {
	t.Parallel()
	t.Run("running guest is never disabled", func(t *testing.T) {
		t.Parallel()
		scene := handlers.NewHADeleteScene()
		scene.Manage(101, "pve-node1", "started", "error")
		scene.Do(func(s *handlers.HADeleteScene) { s.Power[101] = "running" })
		r := newHAReboot(t, scene, "soft")

		requireHARetriable(t, r.run(settleTestCtx()), "ha-manager remove")
		noHAWrites(t, scene.EventsSnapshot())
	})
	t.Run("stopped guest is cleared and started", func(t *testing.T) {
		t.Parallel()
		scene := handlers.NewHADeleteScene()
		scene.Manage(101, "pve-node1", "started", "error")
		r := newHAReboot(t, scene, "soft")

		if err := r.run(settleTestCtx()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		events := scene.EventsSnapshot()
		disabled := eventIndex(events, "UpdateHaResources:vm:101=disabled")
		started := eventIndex(events, "UpdateHaResources:vm:101=started")
		if disabled < 0 || started < 0 || disabled > started {
			t.Fatalf("want disabled and then started, got %v", events)
		}
	})
}

// TestHandleRebootVM_IgnoredResourceDefers defers the reboot of a VM whose
// resource is ignored, and the error carries the typed sentinel.
func TestHandleRebootVM_IgnoredResourceDefers(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "ignored", "started")
	r := newHAReboot(t, scene, "soft")

	err := r.run(settleTestCtx())
	requireHARetriable(t, err)
	if !errors.Is(err, pve.ErrHAResourceIgnored) {
		t.Fatalf("want the ignored-resource sentinel in the chain, got %v", err)
	}
	noHAWrites(t, scene.EventsSnapshot())
}

// TestHandleRebootVM_FenceDefersWithoutWrites defers the reboot while the
// service is in fence.
func TestHandleRebootVM_FenceDefersWithoutWrites(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "fence")
	r := newHAReboot(t, scene, "soft")

	requireHARetriable(t, r.run(settleTestCtx()))
	noHAWrites(t, scene.EventsSnapshot())
	if len(r.rebootNodes) != 0 || len(r.resetNodes) != 0 {
		t.Fatalf("no reboot or reset may be sent, got %v %v", r.rebootNodes, r.resetNodes)
	}
}

// TestHandleRebootVM_UnreadableTokenPermissionsDefer defers the restart of an
// HA-managed VM when the token's permissions cannot be read, rather than
// guessing the path.
func TestHandleRebootVM_UnreadableTokenPermissionsDefer(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.Do(func(s *handlers.HADeleteScene) { s.Perms = nil })
	r := newHAReboot(t, scene, "soft")

	requireHARetriable(t, r.run(settleTestCtx()), "permissions")
	noHAWrites(t, scene.EventsSnapshot())
}

// staleHAViewScene is VM 101 on node1, not HA-managed, in a cluster whose CRM
// timestamp is an hour old, so the HA view cannot be proven.
func staleHAViewScene() *handlers.HADeleteScene {
	scene := handlers.NewHADeleteScene()
	scene.Place(101, "pve-node1")
	scene.Do(func(s *handlers.HADeleteScene) { s.CRMAge = 3600 })
	return scene
}

// TestHandleRebootVM_UnknownHAViewByMode pins what an HA view that cannot be
// proven does to the reboot of a VM that is not in HA. With pve.ha.mode off the
// CPI leaves HA alone, so the reboot never reads it and never defers on it. In
// auto and managed mode the reboot defers, because a direct reset of a VM that
// turns out to be HA-managed would fight the CRM.
func TestHandleRebootVM_UnknownHAViewByMode(t *testing.T) {
	t.Parallel()
	t.Run("off reboots directly", func(t *testing.T) {
		t.Parallel()
		scene := staleHAViewScene()
		r := newHAReboot(t, scene, "soft")
		r.deps.Config.HA = nil

		if err := r.run(settleTestCtx()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(r.rebootNodes) != 1 || r.rebootNodes[0] != "pve-node1" {
			t.Fatalf("soft reboot nodes = %v, want [pve-node1]", r.rebootNodes)
		}
		noHAWrites(t, scene.EventsSnapshot())
	})
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode+" defers", func(t *testing.T) {
			t.Parallel()
			scene := staleHAViewScene()
			r := newHAReboot(t, scene, "soft")
			m := config.HAChoice(mode)
			r.deps.Config.HA = &config.HAConfig{Mode: &m}

			requireHARetriable(t, r.run(settleTestCtx()), "HA disposition")
			if len(r.rebootNodes) != 0 || len(r.resetNodes) != 0 {
				t.Fatalf("no reboot or reset may be sent on an unknown HA view, got %v %v", r.rebootNodes, r.resetNodes)
			}
		})
	}
}

// TestHandleRebootVM_MovedAgainAfterTheWaitIsFollowedInTheCall answers config
// missing to the first status read, because the VM migrated after the wait. The
// reboot waits again, finds the VM on node2, and reboots it there in the same
// call.
func TestHandleRebootVM_MovedAgainAfterTheWaitIsFollowedInTheCall(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Place(101, "pve-node1")
	r := newHAReboot(t, scene, "soft")
	statusCalls := 0
	r.withStatus(func() (map[string]any, error) {
		statusCalls++
		if statusCalls == 1 {
			scene.Place(101, "pve-node2")
			return nil, notFoundAPIErr()
		}
		return map[string]any{"status": "running"}, nil
	})

	if err := r.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.rebootNodes) != 1 || r.rebootNodes[0] != "pve-node2" {
		t.Fatalf("soft reboot nodes = %v, want [pve-node2]", r.rebootNodes)
	}
}

// TestHandleRebootVM_MovedTwiceIsRetriableAfterOneMorePass keeps the VM moving
// under every status read. The reboot resolves it once more, then returns a
// retriable error and sends nothing.
func TestHandleRebootVM_MovedTwiceIsRetriableAfterOneMorePass(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Place(101, "pve-node1")
	r := newHAReboot(t, scene, "soft")
	statusCalls := 0
	r.withStatus(func() (map[string]any, error) {
		statusCalls++
		if statusCalls%2 == 1 {
			scene.Place(101, "pve-node2")
		} else {
			scene.Place(101, "pve-node1")
		}
		return nil, notFoundAPIErr()
	})

	requireHARetriable(t, r.run(settleTestCtx()), "answered that the VM is missing")
	if statusCalls != 2 {
		t.Fatalf("status read %d times, want 2 passes", statusCalls)
	}
	if len(r.rebootNodes) != 0 || len(r.resetNodes) != 0 {
		t.Fatalf("no reboot or reset may be sent, got %v %v", r.rebootNodes, r.resetNodes)
	}
}
