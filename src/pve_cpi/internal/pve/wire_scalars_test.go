package pve_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// These tests feed the hand-decoded /cluster/status, SDN, and storage rows the
// scalar shapes PVE has actually been seen to emit (JSON booleans, quoted
// integers, quoted flags) rather than the integer shape the spec documents. A
// plain int64 field rejects the row, and every one of these loops used to
// treat a rejected row as absent, so a whole node, zone, or pool silently
// dropped out of the result.

func TestClusterNodePeerIPs_OnlineWireShapes(t *testing.T) {
	t.Parallel()
	c := newPeersClient(
		json.RawMessage(`{"type":"node","name":"a","ip":"10.0.0.1","online":1}`),
		json.RawMessage(`{"type":"node","name":"b","ip":"10.0.0.2","online":true}`),
		json.RawMessage(`{"type":"node","name":"c","ip":"10.0.0.3","online":"1"}`),
		json.RawMessage(`{"type":"node","name":"d","ip":"10.0.0.4","online":0}`),
		json.RawMessage(`{"type":"node","name":"e","ip":"10.0.0.5","online":false}`),
		json.RawMessage(`{"type":"node","name":"f","ip":"10.0.0.6","online":"0"}`),
	)

	peers, err := pve.ClusterNodePeerIPs(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if !reflect.DeepEqual(peers, want) {
		t.Errorf("peers = %v, want %v", peers, want)
	}

	addrs, err := pve.ClusterNodeAddressMap(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantAddrs := map[string]string{"a": "10.0.0.1", "b": "10.0.0.2", "c": "10.0.0.3"}
	if !reflect.DeepEqual(addrs, wantAddrs) {
		t.Errorf("addrs = %v, want %v", addrs, wantAddrs)
	}
}

func TestNextVNI_StringEncodedTagsAreStillExcluded(t *testing.T) {
	t.Parallel()
	c := newVNIClientWithZones(
		[]json.RawMessage{json.RawMessage(`{"vnet":"bosh1","zone":"z","tag":"5000","vlanaware":"1"}`)},
		[]json.RawMessage{json.RawMessage(`{"zone":"evpn1","type":"evpn","vrf-vxlan":"5001"}`)},
	)

	vni, err := pve.NextVNI(context.Background(), c, 5000, 5002, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vni != 5002 {
		t.Fatalf("VNI = %d, want 5002 (5000 is a vnet tag and 5001 a zone vrf-vxlan, both quoted on the wire)", vni)
	}
}

func TestNextVNI_UndecodableZoneRow_WarnsAndKeepsOtherZones(t *testing.T) {
	t.Parallel()
	c := newVNIClientWithZones(
		nil,
		[]json.RawMessage{
			json.RawMessage(`{"zone":"broken","type":"evpn","vrf-vxlan":{"nested":true}}`),
			zoneRow("evpn1", 5001, 0),
		},
	)
	obsLogger, obs := log.NewObservedLogger(log.LevelWarn)

	vni, err := pve.NextVNI(context.Background(), c, 5000, 5001, obsLogger)
	if err != nil {
		t.Fatalf("a bad zone row must not fail allocation: %v", err)
	}
	if vni != 5000 {
		t.Fatalf("VNI = %d, want 5000 (5001 is reserved by the zone that did decode)", vni)
	}
	var warned bool
	for _, e := range obs.All() {
		if e.Level != log.LevelWarn {
			continue
		}
		if e.Attrs["zone"] == "broken" {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a Warn naming zone %q, got %+v", "broken", obs.All())
	}
}
