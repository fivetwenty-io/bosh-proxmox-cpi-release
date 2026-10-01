package handlers

import (
	"context"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// TestLegacyUnusedSoleHolderFailsWhenANodeCannotBeListed calls the rule
// directly on the same cluster, so its own reference proof meets the
// unlisted member. The strict listing fails retriable rather than answer for
// part of the cluster.
func TestLegacyUnusedSoleHolderFailsWhenANodeCannotBeListed(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, _, _ := legacyRetainVolumeFixture(t, volume, "unused0")
			deps.PVE = unlistedNodePVE{legacyDestroyPVE: deps.PVE.(legacyDestroyPVE)}
			sole, err := legacyUnusedSoleHolder(context.Background(), deps, "n1", 777, volume, volume)
			if sole || err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("the reference proof answered with a member unlisted: sole=%t err=%v", sole, err)
			}
			if client.moves != 0 {
				t.Fatal("the failed proof moved the volume")
			}
		})
	}
}
