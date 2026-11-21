package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestMustRegister_EveryCallSharesOneNodeNotesRead pins that every CPI call
// runs with a node notes cache, so each HA profile and node health read in
// one set_vm_metadata, attach_disk, or other call shares one read of each
// node's notes instead of reading them again.
func TestMustRegister_EveryCallSharesOneNodeNotesRead(t *testing.T) {
	t.Parallel()
	d := cpi.NewDispatcher(log.NewNopLogger())
	var calls, cached int
	for _, method := range []string{"set_vm_metadata", "attach_disk"} {
		mustRegister(d, method, cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			calls++
			if pve.WithNodeNotesCache(ctx) == ctx {
				cached++
			}
			return nil, nil
		}))
	}
	for _, method := range []string{"set_vm_metadata", "attach_disk"} {
		if resp := d.Handle(context.Background(), &jsonrpc.Request{Method: method}); resp.Error != nil {
			t.Fatalf("%s: %+v", method, resp.Error)
		}
	}
	if calls != 2 || cached != 2 {
		t.Fatalf("%d of %d calls ran with a node notes cache, want every call", cached, calls)
	}
}
