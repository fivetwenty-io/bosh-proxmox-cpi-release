package endpointcache

import (
	"log/slog"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// Level returns the level of a cache or discovery line that the failure
// handling rules give Debug. Under the default auto mode the line stays at
// Debug, so an upgrade adds no noise. Under an explicit "on" it becomes Warn,
// because the operator asked for something they are not getting. configured
// is the configured mode from CPIConfig.DiscoveryModeValue, not the effective
// one, which is "on" in the ordinary auto case of one endpoint and a token.
// Every mode but DiscoveryModeOn, including the empty mode, gives Debug. A
// line the rules keep at Debug whatever the mode, such as the missing
// directory beside the journal, logs at Debug directly and does not call
// Level.
func Level(configured config.DiscoveryMode) slog.Level {
	if configured == config.DiscoveryModeOn {
		return log.LevelWarn
	}
	return log.LevelDebug
}
