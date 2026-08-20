package config_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// TestParkedAnchorStrictValue covers the tri-state resolution of
// pve.parked_anchor_strict: unset means strict (true), and only an explicit
// false disarms the invariant. A nil receiver is strict too, matching every
// other accessor's nil-safety.
func TestParkedAnchorStrictValue(t *testing.T) {
	t.Parallel()

	if !(*config.CPIConfig)(nil).ParkedAnchorStrictValue() {
		t.Error("nil receiver must resolve strict (true)")
	}
	if !(&config.CPIConfig{}).ParkedAnchorStrictValue() {
		t.Error("unset field must resolve strict (true)")
	}
	on := true
	if !(&config.CPIConfig{ParkedAnchorStrict: &on}).ParkedAnchorStrictValue() {
		t.Error("explicit true must resolve strict")
	}
	off := false
	if (&config.CPIConfig{ParkedAnchorStrict: &off}).ParkedAnchorStrictValue() {
		t.Error("explicit false must disarm the invariant")
	}
}
