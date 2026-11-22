package handlers

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestColoMove_TheMoveLogNamesTheMovingVM pins the fields of the move's log
// lines in a call that moves a member other than its own VM. vmid names the
// call's VM, moving_vmid the VM that moves, and partner_vmid the partner that
// stays, and no key repeats.
func TestColoMove_TheMoveLogNamesTheMovingVM(t *testing.T) {
	t.Parallel()
	for _, timedOut := range []bool{false, true} {
		g := newColoGroup("pve1", "pve2", "pve3", "pve3")
		g.stub.resComments[haResourceSid(102)] = aaIdent("web").resourceMarker("", coloNow().Unix()-90)
		h := g.harness()
		if timedOut {
			h.migrate = func(context.Context, int, string, string) error { return context.DeadlineExceeded }
		}
		g.tag(100, 101, 102, 103)
		logger, obs := log.NewObservedLogger(slog.LevelDebug)
		logger = logger.With(log.Int(metadataKeyVMID, 100))
		coloJoin(g.deps(h, coloClaimDir(t)), 100, "0", logger)
		if got := h.moves(); len(got) != 1 || got[0].vmid != 103 {
			t.Fatalf("migrations = %+v; want 103, born last of its pair, moved", got)
		}
		lines := map[string]slog.Level{
			"PVE HA moves it to another node before the rule is written": slog.LevelInfo,
		}
		if timedOut {
			lines["PVE HA did not finish moving"] = slog.LevelWarn
		} else {
			lines["PVE HA moved the VM off its anti-affinity partner's node"] = slog.LevelInfo
		}
		for part, level := range lines {
			found := false
			for _, e := range obs.All() {
				if e.Level != level || !strings.Contains(e.Message, part) {
					continue
				}
				found = true
				want := map[string]int64{metadataKeyVMID: 100, "moving_vmid": 103, "partner_vmid": 102}
				got := map[string]int64{}
				for k := range want {
					if v, ok := e.Attrs[k].(int64); ok {
						got[k] = v
					}
				}
				if !maps.Equal(got, want) {
					t.Errorf("timed out %v, line %q: fields %v; want %v", timedOut, part, e.Attrs, want)
				}
				for k := range e.Attrs {
					if k == "joiner" || strings.HasSuffix(k, "_2") {
						t.Errorf("timed out %v, line %q: fields %v; want no joiner and no repeated key",
							timedOut, part, e.Attrs)
					}
				}
			}
			if !found {
				t.Errorf("timed out %v: no %v line with %q", timedOut, level, part)
			}
		}
	}
}
