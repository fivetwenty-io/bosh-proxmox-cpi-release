package pve

import (
	"context"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// canAskHealth reports whether c carries the cluster and nodes services that
// NodeHealth reads. A backend built without them keeps its pre-health choice.
func canAskHealth(c Client) bool {
	return c != nil && c.Cluster() != nil && c.Nodes() != nil
}

// healthReportFor reads NodeHealth through c. ok is false when c carries no
// cluster or nodes service to ask, which only a client built without a
// cluster connection does; a caller then keeps its pre-health behavior.
func healthReportFor(ctx context.Context, c Client) (report *NodeHealthReport, ok bool, err error) {
	if !canAskHealth(c) {
		return nil, false, nil
	}
	report, err = NodeHealth(ctx, c.Cluster(), c.Nodes(), NodeHealthOptions{})
	if err != nil {
		return nil, true, cpierrors.WrapAs(WrapError(err), cpierrors.TypeRetriableCloud,
			"read node health to choose a node that can answer")
	}
	return report, true, nil
}

// storageReadNode picks the node a storage call is addressed to. It walks
// prefer in order, then the storage's own node list, and then, only when the
// storage has no node list, every other node PVE reports able to answer. A
// node is a candidate only when NodeHealth calls it a proxy target, and when
// the storage carries a node list a node outside that list is never a
// candidate, because the storage is not mounted there.
//
// The last step exists for storage reads alone. It must never answer whether a
// VM exists or who holds a disk, because those questions belong to the node
// that owns the guest.
//
// An empty answer is an error, never a dead node. The error is retriable, since
// a node that is down or in transition comes back.
func storageReadNode(ctx context.Context, c Client, what string, storageNodes, prefer []string) (string, error) {
	report, ok, err := healthReportFor(ctx, c)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", cpierrors.Cloud("backend: %s needs a cluster client to confirm a node answers", what)
	}
	allowed := func(n string) bool {
		if n == "" || !report.ProxyTarget(n) {
			return false
		}
		if len(storageNodes) == 0 {
			return true
		}
		for _, s := range storageNodes {
			if s == n {
				return true
			}
		}
		return false
	}
	for _, n := range prefer {
		if allowed(n) {
			return n, nil
		}
	}
	for _, n := range storageNodes {
		if allowed(n) {
			return n, nil
		}
	}
	if len(storageNodes) == 0 {
		// Online nodes come before nodes in maintenance, which still answer
		// but are the last resort.
		for _, e := range report.Nodes {
			if e.ProxyTarget && e.Health == NodeHealthOnline {
				return e.Name, nil
			}
		}
		for _, e := range report.Nodes {
			if e.ProxyTarget {
				return e.Name, nil
			}
		}
	}
	return "", cpierrors.Retriable("backend: %s found no node that answers%s; %s",
		what, storageNodesSuffix(storageNodes), unanswerableSummary(report, prefer, storageNodes))
}

func storageNodesSuffix(storageNodes []string) string {
	if len(storageNodes) == 0 {
		return ""
	}
	return " among the storage's nodes " + strings.Join(storageNodes, ", ")
}

// unanswerableSummary names why the candidates were refused, so the operator
// can see which node to bring back.
func unanswerableSummary(report *NodeHealthReport, prefer, storageNodes []string) string {
	seen := map[string]bool{}
	var parts []string
	for _, group := range [][]string{prefer, storageNodes} {
		for _, n := range group {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			if e, found := report.Node(n); found {
				parts = append(parts, fmt.Sprintf("%s (%s)", n, e.Reason))
			} else {
				parts = append(parts, fmt.Sprintf("%s (the cluster does not list it)", n))
			}
		}
	}
	if len(parts) == 0 {
		return "the cluster reports no node able to answer"
	}
	return "refused: " + strings.Join(parts, ", ")
}

// requireProxyTarget confirms node can answer a call addressed to it and
// returns it, or returns the retriable error that names why it cannot.
func requireProxyTarget(ctx context.Context, c Client, what, node string) (string, error) {
	report, ok, err := healthReportFor(ctx, c)
	if err != nil {
		return "", err
	}
	if !ok {
		return node, nil
	}
	if report.ProxyTarget(node) {
		return node, nil
	}
	return "", cpierrors.Retriable("backend: %s: node %s cannot answer; %s", what, node,
		unanswerableSummary(report, []string{node}, nil))
}
