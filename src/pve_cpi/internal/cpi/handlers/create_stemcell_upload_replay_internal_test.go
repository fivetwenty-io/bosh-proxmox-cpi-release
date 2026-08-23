// Package handlers: white-box tests for uploadStemcellImage's pre-retry
// name sweep: a retry means the previous multipart POST died mid-flight, and
// PVE may still have committed the file, so the target name must be cleared
// before re-uploading (a duplicate import upload is rejected with HTTP 409).
package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// Step labels for urStorage.sequence.
const (
	urStepUpload = "upload"
	urStepSweep  = "sweep"
)

// urStorage scripts Upload and DeleteVolumeIfExistsAsync per call and records
// the interleaving so tests can assert the sweep runs between upload attempts.
type urStorage struct {
	sdkstorage.Service

	mu        sync.Mutex
	sequence  []string
	uploadFn  func(call int) (string, error)
	sweepFn   func(call int, volume string) (bool, string, error)
	uploads   int
	sweeps    int
	sweptVols []string
}

func (s *urStorage) Upload(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
	// Drain the body like the real endpoint would; each attempt must hand
	// over a freshly opened reader.
	_, _ = io.ReadAll(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads++
	s.sequence = append(s.sequence, urStepUpload)
	return s.uploadFn(s.uploads)
}

func (s *urStorage) DeleteVolumeIfExistsAsync(_ context.Context, _, _, volume string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweeps++
	s.sequence = append(s.sequence, urStepSweep)
	s.sweptVols = append(s.sweptVols, volume)
	return s.sweepFn(s.sweeps, volume)
}

// urTasks scripts task awaits for upload tasks; call counts are 1-based.
type urTasks struct {
	sdktasks.Service
	mu        sync.Mutex
	waitCalls int
	waitFn    func(call int, upid string) (*sdktasks.Status, error)
}

func (t *urTasks) Wait(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.waitCalls++
	if t.waitFn != nil {
		return t.waitFn(t.waitCalls, upid)
	}
	return &sdktasks.Status{ExitStatus: "OK"}, nil
}

func (t *urTasks) GetStatus(_ context.Context, _, upid string) (*sdktasks.Status, error) {
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

// urClient wires the storage service and an optional tasks service;
// uploadStemcellImage touches nothing else when every UPID it sees is empty.
type urClient struct {
	storage sdkstorage.Service
	tasks   sdktasks.Service
}

func (c *urClient) QEMU() sdkqemu.Service                     { return nil }
func (c *urClient) Nodes() sdknodes.Service                   { return nil }
func (c *urClient) Tasks() sdktasks.Service                   { return c.tasks }
func (c *urClient) Storage() sdkstorage.Service               { return c.storage }
func (c *urClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *urClient) Cluster() sdkcluster.Service               { return nil }
func (c *urClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *urClient) Pools() pve.PoolService                    { return nil }

func urImageFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "img.qcow2")
	if err := os.WriteFile(path, []byte("qcow2-bytes"), 0o600); err != nil {
		t.Fatalf("write image fixture: %v", err)
	}
	return path
}

func urDeps(storage sdkstorage.Service) Deps {
	return Deps{PVE: &urClient{storage: storage}, Logger: log.NewNopLogger()}
}

func urDepsWithTasks(storage sdkstorage.Service, tasks sdktasks.Service) Deps {
	return Deps{PVE: &urClient{storage: storage, tasks: tasks}, Logger: log.NewNopLogger()}
}

