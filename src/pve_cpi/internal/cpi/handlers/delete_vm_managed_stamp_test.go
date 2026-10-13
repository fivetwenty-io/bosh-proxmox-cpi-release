package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const (
	// managedStampCluster is the cluster tag the managed VM carries by
	// default, which the alias makes this cluster's stamp.
	managedStampCluster = "0a1b2c3d"
	managedStampToken   = "4e5f6a7b"
	// managedStampLive is the stamp the fixture's root CA gives, which only
	// the identity read can supply.
	managedStampLive = "11111111"
)

type managedStampDeleteCase struct {
	name    string
	cid     string
	errType cpierrors.Type
	errText string
	deleted bool
	absent  bool
	// stamp is the VM's cluster tag; empty means managedStampCluster. The
	// alias for managedStampCluster is set unless noAlias is.
	stamp     string
	noAlias   bool
	readFails bool
	// closed is the state the record reaches when the VMID is absent and the
	// CID names this cluster, so the managed delete closes it.
	closed aj.State
}

// TestManagedVMDeleteChecksTheStampedCID pins that a stamped CID's cluster and
// token are checked before the allocation journal decides a delete. The
// journal names VM 777 by its VMID alone, so a CID that names another cluster,
// or another VM that once held VMID 777, must never reach the managed delete
// of this cluster's VM. A CID that names another cluster is refused even when
// the VMID is gone from this cluster, because the managed delete would close
// this cluster's record and answer success for a VM that may still run there.
// A CID that names this cluster, by an alias or by the live identity read,
// closes the record of an absent VMID, and a failed identity read is
// retriable.
func TestManagedVMDeleteChecksTheStampedCID(t *testing.T) {
	const cluster, token, live = managedStampCluster, managedStampToken, managedStampLive
	for _, tc := range []managedStampDeleteCase{
		{name: "another cluster is refused", cid: "777-99aabbcc-" + token,
			errType: cpierrors.TypeCloud, errText: "cluster-mismatch"},
		{name: "another token answers VMNotFound", cid: "777-" + cluster + "-11112222",
			errType: cpierrors.TypeVMNotFound},
		{name: "another cluster is refused when the VMID is absent", cid: "777-99aabbcc-" + token, absent: true,
			errType: cpierrors.TypeCloud, errText: "does not exist on this cluster"},
		{name: "the VM's own CID is deleted", cid: "777-" + cluster + "-" + token, deleted: true},
		{name: "an alias stamp closes the record when the VMID is absent", cid: "777-" + cluster + "-" + token,
			absent: true, closed: aj.Deleted},
		{name: "the live stamp closes the record when the VMID is absent", cid: "777-" + live + "-" + token,
			stamp: live, noAlias: true, absent: true, closed: aj.Deleted},
		{name: "a failed identity read is retriable when the VMID is absent", cid: "777-99aabbcc-" + token,
			absent: true, readFails: true, errType: cpierrors.TypeRetriableCloud, errText: "cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) { runManagedStampDeleteCase(t, tc) })
	}
}

func runManagedStampDeleteCase(t *testing.T, tc managedStampDeleteCase) {
	t.Helper()
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	stamp := tc.stamp
	if stamp == "" {
		stamp = managedStampCluster
	}
	// The VM's cluster tag settles the cluster check while the VM is
	// present. The alias makes managedStampCluster this cluster's stamp
	// without an identity read, which an absent VMID needs.
	if !tc.noAlias {
		deps.Config.ClusterStampAliases = []string{managedStampCluster}
	}
	if tc.readFails {
		denied := &sdkerrors.APIError{HTTPCode: 403}
		client.certificateErr = map[string]error{"n1": denied, "n2": denied}
	}
	client.state.configs[777]["tags"] = strings.Join([]string{ownershipTag,
		clusterStampTagPrefix + stamp, cidStampTagPrefix + managedStampToken, tagRetainEphemeral}, ";")
	var absentConfig map[string]any
	if tc.absent {
		absentConfig = client.state.configs[777]
		delete(client.state.configs, 777)
	}

	_, err := HandleDeleteVM(deps).Handle(context.Background(),
		[]json.RawMessage{planJSON(t, tc.cid)}, jsonrpc.Context{})
	after, inspectErr := journal.Inspect(vmID)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	switch {
	case tc.closed != "":
		if err != nil {
			t.Fatalf("delete_vm %s on an absent VMID: %v", tc.cid, err)
		}
		if after.State != tc.closed {
			t.Fatalf("delete_vm %s on an absent VMID left the record %s, want %s", tc.cid, after.State, tc.closed)
		}
	case tc.deleted:
		if err != nil {
			t.Fatalf("delete_vm %s: %v", tc.cid, err)
		}
		if client.state.configs[777] != nil || after.State != aj.VMDeletedRetained {
			t.Fatalf("delete_vm %s left the VM or its record: state=%s", tc.cid, after.State)
		}
	default:
		if err == nil || !cpierrors.IsType(err, tc.errType) || !strings.Contains(err.Error(), tc.errText) {
			t.Fatalf("delete_vm %s: err = %v, want a %s error containing %q", tc.cid, err, tc.errType, tc.errText)
		}
		if tc.absent {
			client.state.configs[777] = absentConfig
		}
		if client.state.configs[777] == nil || client.moves != 0 || after.State != aj.ReadyToReturn {
			t.Fatalf("delete_vm %s touched VM 777: config present=%v moves=%d state=%s",
				tc.cid, client.state.configs[777] != nil, client.moves, after.State)
		}
	}
}
