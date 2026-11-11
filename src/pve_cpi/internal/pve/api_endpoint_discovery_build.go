package pve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

const (
	// maxDiscoveredNodes is the most nodes a discovery result holds.
	maxDiscoveredNodes = 64
	// discoveryResolveTimeout is how long the builder waits for one name.
	discoveryResolveTimeout = 2 * time.Second
)

// DiscoveryOutcome is the enumeration a discovery result records. It never
// holds error text.
type DiscoveryOutcome string

// The outcomes a built result can have. A read that fails outright has no
// outcome, because it produces no result.
const (
	// DiscoveryOK means the result holds nodes and the status was read.
	DiscoveryOK DiscoveryOutcome = "ok"
	// DiscoveryNoStatus means the result holds nodes, but GET /cluster/status
	// could not be read, so the order comes from the /nodes status field.
	DiscoveryNoStatus DiscoveryOutcome = "no_status"
	// DiscoveryNoCandidates means no peer survived, whether the cluster has
	// one node, or every other name was left out.
	DiscoveryNoCandidates DiscoveryOutcome = "no_candidates"
	// DiscoveryStandalone means the host is not in a cluster.
	DiscoveryStandalone DiscoveryOutcome = "standalone"
	// DiscoveryPublicSuffix means the seed's domain is a public suffix, so no
	// name could be derived under it.
	DiscoveryPublicSuffix DiscoveryOutcome = "public_suffix"
)

// Build failures. As with the read failures, each means no result.
var (
	// ErrDiscoveryInput means the caller gave the builder no resolver, or a
	// seed that is not a host:port with a host. The seed always carries a
	// port, so one without is a caller bug and not a cluster to back off from.
	ErrDiscoveryInput = errors.New("discovery: a name resolver and a seed of the form host:port are required")
)

// NameResolver reports whether a derived host name resolves, by returning nil
// when it does. A resolver should honor its context. The builder does not wait
// past the timeout for one that ignores it, and counts the name as unresolved,
// but that lookup goes on running in its goroutine and can outlive the
// builder. The builder runs at most maxDiscoveredNodes lookups at once.
type NameResolver func(ctx context.Context, host string) error

// DropReason says why the builder left a node out of a result. It is an
// enumeration and never holds error text.
type DropReason string

// The reasons a node is left out.
const (
	// DropAmbiguousName is a name that is all digits, a 0x hex spelling, or
	// one of the loopback aliases.
	DropAmbiguousName DropReason = "ambiguous_name"
	// DropDuplicateName is a name that an earlier row already used.
	DropDuplicateName DropReason = "duplicate_name"
	// DropSeedNode is the node that answered, which the pool already holds
	// through the seed.
	DropSeedNode DropReason = "seed_node"
	// DropNoFingerprint is a node whose /nodes ssl_fingerprint is missing or
	// malformed.
	DropNoFingerprint DropReason = "no_fingerprint"
	// DropPublicSuffix is a node whose derived host falls outside the seed's
	// registrable domain.
	DropPublicSuffix DropReason = "public_suffix"
	// DropUnaddressable is a node whose derived address is not a legal
	// endpoint.
	DropUnaddressable DropReason = "unaddressable"
	// DropConfigured is a node whose address is a configured endpoint.
	DropConfigured DropReason = "configured"
	// DropUnresolved is a node whose derived name did not resolve in time.
	DropUnresolved DropReason = "unresolved"
	// DropOverCap is a node past the 64-node limit.
	DropOverCap DropReason = "over_cap"
)

// DroppedNode records one node the builder left out, for the caller's Debug
// line.
type DroppedNode struct {
	Name   string
	Reason DropReason
}

