package handlers

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

type nfsDeleteConvergenceClient struct {
	pve.Client
	reader           *nfsDeleteConvergenceNodes
	visibilityChecks int
}

func (c *nfsDeleteConvergenceClient) Nodes() nodes.Service { return c.reader }
func (c *nfsDeleteConvergenceClient) StorageAuditVisibility(context.Context) error {
	c.visibilityChecks++
	return nil
}

type nfsDeleteConvergenceNodes struct {
	nodes.Service
	started      time.Time
	recoverAfter time.Duration
	calls        int
	wrongTarget  bool
}

func (n *nfsDeleteConvergenceNodes) ListStorageContent(_ context.Context, node, storage string, _ *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	n.calls++
	if node != "lab-pve-cpi-0" || storage != "a" {
		n.wrongTarget = true
	}
	if n.recoverAfter == 0 || time.Since(n.started) < n.recoverAfter {
		return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "failed to create /mnt/pve/a/template/iso: File exists"}
	}
	listing := nodes.ListStorageContentResponse{}
	return &listing, nil
}
func TestManagedVMDeleteNFSDirectoryConvergence(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		recoverAfter, parentLimit, elapsed time.Duration
		observed                           bool
	}{
		{name: "default directory cache expires after sixty seconds", recoverAfter: 60 * time.Second, elapsed: 60 * time.Second, observed: true},
		{name: "persistent listing failure stops at ninety seconds", elapsed: 90 * time.Second},
		{name: "shorter caller deadline remains authoritative", parentLimit: 15 * time.Second, elapsed: 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				testNFSDeleteConvergence(t, tc.recoverAfter, tc.parentLimit, tc.elapsed, tc.observed)
			})
		})
	}
}
func testNFSDeleteConvergence(t *testing.T, recovery, parentLimit, elapsed time.Duration, observed bool) {
	t.Helper()
	deps, journal, _, record := deleteManagedFixture(t)
	handle, err := journal.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	admitVMDeleteObservationTest(t, deps, journal, handle)
	original := handle.Record().Steps
	start := time.Now()
	reader := &nfsDeleteConvergenceNodes{Service: deps.PVE.Nodes(), started: start, recoverAfter: recovery}
	client := &nfsDeleteConvergenceClient{Client: deps.PVE, reader: reader}
	deps.PVE = client
	ctx := t.Context()
	if parentLimit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, parentLimit)
		defer cancel()
	}
	submissions := 0
	const upid = "UPID:lab-pve-cpi-0:iso-delete"
	target := aj.Target{Node: "lab-pve-cpi-0", Storage: "a", IntendedVolume: cleanupTestISO}
	err = managedVMDeleteTask(ctx, deps, handle, "volume", target, func() (any, error) {
		submissions++
		return upid, nil
	}, func() error { return awaitManagedVMVolumeAbsence(ctx, deps, target.Node, target.IntendedVolume) })
	after := handle.Record()
	if submissions != 1 || reader.wrongTarget || reader.calls < 2 || !reflect.DeepEqual(original, after.Steps[:len(original)]) {
		t.Fatalf("mutation replay, changed target, or rewritten history: submissions=%d calls=%d error=%v", submissions, reader.calls, err)
	}
	if got := time.Since(start); got != elapsed {
		t.Fatalf("observation elapsed %v, want %v: %v", got, elapsed, err)
	}
	last := after.Steps[len(after.Steps)-1]
	if observed {
		if err != nil || last.State != aj.Observed || client.visibilityChecks != 1 {
			t.Fatalf("complete absence not observed: %v state=%s visibility=%d", err, last.State, client.visibilityChecks)
		}
	} else {
		if !errors.Is(err, context.DeadlineExceeded) || last.State != aj.Submitted || last.UPID != upid || client.visibilityChecks != 0 {
			t.Fatalf("uncertainty lost: %v state=%s upid=%s", err, last.State, last.UPID)
		}
		if !strings.Contains(err.Error(), "listing_http_500") || strings.Contains(err.Error(), "/mnt/pve") {
			t.Fatalf("unsafe or missing listing classification: %v", err)
		}
	}
}
