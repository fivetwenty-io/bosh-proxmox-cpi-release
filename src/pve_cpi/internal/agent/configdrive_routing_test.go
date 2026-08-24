package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

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

func TestConfigDrive_Configure_AwaitPhaseFailureSkipsInlineRePost(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve2:00001234:00000000:66aabbcc:imgcopy:local:root@pam:"
	attempts := 0
	var uploadHosts []string
	storageSvc := &fakeStorageSvc{}
	storageSvc.uploadFn = func(ctx context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
		attempts++
		uploadHosts = append(uploadHosts, pve.UploadHostFromContext(ctx))
		if attempts == 1 {
			return upid, nil
		}
		return "", nil
	}
	var retrySweepHost string
	sweeps := 0
	storageSvc.deleteVolumeIfExistsFn = func(ctx context.Context, _, _, _ string) (bool, error) {
		sweeps++
		if sweeps == 2 { // the in-loop pre-retry sweep
			retrySweepHost = pve.UploadHostFromContext(ctx)
		}
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
		t.Fatalf("Configure must succeed after the un-pinned retry: %v", err)
	}
	// The await-phase failure must not trigger an inline re-POST: the POST
	// already went through, so the fallback only un-pins and lets the retry
	// loop's pre-retry sweep clear the possibly committed name first.
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2 (one pinned POST, one un-pinned retry)", attempts)
	}
	if uploadHosts[0] != "10.0.0.2" || uploadHosts[1] != "" {
		t.Errorf("upload hosts = %v, want [10.0.0.2 \"\"]", uploadHosts)
	}
	if len(tasksSvc.hosts) < 1 || tasksSvc.hosts[0] != "10.0.0.2" {
		t.Errorf("await hosts = %v, want the first await pinned to 10.0.0.2", tasksSvc.hosts)
	}
	if sweeps != 2 || retrySweepHost != "" {
		t.Errorf("sweeps = %d, retry sweep host = %q; want 2 sweeps with an un-pinned retry sweep", sweeps, retrySweepHost)
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
