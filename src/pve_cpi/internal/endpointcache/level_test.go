package endpointcache

import (
	"log/slog"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func TestLevelFollowsTheConfiguredMode(t *testing.T) {
	for mode, want := range map[config.DiscoveryMode]slog.Level{
		config.DiscoveryModeAuto: log.LevelDebug,
		config.DiscoveryModeOff:  log.LevelDebug,
		"":                       log.LevelDebug,
		config.DiscoveryModeOn:   log.LevelWarn,
	} {
		if got := Level(mode); got != want {
			t.Errorf("Level(%q) = %v, want %v", mode, got, want)
		}
	}
}
