// Adopt-and-wait on a racing concurrent template-replica clone.
//
// When two CPI invocations independently decide a node needs a per-node stemcell
// replica, both can pass the settled-only existence check (ResolveTemplateVMIDForNode
// matches only frozen templates) while a winner is still mid-build, and both then
// clone — producing a duplicate, half-built replica template. The vSphere and
// Azure CPIs solve the equivalent race by catching the create collision and
// waiting on the winner's artifact instead of building a second one.
//
// PVE exposes the winner's in-flight build directly: the replica VM carries its
// identity tags (sha + per-node) from creation, but Template flips true only after
// the freeze, and the guest config lock reads "create"/"clone" while the build is
// running. findReplicaCandidate observes that mid-build VM (which the settled-only
// resolver hides); AdoptReplicaTemplate polls it to a settled template, bounded by
// a timeout, and reports the adopted VMID so the loser skips its duplicate build.
package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// replicaAdoptPollInterval is the base delay between adopt polls while a winner
// is still building. A per-attempt jitter is added so concurrent losers do not
// synchronize their polling.
const replicaAdoptPollInterval = 2 * time.Second

// replicaListItem decodes the GET /nodes/{node}/qemu list fields the adopt path
// needs. It differs from qemuListItem only by Lock: PVE reports "lock":"clone" /
// "create" on a guest while a clone or fresh-import build is in flight, which is
// the signal that a discovered replica is not yet safe to adopt.
type replicaListItem struct {
	Vmid     int64    `json:"vmid"`
	Tags     *string  `json:"tags,omitempty"`
	Template *pveBool `json:"template,omitempty"`
	Lock     *string  `json:"lock,omitempty"`
}

// isCloneInProgressLock reports whether a guest-config lock value indicates a
// template clone/create is still in flight. PVE sets lock="clone" on the new
// guest during qm clone and lock="create" while a fresh VM/import materialises;
// either means the replica is being built and must not be adopted yet.
func isCloneInProgressLock(lock string) bool {
	switch strings.TrimSpace(lock) {
	case "clone", "create":
		return true
	default:
		return false
	}
}

// replicaCandidate is a node-local VM carrying BOTH the sha tag and the per-node
// replica tag, regardless of whether it has been frozen into a template yet.
type replicaCandidate struct {
	VMID     int
	Template bool
	Lock     string // "" when settled/unlocked
}

// settled reports whether the candidate is a finished, adoptable replica: frozen
// into a template AND not clone/create-locked.
func (rc replicaCandidate) settled() bool {
	return rc.Template && !isCloneInProgressLock(rc.Lock)
}

// preferReplicaCandidate reports whether cand should replace best as the scan
// winner: a settled candidate always beats an unsettled one; among candidates of
// equal settled-ness, the lower VMID wins.
func preferReplicaCandidate(cand, best replicaCandidate) bool {
	if cand.settled() != best.settled() {
		return cand.settled()
	}
	return cand.VMID < best.VMID
}

// findReplicaCandidate scans node for the lowest-VMID VM tagged with BOTH the sha
// tag and the per-node replica tag. Unlike ResolveTemplateVMIDForNode it does NOT
// require the Template flag, so it also observes an in-flight replica a concurrent
// process is still building (Template==false / lock="create"/"clone"). That is the
// signal that lets a losing process adopt-and-wait on the winner's artifact rather
// than building a duplicate.
//
// Returns (candidate, true, nil) on match; (zero, false, nil) when none match /
// sha8 is empty / the list is nil/empty; (zero, false, err) on an API error.
func findReplicaCandidate(ctx context.Context, c Client, node, sha8 string) (replicaCandidate, bool, error) {
	var zero replicaCandidate
	if ctx == nil {
		return zero, false, cpierrors.Cloud("findReplicaCandidate: ctx must not be nil")
	}
	if c == nil {
		return zero, false, cpierrors.Cloud("findReplicaCandidate: client must not be nil")
	}
	if node == "" {
		return zero, false, cpierrors.Cloud("findReplicaCandidate: node must not be empty")
	}
	if sha8 == "" {
		return zero, false, nil
	}

	shaTag := "bosh-stemcell-sha-" + sha8
	nodeTag := replicaNodeTag(node)

	resp, listErr := c.Nodes().ListQemu(ctx, node, nil)
	if listErr != nil {
		return zero, false, cpierrors.Wrap(listErr,
			fmt.Sprintf("findReplicaCandidate: node %s sha8 %q", node, sha8))
	}
	if resp == nil || len(*resp) == 0 {
		return zero, false, nil
	}

	best := zero
	found := false
	for _, raw := range *resp {
		var item replicaListItem
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if item.Tags == nil {
			continue
		}
		tokens := splitPVETags(*item.Tags)
		hasSHA, hasNode := false, false
		for _, tok := range tokens {
			if tok == shaTag {
				hasSHA = true
			}
			if tok == nodeTag {
				hasNode = true
			}
		}
		if !hasSHA || !hasNode {
			continue
		}
		cand := replicaCandidate{VMID: int(item.Vmid)}
		if item.Template != nil {
			cand.Template = bool(*item.Template)
		}
		if item.Lock != nil {
			cand.Lock = *item.Lock
		}
		// Prefer a settled (frozen, unlocked) replica over an unsettled one so a
		// crashed-mid-build orphan (Template==false, never settles) does not shadow
		// a genuine adoptable template that happens to have a higher VMID — which
		// would otherwise make AdoptReplicaTemplate wait out its whole timeout while
		// an adoptable artifact sits right there. Among candidates of equal
		// settled-ness, the lowest VMID wins (matches ResolveTemplateVMIDForNode).
		if !found || preferReplicaCandidate(cand, best) {
			best = cand
			found = true
		}
	}
	return best, found, nil
}