// DiscoveredNode is one peer the result keeps.
type DiscoveredNode struct {
	// Name is the node name, which passed the node-name helper.
	Name string
	// Address is the derived host:port, in the endpoints list grammar.
	Address string
	// Pins is the node's pin set. It starts with the /nodes fingerprint and
	// follows with the node's own certificate fingerprints, without repeats.
	Pins []endpointpool.Pin
	// Online is the flag the order used. It is the /cluster/status online flag
	// for a quorate status, and the /nodes status field when there is no
	// status. It is false for every node when the status is not quorate,
	// because a minority node reports the majority offline and those flags
	// cannot be trusted.
	Online bool
}

// DiscoveryResult is what the builder makes from one run of reads.
type DiscoveryResult struct {
	Outcome DiscoveryOutcome
	// Quorate is true only when /cluster/status was read and reported quorum.
	Quorate bool
	// RootCA is the cluster identity, the SHA-256 of the root CA's DER bytes.
	RootCA [sha256.Size]byte
	// Nodes holds at most maxDiscoveredNodes peers in preference order.
	Nodes []DiscoveredNode
	// Dropped lists every node left out and why.
	Dropped []DroppedNode
}

// DiscoveryInput is what the builder needs besides the reads.
type DiscoveryInput struct {
	// Seed is the configured endpoint the reads went through, as host:port.
	// Node addresses derive from it.
	Seed string
	// Configured lists every configured endpoint. A node whose address
	// normalizes to one of them is left out, and the seed counts as one.
	Configured []string
	// Resolver reports whether a derived name resolves.
	Resolver NameResolver
	// ResolveTimeout bounds one name. Zero means two seconds.
	ResolveTimeout time.Duration
}

// buildDiscoveryResult turns the reads into a result. It makes no API call
// and writes nothing, and its only effect outside the arguments is the
// resolver the caller injected.
//
// A node name that breaks the pve-node format, or no nodes at all, or a zero
// root, or no resolver, or a seed that is not host:port, returns an error from
// this package's discovery errors and no result.
// A name that passes the format but is ambiguous leaves only that node out.
//
// A standalone host, which is a status without a cluster row next to one
// node, gets the standalone outcome. A seed that cannot derive names gets the
// public_suffix outcome when its domain is a public suffix and no_candidates
// when the seed is not a name, as for a seed whose last label is all digits.
// Otherwise the builder leaves out the node that answered, which without a
// status is the only node /nodes lists, a node with a
// missing or malformed ssl_fingerprint, a node whose address is a configured
// endpoint or repeats, and a node whose derived name does not resolve within
// the timeout. It resolves names in parallel, in batches that never exceed the
// space left under the 64-node cap, in preference order.
//
// The order puts the nodes a quorate status reports online first, in name
// order, and the rest after them. A status that is not quorate has online
// flags that cannot be trusted, since a minority node reports every majority
// member offline, so those results keep every node in name order and report
// none of them online. Without a status the order uses the /nodes status
// field.
func buildDiscoveryResult(ctx context.Context, reads DiscoveryReads, in DiscoveryInput) (DiscoveryResult, error) {
	if ctx == nil {
		return DiscoveryResult{}, errors.New("discovery: ctx must not be nil")
	}
	if in.Resolver == nil {
		return DiscoveryResult{}, ErrDiscoveryInput
	}
	if host, _, err := net.SplitHostPort(in.Seed); err != nil || host == "" {
		return DiscoveryResult{}, ErrDiscoveryInput
	}
	if len(reads.Nodes) == 0 {
		return DiscoveryResult{}, ErrDiscoveryNodes
	}
	if reads.RootCA == ([sha256.Size]byte{}) {
		return DiscoveryResult{}, ErrDiscoveryRoot
	}
	for _, row := range reads.Nodes {
		if errors.Is(endpointcache.ValidateNodeName(row.Name), endpointcache.ErrNodeNameFormat) {
			return DiscoveryResult{}, ErrDiscoveryNodeName
		}
	}
	result := DiscoveryResult{
		RootCA:  reads.RootCA,
		Quorate: reads.Status.Available && reads.Status.Quorate,
	}
	if reads.Status.Available && !reads.Status.InCluster && len(reads.Nodes) == 1 {
		result.Outcome = DiscoveryStandalone
		return result, nil
	}
	deriver, err := endpointcache.NewDeriver(in.Seed)
	if err != nil {
		result.Outcome = DiscoveryNoCandidates
		if errors.Is(err, endpointcache.ErrPublicSuffix) {
			result.Outcome = DiscoveryPublicSuffix
		}
		return result, nil
	}
	candidates := discoveryCandidates(reads, in, deriver, &result)
	timeout := in.ResolveTimeout
	if timeout <= 0 {
		timeout = discoveryResolveTimeout
	}
	if err := resolveCandidates(ctx, candidates, in.Resolver, timeout, &result); err != nil {
		return DiscoveryResult{}, err
	}
	switch {
	case len(result.Nodes) == 0:
		result.Outcome = DiscoveryNoCandidates
	case !reads.Status.Available:
		result.Outcome = DiscoveryNoStatus
	default:
		result.Outcome = DiscoveryOK
	}
	return result, nil
}

