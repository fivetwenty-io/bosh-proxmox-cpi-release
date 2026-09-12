package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

type isoReadbackNodes struct {
	presenceNodes
	detail *nodes.GetStorageContentResponse
}

func (n isoReadbackNodes) GetStorageContent(context.Context, string, string, string) (*nodes.GetStorageContentResponse, error) {
	return n.detail, nil
}

func TestManagedISOReadbackUsesContentIdentity(t *testing.T) {
	const volume = "nfs:iso/vm-8009-config.iso"
	const size = uint64(10485760)
	for _, tc := range []struct {
		name         string
		change       func(map[string]any)
		detailFormat string
		detailSize   uint64
		listErr      error
		extra        string
		wantError    bool
	}{
		{name: "real PVE raw detail and ISO listing"},
		{name: "ISO detail compatibility", detailFormat: "iso"},
		{name: "qcow2 detail", detailFormat: "qcow2", wantError: true},
		{name: "detail size changed", detailSize: size + 1, wantError: true},
		{name: "wrong content", change: func(r map[string]any) { r["content"] = "images" }, wantError: true},
		{name: "missing content", change: func(r map[string]any) { delete(r, "content") }, wantError: true},
		{name: "wrong listed format", change: func(r map[string]any) { r["format"] = "raw" }, wantError: true},
		{name: "wrong listed size", change: func(r map[string]any) { r["size"] = size + 1 }, wantError: true},
		{name: "negative size", change: func(r map[string]any) { r["size"] = -1 }, wantError: true},
		{name: "unknown file change time", change: func(r map[string]any) { delete(r, "ctime") }, wantError: true},
		{name: "different artifact", change: func(r map[string]any) { r["volid"] = "nfs:iso/other.iso" }, wantError: true},
		{name: "permission failure", listErr: errors.New("denied"), wantError: true},
		{name: "malformed trailing row", extra: "{}", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := map[string]any{"volid": volume, "content": "iso", "format": "iso", "size": size, "ctime": 1788969753}
			if tc.change != nil {
				tc.change(row)
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			listing := nodes.ListStorageContentResponse{encoded}
			if tc.extra != "" {
				listing = append(listing, json.RawMessage(tc.extra))
			}
			format := tc.detailFormat
			if format == "" {
				format = "raw"
			}
			detailSize := tc.detailSize
			if detailSize == 0 {
				detailSize = size
			}
			n := isoReadbackNodes{presenceNodes: presenceNodes{listing: &listing, err: tc.listErr}, detail: &nodes.GetStorageContentResponse{Format: format, Size: sdkclient.PVEInt(detailSize)}}
			m := managedVMAllocation{deps: Deps{PVE: presencePVE{nodeClient: n}}}
			got, err := m.observeTargetVolume(t.Context(), StoragePlanTarget{Role: storageRoleISO, Node: "pve1", StorageID: "nfs", VirtualBytes: size}, volume, 1)
			if (err != nil) != tc.wantError {
				t.Fatalf("size=%d error=%v, want rejection=%v", got, err, tc.wantError)
			}
			if err == nil && got != size {
				t.Fatal("wrong verified size")
			}
			if err == nil {
				evidence, evidenceErr := pve.ObserveStorageISOContent(t.Context(), m.deps.PVE, "pve1", volume, size)
				if evidenceErr != nil || evidence == nil || evidence.Size != size || evidence.CTime != 1788969753 {
					t.Fatalf("ISO task correlation evidence missing: %+v %v", evidence, evidenceErr)
				}
			}
		})
	}
}
