package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

const (
	// haClockWalkLimit bounds the whole walk over the peers. A request addressed
	// to another node has no header deadline of its own in the endpoint pool, so
	// a peer that accepts the connection and never answers would otherwise wait
	// out pveproxy's timeout, and each HA decision reads the clock twice.
	haClockWalkLimit = 15 * time.Second

	// haClockMaxPeers is how many members the walk asks, which is the CRM
	// master and two others. Any member that answers is as good as another, so
	// more attempts add little.
	haClockMaxPeers = 3
)

type haClockLimitKey struct{}

// WithTestHAClockWalkLimit shortens the time limit on the walk over peers for
// a test, so that a hung peer is cut off without a real wait. Production code
// MUST NOT call it.
func WithTestHAClockWalkLimit(ctx context.Context, limit time.Duration) context.Context {
	return context.WithValue(ctx, haClockLimitKey{}, limit)
}

func haClockWalkLimitFor(ctx context.Context) time.Duration {
	if limit, ok := ctx.Value(haClockLimitKey{}).(time.Duration); ok && limit > 0 {
		return limit
	}
	return haClockWalkLimit
}

// haClock is the clock that one HA read measures its timestamps against. now
// is that clock in Unix seconds, and node is the cluster member that served it.
// entryErr is the entry node's own failure when another member's clock was
// used, and it is nil when the entry node's clock answered.
type haClock struct {
	now      int64
	node     string
	entryErr error
}

// describe names the clock for a reason text, such as "on the entry node's
// clock" or "on node pve2's clock".
func (c haClock) describe() string {
	if c.entryErr == nil {
		return "on the entry node's clock"
	}
	return "on node " + c.node + "'s clock"
}

// readHAClock reads the clock that an HA view's timestamps are measured
// against. It reads the entry node's clock first. A node that answers its own
// clock read with a server error, which a node does while systemd shuts it
// down because timedatectl then fails, is a node whose API still answers but
// whose node-local commands fail. The manager status that the entry node
// serves comes from the cluster file system and stays good, so the read takes
// the clock of another member instead, through the same entry node, in the
// order peers gives. The first member that answers with a clock is used.
//
// Only a server error moves the read. A transport failure, a 4xx, or an
// answer with no clock fails the read as it did before, so a minority entry,
// an entry the pool cannot reach, or a wrong path never borrows a peer's
// clock. Every other protection of the HA view stays where it was: the entry
// node must still report the cluster quorate, the manager status must still
// come through the entry node, and the closing cluster-status check must
// still find the same entry node. A peer's clock only measures how old the
// entry node's copy of the manager status is, and a member that the entry
// node can no longer reach cannot answer through it.
//
// When peers is empty, such as on a standalone host, a server error is retried
// like any other transient failure, as the read did before peers existed. When
// peers is not empty, the entry node's server error goes to the peers at once,
// and each peer gets one attempt, so a broken entry node costs one request and
// not the full retry budget. The walk asks at most haClockMaxPeers members and
// stops after haClockWalkLimit, so a peer that never answers cannot hold a
// decision.
func readHAClock(
	ctx context.Context, nodesAPI NodeHealthNodes, entry string, peers []string, label string,
) (haClock, error) {
	var (
		now      int64
		answered error
	)
	err := RetryOnTransient(ctx, nil, label, 0, func() error {
		resp, inner := nodesAPI.ListTime(ctx, entry)
		if inner != nil {
			if len(peers) > 0 && nodeServerError(inner) {
				answered = inner
				return nil
			}
			return inner
		}
		if resp == nil || resp.Time.Int() <= 0 {
			return cpierrors.Retriable("node %s returned no clock", entry)
		}
		now = resp.Time.Int()
		return nil
	})
	if err != nil {
		return haClock{}, err
	}
	if answered == nil {
		return haClock{now: now, node: entry}, nil
	}

	if len(peers) > haClockMaxPeers {
		peers = peers[:haClockMaxPeers]
	}
	walkCtx, cancel := context.WithTimeout(ctx, haClockWalkLimitFor(ctx))
	defer cancel()
	failures := make([]string, 0, len(peers))
	for _, peer := range peers {
		if ctxErr := walkCtx.Err(); ctxErr != nil {
			failures = append(failures, ctxErr.Error())
			break
		}
		resp, peerErr := nodesAPI.ListTime(walkCtx, peer)
		switch {
		case peerErr != nil:
			failures = append(failures, peer+": "+peerErr.Error())
		case resp == nil || resp.Time.Int() <= 0:
			failures = append(failures, peer+": returned no clock")
		default:
			return haClock{now: resp.Time.Int(), node: peer, entryErr: answered}, nil
		}
	}
	return haClock{}, fmt.Errorf("%w; no other member's clock answered either (%s)",
		answered, strings.Join(failures, "; "))
}

// nodeServerError reports whether err is a 5xx status that the node answered
// with, rather than a failure to reach it.
func nodeServerError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	code, ok := apiHTTPCode(err)
	return ok && code >= 500 && code <= 599
}

// haClockPeers lists the members whose clock an HA read may use when the entry
// node's own clock read fails with a server error. It holds every node that
// cluster status reports online, other than the entry node. The CRM master
// comes first, because it wrote the CRM timestamp on its own clock, so its
// clock measures that timestamp without any skew between nodes. The others
// follow in name order.
func haClockPeers(view *clusterView, entry, master string) []string {
	if view == nil {
		return nil
	}
	peers := make([]string, 0, len(view.nodes))
	for name, row := range view.nodes {
		if name == "" || name == entry || name == master || !row.Online {
			continue
		}
		peers = append(peers, name)
	}
	sort.Strings(peers)
	if row, ok := view.nodes[master]; ok && master != "" && master != entry && row.Online {
		peers = append([]string{master}, peers...)
	}
	return peers
}

// decodeMasterNode reads manager status's master_node field. The field only
// orders the clock peers, so a value that is missing or not a string reads as
// no master and never fails the manager status.
func decodeMasterNode(raw json.RawMessage) string {
	if !jsonPresent(raw) {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return ""
	}
	return name
}
