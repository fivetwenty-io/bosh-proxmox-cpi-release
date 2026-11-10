package endpointcache

import (
	"log/slog"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// Level returns the level of a cache or discovery line that the failure
// handling rules give Debug. Under the default auto mode the line stays at
// Debug, so an upgrade adds no noise. Under an explicit "on" it becomes Warn,
// because the operator asked for something they are not getting. explicitOn
// is whether the configured mode, not the effective one, is "on". A line the
// rules keep at Debug whatever the mode, such as the missing directory beside
// the journal, logs at Debug directly and does not call Level.
func Level(explicitOn bool) slog.Level {
	if explicitOn {
		return log.LevelWarn
	}
	return log.LevelDebug
}
