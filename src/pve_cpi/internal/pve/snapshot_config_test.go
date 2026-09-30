package pve_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// snapConfigNodes serves GET /nodes/{node}/qemu/{vmid}/snapshot/{name}/config
// and records the path arguments it was asked for.
type snapConfigNodes struct {
	nodes.Service
	response *nodes.ListQemuSnapshotConfigResponse
	err      error
	asked    []string
}

func (n *snapConfigNodes) ListQemuSnapshotConfig(_ context.Context, node, vmid, name string) (*nodes.ListQemuSnapshotConfigResponse, error) {
	n.asked = append(n.asked, node, vmid, name)
	return n.response, n.err
}

func snapConfigClient(n *snapConfigNodes) pve.Client {
	return &snapMockClient{nodesSvc: n}
}

func TestSnapshotConfigDecodesTheSnapshotSection(t *testing.T) {
	t.Parallel()
	raw := nodes.ListQemuSnapshotConfigResponse(`{"scsi0":"a:123/vm-123-disk-0.qcow2,size=10G","vmstate":"local:123/vm-123-state-pre.raw","snaptime":1700000000}`)
	fake := &snapConfigNodes{response: &raw}
	cfg, err := pve.SnapshotConfig(context.Background(), snapConfigClient(fake), "pve2", 123, "pre")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := pve.ConfigString(cfg, "vmstate"); got != "local:123/vm-123-state-pre.raw" {
		t.Fatalf("vmstate = %q", got)
	}
	if got, _ := pve.ConfigString(cfg, "scsi0"); !strings.HasPrefix(got, "a:123/vm-123-disk-0.qcow2") {
		t.Fatalf("scsi0 = %q", got)
	}
	if strings.Join(fake.asked, "/") != "pve2/123/pre" {
		t.Fatalf("read the wrong snapshot: %v", fake.asked)
	}
}

func TestSnapshotConfigRefusesUnprovableReads(t *testing.T) {
	t.Parallel()
	empty := nodes.ListQemuSnapshotConfigResponse{}
	null := nodes.ListQemuSnapshotConfigResponse(`null`)
	malformed := nodes.ListQemuSnapshotConfigResponse(`["scsi0"]`)
	apiErr := sdkerrors.ParseAPIError(500, []byte(`{"message":"snapshot 'pre' does not exist"}`))
	for _, tc := range []struct {
		name string
		fake *snapConfigNodes
	}{
		{"read failed", &snapConfigNodes{err: apiErr}},
		{"nil response", &snapConfigNodes{}},
		{"empty response", &snapConfigNodes{response: &empty}},
		{"null response", &snapConfigNodes{response: &null}},
		{"not an object", &snapConfigNodes{response: &malformed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := pve.SnapshotConfig(context.Background(), snapConfigClient(tc.fake), "pve2", 123, "pre")
			if err == nil || cfg != nil {
				t.Fatalf("unprovable snapshot read accepted: %v, %v", cfg, err)
			}
		})
	}
}

// TestSnapshotConfigKeepsTheAPIVerdictClassifiable pins that a failed read
// wraps the SDK error, so an audit can describe it without repeating raw text.
func TestSnapshotConfigKeepsTheAPIVerdictClassifiable(t *testing.T) {
	t.Parallel()
	apiErr := sdkerrors.ParseAPIError(500, []byte(`{"message":"snapshot 'pre' does not exist"}`))
	_, err := pve.SnapshotConfig(context.Background(), snapConfigClient(&snapConfigNodes{err: apiErr}), "pve2", 123, "pre")
	if !errors.Is(err, apiErr) {
		t.Fatalf("API error lost from the chain: %v", err)
	}
	if got := pve.DescribeAuditError(err); got != "HTTP 500: snapshot 'pre' does not exist" {
		t.Fatalf("description = %q", got)
	}
}