func urCtx() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// TestUploadStemcellImage_RetrySweepsCommittedPartialUpload scripts the
// dropped-response replay: attempt 1's POST dies mid-flight after PVE
// committed the file. The retry must clear the target name (the sweep
// reports it existed) before re-uploading, or the re-upload 409s forever.
func TestUploadStemcellImage_RetrySweepsCommittedPartialUpload(t *testing.T) {
	t.Parallel()
	s := &urStorage{
		uploadFn: func(call int) (string, error) {
			if call == 1 {
				return "", fmt.Errorf("Post \"/nodes/pve1/storage/local/upload\": %w", io.EOF)
			}
			return "", nil
		},
		sweepFn: func(_ int, _ string) (bool, string, error) {
			return true, "", nil // the drop had committed; the sweep cleared it
		},
	}

	err := uploadStemcellImage(urCtx(), urDeps(s), "pve1", "local", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.uploads != 2 {
		t.Errorf("uploads: want 2 (dropped attempt + retry), got %d", s.uploads)
	}
	if s.sweeps != 1 {
		t.Fatalf("sweeps: want exactly 1 before the retry, got %d", s.sweeps)
	}
	want := []string{urStepUpload, urStepSweep, urStepUpload}
	for i, step := range want {
		if i >= len(s.sequence) || s.sequence[i] != step {
			t.Fatalf("call sequence: want %v, got %v", want, s.sequence)
		}
	}
	if s.sweptVols[0] != "local:import/img.qcow2" {
		t.Errorf("sweep volume: want local:import/img.qcow2, got %s", s.sweptVols[0])
	}
}

// TestUploadStemcellImage_SweepLockFaultRidesBackoff verifies that a sweep
// failing on the same retryable fault class the loop handles is returned to
// the loop (riding the backoff) instead of pressing on into an upload that
// would turn the retryable fault into a permanent 409.
func TestUploadStemcellImage_SweepLockFaultRidesBackoff(t *testing.T) {
	t.Parallel()
	s := &urStorage{
		uploadFn: func(call int) (string, error) {
			if call == 1 {
				return "", fmt.Errorf("Post \"/nodes/pve1/storage/local/upload\": %w", io.EOF)
			}
			return "", nil
		},
		sweepFn: func(call int, _ string) (bool, string, error) {
			if call == 1 {
				return false, "", errors.New("cfs-lock 'storage-local' error: got lock request timeout")
			}
			return false, "", nil
		},
	}

	err := uploadStemcellImage(urCtx(), urDeps(s), "pve1", "local", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{urStepUpload, urStepSweep, urStepSweep, urStepUpload}
	if len(s.sequence) != len(want) {
		t.Fatalf("call sequence: want %v, got %v", want, s.sequence)
	}
	for i, step := range want {
		if s.sequence[i] != step {
			t.Fatalf("call sequence: want %v, got %v", want, s.sequence)
		}
	}
}

// TestUploadStemcellImage_ReAwaitsOwnTaskInsteadOfSweeping covers the
// known-task arm: the upload POST succeeded and returned a UPID, but the
// await's own poll failed transiently while the upload task was still
// running. The retry must re-await that SAME task, never sweep the file a
// live upload task is writing, and a completed task converts to success
// without a second upload.
func TestUploadStemcellImage_ReAwaitsOwnTaskInsteadOfSweeping(t *testing.T) {
	t.Parallel()
	s := &urStorage{
		uploadFn: func(_ int) (string, error) {
			return "UPID:pve1:000A1B2C:00112233:66aabbcc:imgcopy:0:root@pam:", nil
		},
		sweepFn: func(_ int, _ string) (bool, string, error) {
			t.Error("the sweep must never run while our own upload task is unresolved")
			return false, "", nil
		},
	}
	tasks := &urTasks{
		waitFn: func(call int, upid string) (*sdktasks.Status, error) {
			if call == 1 {
				return nil, fmt.Errorf("Get \"/nodes/pve1/tasks/%s/status\": %w", upid, io.EOF)
			}
			return &sdktasks.Status{ExitStatus: "OK"}, nil
		},
	}

	err := uploadStemcellImage(urCtx(), urDepsWithTasks(s, tasks), "pve1", "local", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.uploads != 1 {
		t.Errorf("uploads: want exactly 1 (the prior task completed; no re-upload), got %d", s.uploads)
	}
	if s.sweeps != 0 {
		t.Errorf("sweeps: want 0, got %d", s.sweeps)
	}
	if tasks.waitCalls != 2 {
		t.Errorf("task awaits: want 2 (blipped await + re-await of the same task), got %d", tasks.waitCalls)
	}
}

// TestUploadStemcellImage_PermanentSweepFailureWarnsAndProceeds pins the
// best-effort arm: a sweep failing on a fault class the loop does not retry
// must not abort the retry; the re-upload proceeds and surfaces the truth.
func TestUploadStemcellImage_PermanentSweepFailureWarnsAndProceeds(t *testing.T) {
	t.Parallel()
	s := &urStorage{
		uploadFn: func(call int) (string, error) {
			if call == 1 {
				return "", fmt.Errorf("Post \"/nodes/pve1/storage/local/upload\": %w", io.EOF)
			}
			return "", nil
		},
		sweepFn: func(_ int, _ string) (bool, string, error) {
			return false, "", errors.New("volume deletion not supported for content type 'import'")
		},
	}

	err := uploadStemcellImage(urCtx(), urDeps(s), "pve1", "local", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("a permanently failing sweep must not fail the upload retry, got: %v", err)
	}
	want := []string{urStepUpload, urStepSweep, urStepUpload}
	if len(s.sequence) != len(want) {
		t.Fatalf("call sequence: want %v, got %v", want, s.sequence)
	}
	for i, step := range want {
		if s.sequence[i] != step {
			t.Fatalf("call sequence: want %v, got %v", want, s.sequence)
		}
	}
}

// TestUploadStemcellImage_LockVerdictTaskSweepsAndReuploads pins the
// resolved-task arm: the upload task itself fails with a per-storage
// lock-timeout VERDICT (retryable, but fully resolved). The retry must sweep
// the task's partial file and re-upload, never re-poll the dead task until
// the budget burns out.
func TestUploadStemcellImage_LockVerdictTaskSweepsAndReuploads(t *testing.T) {
	t.Parallel()
	s := &urStorage{
		uploadFn: func(call int) (string, error) {
			if call == 1 {
				return "UPID:pve1:0000AB12:00112233:66aabbcc:imgcopy:0:root@pam:", nil
			}
			return "", nil
		},
		sweepFn: func(_ int, _ string) (bool, string, error) {
			return true, "", nil
		},
	}
	tasks := &urTasks{
		waitFn: func(_ int, _ string) (*sdktasks.Status, error) {
			// The SDK's own resolved-task failure shape ("task failed: <exit
			// text>"): AwaitTask must normalize it into the CPI's verdict
			// render before IsTaskExitVerdict can recognize it.
			return nil, fmt.Errorf("task failed: %s",
				"can't lock file '/var/lock/pve-manager/pve-storage-local' - got timeout")
		},
	}

	err := uploadStemcellImage(urCtx(), urDepsWithTasks(s, tasks), "pve1", "local", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("a lock-verdict upload task must be re-driven to success, got: %v", err)
	}
	want := []string{urStepUpload, urStepSweep, urStepUpload}
	if len(s.sequence) != len(want) {
		t.Fatalf("call sequence: want %v, got %v", want, s.sequence)
	}
	for i, step := range want {
		if s.sequence[i] != step {
			t.Fatalf("call sequence: want %v, got %v", want, s.sequence)
		}
	}
	if tasks.waitCalls != 1 {
		t.Errorf("task awaits: want 1 (a resolved verdict must not be re-polled), got %d", tasks.waitCalls)
	}
}

// TestUploadStemcellImage_PollTimeoutOnReAwaitDoesNotSweep pins the
// unresolved-task arm: the re-await outruns its poll budget while the upload
// task still runs. The failure must surface (retriable) without sweeping the
// file that live task is writing and without a second upload.
func TestUploadStemcellImage_PollTimeoutOnReAwaitDoesNotSweep(t *testing.T) {
	t.Parallel()
	s := &urStorage{
		uploadFn: func(_ int) (string, error) {
			return "UPID:pve1:0000CD34:00112233:66aabbcc:imgcopy:0:root@pam:", nil
		},
		sweepFn: func(_ int, _ string) (bool, string, error) {
			t.Error("the sweep must never run while the task is unresolved")
			return false, "", nil
		},
	}
	tasks := &urTasks{
		waitFn: func(call int, upid string) (*sdktasks.Status, error) {
			if call == 1 {
				return nil, fmt.Errorf("Get \"/nodes/pve1/tasks/%s/status\": %w", upid, io.EOF)
			}
			// Wait window elapsed with no terminal exit status: still running.
			return &sdktasks.Status{ExitStatus: ""}, nil
		},
	}

	err := uploadStemcellImage(urCtx(), urDepsWithTasks(s, tasks), "pve1", "local", "img.qcow2", urImageFile(t), "")
	if err == nil {
		t.Fatal("an unresolved upload task must surface, not silently succeed")
	}
	if s.uploads != 1 {
		t.Errorf("uploads: want 1 (no re-upload over a running task), got %d", s.uploads)
	}
	if s.sweeps != 0 {
		t.Errorf("sweeps: want 0, got %d", s.sweeps)
	}
	if tasks.waitCalls != 2 {
		t.Errorf("task awaits: want 2 (blipped await + re-await), got %d", tasks.waitCalls)
	}
}
