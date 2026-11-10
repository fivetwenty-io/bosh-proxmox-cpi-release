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
	// ErrDiscoveryInput means the caller gave the builder no resolver.
	ErrDiscoveryInput = errors.New("discovery: a name resolver is required")
)

// NameResolver reports whether a derived host name resolves, by returning nil
// when it does. The builder bounds each call with a timeout.
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
	// DropDuplicateAddress is a node whose address an earlier node has.
	DropDuplicateAddress DropReason = "duplicate_address"
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
	// Online is the flag the order used. It is the /cluster/status flag for a
	// quorate status and the /nodes status field otherwise.
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

// BuildDiscoveryResult turns the reads into a result. It makes no API call
// and writes nothing, and its only effect outside the arguments is the
// resolver the caller injected.
//
// A node name that breaks the pve-node format, or no nodes at all, or a zero
// root, returns an error from this package's discovery errors and no result.
// A name that passes the format but is ambiguous leaves only that node out.
//
// A standalone host, which is a status without a cluster row next to one
// node, gets the standalone outcome. A seed that cannot derive names gets the
// public_suffix outcome when its domain is a public suffix and no_candidates
// when the seed is not a name, as for a seed whose last label is all digits.
// Otherwise the builder leaves out the node that answered, a node with a
// missing or malformed ssl_fingerprint, a node whose address is a configured
// endpoint or repeats, and a node whose derived name does not resolve within
// the timeout. It resolves names in parallel, in batches that never exceed the
// space left under the 64-node cap, in preference order.
//
// The order puts the nodes a quorate status reports online first, in name
// order, and the rest after them. A status that is not quorate has online
// flags that cannot be trusted, since a minority node reports every majority
// member offline, so those results keep every node in name order. Without a
// status the order uses the /nodes status field.
func BuildDiscoveryResult(ctx context.Context, reads DiscoveryReads, in DiscoveryInput) (DiscoveryResult, error) {
	if ctx == nil {
		return DiscoveryResult{}, errors.New("discovery: ctx must not be nil")
	}
	if in.Resolver == nil {
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
	seenAddresses := map[string]bool{}
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
		if reads.Status.Local != "" && strings.EqualFold(reads.Status.Local, row.Name) {
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
		normalized := normalizedEndpoint(address)
		switch {
		case configured[normalized]:
			drop(row.Name, DropConfigured)
			continue
		case seenAddresses[normalized]:
			drop(row.Name, DropDuplicateAddress)
			continue
		}
		seenAddresses[normalized] = true
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
			Online:  nodeOnline(reads, row),
		})
	}
	orderByOnline := !reads.Status.Available || reads.Status.Quorate
	slices.SortStableFunc(candidates, func(a, b DiscoveredNode) int {
		if orderByOnline && a.Online != b.Online {
			if a.Online {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return candidates
}

// nodeOnline returns the online flag the order uses for row.
func nodeOnline(reads DiscoveryReads, row DiscoveryNodeRead) bool {
	if reads.Status.Available && reads.Status.Quorate {
		return reads.Status.Online[row.Name]
	}
	return row.Status == nodeStatusOnlineValue
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
	next := 0
	for next < len(candidates) && len(result.Nodes) < maxDiscoveredNodes {
		batch := candidates[next:min(next+maxDiscoveredNodes-len(result.Nodes), len(candidates))]
		next += len(batch)
		resolved := make([]bool, len(batch))
		var wg sync.WaitGroup
		for i, candidate := range batch {
			wg.Go(func() {
				resolved[i] = resolvesWithin(ctx, resolver, candidate.Address, timeout)
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
// timeout.
func resolvesWithin(ctx context.Context, resolver NameResolver, address string, timeout time.Duration) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return resolver(bounded, host) == nil && bounded.Err() == nil
}

// LookupHostResolver returns a NameResolver that asks r, or the default
// resolver when r is nil, and counts a name with no address as a failure.
func LookupHostResolver(r *net.Resolver) NameResolver {
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