// AdoptReplicaTemplate is the adopt-and-wait response to a concurrent winner
// building the same per-node replica: rather than building a duplicate, the
// loser waits for the winner's artifact to become a settled template.
//
//   - (vmid, true, nil)  — a settled replica template was found and adopted.
//   - (0, false, nil)    — no candidate present, or an in-flight candidate
//     vanished (winner rolled back), or timeout<=0 with an unsettled candidate;
//     in every (0,false,nil) case the caller should build the replica itself.
//   - (0, false, err)    — the adopt deadline elapsed while a winner was still
//     building (TypeRetriableCloud, so the director re-drives), or an API error.
//
// timeout bounds the wait. With timeout<=0 the function performs a single probe
// and never blocks: a settled replica is still adopted, but an in-flight one is
// reported not-adopted so the caller's legacy build path runs unchanged. Callers
// gate invocation on the feature knob so behaviour is byte-identical when off.
func AdoptReplicaTemplate(ctx context.Context, c Client, node, sha8 string, timeout time.Duration) (int, bool, error) {
	return adoptReplicaTemplate(ctx, c, node, sha8, timeout, defaultLockClock())
}

func adoptReplicaTemplate(
	ctx context.Context, c Client, node, sha8 string, timeout time.Duration, clk lockClock,
) (int, bool, error) {
	cand, found, err := findReplicaCandidate(ctx, c, node, sha8)
	if err != nil {
		return 0, false, err
	}
	if !found {
		return 0, false, nil
	}
	if cand.settled() {
		return cand.VMID, true, nil
	}
	// A candidate exists but a concurrent winner is still building it.
	if timeout <= 0 {
		// Adopt-and-wait disabled: do not block. The caller falls back to its
		// legacy build path, preserving byte-identical behaviour when off.
		return 0, false, nil
	}

	deadline := clk.now().Add(timeout)

	// Derive a context whose deadline matches the adoption deadline so API calls
	// inside findReplicaCandidate (notably ListQemu) cannot stall past it. The
	// loop's clock-based deadline check is kept as the primary gate; this context
	// provides a hard cap on any single API call's wall time.
	adoptCtx, adoptCancel := context.WithDeadline(ctx, deadline)
	defer adoptCancel()

	for {
		now := clk.now()
		if !now.Before(deadline) {
			return 0, false, cpierrors.WrapAs(
				cpierrors.Cloud(
					"AdoptReplicaTemplate: replica vmid=%d for sha8=%s on node %q still building (lock=%q) after %s",
					cand.VMID, sha8, node, cand.Lock, timeout),
				cpierrors.TypeRetriableCloud,
				"AdoptReplicaTemplate: adopt-wait timeout")
		}
		wait := replicaAdoptPollInterval + time.Duration(jitterInt64N(int64(replicaAdoptPollInterval)))
		if remaining := deadline.Sub(now); wait > remaining {
			wait = remaining
		}
		if sleepErr := clk.sleep(ctx, wait); sleepErr != nil {
			return 0, false, cpierrors.WrapAs(sleepErr, cpierrors.TypeRetriableCloud,
				"AdoptReplicaTemplate: interrupted waiting to adopt")
		}

		cand, found, err = findReplicaCandidate(adoptCtx, c, node, sha8)
		if err != nil {
			return 0, false, err
		}
		if !found {
			// The in-flight candidate vanished (winner's build failed and rolled
			// back). There is no artifact to adopt; let the caller build.
			return 0, false, nil
		}
		if cand.settled() {
			return cand.VMID, true, nil
		}
	}
}
