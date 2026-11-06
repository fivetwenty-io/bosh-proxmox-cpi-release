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
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// These tests drive delete_vm through the handler on the legacy path, with
// the PVE HA view served by the delete scene. They pin which HA states defer
// the delete before anything is written, and which stop each managed VM
// takes before the destroy.

// legacyHADelete is a handler-level delete of VM 101, which the cluster scan
// locates on node, against scene.
type legacyHADelete struct {
	scene  *handlers.HADeleteScene
	deps   handlers.Deps
	purged bool
}

func newLegacyHADelete(t *testing.T, scene *handlers.HADeleteScene, node string, cfg *config.CPIConfig) *legacyHADelete {
	t.Helper()
	d := &legacyHADelete{scene: scene}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			d.purged = params != nil && params.Purge != nil && *params.Purge
			// The destroy with purge removes the HA resource, as PVE does.
			scene.Do(func(s *handlers.HADeleteScene) {
				s.Events = append(s.Events, "DeleteQemu")
				if d.purged {
					delete(s.Requested, "vm:101")
					delete(s.Services, "vm:101")
				}
			})
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, _ string, _ int) error { return nil },
	}
	d.deps = testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, agentSvc)
	d.deps.Config = cfg
	d.deps.PVE = &handlers.HAScenePermsClient{Scene: scene, Client: &mockPVEClient{
		qemuSvc:    &handlers.HASceneQEMU{Service: qemuSvc, Scene: scene},
		nodesSvc:   &handlers.HASceneNodes{Service: nodesSvc, Scene: scene},
		tasksSvc:   tasksSvc,
		storageSvc: &mockStorageService{},
		clusterSvc: &handlers.HASceneCluster{Service: defaultClusterSvc(101, node), Scene: scene},
		poolsSvc:   &noopPoolService{},
	}}
	return d
}

func (d *legacyHADelete) run() error {
	_, err := handlers.HandleDeleteVM(d.deps).Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})
	return err
}

// requireHARetriable fails unless err is a retriable CPI error naming every
// fragment of want.
func requireHARetriable(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a retriable error naming %q, got nil", want)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error %q does not name %q", err, w)
		}
	}
}

// eventIndex is the index of the first event that has prefix, or -1.
func eventIndex(events []string, prefix string) int {
	for i, ev := range events {
		if strings.HasPrefix(ev, prefix) {
			return i
		}
	}
	return -1
}

// TestHandleDeleteVM_HAStatesThatDeferWriteNothing defers the delete of a VM
// whose service or node is in fence, whose service is in recovery, whose node
// the CRM reports unknown, or whose LRM timestamp is stale. Each answers the
// Director with a retriable error, and the delete records no stop, no
// destroy, and no HA write.
func TestHandleDeleteVM_HAStatesThatDeferWriteNothing(t *testing.T) {
	cases := []struct {
		name  string
		node  string
		setup func(s *handlers.HADeleteScene)
		want  []string
	}{
		{name: "service in fence", node: "pve-node2", want: []string{"deferred", "fence"},
			setup: func(s *handlers.HADeleteScene) {
				s.Manage(101, "pve-node2", "started", "fence")
				s.MarkNodeDead("pve-node2")
				s.NodeState["pve-node2"] = "fence"
			}},
		{name: "node in fence", node: "pve-node2", want: []string{"deferred", "fenced"},
			setup: func(s *handlers.HADeleteScene) {
				s.Manage(101, "pve-node2", "started", "started")
				s.NodeState["pve-node2"] = "fence"
			}},
		{name: "recovery", node: "pve-node1", want: []string{"HA state is unknown", "does not report node"},
			setup: func(s *handlers.HADeleteScene) {
				s.Manage(101, "pve-node1", "started", "recovery")
			}},
		{name: "node unknown to the CRM", node: "pve-node1", want: []string{"deferred", "CRM reports the node unknown"},
			setup: func(s *handlers.HADeleteScene) {
				s.Manage(101, "pve-node1", "started", "started")
				s.NodeState["pve-node1"] = "unknown"
			}},
		{name: "stale LRM timestamp", node: "pve-node1", want: []string{"HA state is unknown", "no usable state"},
			setup: func(s *handlers.HADeleteScene) {
				s.Manage(101, "pve-node1", "started", "started")
				s.LRMAge["pve-node1"] = 3600
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scene := handlers.NewHADeleteScene()
			tc.setup(scene)
			d := newLegacyHADelete(t, scene, tc.node, dlbEnabledConfig())

			requireHARetriable(t, d.run(), tc.want...)
			if events := scene.EventsSnapshot(); len(events) != 0 {
				t.Fatalf("a deferred delete must write nothing; events: %v", events)
			}
		})
	}
}

// TestHandleDeleteVM_IgnoredHAResourceStopsDirectlyThenPurges stops a VM
// whose HA resource is ignored directly, because the CRM no longer acts on
// it, and then destroys it with purge, which removes the resource.
func TestHandleDeleteVM_IgnoredHAResourceStopsDirectlyThenPurges(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "ignored", "")
	d := newLegacyHADelete(t, scene, "pve-node1", dlbEnabledConfig())

	if err := d.run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := scene.EventsSnapshot()
	stop, destroy := eventIndex(events, "Stop:pve-node1/101"), eventIndex(events, "DeleteQemu")
	if stop == -1 || destroy == -1 || stop > destroy {
		t.Fatalf("the ignored VM must be stopped directly before the destroy; events: %v", events)
	}
	if at := eventIndex(events, "UpdateHaResources:"); at != -1 {
		t.Fatalf("an ignored resource takes no HA state write; events: %v", events)
	}
	if !d.purged {
		t.Fatal("the destroy must purge, which removes the HA resource")
	}
}

