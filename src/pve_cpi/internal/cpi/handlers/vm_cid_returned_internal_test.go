package handlers

import (
	"fmt"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// returnedVMID is the VMID a create_vm result's CID names, whether the CID is
// bare or stamped.
func returnedVMID(v any) (int, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("create_vm returned CID %v, which is not a string", v)
	}
	vmid, ok := vmcid.VMID(s)
	if !ok {
		return 0, fmt.Errorf("create_vm returned CID %q, which does not parse", s)
	}
	return vmid, nil
}

// returnedNamesVM reports whether a create_vm result's CID names vmid.
func returnedNamesVM(v any, vmid int) bool {
	got, err := returnedVMID(v)
	return err == nil && got == vmid
}
