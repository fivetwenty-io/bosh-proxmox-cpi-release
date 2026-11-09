package main

import (
	"context"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// commandContext returns the context an operator command runs its PVE calls
// under. Like the dispatcher does for a CPI call, it installs an affinity
// scope, so a tether the command's mutations make lasts as long as the command.
// The returned cancel function releases the scope and then cancels the
// deadline, and the caller defers it.
func commandContext(budget time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	ctx, release := endpointpool.WithAffinityScope(ctx)
	return ctx, func() {
		release()
		cancel()
	}
}
