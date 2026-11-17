package pve

import (
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// midScanFlipClient builds a two-member cluster whose listings both answer.
// pve1's guest 596 then can't be read, with the 595 of a node that has gone
// down, and the status reports pve1 offline once that read has failed, or
// keeps reporting it online when stayOnline is set. pve2's guest 700 holds
// local-lvm:vm-700-disk-0 when holder is set. The returned function counts the
// config reads of pve1's guests.
func midScanFlipClient(holder, stayOnline bool) (*lgClient, func() int) {
	pve1Reads := 0
	q := &mnQEMU{configFn: func(node string, vmid int) (map[string]any, error) {
		if node == "pve1" {
			pve1Reads++
			return nil, lgProxyUnreachable()
		}
		if holder && vmid == 700 {
			return map[string]any{"scsi0": "local-lvm:vm-700-disk-0,size=8G"}, nil
		}
		return map[string]any{"scsi0": "local-lvm:vm-701-disk-0,size=8G"}, nil
	}}
	c := &lgClient{
		cluster: &lgCluster{
			nodes: []string{"pve1", "pve2"},
			statusFn: func() (*sdkcluster.ListStatusResponse, error) {
				return lgStatus(map[string]bool{"pve1": stayOnline || pve1Reads == 0, "pve2": true})()
			},
		},
		nodes: &lgNodes{listings: map[string][]string{
			"pve1": {`{"vmid": 596}`, `{"vmid": 597}`},
			"pve2": {`{"vmid": 700}`},
		}},
		qemu: q,
	}
	return c, func() int { return pve1Reads }
}

// TestFindVMByDiskVolid_NodeGoingOfflineMidScanDoesNotBlockHolderElsewhere: a
// node that went down after its listing answered is left out once the quorate
// cluster reports it offline, its other guests aren't read, and the holder on
// the online node still settles the answer.
func TestFindVMByDiskVolid_NodeGoingOfflineMidScanDoesNotBlockHolderElsewhere(t *testing.T) {
	t.Parallel()
	c, pve1Reads := midScanFlipClient(true, false)
	vmid, node, err := FindVMByDiskVolid(lgCtx(), c, "local-lvm:vm-700-disk-0")
	if err != nil {
		t.Fatalf("a node going offline mid-scan must not fail a scan whose holder is elsewhere, got: %v", err)
	}
	if vmid != 700 || node != "pve2" {
		t.Fatalf("holder = %d on %q; want 700 on pve2", vmid, node)
	}
	if got := pve1Reads(); got != 1 {
		t.Fatalf("pve1's guests were read %d times; want one read before the node is left out", got)
	}
}

// TestFindVMByDiskVolid_NodeGoingOfflineMidScanLeavesAbsenceUnproven: with no
// holder on the online node, the node left out mid-scan keeps the answer
// unproven, as a node excluded before the scan does.
func TestFindVMByDiskVolid_NodeGoingOfflineMidScanLeavesAbsenceUnproven(t *testing.T) {
	t.Parallel()
	c, _ := midScanFlipClient(false, false)
	_, _, err := FindVMByDiskVolid(lgCtx(), c, "local-lvm:vm-700-disk-0")
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "excluded as offline") || !strings.Contains(err.Error(), "pve1") {
		t.Fatalf("the error must say that pve1 was excluded as offline, got: %v", err)
	}
}

// TestFindVMByDiskVolid_ConfigErrorOnOnlineNodeStillFails: a config read that
// fails on a node the cluster still reports online fails the scan, because a
// fault on the VM that holds the disk must never read as "not attached".
func TestFindVMByDiskVolid_ConfigErrorOnOnlineNodeStillFails(t *testing.T) {
	t.Parallel()
	c, _ := midScanFlipClient(true, true)
	_, _, err := FindVMByDiskVolid(lgCtx(), c, "local-lvm:vm-700-disk-0")
	if err == nil || !strings.Contains(err.Error(), "Config error for vm 596") {
		t.Fatalf("want the config error on the online node, got: %v", err)
	}
}

// TestResolveDiskHolderScan_CreatedVolumeRidesOutANodeLeftOutMidScan: a node
// the cluster begins reporting offline during the scan leaves the answer
// unproven for every volume, except one the request created, which resolves
// as held by nothing.
func TestResolveDiskHolderScan_CreatedVolumeRidesOutANodeLeftOutMidScan(t *testing.T) {
	t.Parallel()
	const volid = "local-lvm:vm-700-disk-0"
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999}

	c, _ := midScanFlipClient(false, false)
	holder, err := resolveDiskHolderScan(lgCtx(), c, nil, volid, cfg, nil, VolumeCreatedByThisRequest(volid))
	if err != nil {
		t.Fatalf("a volume this request created must resolve while a node drops mid-scan, got: %v", err)
	}
	if holder.found {
		t.Fatalf("holder = %+v; want no holder", holder)
	}

	c, _ = midScanFlipClient(false, false)
	_, err = resolveDiskHolderScan(lgCtx(), c, nil, volid, cfg, nil, CreatedVolume{})
	var refusal *DiskHolderOfflineRefusal
	if !errors.As(err, &refusal) || len(refusal.Nodes) != 1 || refusal.Nodes[0] != "pve1" {
		t.Fatalf("without the created record the scan must refuse naming pve1, got: %v", err)
	}
}

// TestResolveDiskHolderScan_CreatedVolumeKeepsAConfigErrorOnAnOnlineNode: the
// created record excuses only nodes the cluster reports offline. A config
// read that fails on a node still reported online fails the scan as before.
func TestResolveDiskHolderScan_CreatedVolumeKeepsAConfigErrorOnAnOnlineNode(t *testing.T) {
	t.Parallel()
	const volid = "local-lvm:vm-700-disk-0"
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999}

	c, _ := midScanFlipClient(false, true)
	_, err := resolveDiskHolderScan(lgCtx(), c, nil, volid, cfg, nil, VolumeCreatedByThisRequest(volid))
	if err == nil || !strings.Contains(err.Error(), "Config error for vm 596") {
		t.Fatalf("want the config error on the online node, got: %v", err)
	}
}

// TestResolveDiskHolderScan_CreatedVolumeStillSeesAHolder: the created record
// never hides a holder the scan found on a node it could read.
func TestResolveDiskHolderScan_CreatedVolumeStillSeesAHolder(t *testing.T) {
	t.Parallel()
	const volid = "local-lvm:vm-700-disk-0"
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999}

	c, _ := midScanFlipClient(true, false)
	holder, err := resolveDiskHolderScan(lgCtx(), c, nil, volid, cfg, nil, VolumeCreatedByThisRequest(volid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !holder.found || holder.vmid != 700 || holder.node != "pve2" {
		t.Fatalf("holder = %+v; want VM 700 on pve2", holder)
	}
}