// discoveryCandidates applies the checks that need no network to every row and
// returns the survivors in preference order. It records each node it drops in
// result.
func discoveryCandidates(
	reads DiscoveryReads, in DiscoveryInput, deriver *endpointcache.Deriver, result *DiscoveryResult,
) []DiscoveredNode {
	configured := normalizedEndpointSet(append([]string{in.Seed}, in.Configured...))
	seenNames := map[string]bool{}
	var candidates []DiscoveredNode
	drop := func(name string, reason DropReason) {
		result.Dropped = append(result.Dropped, DroppedNode{Name: name, Reason: reason})
	}
	for _, row := range reads.Nodes {
		key := strings.ToLower(row.Name)
		switch {
		case endpointcache.ValidateNodeName(row.Name) != nil:
			drop(row.Name, DropAmbiguousName)
			continue
		case seenNames[key]:
			drop(row.Name, DropDuplicateName)
			continue
		}
		seenNames[key] = true
		if isSeedNode(reads.Status, len(reads.Nodes), row.Name) {
			drop(row.Name, DropSeedNode)
			continue
		}
		fingerprint, err := endpointpool.ParsePin(row.Fingerprint)
		if err != nil {
			drop(row.Name, DropNoFingerprint)
			continue
		}
		address, err := deriver.Address(row.Name)
		if err != nil {
			reason := DropUnaddressable
			if errors.Is(err, endpointcache.ErrPublicSuffix) {
				reason = DropPublicSuffix
			}
			drop(row.Name, reason)
			continue
		}
		if configured[normalizedEndpoint(address)] {
			drop(row.Name, DropConfigured)
			continue
		}
		pins := []endpointpool.Pin{fingerprint}
		for _, pin := range row.CertPins {
			if !slices.Contains(pins, pin) {
				pins = append(pins, pin)
			}
		}
		candidates = append(candidates, DiscoveredNode{
			Name:    row.Name,
			Address: address,
			Pins:    pins,
			Online:  nodeOnline(reads.Status, row),
		})
	}
	slices.SortStableFunc(candidates, func(a, b DiscoveredNode) int {
		return compareNodePreference(a.Online, b.Online, a.Name, b.Name)
	})
	return candidates
}

// isSeedNode reports whether name is the node that answered the reads. A
// status names it. Without a status, a /nodes list of exactly one row can only
// be the node that answered.
func isSeedNode(status DiscoveryStatus, rowCount int, name string) bool {
	switch {
	case status.Local != "":
		return strings.EqualFold(status.Local, name)
	case !status.Available:
		return rowCount == 1
	}
	return false
}

// nodeOnline returns the online flag the order uses for row. A quorate status
// gives its own flag and no status gives the /nodes status field. A status
// that is not quorate gives none, so every node reads offline and the order
// falls back to names.
func nodeOnline(status DiscoveryStatus, row DiscoveryNodeRead) bool {
	switch {
	case !status.Available:
		return row.Status == nodeStatusOnlineValue
	case status.Quorate:
		return status.Online[row.Name]
	}
	return false
}

