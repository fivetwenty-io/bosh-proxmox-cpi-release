package endpointcache

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func TestLevelFollowsTheConfiguredMode(t *testing.T) {
	if got := Level(false); got != log.LevelDebug {
		t.Errorf("Level under auto = %v, want Debug", got)
	}
	if got := Level(true); got != log.LevelWarn {
		t.Errorf("Level under an explicit on = %v, want Warn", got)
	}
}
