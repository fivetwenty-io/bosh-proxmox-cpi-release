package hooks

import (
	"sort"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// Registry maps each built-in hook name to its constructor. Config validation
// (internal/config) rejects any pve.hooks entry absent from this map, and
// cmd/cpi/main.go resolves configured names through it when building the
// dispatcher. Adding a hook means adding one entry here.
var Registry = map[string]func(*log.Logger) cpi.Hook{
	"audit_log": NewAuditLogHook,
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
