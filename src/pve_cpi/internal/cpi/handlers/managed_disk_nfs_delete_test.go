package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
)

type nfsDeletePVE struct{ *lifecycleFlowPVE }

func (c nfsDeletePVE) Storage() storage.Service {
	return nfsDeleteStorage{lifecycleFlowStorage{managedDiskTestStorage: managedDiskTestStorage{state: c.state}, c: c.lifecycleFlowPVE}}
}

type nfsDeleteStorage struct{ lifecycleFlowStorage }

func (s nfsDeleteStorage) Exists(ctx context.Context, node, pool, volume string) (bool, error) {
	if s.c.state.volumes[volume] == nil {
		return false, errors.New("volume_size_info failed - no format")
	}
	return s.lifecycleFlowStorage.Exists(ctx, node, pool, volume)
}

func TestManagedDiskNFSDeleteUsesAuthoritativeAbsence(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	delete(client.state.configs[777], "scsi1")
	deps.PVE = nfsDeletePVE{client}
	args := []json.RawMessage{planJSON(t, cid)}
	if _, err := HandleDeleteDisk(deps).Handle(t.Context(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("completed NFS deletion was not observed: %v", err)
	}
	record, err := journal.Inspect(id)
	if err != nil || record.State != aj.Deleted || client.deletes != 1 || len(client.state.volumes) != 0 {
		t.Fatalf("deletion lacks terminal physical evidence: %v %+v", err, record)
	}
	if record.Steps[1].State != aj.Observed || record.Steps[1].UPID == "" {
		t.Fatal("successful deletion lost task or observed outcome")
	}
	if _, err := HandleDeleteDisk(deps).Handle(t.Context(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("terminal NFS disk retry queried its absent image: %v", err)
	}
	if client.deletes != 1 {
		t.Fatal("terminal retry submitted another deletion")
	}
}

func TestManagedDiskHasDiskUsesVerifiedMembership(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	delete(client.state.configs[777], "scsi1")
	deps.PVE = nfsDeletePVE{client}
	args := []json.RawMessage{planJSON(t, cid)}
	value, err := HandleHasDisk(deps).Handle(t.Context(), args, jsonrpc.Context{})
	if err != nil || value != true {
		t.Fatalf("present owned managed disk not found: %v %v", value, err)
	}
	if _, err := HandleDeleteDisk(deps).Handle(t.Context(), args, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	value, err = HandleHasDisk(deps).Handle(t.Context(), args, jsonrpc.Context{})
	if err != nil || value != false {
		t.Fatalf("terminal NFS absence not preserved: %v %v", value, err)
	}
	client.visibilityErr = errors.New("incomplete visibility")
	if _, err := HandleHasDisk(deps).Handle(t.Context(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("unknown absence accepted")
	}
}
