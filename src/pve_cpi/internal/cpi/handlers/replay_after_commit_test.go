// replay_after_commit_test.go: handler-level tests for the replay-after-
// commit tolerances: a PVE mutation whose POST committed server-side but
// whose response was dropped replays on retry, and each site must converge
// on the goal state instead of double-applying or failing.
package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// TestHandleResizeDisk_ReplayConvergesAtTarget scripts the relative-resize
// replay: PVE applies the "+NG" growth, then the response drops. The retry
// must re-read the live size and recognize the target as reached and never
// re-issue the same delta, which would land the disk at target plus delta.
func TestHandleResizeDisk_ReplayConvergesAtTarget(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	sizeGiB := 10 // live disk size the fake tracks; target below is 15 GiB
	resizeCalls := 0
	configCalls := 0

	qemuSvc := &resizeQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()
			configCalls++
			// Calls 1 and 2 resolve the disk by bare volid; later calls
			// serve the live size for the delta read and any replay re-read.
			if configCalls <= 2 {
				return map[string]any{diskSlot: diskCID}, nil
			}
			return map[string]any{diskSlot: fmt.Sprintf("%s,size=%dG", diskCID, sizeGiB)}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, deltaGiB int) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			resizeCalls++
			sizeGiB += deltaGiB // PVE commits the growth...
			if resizeCalls == 1 {
				// ...then the response drops mid-flight.
				return "", fmt.Errorf("Put \"/nodes/pve1/qemu/100/resize\": %w", io.EOF)
			}
			return "", nil
		},
	}

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	// 15360 MiB = 15 GiB target; current 10 GiB → one +5G submit.
	_, err := h.Handle(fastRetryCtx(context.Background()),
		marshalArgs(mustEncodeDiskCID(t, diskCID, nil), 15360), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("replayed resize must converge, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sizeGiB != 15 {
		t.Errorf("final size: want the 15 GiB target (not target plus delta), got %d GiB", sizeGiB)
	}
	if resizeCalls != 1 {
		t.Errorf("resize submits: want 1 (the replay re-reads and converges), got %d", resizeCalls)
	}
}

// TestHandleSnapshotDisk_ReplayLeavesExactlyOneSnapshot scripts the
// same-name replay: the snapshot name is generated once per handler call, so
// a committed-then-dropped create replays into "already exists". The handler
// must recognize the committed snapshot as the goal state (returning its CID
// with exactly one snapshot in existence) instead of failing and provoking a
// Director redo that orphans the first snapshot.
func TestHandleSnapshotDisk_ReplayLeavesExactlyOneSnapshot(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var committed []string
	snapCalls := 0

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{diskSlot: diskCID}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, name string, _ map[string]any) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			snapCalls++
			if snapCalls == 1 {
				committed = append(committed, name) // PVE commits...
				return "", fmt.Errorf("Post \"/nodes/pve1/qemu/100/snapshot\": %w", io.EOF)
			}
			return "", fmt.Errorf("500 snapshot name '%s' already exists", name)
		},
		listSnapshotsFn: func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()
			out := make([]map[string]any, 0, 1+len(committed))
			out = append(out, map[string]any{"name": "current"})
			for _, name := range committed {
				out = append(out, map[string]any{"name": name})
			}
			return out, nil
		},
	}
	clusterSvc := resizeClusterWith(100)

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, clusterSvc, nil))
	result, err := h.Handle(fastRetryCtx(context.Background()),
		marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("replayed snapshot create must be success, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(committed) != 1 {
		t.Fatalf("exactly one snapshot must exist after the replay, got %d", len(committed))
	}
	if snapCalls != 2 {
		t.Errorf("snapshot submits: want 2 (dropped attempt + replay), got %d", snapCalls)
	}
	sid, ok := result.(string)
	if !ok || sid == "" {
		t.Fatalf("result: want non-empty snapshot_cid string, got %T %v", result, result)
	}
	if want := committed[0]; !strings.Contains(sid, want) {
		t.Errorf("snapshot_cid %q must reference the committed snapshot %q", sid, want)
	}
}

