package pve

import (
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
)

// TestNodeNameHelperFormatAgreesWithStorageIdentityGrammar guards the two
// node-name checks against drifting apart. The endpoint cache helper applies
// the grammar validStorageIdentityNode applies, plus a 63-character cap, so
// every name within the cap must get the same format verdict from both.
func TestNodeNameHelperFormatAgreesWithStorageIdentityGrammar(t *testing.T) {
	t.Parallel()
	check := func(name string) {
		t.Helper()
		got := !errors.Is(endpointcache.ValidateNodeName(name), endpointcache.ErrNodeNameFormat)
		want := validStorageIdentityNode(name)
		if got != want {
			t.Errorf("format verdict for %q = %v, validStorageIdentityNode = %v", name, got, want)
		}
	}

	alphabet := []string{"a", "Z", "0", "9", "-", ".", "_", " ", "\n", "\x00", "é", "１"}
	names := []string{""}
	for length := 1; length <= 4; length++ {
		var next []string
		for _, prefix := range names {
			if len(prefix) == 0 && length > 1 {
				continue
			}
			for _, c := range alphabet {
				next = append(next, prefix+c)
			}
		}
		for _, name := range next {
			check(name)
		}
		names = next
	}

	for _, name := range []string{
		"", "pve1", "pve-1", "-pve", "pve-", "pve.1", "pve_1", "localhost", "127", "0x7f",
		strings.Repeat("a", 63),
		"a" + strings.Repeat("-", 61) + "a",
	} {
		check(name)
	}

	// The cap is the one place the helper is stricter on purpose.
	long := strings.Repeat("a", endpointcache.MaxNodeNameLength+1)
	if !validStorageIdentityNode(long) || endpointcache.ValidNodeName(long) {
		t.Errorf("a %d-character name should pass only the storage identity grammar", len(long))
	}
}
