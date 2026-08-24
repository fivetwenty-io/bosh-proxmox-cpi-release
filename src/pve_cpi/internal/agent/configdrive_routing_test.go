package agent

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
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