// compareNodePreference orders online nodes before the rest, and each group by
// name.
func compareNodePreference(aOnline, bOnline bool, aName, bName string) int {
	if aOnline != bOnline {
		if aOnline {
			return -1
		}
		return 1
	}
	return strings.Compare(aName, bName)
}

// normalizedEndpoint returns the pool's comparison form of a host:port, or an
// empty string when it is not a valid endpoint.
func normalizedEndpoint(addr string) string {
	host, port, ok := config.NormalizeEndpoint(addr)
	if !ok {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// normalizedEndpointSet returns the set of normalized forms of addrs, skipping
// any that is not a valid endpoint.
func normalizedEndpointSet(addrs []string) map[string]bool {
	set := make(map[string]bool, len(addrs))
	for _, addr := range addrs {
		if normalized := normalizedEndpoint(addr); normalized != "" {
			set[normalized] = true
		}
	}
	return set
}

// resolveCandidates resolves candidates in preference order until the result
// holds maxDiscoveredNodes nodes. Each round resolves, in parallel, as many
// names as there are places left, and a name that fails leaves its place for
// the next round. It records every candidate that does not resolve, and every
// one it never reached, in result. A canceled ctx returns its error, because a
// result built from cut-off lookups would drop nodes that do resolve.
func resolveCandidates(
	ctx context.Context, candidates []DiscoveredNode, resolver NameResolver, timeout time.Duration,
	result *DiscoveryResult,
) error {
	// slots bounds the lookups running at once. A lookup that ignores its
	// context keeps its slot until it returns, so a name that cannot get one
	// before its timeout counts as unresolved.
	slots := make(chan struct{}, maxDiscoveredNodes)
	next := 0
	for next < len(candidates) && len(result.Nodes) < maxDiscoveredNodes {
		batch := candidates[next:min(next+maxDiscoveredNodes-len(result.Nodes), len(candidates))]
		next += len(batch)
		resolved := make([]bool, len(batch))
		var wg sync.WaitGroup
		for i, candidate := range batch {
			wg.Go(func() {
				resolved[i] = resolvesWithin(ctx, resolver, candidate.Address, timeout, slots)
			})
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("discovery: name resolution ended: %w", err)
		}
		for i, candidate := range batch {
			if resolved[i] {
				result.Nodes = append(result.Nodes, candidate)
				continue
			}
			result.Dropped = append(result.Dropped, DroppedNode{Name: candidate.Name, Reason: DropUnresolved})
		}
	}
	for _, candidate := range candidates[next:] {
		result.Dropped = append(result.Dropped, DroppedNode{Name: candidate.Name, Reason: DropOverCap})
	}
	return nil
}

// resolvesWithin reports whether resolver accepts the host of address within
// timeout. The lookup runs in its own goroutine, so a resolver that ignores
// its context cannot hold the caller past the timeout. Such a lookup is left
// to finish on its own and keeps its slot until it does.
func resolvesWithin(
	ctx context.Context, resolver NameResolver, address string, timeout time.Duration, slots chan struct{},
) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case slots <- struct{}{}:
	case <-bounded.Done():
		return false
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-slots }()
		done <- resolver(bounded, host)
	}()
	select {
	case err = <-done:
		return err == nil && bounded.Err() == nil
	case <-bounded.Done():
		return false
	}
}

// lookupHostResolver returns a NameResolver that asks r, or the default
// resolver when r is nil, and counts a name with no address as a failure.
func lookupHostResolver(r *net.Resolver) NameResolver {
	if r == nil {
		r = net.DefaultResolver
	}
	return func(ctx context.Context, host string) error {
		addrs, err := r.LookupHost(ctx, host)
		if err != nil {
			return err
		}
		if len(addrs) == 0 {
			return errors.New("the name has no addresses")
		}
		return nil
	}
}
