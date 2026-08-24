package hooks

import (
	"sort"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
)

// Registry maps each built-in hook name to its constructor. Config validation
// (internal/config) rejects any pve.hooks entry absent from this map, and
// cmd/cpi/main.go resolves configured names through it when building the
// dispatcher. Adding a hook means adding one entry here. Each constructor takes
// a Deps so config-driven hooks (lb_register, external_command) read their
// sub-block; observe-only hooks (audit_log) use only Deps.Logger.
var Registry = map[string]func(Deps) cpi.Hook{
	"audit_log":        NewAuditLogHook,
	"notes_audit":      NewNotesAuditHook,
	"lb_register":      NewLBRegisterHook,
	"external_command": NewExternalCommandHook,
}

// Known reports whether name is a registered built-in hook.
func Known(name string) bool {
	_, ok := Registry[name]
	return ok
}

// Names returns the registered hook names in sorted order, for error messages
// that enumerate the valid choices.
func Names() []string {
	names := make([]string, 0, len(Registry))
	for name := range Registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