// TestHandleRebootVM_StartFailsButStatusProbeShowsRunning covers the probe
// arm of the start tolerance: the start submit fails with a rejection whose
// text does NOT say "already running", but the live status probe shows the
// VM running: a prior committed start reached the goal state, so reboot_vm
// must report success.
func TestHandleRebootVM_StartFailsButStatusProbeShowsRunning(t *testing.T) {
	t.Parallel()

	statusCalls := 0
	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			statusCalls++
			if statusCalls == 1 {
				return map[string]any{"status": "stopped"}, nil
			}
			return map[string]any{"status": "running"}, nil
		},
		startFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", errors.New("500 start failed: org.freedesktop.systemd1 timeout")
		},
	}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nil, nil, "soft"))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("start failure with a running VM must be success, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if statusCalls < 2 {
		t.Errorf("expected the live status probe after the failed start, got %d status reads", statusCalls)
	}
}

// TestCreateVM_StartReplayFindsVMRunning covers create_vm's arm of the same
// tolerance: the start submit fails without the "already running" text, but
// the live status probe shows the VM running (a replay of a committed start).
// create_vm must complete (returning the VM CID) instead of rolling the VM
// back out from under a start that actually succeeded.
func TestCreateVM_StartReplayFindsVMRunning(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{
		startFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", errors.New("500 unable to start VM: hypervisor rejected the duplicate start")
		},
		statusFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"status": "running"}, nil
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, a))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	result, err := h.Handle(context.Background(), args, mkCtx("start-replay"))
	if err != nil {
		t.Fatalf("start replay with a running VM must complete create_vm, got: %v", err)
	}
	pair, ok := result.([]any)
	if !ok || len(pair) == 0 || pair[0] == nil || fmt.Sprintf("%v", pair[0]) == "" {
		t.Fatalf("result: want [vm_cid, networks] with a non-empty CID, got %T %v", result, result)
	}
	if len(a.removeCalls) != 0 {
		t.Errorf("no rollback may run: agent.Remove called %d times", len(a.removeCalls))
	}
}

// TestHandleResizeDisk_DroppedPostSettlesBeforeResubmit covers the unnamed-
// task arm: the resize POST's response drops, but PVE created the task and it
// commits only AFTER the retry's first re-read. The bounded settle window
// must observe the landing and converge without a second submit, instead of
// trusting the stale size and re-issuing the full delta.
func TestHandleResizeDisk_DroppedPostSettlesBeforeResubmit(t *testing.T) {
	defer handlers.SetResizeSettleBounds(0, time.Second)()

	var mu sync.Mutex
	deltasSubmitted := 0
	resizeCalls := 0
	configCalls := 0

	qemuSvc := &resizeQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()
			configCalls++
			switch {
			case configCalls <= 2:
				return map[string]any{diskSlot: diskCID}, nil
			case configCalls <= 4:
				// Call 3: the delta read. Call 4: the replay re-read taken
				// while the unnamed resize task is still running.
				return map[string]any{diskSlot: diskCID + ",size=10G"}, nil
			default:
				// Calls 5+: the settle polls; the task has landed.
				return map[string]any{diskSlot: diskCID + ",size=15G"}, nil
			}
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, deltaGiB int) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			resizeCalls++
			deltasSubmitted += deltaGiB
			return "", fmt.Errorf("Put \"/nodes/pve1/qemu/100/resize\": %w", io.EOF)
		},
	}

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(fastRetryCtx(context.Background()),
		marshalArgs(mustEncodeDiskCID(t, diskCID, nil), 15360), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("settled replay must converge, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if resizeCalls != 1 {
		t.Errorf("resize submits: want 1 (the settle window observed the landing), got %d", resizeCalls)
	}
	if deltasSubmitted != 5 {
		t.Errorf("total submitted growth: want exactly the 5 GiB delta, got %d GiB", deltasSubmitted)
	}
}

