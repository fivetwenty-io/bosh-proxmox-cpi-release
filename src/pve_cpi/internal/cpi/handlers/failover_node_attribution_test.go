package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// A 595 from pveproxy says the node in the request path is unreachable. Every
// entry answers it the same way, so it says nothing about the entry that
// relayed it. These tests send reads that the dead-node delete and the disk
// motion handlers address to another node, answer 595 on every pool server,
// and check that the request never moves to a second server and that the entry
// stays first for the next request.
//
// Each pool entry knows the node it serves, so the equality check of the
// attribution rule runs. A 595 for a node that is none of the entries' own is
// unattributable, and a 595 for an entry's own node is attributable.

const (
	fwFirstNode     = "pve01"
	fwSecondNode    = "pve03"
	fwDeadNode      = "pve-dead"
	fw595Reason     = "hostname lookup failed - no route to host"
	fwStatusProxy   = 595
	fwReplicaNode   = "pve02"
	fwReplicaVMID   = 100
	fwReplicaVolume = "local-zfs:vm-100-disk-0"
)

// fwNodeRoutes answers every request below /nodes/<node>/ with a 595, and a
// pool read with a claim, which stands for any other read.
func fwNodeRoutes(node string) *pvetest.Routes {
	routes := pvetest.NewRoutes()
	routes.Fail(http.MethodGet, "/nodes/"+node+"/", fwStatusProxy, fw595Reason)
	routes.JSON(http.MethodGet, "/pools/", http.StatusOK, map[string]any{"comment": fwClaim, "members": []any{}})
	return routes
}

// fwNameEntries gives the pool entries at the two servers' addresses the node
// names fwFirstNode and fwSecondNode, for the clients built afterwards.
func fwNameEntries(t *testing.T, a, b *pvetest.Server) {
	t.Helper()
	t.Cleanup(pve.SetEndpointNodesForTest(map[string]string{a.Addr: fwFirstNode, b.Addr: fwSecondNode}))
}

// fwAssertEntryStaysFirst checks that the pool's second server read nothing and
// that the next ordinary read is answered by the first, which a demoted first
// entry would not be.
func fwAssertEntryStaysFirst(t *testing.T, client pve.Client, a, b *pvetest.Server) {
	t.Helper()
	before := fwGets(a)
	comment, found, err := client.Pools().GetPoolComment(fwCtx(t), "p1")
	if err != nil || !found || comment != fwClaim {
		t.Fatalf("the next read = %q, %v, %v, want the first server's answer", comment, found, err)
	}
	if fwGets(a) != before+1 {
		t.Errorf("the first server read %d GETs for the next read, want 1", fwGets(a)-before)
	}
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Errorf("the second server read %d requests, want 0 (the 595 moved a request or demoted the entry)", n)
	}
}

// The disk-motion handler asks the node a VM is on for its config to learn
// whether a reassignment already landed. When that node is down, the answer is
// a 595 from every entry. The read stays on the first server and surfaces the
// 595 to its caller.
func TestDiskMotionConfigReadOfADeadNodeNeverMovesTheRequest(t *testing.T) {
	lab := newFWLab(t)
	a := lab.live(t, fwNodeRoutes(fwDeadNode))
	b := lab.live(t, fwNodeRoutes(fwDeadNode))
	fwNameEntries(t, a, b)
	client := fwClient(t, a.Addr, b.Addr)
	deps := Deps{PVE: client}

	_, found, err := transferredVolidOn(fwCtx(t), deps, fwDeadNode, 7001, "disk-stable-id")
	if err == nil || found {
		t.Fatalf("transferredVolidOn = found %v, err %v, want the 595", found, err)
	}
	if !strings.Contains(err.Error(), fw595Reason) {
		t.Errorf("error = %v, want it to carry the proxy's reason %q", err, fw595Reason)
	}
	if fwGets(a) == 0 {
		t.Error("the first server never read the request")
	}
	fwAssertEntryStaysFirst(t, client, a, b)
}