// TestHandleDeleteVM_ErroredHAResource walks a resource in error, stopped and
// running, with and without Sys.Console. With Sys.Console the delete stops
// the VM through PVE HA. Without it, a stopped VM is destroyed with purge and
// no HA write, and a running VM defers with the error that names Sys.Console.
func TestHandleDeleteVM_ErroredHAResource(t *testing.T) {
	cases := []struct {
		name       string
		running    bool
		sysConsole bool
		wantHA     bool
		wantErr    []string
	}{
		{name: "stopped with Sys.Console", sysConsole: true, wantHA: true},
		{name: "running with Sys.Console", running: true, sysConsole: true, wantHA: true},
		{name: "stopped without Sys.Console"},
		{name: "running without Sys.Console", running: true, wantErr: []string{"Sys.Console"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scene := handlers.NewHADeleteScene()
			scene.Manage(101, "pve-node1", "started", "error")
			scene.Do(func(s *handlers.HADeleteScene) {
				if tc.running {
					s.Power[101] = "running"
				}
				if !tc.sysConsole {
					delete(s.Perms, "Sys.Console")
				}
			})
			d := newLegacyHADelete(t, scene, "pve-node1", dlbEnabledConfig())

			err := d.run()
			events := scene.EventsSnapshot()
			if tc.wantErr != nil {
				requireHARetriable(t, err, tc.wantErr...)
				if at := eventIndex(events, "DeleteQemu"); at != -1 {
					t.Fatalf("a deferred delete must not destroy; events: %v", events)
				}
				if at := eventIndex(events, "Stop:"); at != -1 {
					t.Fatalf("a deferred delete must not stop the VM directly; events: %v", events)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			destroy := eventIndex(events, "DeleteQemu")
			if destroy == -1 || !d.purged {
				t.Fatalf("the VM must be destroyed with purge; purged %v, events: %v", d.purged, events)
			}
			if at := eventIndex(events, "Stop:"); at != -1 {
				t.Fatalf("an errored HA resource must not be stopped directly; events: %v", events)
			}
			haWrite := eventIndex(events, "UpdateHaResources:vm:101=")
			switch {
			case tc.wantHA && (haWrite == -1 || haWrite > destroy):
				t.Fatalf("with Sys.Console the VM must be stopped through PVE HA before the destroy; events: %v", events)
			case !tc.wantHA && haWrite != -1:
				t.Fatalf("without Sys.Console a stopped VM takes no HA write; events: %v", events)
			}
		})
	}
}

// TestHandleDeleteVM_FastPathTakesTheSynchronousPathForAnHAManagedVM keeps an
// HA-managed VM off the fast path. The stop goes through PVE HA and is
// awaited before the destroy, which never uses skiplock.
func TestHandleDeleteVM_FastPathTakesTheSynchronousPathForAnHAManagedVM(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	cfg := dlbEnabledConfig()
	cfg.FastPathDelete = boolPtr(true)
	d := newLegacyHADelete(t, scene, "pve-node1", cfg)
	var skiplock bool
	nodesSvc := d.deps.PVE.(*handlers.HAScenePermsClient).Client.(*mockPVEClient).nodesSvc.(*handlers.HASceneNodes)
	inner := nodesSvc.Service.(*mockNodesService)
	destroy := inner.deleteQemuFn
	inner.deleteQemuFn = func(ctx context.Context, node, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
		skiplock = skiplock || (params != nil && params.Skiplock != nil && *params.Skiplock)
		return destroy(ctx, node, vmid, params)
	}

	if err := d.run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := scene.EventsSnapshot()
	stop, del := eventIndex(events, "UpdateHaResources:vm:101=stopped"), eventIndex(events, "DeleteQemu")
	if stop == -1 || del == -1 || stop > del {
		t.Fatalf("the HA-managed VM must be stopped through PVE HA before the destroy; events: %v", events)
	}
	if at := eventIndex(events, "Stop:"); at != -1 {
		t.Fatalf("the fast path's fire-and-forget stop must not run; events: %v", events)
	}
	if skiplock {
		t.Fatal("the synchronous path never destroys an HA-managed VM with skiplock")
	}
	if !d.purged {
		t.Fatal("the destroy must purge, which removes the HA resource")
	}
}

// TestHandleDeleteVM_HandRegisteredHAResourceWithHAOff stops a VM that an
// operator registered with PVE HA by hand through PVE HA, although every
// pve.ha setting is off, because the CRM would otherwise restart it.
func TestHandleDeleteVM_HandRegisteredHAResourceWithHAOff(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	cfg := testConfig()
	if cfg.HA != nil {
		t.Fatalf("the base config must leave pve.ha unset: %+v", cfg.HA)
	}
	d := newLegacyHADelete(t, scene, "pve-node1", cfg)

	if err := d.run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := scene.EventsSnapshot()
	stop, destroy := eventIndex(events, "UpdateHaResources:vm:101=stopped"), eventIndex(events, "DeleteQemu")
	if stop == -1 || destroy == -1 || stop > destroy {
		t.Fatalf("a hand-registered resource must be stopped through PVE HA before the destroy; events: %v", events)
	}
	if at := eventIndex(events, "DeleteHaResources:"); at != -1 && at < destroy {
		t.Fatalf("the HA resource was removed before the destroy; events: %v", events)
	}
	for _, prefix := range []string{"Stop:", "CreateHaRules", "UpdateHaRules"} {
		if at := eventIndex(events, prefix); at != -1 {
			t.Fatalf("with pve.ha off the delete writes no %s; events: %v", prefix, events)
		}
	}
	if !d.purged {
		t.Fatal("the destroy must purge, which removes the HA resource")
	}
}
