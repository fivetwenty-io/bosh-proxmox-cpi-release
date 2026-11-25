package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStorageVMExecution_HAOffFalseEncodesAsBeforeTheField pins the bytes of an
// execution whose VM keeps HA. A release that predates HAOff decodes plans
// strictly and refuses an unknown key, so a false HAOff must leave the encoding
// identical to the one written before the field existed. The golden string
// lists the field set of that release in its declaration order.
func TestStorageVMExecution_HAOffFalseEncodesAsBeforeTheField(t *testing.T) {
	t.Parallel()
	balloon := 512
	e := StorageVMExecution{
		Version: 1, InputFingerprint: "fp", Node: "pve01", Storage: "s1", StorageType: "nfs",
		DiskFormat: "qcow2", RootGiB: 20, MaxAttempts: 2, Cores: 2, Sockets: 1, MemoryMiB: 2048,
		Hotplug: "disk", NUMA: true, Tags: "a;b", InitialName: "vm-1",
		RootOptions: map[string]string{"discard": "on"}, RootDevice: "scsi0", SCSIController: "virtio-scsi-single",
		EphemeralGiB: 10, EphemeralStorage: "s2", CPUType: "host", BalloonMiB: &balloon,
		Pool: "p", PoolComment: "c", PoolLayer: "l", PoolDirector: "d", PoolDeployment: "dep", PoolInstanceGroup: "ig",
	}
	const golden = `{"Version":1,"InputFingerprint":"fp","Node":"pve01","Storage":"s1","StorageType":"nfs",` +
		`"DiskFormat":"qcow2","RootGiB":20,"MaxAttempts":2,"Cores":2,"Sockets":1,"MemoryMiB":2048,` +
		`"Hotplug":"disk","NUMA":true,"Tags":"a;b","InitialName":"vm-1","RootOptions":{"discard":"on"},` +
		`"RootDevice":"scsi0","SCSIController":"virtio-scsi-single","EphemeralGiB":10,"EphemeralStorage":"s2",` +
		`"CPUType":"host","BalloonMiB":512,"Pool":"p","PoolComment":"c","PoolLayer":"l","PoolDirector":"d",` +
		`"PoolDeployment":"dep","PoolInstanceGroup":"ig"}`
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != golden {
		t.Fatalf("encoding changed:\n got %s\nwant %s", raw, golden)
	}
	if strings.Contains(string(raw), "HAOff") {
		t.Fatalf("a false HAOff reached the encoding: %s", raw)
	}
	e.HAOff = true
	raw, err = json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), `,"HAOff":true}`) {
		t.Fatalf("a true HAOff is missing from %s", raw)
	}
}
