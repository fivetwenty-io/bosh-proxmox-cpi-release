package handlers

import (
	"slices"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// configWriteParams returns the parameters of a guarded config write, or nil
// for any other call.
func configWriteParams(call ManagedAllocationMutation) *sdknodes.UpdateQemuConfigParams {
	if call.Service != managedServiceNodes || call.Method != "UpdateQemuConfig" {
		return nil
	}
	params, _ := call.Args[managedArgumentParams].(*sdknodes.UpdateQemuConfigParams)
	return params
}

// configWriteDeletesOrReverts reports whether a guarded config write deletes or
// reverts a key, which are the only writes whose admission and observation
// need the holder's pending view.
func configWriteDeletesOrReverts(call ManagedAllocationMutation) bool {
	params := configWriteParams(call)
	return params != nil && (params.Delete != nil || params.Revert != nil)
}

// configWriteDeletedSlots returns the keys a guarded config write deletes when
// the write deletes bus slots and does nothing else but carry a digest, which
// is the write the pending-delete helper sends. It returns nil for any other
// write.
func configWriteDeletedSlots(call ManagedAllocationMutation) []string {
	params := configWriteParams(call)
	if params == nil || params.Delete == nil {
		return nil
	}
	rest := *params
	rest.Delete, rest.Digest = nil, nil
	fields, err := lifecycleMutationFields(&rest)
	if err != nil || len(fields) != 0 {
		return nil
	}
	keys := strings.Split(*params.Delete, ",")
	for _, key := range keys {
		if !isDiskOptionKey(key) || strings.HasPrefix(key, "unused") {
			return nil
		}
	}
	return keys
}

// pendingDeleteOnlyChange reports whether views differ from before only by a
// pending delete of each key in deleted. Every key before held is still in the
// current view with the same value, the current view has no key before
// didn't, no key carries a pending value, and the keys whose delete is pending
// are exactly the deleted ones. The digest is left out, because writing the
// pending section changes it.
func pendingDeleteOnlyChange(before map[string]any, views pve.QemuViews, deleted []string) bool {
	values, deletes := views.PendingChanges()
	want := slices.Clone(deleted)
	slices.Sort(want)
	if len(values) != 0 || !slices.Equal(deletes, want) {
		return false
	}
	current := views.Current()
	for key := range before {
		if key == pveConfigKeyDigest {
			continue
		}
		was, _ := pve.ConfigString(before, key)
		now, present := pve.ConfigString(current, key)
		if !present || was != now {
			return false
		}
	}
	for key := range current {
		if _, present := before[key]; !present && key != pveConfigKeyDigest {
			return false
		}
	}
	return true
}
