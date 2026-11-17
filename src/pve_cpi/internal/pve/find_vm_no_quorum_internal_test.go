package pve

import (
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// TestFindVMAuthoritative_StopsProbingOfflineMemberWithoutQuorum: a lookup on
// a cluster that has lost quorum probes a member the entry node lists offline
// once, not through the whole retry budget, and fails retriably naming the
// lost quorum instead of proving absence.
func TestFindVMAuthoritative_StopsProbingOfflineMemberWithoutQuorum(t *testing.T) {
	t.Parallel()
	q := &mnQEMU{}
	q.configFn = func(node string, _ int) (map[string]any, error) {
		if node == "lab-pve-cpi-az2-0" {
			return nil, lgProxyUnreachable()
		}
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}
	c := &lgClient{
		cluster: &lgCluster{
			nodes: []string{"lab-pve-cpi-az2-0", "lab-pve-cpi-az2-1"},
			statusFn: func() (*sdkcluster.ListStatusResponse, error) {
				return lgStatusQuorate(false, map[string]bool{"lab-pve-cpi-az2-0": false, "lab-pve-cpi-az2-1": true})()
			},
		},
		nodes: &lgNodes{},
		qemu:  q,
	}
	_, err := FindVMAuthoritative(lgCtx(), c, 210534)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the lookup must fail retriably, got: %v", err)
	}
	if !strings.Contains(err.Error(), "member lab-pve-cpi-az2-0 is offline and the entry node does not report the cluster quorate") {
		t.Fatalf("the failure must name the lost quorum, got: %v", err)
	}
	probes := 0
	for _, n := range q.probed {
		if n == "lab-pve-cpi-az2-0" {
			probes++
		}
	}
	if probes != 1 {
		t.Fatalf("lab-pve-cpi-az2-0 got %d probes, want 1", probes)
	}
}
