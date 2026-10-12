package handlers

import (
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// parseVMCIDArg parses the vm_cid argument of op once, at handler entry. The
// raw string stays available for logs and error text; every PVE call uses the
// returned value's PathID.
func parseVMCIDArg(op, raw string) (vmcid.VMCID, error) {
	cid, err := vmcid.Parse(raw)
	if err != nil {
		return vmcid.VMCID{}, cpierrors.Cloud("%s: vm_cid %q is not a valid VM CID: %s", op, raw, err.Error())
	}
	return cid, nil
}