// TestHandleResizeDisk_ReAwaitsPriorTaskBeforeReRead covers the known-task
// arm: the resize POST returned a UPID but the await's own poll failed
// transiently while the task still ran. The retry must re-await that SAME
// task before re-reading, so the recomputed delta sees the settled size and
// converges without a second submit.
func TestHandleResizeDisk_ReAwaitsPriorTaskBeforeReRead(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	resizeCalls := 0
	configCalls := 0
	waitCalls := 0

	qemuSvc := &resizeQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()
			configCalls++
			if configCalls <= 2 {
				return map[string]any{diskSlot: diskCID}, nil
			}
			if configCalls == 3 {
				return map[string]any{diskSlot: diskCID + ",size=10G"}, nil
			}
			// The re-read happens only after the re-await settled the task.
			if waitCalls < 2 {
				t.Errorf("re-read before the prior task settled (wait calls %d)", waitCalls)
			}
			return map[string]any{diskSlot: diskCID + ",size=15G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			resizeCalls++
			return "UPID:pve1:0000BEEF:00112233:66aabbcc:resize:100:root@pam:", nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			mu.Lock()
			defer mu.Unlock()
			waitCalls++
			if waitCalls == 1 {
				return nil, fmt.Errorf("Get \"/nodes/pve1/tasks/%s/status\": %w", upid, io.EOF)
			}
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), tasksSvc))
	_, err := h.Handle(fastRetryCtx(context.Background()),
		marshalArgs(mustEncodeDiskCID(t, diskCID, nil), 15360), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("re-awaited replay must converge, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if resizeCalls != 1 {
		t.Errorf("resize submits: want 1 (the re-awaited task carried the growth), got %d", resizeCalls)
	}
	if waitCalls != 2 {
		t.Errorf("task awaits: want 2 (blipped await + re-await of the same task), got %d", waitCalls)
	}
}

// TestHandleSnapshotDisk_HalfCreatedSnapshotIsNotTheGoalState is the control
// for the replay probe: a snapshot entry still carrying snapstate (a
// half-created snapshot the failed task left behind) is not a committed
// snapshot, so the replay's own error must surface instead of returning a
// CID for a snapshot that never completed.
func TestHandleSnapshotDisk_HalfCreatedSnapshotIsNotTheGoalState(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	snapCalls := 0

	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{diskSlot: diskCID}, nil
		},
		snapshotFn: func(_ context.Context, _ string, _ int, name string, _ map[string]any) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			snapCalls++
			if snapCalls == 1 {
				return "", fmt.Errorf("Post \"/nodes/pve1/qemu/100/snapshot\": %w", io.EOF)
			}
			return "", fmt.Errorf("500 snapshot name '%s' already exists", name)
		},
		listSnapshotsFn: func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
			return []map[string]any{
				{"name": "current"},
				// The failed first attempt left a half-created entry behind.
				{"name": "irrelevant-placeholder", "snapstate": "prepare"},
			}, nil
		},
	}

	// The listing cannot know the generated name up front, so capture it and
	// serve it with snapstate from a second fake wired after the fact.
	captured := ""
	inner := qemuSvc.snapshotFn
	qemuSvc.snapshotFn = func(ctx context.Context, node string, vmid int, name string, opts map[string]any) (string, error) {
		mu.Lock()
		captured = name
		mu.Unlock()
		return inner(ctx, node, vmid, name, opts)
	}
	qemuSvc.listSnapshotsFn = func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		return []map[string]any{
			{"name": "current"},
			{"name": captured, "snapstate": "prepare"},
		}, nil
	}

	h := handlers.HandleSnapshotDisk(snapDeps(qemuSvc, resizeClusterWith(100), nil))
	_, err := h.Handle(fastRetryCtx(context.Background()),
		marshalArgs(mustEncodeDiskCID(t, diskCID, nil)), jsonrpc.Context{})
	if err == nil {
		t.Fatal("a half-created snapshot must not be reported as success")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the replay's own error must surface, got: %v", err)
	}
}

