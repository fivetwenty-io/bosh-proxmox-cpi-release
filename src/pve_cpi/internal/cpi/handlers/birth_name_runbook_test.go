package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestDiskBirthNameRunbookHeadingExists pins the heading the birth-name
// refusal quotes, so a rename of the section can't leave the pointer dangling.
func TestDiskBirthNameRunbookHeadingExists(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	heading := strings.TrimSuffix(strings.TrimPrefix(pve.DiskBirthNameRunbook, `see "`), `" in docs/troubleshooting.md of bosh-proxmox-cpi-release`)
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "#") && strings.TrimSpace(strings.TrimLeft(line, "#")) == heading {
			return
		}
	}
	t.Fatalf("docs/troubleshooting.md has no heading %q", heading)
}

// TestBirthNameRefusalReachesTheHandlersTyped shows that a handler's wraps
// keep the refusal typed, which has_disk and retention rely on.
func TestBirthNameRefusalReachesTheHandlersTyped(t *testing.T) {
	s := buildBirthNameCollision(t)
	err := callHandler(t, HandleResizeDisk(s.deps), s.cid, 6144)
	held, ok := pve.IsDiskBirthNameHeld(err)
	if !ok {
		t.Fatalf("resize_disk = %v, want a DiskBirthNameHeldError in the chain", err)
	}
	if held.StableID != s.token || held.BirthVolid != birthNameCollision || len(held.Holders) != 1 || held.Holders[0].VMID != 777 {
		t.Fatalf("refusal = %+v", held)
	}
}
