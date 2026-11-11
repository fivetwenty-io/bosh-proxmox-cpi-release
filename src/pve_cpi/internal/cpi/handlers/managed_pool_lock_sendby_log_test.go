package handlers

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// sendByRefusalLines returns the Debug lines in obs that report a request not
// sent for its send-by deadline.
func sendByRefusalLines(obs *log.Observer) []log.Entry {
	var out []log.Entry
	for _, e := range obs.All() {
		if e.Level == slog.LevelDebug && e.Message == "pve endpoints: DELETE /pools not sent, send-by deadline passed" {
			out = append(out, e)
		}
	}
	return out
}

// A steal's delete refused by the guard before it reaches the pool writes the
// Debug line the pool writes for its own refusals, once per refusal, with the
// same fields.
func TestGuardedStealDeletePastSendByLogsTheRefusalOnce(t *testing.T) {
	claim := "owner=crashed@h/3-c-1 exp=2"
	pools := &lockGuardPools{found: true, comment: claim}
	wire := &sendByWire{late: func(int) bool { return false }}
	guard := newLockGuardOver(t, storeClient{pools: sendByWirePools{PoolService: pools, wire: wire}},
		&lockGuardEvents{})
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	ctx := log.IntoContext(t.Context(), logger)
	late := endpointpool.WithSendBy(ctx, time.Now().Add(-time.Millisecond), http.MethodDelete, "/pools")

	for i := 1; i <= 2; i++ {
		if err := guard.Client().Pools().DeletePool(late, lockGuardPool); err == nil {
			t.Fatal("a delete past its send-by deadline was not refused")
		}
		lines := sendByRefusalLines(obs)
		if len(lines) != i {
			t.Fatalf("after %d refusals the guard wrote %d refusal lines: %+v", i, len(lines), obs.All())
		}
		if lines[i-1].Attrs["method"] != http.MethodDelete || lines[i-1].Attrs["path"] != "/pools" {
			t.Fatalf("refusal line fields = %v, want method DELETE and path /pools", lines[i-1].Attrs)
		}
	}
	if len(wire.recorded()) != 0 {
		t.Fatalf("a refused delete reached the transport: %+v", wire.recorded())
	}
}

// A refusal that comes back from the transport was logged by the pool, and a
// pool other than a lock sentinel is left to the transport, so the guard
// writes no line for either.
func TestGuardWritesNoSendByLineForARefusalThePoolMakes(t *testing.T) {
	for name, tc := range map[string]struct {
		pool     string
		deadline time.Time
	}{
		"the transport refuses a sentinel delete":     {lockGuardPool, time.Now().Add(time.Minute)},
		"a pool other than a lock sentinel is passed": {"bosh-director", time.Now().Add(-time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			claim := "owner=crashed@h/3-c-1 exp=2"
			pools := &lockGuardPools{found: true, comment: claim}
			wire := &sendByWire{}
			guard := newLockGuardOver(t, storeClient{pools: sendByWirePools{PoolService: pools, wire: wire}},
				&lockGuardEvents{})
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			ctx := log.IntoContext(t.Context(), logger)
			ctx = endpointpool.WithSendBy(ctx, tc.deadline, http.MethodDelete, "/pools")
			if tc.pool == lockGuardPool {
				ctx = pve.WithExpectedLockClaim(ctx, claim)
			}

			if err := guard.Client().Pools().DeletePool(ctx, tc.pool); err == nil {
				t.Fatal("the delete was not refused")
			}
			if refused, _ := wire.refusedDeletes(); refused != 1 {
				t.Fatalf("the delete did not meet the transport: %+v", wire.recorded())
			}
			if lines := sendByRefusalLines(obs); len(lines) != 0 {
				t.Fatalf("the guard wrote %d refusal lines for a refusal the pool makes: %+v", len(lines), lines)
			}
		})
	}
}