// The dead-node delete lists the replica partner's storage to learn which
// volumes a verified copy covers. When the partner is down too, the listing is
// a 595. The retry helper around it replays the read, and every replay stays on
// the first server.
func TestDeadNodeDeleteReplicaListingOfADeadNodeNeverMovesTheRequest(t *testing.T) {
	rules, err := json.Marshal(map[string]any{"data": []map[string]any{{
		"rule":      naRuleNameFor(fwReplicaVMID),
		"type":      naRuleType,
		"resources": "vm:100",
		"nodes":     "pve01," + fwReplicaNode,
		"strict":    1,
		"comment": ha.WithReplicaPartner(
			ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(), []string{"local-zfs"}), fwReplicaNode),
	}}})
	if err != nil {
		t.Fatalf("marshal rules: %v", err)
	}
	routes := func() *pvetest.Routes {
		r := fwNodeRoutes(fwReplicaNode)
		r.HandleFunc(http.MethodGet, "/cluster/ha/rules", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(rules)
		})
		return r
	}
	lab := newFWLab(t)
	a, b := lab.live(t, routes()), lab.live(t, routes())
	fwNameEntries(t, a, b)
	client := fwClient(t, a.Addr, b.Addr)
	deps := Deps{PVE: client, RequestDirectorUUID: aaDirector}
	storages := []pve.StorageInfo{{Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images"}}
	live := map[string]any{"scsi0": fwReplicaVolume + ",size=10G"}

	got, err := deadNodeReplicaCopies(fwCtx(t), deps, fwReplicaVMID, fwReplicaNode, storages, live, nil)
	if err == nil {
		t.Fatalf("deadNodeReplicaCopies = %v, want the refusal for the partner's listing", got)
	}
	if !strings.Contains(err.Error(), "listing unavailable") {
		t.Errorf("error = %v, want the listing-unavailable refusal", err)
	}
	if fwGets(a) < 2 {
		t.Errorf("the first server read %d GETs, want the rule read and the listing", fwGets(a))
	}
	fwAssertEntryStaysFirst(t, client, a, b)
}

// A 595 for a path that names no node is the entry's own trouble, and a read
// hops to the next server. This is the contrast that makes the two tests above
// meaningful, because it shows the harness sees a hop when one happens.
func TestNodeAttributionAnEntryCausedStatusStillHopsAGet(t *testing.T) {
	lab := newFWLab(t)
	aRoutes := fwNodeRoutes(fwDeadNode)
	aRoutes.Fail(http.MethodGet, "/pools/", fwStatusProxy, fw595Reason)
	a := lab.live(t, aRoutes)
	b := lab.live(t, fwNodeRoutes(fwDeadNode))
	fwNameEntries(t, a, b)
	client := fwClient(t, a.Addr, b.Addr)

	comment, found, err := client.Pools().GetPoolComment(fwCtx(t), "p1")
	if err != nil || !found || comment != fwClaim {
		t.Fatalf("read = %q, %v, %v, want the second server's answer", comment, found, err)
	}
	if fwGets(b) != 1 {
		t.Errorf("the second server read %d GETs, want the one hop", fwGets(b))
	}
}

// A 595 for the node the entry itself serves is the entry's own trouble, so a
// read hops to the next server. Together with the dead-node tests, which use a
// node that is no entry's own, this shows the rule compares the path's node with
// the entry's node and does not blame every entry that knows its node.
func TestNodeAttributionA595ForTheEntrysOwnNodeStillHopsAGet(t *testing.T) {
	lab := newFWLab(t)
	a := lab.live(t, fwNodeRoutes(fwFirstNode))
	b := lab.live(t, nil)
	fwNameEntries(t, a, b)
	client := fwClient(t, a.Addr, b.Addr)

	_, err := client.Nodes().ListStorageContent(fwCtx(t), fwFirstNode, "local", nil)
	if err != nil {
		t.Fatalf("ListStorageContent: %v", err)
	}
	if fwGets(b) != 1 {
		t.Errorf(
			"the second server read %d GETs, want the one hop that a 595 for the entry's own node causes",
			fwGets(b),
		)
	}
}
