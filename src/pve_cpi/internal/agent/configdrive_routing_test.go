package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// newRoutedISOAgent builds a ConfigDrive whose resolver maps node pve2 to a
// direct address via an explicit entry (no discovery, no client calls).
func newRoutedISOAgent(storageSvc *fakeStorageSvc, nodesSvc *fakeNodesSvc, directHost string) *ConfigDrive {
	if storageSvc == nil {
		storageSvc = &fakeStorageSvc{}
	}
	if nodesSvc == nil {
		nodesSvc = &fakeNodesSvc{}
	}
	client := &fakePVEClient{storageSvc: storageSvc, nodesSvc: nodesSvc}
	resolver := pve.NewNodeEndpointResolver(client,
		map[string]string{"pve2": directHost}, "pve1.example.com", false, log.NewNopLogger())
	cd := newConfigDriveForTest(client, "local", log.NewNopLogger())
	cd.nodeEndpoints = resolver
	return cd
}

func TestConfigDrive_Configure_UploadsDirectlyToMappedNode(t *testing.T) {
	t.Parallel()

	var uploadHost, sweepHost, attachHost string
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(ctx context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		uploadHost = pve.UploadHostFromContext(ctx)
		return "", nil
	}
	sweeps := 0
	storageSvc.deleteVolumeIfExistsFn = func(ctx context.Context, _, _, _ string) (bool, error) {
		sweeps++
		sweepHost = pve.UploadHostFromContext(ctx)
		return false, nil
	}
	nodesSvc := &fakeNodesSvc{}
	nodesSvc.updateConfigFn = func(ctx context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
		attachHost = pve.UploadHostFromContext(ctx)
		return nil
	}
	a := newRoutedISOAgent(storageSvc, nodesSvc, "pve2.example.com")

	if err := a.Configure(context.Background(), "pve2", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if uploadHost != "pve2.example.com" {
		t.Errorf("upload ctx host = %q, want pve2.example.com (direct-to-node)", uploadHost)
	}
	// Configure's pre-upload sweep runs before uploadISO and stays un-pinned.
	if sweeps != 1 || sweepHost != "" {
		t.Errorf("pre-upload sweep: calls=%d host=%q, want 1 un-pinned call", sweeps, sweepHost)
	}
	if attachHost != "" {
		t.Errorf("attach ctx host = %q, want empty (config writes stay on the plain ctx)", attachHost)
	}
}

func TestConfigDrive_Configure_UnmappedNodeStaysUnpinned(t *testing.T) {
	t.Parallel()

	uploadHost := "sentinel"
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(ctx context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		uploadHost = pve.UploadHostFromContext(ctx)
		return "", nil
	}
	// Node pve9 has no entry and discovery is off.
	a := newRoutedISOAgent(storageSvc, nil, "pve2.example.com")

	if err := a.Configure(context.Background(), "pve9", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if uploadHost != "" {
		t.Errorf("upload ctx host = %q, want empty for an unmapped node", uploadHost)
	}
}

func TestConfigDrive_Configure_PinnedRetrySweepStaysPinned(t *testing.T) {
	t.Parallel()

	dropErr := fmt.Errorf("request failed after 1 attempt(s): %w", &url.Error{
		Op:  "Post",
		URL: "https://pve2.example.com:8006/api2/json/nodes/pve2/storage/local/upload",
		Err: io.EOF,
	})
	attempts := 0
	var retrySweepHost string
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		attempts++
		if attempts == 1 {
			return "", dropErr
		}
		return "", nil
	}
	sweeps := 0
	storageSvc.deleteVolumeIfExistsFn = func(ctx context.Context, _, _, _ string) (bool, error) {
		sweeps++
		if sweeps == 2 { // the in-loop pre-retry sweep
			retrySweepHost = pve.UploadHostFromContext(ctx)
		}
		return false, nil
	}
	a := newRoutedISOAgent(storageSvc, nil, "pve2.example.com")

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := a.Configure(ctx, "pve2", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2", attempts)
	}
	if retrySweepHost != "pve2.example.com" {
		t.Errorf("pre-retry sweep ctx host = %q, want pve2.example.com (pinned with the upload)", retrySweepHost)
	}
}

func TestConfigDrive_Configure_TLSFailureFallsBackToEndpoint(t *testing.T) {
	t.Parallel()

	attempts := 0
	var hosts []string
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(ctx context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		attempts++
		hosts = append(hosts, pve.UploadHostFromContext(ctx))
		if pve.UploadHostFromContext(ctx) != "" {
			return "", fmt.Errorf("Post %q: %w",
				"https://10.0.0.2:8006/api2/json/nodes/pve2/storage/local/upload",
				x509.HostnameError{Certificate: &x509.Certificate{}, Host: "10.0.0.2"})
		}
		return "", nil
	}
	a := newRoutedISOAgent(storageSvc, nil, "10.0.0.2")

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := a.Configure(ctx, "pve2", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure must succeed via the endpoint fallback: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2 (pinned failure, un-pinned success)", attempts)
	}
	if hosts[0] != "10.0.0.2" || hosts[1] != "" {
		t.Errorf("attempt hosts = %v, want [10.0.0.2 \"\"]", hosts)
	}
}

// TestConfigDrive_Configure_NonVerificationTLSHandshakeFallsBackToEndpoint is
// the F-01 regression: a TLS handshake failure that is NOT certificate
// verification (a non-TLS listener answering the routed port -- ssh, plain
// HTTP pveproxy, an L4 balancer) must still degrade to the proxied path
// instead of failing create_vm permanently. Before the fix neither
// IsTLSCertVerificationFailure nor IsDirectDialFailure matched
// tls.RecordHeaderError, so this upload failed on attempt 0 with no
// fallback and no retry (IsRetryableOrLockFault was also false).
func TestConfigDrive_Configure_NonVerificationTLSHandshakeFallsBackToEndpoint(t *testing.T) {
	t.Parallel()

	attempts := 0
	var hosts []string
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(ctx context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		attempts++
		hosts = append(hosts, pve.UploadHostFromContext(ctx))
		if pve.UploadHostFromContext(ctx) != "" {
			return "", fmt.Errorf("Post %q: %w",
				"https://10.0.0.13:22/api2/json/nodes/pve2/storage/local/upload",
				tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"})
		}
		return "", nil
	}
	a := newRoutedISOAgent(storageSvc, nil, "10.0.0.13:22")

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := a.Configure(ctx, "pve2", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure must succeed via the endpoint fallback: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2 (pinned handshake failure, un-pinned success)", attempts)
	}
	if hosts[0] != "10.0.0.13:22" || hosts[1] != "" {
		t.Errorf("attempt hosts = %v, want [10.0.0.13:22 \"\"]", hosts)
	}
	if _, ok := a.nodeEndpoints.HostFor(context.Background(), "pve2"); ok {
		t.Error("HostFor(pve2) after the fallback: expected the failed route to be memoized away")
	}
}

// TestConfigDrive_Configure_HandshakePhaseReadTimeoutFallsBackToEndpoint is
// the F-02 regression: a read-phase net.OpError (the shape a stalled or
// reset TLS handshake produces once the TCP connect already succeeded) must
// be fallback-eligible on the FIRST attempt, not merely retried pinned to
// the same dead route for the whole budget. Before the fix this shape
// satisfied IsTransientTransport (retryable) but not the fallback predicate,
// so every attempt re-dialed the same unreachable handshake target.
func TestConfigDrive_Configure_HandshakePhaseReadTimeoutFallsBackToEndpoint(t *testing.T) {
	t.Parallel()

	attempts := 0
	var hosts []string
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(ctx context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		attempts++
		hosts = append(hosts, pve.UploadHostFromContext(ctx))
		if pve.UploadHostFromContext(ctx) != "" {
			return "", &url.Error{
				Op:  "Post",
				URL: "https://10.0.0.2:8006/api2/json/nodes/pve2/storage/local/upload",
				Err: &net.OpError{Op: "read", Net: "tcp", Err: routingTimeoutErr{}},
			}
		}
		return "", nil
	}
	a := newRoutedISOAgent(storageSvc, nil, "10.0.0.2")

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := a.Configure(ctx, "pve2", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure must succeed via the endpoint fallback: %v", err)
	}
	// Exactly 2 attempts: falling back on the FIRST pinned failure, not
	// burning the retry budget pinned to the dead route first.
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2 (pinned handshake-phase failure, un-pinned success)", attempts)
	}
	if hosts[0] != "10.0.0.2" || hosts[1] != "" {
		t.Errorf("attempt hosts = %v, want [10.0.0.2 \"\"]", hosts)
	}
}

// routingTimeoutErr implements net.Error with Timeout()==true, the shape
// net.OpError.Timeout() delegates to, without needing a real syscall error.
type routingTimeoutErr struct{}

func (routingTimeoutErr) Error() string   { return "i/o timeout" }
func (routingTimeoutErr) Timeout() bool   { return true }
func (routingTimeoutErr) Temporary() bool { return true }

// routedTasksSvc records the CPI upload-host context stamp per Wait call and
// scripts outcomes by 1-based call count.
type routedTasksSvc struct {
	sdktasks.Service
	hosts  []string
	waitFn func(call int, upid string) (*sdktasks.Status, error)
}

func (s *routedTasksSvc) Wait(ctx context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	s.hosts = append(s.hosts, pve.UploadHostFromContext(ctx))
	if s.waitFn != nil {
		return s.waitFn(len(s.hosts), upid)
	}
	return &sdktasks.Status{ExitStatus: "OK"}, nil
}

// routedTasksClient wires a tasks service into the ConfigDrive fakes.
type routedTasksClient struct {
	*fakePVEClient
	tasksSvc sdktasks.Service
}

func (c *routedTasksClient) Tasks() sdktasks.Service { return c.tasksSvc }

// dialRefusedErr mimics the SDK's typed error for a dial that never
// connected: a ConnectionError whose chain carries *net.OpError{Op: "dial"}.
func dialRefusedErr(host string) error {
	return &sdkerrors.ConnectionError{
		Host: host, Port: 8006, Message: "failed to establish a connection",
		Cause: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
	}
}

func TestConfigDrive_Configure_DialFailureFallsBackToEndpoint(t *testing.T) {
	t.Parallel()

	attempts := 0
	var hosts []string
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(ctx context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		attempts++
		hosts = append(hosts, pve.UploadHostFromContext(ctx))
		if pve.UploadHostFromContext(ctx) != "" {
			return "", dialRefusedErr("10.0.0.2")
		}
		return "", nil
	}
	a := newRoutedISOAgent(storageSvc, nil, "10.0.0.2")

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := a.Configure(ctx, "pve2", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure must succeed via the endpoint fallback: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2 (pinned dial failure, un-pinned success)", attempts)
	}
	if hosts[0] != "10.0.0.2" || hosts[1] != "" {
		t.Errorf("attempt hosts = %v, want [10.0.0.2 \"\"]", hosts)
	}
	// The dead route is memoized: later lookups for the node skip it.
	if _, ok := a.nodeEndpoints.HostFor(context.Background(), "pve2"); ok {
		t.Error("HostFor(pve2) after the fallback: expected the failed route to be memoized away")
	}
}

// TestConfigDrive_Configure_AwaitPhaseFailureReAwaitsRatherThanRePosts covers
// the F1 fix: a dial failure BETWEEN the POST landing and its await resolving
// leaves the upload task's outcome unresolved (IsTaskExitVerdict false), so
// the retry loop's next iteration must re-await that SAME pending UPID
// un-pinned rather than sweep the target name and re-POST. Before the fix
// this case unconditionally swept and re-uploaded — exactly the
// still-running-task double-submit / delete-what-is-being-written hazard F1
// and F6 describe, here on the await-phase fallback path specifically.
func TestConfigDrive_Configure_AwaitPhaseFailureReAwaitsRatherThanRePosts(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve2:00001234:00000000:66aabbcc:imgcopy:local:root@pam:"
	attempts := 0
	var uploadHosts []string
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(ctx context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		attempts++
		uploadHosts = append(uploadHosts, pve.UploadHostFromContext(ctx))
		return upid, nil
	}
	sweeps := 0
	storageSvc.deleteVolumeIfExistsFn = func(_ context.Context, _, _, _ string) (bool, error) {
		sweeps++
		return false, nil
	}
	tasksSvc := &routedTasksSvc{
		waitFn: func(call int, _ string) (*sdktasks.Status, error) {
			if call == 1 {
				// The direct host died between the POST and the await.
				return nil, dialRefusedErr("10.0.0.2")
			}
			return &sdktasks.Status{ExitStatus: "OK"}, nil
		},
	}
	a := newRoutedISOAgent(storageSvc, nil, "10.0.0.2")
	base, _ := a.pveSvc.(*fakePVEClient)
	a.pveSvc = &routedTasksClient{fakePVEClient: base, tasksSvc: tasksSvc}

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := a.Configure(ctx, "pve2", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure must succeed after the un-pinned re-await: %v", err)
	}
	// The POST is never re-submitted: the first (and only) attempt already
	// committed a task, and re-POSTing while its outcome is unresolved would
	// risk double-applying it.
	if attempts != 1 {
		t.Fatalf("upload attempts = %d, want 1 (the pinned POST is never re-submitted)", attempts)
	}
	if uploadHosts[0] != "10.0.0.2" {
		t.Errorf("upload hosts = %v, want [10.0.0.2]", uploadHosts)
	}
	if len(tasksSvc.hosts) != 2 || tasksSvc.hosts[0] != "10.0.0.2" || tasksSvc.hosts[1] != "" {
		t.Errorf("await hosts = %v, want [10.0.0.2 \"\"] (pinned failure, un-pinned re-await of the same UPID)",
			tasksSvc.hosts)
	}
	// No pre-retry sweep: the task never resolved with a failure verdict, so
	// there is no evidence it wrote anything to sweep, and the upload it may
	// still be writing must not be deleted out from under it.
	if sweeps != 1 {
		t.Errorf("sweeps = %d, want 1 (Configure's pre-upload sweep only, no pre-retry sweep)", sweeps)
	}
}

// TestConfigDrive_Configure_UnresolvedPollTimeoutDoesNotDeleteLiveUpload is
// the F1 CRITICAL regression named in the review: the upload POST succeeds
// (a real UPID is returned) and the await gives up with an UNRESOLVED poll
// timeout (the SDK's own internal poll window elapsing, task.go's
// pollTimeoutUnresolved shape) rather than learning the task's outcome.
// Before the fix this shape satisfied IsTransientTransport, so the retry
// loop retried and its pre-retry sweep DELETED the ISO the still-running
// upload task was writing, then re-uploaded into the freed name — corrupting
// or racing a live task. After the fix the shape fails
// IsRetryableOrLockFault, so the loop must stop after the single attempt:
// no pre-retry sweep, no re-upload, and the caller sees a Director-retriable
// error instead of a torn file.
func TestConfigDrive_Configure_UnresolvedPollTimeoutDoesNotDeleteLiveUpload(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve1:00001234:00000000:66aabbcc:imgcopy:local:root@pam:"
	uploads := 0
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		uploads++
		return upid, nil
	}
	preUploadSweeps := 0
	storageSvc.deleteVolumeIfExistsFn = func(_ context.Context, _, _, _ string) (bool, error) {
		preUploadSweeps++
		return false, nil
	}
	tasksSvc := &routedTasksSvc{
		waitFn: func(_ int, _ string) (*sdktasks.Status, error) {
			// Mirrors vendor/.../pkg/api/tasks/tasks.go's waitForInterval: the
			// SDK poller's own derived-context deadline fired mid-poll while
			// the upload task itself may still be running and writing the
			// file.
			return nil, fmt.Errorf("task polling canceled: %w", context.DeadlineExceeded)
		},
	}
	a := newISOAgent(storageSvc, nil)
	base, _ := a.pveSvc.(*fakePVEClient)
	a.pveSvc = &routedTasksClient{fakePVEClient: base, tasksSvc: tasksSvc}

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	err := a.Configure(ctx, "pve1", 200, baseISOConfig())
	if err == nil {
		t.Fatal("an unresolved poll timeout must surface an error, got success")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("an unresolved poll timeout must stay Director-retriable: %v", err)
	}
	if uploads != 1 {
		t.Errorf("upload POSTs = %d, want 1 (never re-submitted while the task is unresolved)", uploads)
	}
	// Exactly one sweep: Configure's own unconditional pre-upload sweep.
	// uploadISO's in-loop pre-retry sweep must never run here -- deleting the
	// name now would delete the file the (possibly still-running) task is
	// writing.
	if preUploadSweeps != 1 {
		t.Errorf("DeleteVolumeIfExists calls = %d, want 1 (Configure's pre-upload sweep only)", preUploadSweeps)
	}
}

// No t.Parallel(): this test mutates the process-global storage-upload retry
// seam, and a parallel sibling reading it mid-override would race.
func TestConfigDrive_Configure_UploadBudgetHonorsOverride(t *testing.T) {
	defer pve.SetStorageUploadRetryForTest(3)()

	dropErr := fmt.Errorf("request failed after 1 attempt(s): %w", &url.Error{
		Op:  "Post",
		URL: "https://pve1.example.io:8006/api2/json/nodes/pve1/storage/local/upload",
		Err: io.EOF,
	})
	attempts := 0
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		attempts++
		return "", dropErr
	}
	a := newISOAgent(storageSvc, nil)

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := a.Configure(ctx, "pve1", 200, baseISOConfig()); err == nil {
		t.Fatal("Configure: expected the exhausted upload to fail")
	}
	if attempts != 3 {
		t.Errorf("upload attempts = %d, want the overridden budget 3", attempts)
	}
}