// TestHandleResizeDisk_LockVerdictTaskRedrivesSubmit pins the resolved-task
// arm: the resize task itself fails with a per-storage lock-timeout VERDICT
// (retryable, but fully resolved). The retry must re-read and re-submit the
// resize, never re-poll the dead task until the budget burns out.
func TestHandleResizeDisk_LockVerdictTaskRedrivesSubmit(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	resizeCalls := 0
	configCalls := 0
	waitCalls := 0

	qemuSvc := &resizeQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()
			configCalls++
			if configCalls <= 2 {
				return map[string]any{diskSlot: diskCID}, nil
			}
			// The failed task applied nothing: the size stays at 10G for the
			// delta read and the retry's re-read alike.
			return map[string]any{diskSlot: diskCID + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			resizeCalls++
			if resizeCalls == 1 {
				return "UPID:pve1:0000FACE:00112233:66aabbcc:resize:100:root@pam:", nil
			}
			return "", nil // the re-driven submit completes synchronously
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			mu.Lock()
			defer mu.Unlock()
			waitCalls++
			// The SDK's own resolved-task failure shape ("task failed: <exit
			// text>", not the CPI's rendered verdict): AwaitTask must
			// normalize it before IsTaskExitVerdict can recognize it.
			return nil, fmt.Errorf("task failed: %s", "can't lock file '/var/lock/pve-manager/pve-storage-local' - got timeout")
		},
	}

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), tasksSvc))
	_, err := h.Handle(fastRetryCtx(context.Background()),
		marshalArgs(mustEncodeDiskCID(t, diskCID, nil), 15360), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("a lock-verdict task must be re-driven to success, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if resizeCalls != 2 {
		t.Errorf("resize submits: want 2 (failed task + re-driven submit), got %d", resizeCalls)
	}
	if waitCalls != 1 {
		t.Errorf("task awaits: want 1 (a resolved verdict must not be re-polled), got %d", waitCalls)
	}
}

// TestHandleResizeDisk_PollTimeoutOnReAwaitDoesNotResubmit pins the
// unresolved-task arm: the re-await of the prior attempt's task outruns its
// poll budget while the task still runs. The handler must surface that
// (retriable, for the Director) and never fall through to a stale re-read
// that would re-submit the full delta on top of the still-executing task.
func TestHandleResizeDisk_PollTimeoutOnReAwaitDoesNotResubmit(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	resizeCalls := 0
	configCalls := 0
	waitCalls := 0

	qemuSvc := &resizeQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			mu.Lock()
			defer mu.Unlock()
			configCalls++
			if configCalls <= 2 {
				return map[string]any{diskSlot: diskCID}, nil
			}
			return map[string]any{diskSlot: diskCID + ",size=10G"}, nil
		},
		resizeDiskFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			resizeCalls++
			return "UPID:pve1:0000FEED:00112233:66aabbcc:resize:100:root@pam:", nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			mu.Lock()
			defer mu.Unlock()
			waitCalls++
			if waitCalls == 1 {
				return nil, fmt.Errorf("Get \"/nodes/pve1/tasks/%s/status\": %w", upid, io.EOF)
			}
			// The SDK wait window elapsed with no terminal exit status: the
			// task is still running.
			return &tasks.Status{ExitStatus: ""}, nil
		},
	}

	h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(100), tasksSvc))
	_, err := h.Handle(fastRetryCtx(context.Background()),
		marshalArgs(mustEncodeDiskCID(t, diskCID, nil), 15360), jsonrpc.Context{})
	if err == nil {
		t.Fatal("an unresolved task must surface, not silently converge")
	}
	mu.Lock()
	defer mu.Unlock()
	if resizeCalls != 1 {
		t.Errorf("resize submits: want 1 (no stale resubmit over a running task), got %d", resizeCalls)
	}
	if waitCalls != 2 {
		t.Errorf("task awaits: want 2 (blipped await + re-await), got %d", waitCalls)
	}
}
