package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// TestManagedVMDeleteChecksTheStampedCID pins that a stamped CID's cluster and
// token are checked before the allocation journal decides a delete. The
// journal names VM 777 by its VMID alone, so a CID that names another cluster,
// or another VM that once held VMID 777, must never reach the managed delete
// of this cluster's VM. A CID that names another cluster is refused even when
// the VMID is gone from this cluster, because the managed delete would close
// this cluster's record and answer success for a VM that may still run there.
func TestManagedVMDeleteChecksTheStampedCID(t *testing.T) {
	const (
		cluster = "0a1b2c3d"
		token   = "4e5f6a7b"
	)
	for _, tc := range []struct {
		name    string
		cid     string
		errType cpierrors.Type
		errText string
		deleted bool
		absent  bool
	}{
		{name: "another cluster is refused", cid: "777-99aabbcc-" + token,
			errType: cpierrors.TypeCloud, errText: "cluster-mismatch"},
		{name: "another token answers VMNotFound", cid: "777-" + cluster + "-11112222",
			errType: cpierrors.TypeVMNotFound},
		{name: "another cluster is refused when the VMID is absent", cid: "777-99aabbcc-" + token, absent: true,
			errType: cpierrors.TypeCloud, errText: "does not exist on this cluster"},
		{name: "the VM's own CID is deleted", cid: "777-" + cluster + "-" + token, deleted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, client, journal, vmID, _ := retainDeleteFixture(t)
			// The VM's cluster tag settles the cluster check, and the alias
			// makes the stamp this cluster's without an identity read.
			deps.Config.ClusterStampAliases = []string{cluster}
			client.state.configs[777]["tags"] = strings.Join([]string{ownershipTag,
				clusterStampTagPrefix + cluster, cidStampTagPrefix + token, tagRetainEphemeral}, ";")
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
			if tc.deleted {
				if err != nil {
					t.Fatalf("delete_vm %s: %v", tc.cid, err)
				}
				if client.state.configs[777] != nil || after.State != aj.VMDeletedRetained {
					t.Fatalf("delete_vm %s left the VM or its record: state=%s", tc.cid, after.State)
				}
				return
			}
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
		})
	}
}
