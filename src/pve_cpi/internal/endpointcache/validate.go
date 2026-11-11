package endpointcache

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// pinHexLength is the length of a pin: a SHA-256 fingerprint in hex.
const pinHexLength = 64

// ErrPayloadInvalid means a cache file passed its HMAC and key check but its
// payload breaks a validation rule that makes the whole file unusable, such as
// a node name that fails the pve-node format or a pin that is not 32 bytes of
// hex. It matches ErrFileInvalid, so the reader treats the file as missing and
// the next successful write replaces it. The text names the record and the
// rule and never echoes a value from the file.
var ErrPayloadInvalid = privatefile.Sentinel("endpoint cache: payload fails validation", ErrFileInvalid)

// DropReason says why validation or a merge dropped a single record and kept
// the rest of the file.
type DropReason string

// Drop reasons.
const (
	// DropExpired means the record is older than its bound.
	DropExpired DropReason = "expired"
	// DropFuture means the record's timestamp is more than FutureSkew ahead of
	// the reader's clock.
	DropFuture DropReason = "future"
	// DropAmbiguousName means the node name passes the pve-node format but a
	// resolver could read it as a numeric or loopback address.
	DropAmbiguousName DropReason = "ambiguous_name"
	// DropNotInAZMap means the reader's az_map does not name the hint's node.
	DropNotInAZMap DropReason = "not_in_az_map"
	// DropNotDerived means the node's stored address and port are not what
	// the derivation under the key's seed gives for its name, or the seed
	// derives no address at all.
	DropNotDerived DropReason = "not_derived"
	// DropPublicSuffix means the derivation now refuses the node's host, or
	// the seed's domain, as a public suffix. A genuine file gets here only
	// when an update to the public suffix list moves the line.
	DropPublicSuffix DropReason = "public_suffix"
	// DropIdentityConflict means the stored identity holds a refused identity
	// change, so the reader loads none of the file's nodes until the operator
	// deletes it.
	DropIdentityConflict DropReason = "identity_conflict"
	// DropNotConfigured means the record names an endpoint that is not one of
	// the key's configured endpoints.
	DropNotConfigured DropReason = "not_configured"
	// DropConfiguredEndpoint means the node's derived address is one of the
	// key's configured endpoints, which includes the seed's own node whenever
	// its name derives to the seed. The pool already holds that endpoint under
	// the configured rules, so a discovered entry would hold it a second time
	// under the pin rules.
	DropConfiguredEndpoint DropReason = "configured_endpoint"
	// DropInvalid means the record is malformed in a way that does not make
	// the file unusable, such as a mark for something that is not an endpoint.
	DropInvalid DropReason = "invalid"
	// DropUnusable means the whole file failed validation and is treated as
	// missing.
	DropUnusable DropReason = "unusable"
)

// Drop is a record that validation left out. Its text never echoes a value
// from the file, so a caller can log it at Debug.
type Drop struct {
	// Record names the kind of record, such as "node 3" or "mark".
	Record string
	// Reason says why it was dropped.
	Reason DropReason
}

// String formats the drop for a log line.
func (d Drop) String() string { return d.Record + " dropped: " + string(d.Reason) }

// Validate checks a payload read from the store against key, the reader's own
// key, at time now. It returns a copy that holds only what a reader may use
// now, along with the records it dropped. It does not look at the reader's
// az_map, because a writer keeps a hint that another entry sharing the key
// wrote. See forReader for that check.
//
// A payload is unusable, and Validate returns ErrPayloadInvalid, when its key
// differs from key (ErrKeyMismatch), when a node name fails the pve-node
// format, when a node's address does not pass the endpoint grammar, when its
// port is out of range, when a pin is not 32 bytes of hex or a node holds no
// pin or more than MaxPinsPerNode, when the file holds more than MaxNodes nodes
// or two nodes with one name, when the cluster identity is not 32 bytes of hex
// or a discovery lists nodes without one, when the outcome is one that holds
// no nodes and the discovery lists some, when an outcome, observed_via, or
// a hint's name is not valid, and when endpoint_nodes names something that is
// not an endpoint or holds a name that fails the pve-node format.
//
// A single record is dropped, and the rest of the file stands, when a node name
// is all digits, a 0x hex spelling, or a loopback alias, when a node's address
// and port are not what the key's seed derives for its name, when a node's
// confirmation is more than NodeTTL old or more than FutureSkew ahead, when a
// node's derived address is one of the key's configured endpoints, when a
// mark is older than markTTL, ahead, or names something that is not an
// endpoint or a class that is not enumerated, when observed_via names an
// endpoint outside the key, when an endpoint_nodes key names an endpoint
// outside the key or its name is one a resolver could misread, and when the
// hint is older than hintTTL or ahead.
// A discovery whose own time is ahead is dropped whole, a next-refresh time
// further ahead than MaxRefreshWait is dropped so the refresh is due now, and
// a pin_failed_at more than FutureSkew ahead is dropped so it counts for
// nothing. Dropping the field, not the file, keeps the stored identity.
// When the identity holds a conflict, the identity stays so the refusal stays
// in force, and every node is dropped.
//
// A drop hides a record from use and never deletes it. A writer keeps what the
// file knows, as readStored describes.
func Validate(p *Payload, key Key, now time.Time) (*Payload, []Drop, error) {
	state, drops, err := readStored(p, key, now)
	if err != nil {
		return nil, nil, err
	}
	return state.usable, drops, nil
}

// storedState is a stored payload as a writer reads it under the lock. usable
// is what a reader may use now, as Validate returns it. discovery is the
// stored discovery section as written, kept once the file passes the format
// checks and before any drop, so a write that carries no new discovery puts it
// back unchanged. known is every stored node that passes the format checks and
// is not an ambiguous name, in its normalized spelling, whatever its
// timestamps and whether its address still derives, because a node stays
// known, and its pins bind under verify_ssl false, after the pool stops
// loading it. The zero value is a missing file.
type storedState struct {
	usable    *Payload
	discovery *DiscoverySection
	known     []DiscoveredNode
}

// readStored is Validate with the stored discovery section and the known
// nodes kept beside the usable payload.
func readStored(p *Payload, key Key, now time.Time) (storedState, []Drop, error) {
	if p == nil {
		return storedState{}, nil, fmt.Errorf("%w: no payload", ErrPayloadInvalid)
	}
	if !p.Key.Equal(key) {
		return storedState{}, nil, ErrKeyMismatch
	}
	out := &Payload{Key: key}
	out.Key.Endpoints = slices.Clone(key.Endpoints)
	state := storedState{usable: out}
	var drops []Drop

	if p.Cluster != nil {
		cluster, found, err := validateCluster(*p.Cluster, key)
		if err != nil {
			return storedState{}, nil, err
		}
		drops = append(drops, found...)
		nodes, found, err := validateEndpointNodes(p.Cluster.EndpointNodes, key)
		if err != nil {
			return storedState{}, nil, err
		}
		drops = append(drops, found...)
		cluster.EndpointNodes = nodes
		out.Cluster = &cluster
	}
	if p.Discovery != nil {
		section, known, found, err := validateDiscovery(p.Discovery, keyDerivation(key), now, nil)
		if err != nil {
			return storedState{}, nil, err
		}
		// A node needs the identity its pins were read under, whatever its
		// times, because a writer keeps every known node.
		if len(p.Discovery.Nodes) > 0 && out.Cluster == nil {
			return storedState{}, nil, fmt.Errorf("%w: discovery lists nodes without a cluster identity",
				ErrPayloadInvalid)
		}
		drops = append(drops, found...)
		if section != nil && len(section.Nodes) > 0 && out.Cluster.IdentityConflict {
			section.Nodes = []DiscoveredNode{}
			drops = append(drops, Drop{Record: "nodes", Reason: DropIdentityConflict})
		}
		out.Discovery = section
		state.discovery = p.Discovery.clone()
		state.known = known
	}
	out.Marks, drops = validateMarks(p.Marks, now, drops)
	if p.ColdStart != nil {
		hint, found, err := validateHint(*p.ColdStart, now)
		if err != nil {
			return storedState{}, nil, err
		}
		drops = append(drops, found...)
		out.ColdStart = hint
	}
	return state, drops, nil
}

// forReader applies the one check that depends on the reader: a hint whose
// node the reader's az_map does not name is dropped and the rest of the file
// stands, because two entries that reach one cluster share a key and can carry
// different placement. azNodes are the node names of the reader's effective
// az_map, compared exactly. p is not changed.
func forReader(p *Payload, azNodes []string) (*Payload, []Drop) {
	if p == nil || p.ColdStart == nil || slices.Contains(azNodes, p.ColdStart.Name) {
		return p, nil
	}
	out := p.clone()
	out.ColdStart = nil
	return out, []Drop{{Record: "cold_start", Reason: DropNotInAZMap}}
}

// validateCluster checks the cluster identity against key. An observed_via
// that is not an endpoint makes the file unusable. One that names an endpoint
// outside the key is dropped, and the identity stays, because dropping the
// identity would reopen trust on first use. A kept observed_via is in the
// key's spelling.
func validateCluster(c ClusterIdentity, key Key) (ClusterIdentity, []Drop, error) {
	root, ok := normalizeHex(c.RootCASHA256)
	if !ok {
		return ClusterIdentity{}, nil, fmt.Errorf("%w: cluster root CA digest is not 32 bytes of hex",
			ErrPayloadInvalid)
	}
	c.RootCASHA256 = root
	c.ObservedAt = c.ObservedAt.UTC()
	var drops []Drop
	if c.ObservedVia != "" {
		host, port, ok := config.NormalizeEndpoint(c.ObservedVia)
		if !ok {
			return ClusterIdentity{}, nil, fmt.Errorf("%w: cluster observed_via is not an endpoint", ErrPayloadInvalid)
		}
		c.ObservedVia = net.JoinHostPort(host, strconv.Itoa(port))
		if !slices.Contains(key.Endpoints, c.ObservedVia) {
			c.ObservedVia = ""
			drops = append(drops, Drop{Record: "cluster observed_via", Reason: DropNotConfigured})
		}
	}
	return c, drops, nil
}

// validateEndpointNodes checks the configured endpoints' node names against
// key and returns them keyed by the host:port spelling the key uses. A key
// that is not an endpoint, or a name that fails the pve-node format, makes the
// file unusable. A key that names an endpoint outside the key is dropped alone
// with the not-configured reason, and so is a name a resolver could misread
// and a second spelling of an endpoint an earlier key already named, so the
// rest of the file stands, the identity with it. The result is nil when no
// entry is left.
func validateEndpointNodes(nodes map[string]string, key Key) (map[string]string, []Drop, error) {
	if len(nodes) == 0 {
		return nil, nil, nil
	}
	configured := keyDerivation(key).configured
	out := make(map[string]string, len(nodes))
	var drops []Drop
	for _, endpoint := range sortedKeys(nodes) {
		normalized, ok := normalizeMarkEndpoint(endpoint)
		if !ok {
			return nil, nil, fmt.Errorf("%w: cluster endpoint_nodes names something that is not an endpoint",
				ErrPayloadInvalid)
		}
		if err := ValidateNodeName(nodes[endpoint]); err != nil {
			if errors.Is(err, ErrNodeNameAmbiguous) {
				drops = append(drops, Drop{Record: "endpoint node", Reason: DropAmbiguousName})
				continue
			}
			return nil, nil, fmt.Errorf("%w: cluster endpoint_nodes holds a name that fails the pve-node format",
				ErrPayloadInvalid)
		}
		switch _, repeated := out[normalized]; {
		case !slices.Contains(configured, normalized):
			drops = append(drops, Drop{Record: "endpoint node", Reason: DropNotConfigured})
		case repeated:
			drops = append(drops, Drop{Record: "endpoint node", Reason: DropInvalid})
		default:
			out[normalized] = nodes[endpoint]
		}
	}
	if len(out) == 0 {
		return nil, drops, nil
	}
	return out, drops, nil
}

// normalizeHex returns the lowercase spelling of s when it is 32 bytes of hex.
func normalizeHex(s string) (string, bool) {
	if len(s) != pinHexLength {
		return "", false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", false
	}
	return strings.ToLower(s), true
}

// validateDiscovery checks a discovery section, deriving each node's address
// through derive. It returns the nodes a reader may use in the section, and
// beside it every node the section knows, as storedState describes. The
// section is nil, with no error, when its own time is ahead of now, and the
// known nodes are still returned. A last_failure outside the enumeration makes
// the file unusable, and so does an empty outcome unless the section carries a
// last_failure, which is a section an attempt created.
//
// held is nil on read. A writer passes it to keep in the section a node that
// would be dropped only for its confirmation time, which is a node held reports
// true for. The merge uses it under verify_ssl false for a known node that a
// result carries with its stored pins and its stored confirmation, so the node
// stays known, and its pins keep binding, after the pool stops loading it.
func validateDiscovery(
	d *DiscoverySection, derive derivation, now time.Time, held func(DiscoveredNode) bool,
) (*DiscoverySection, []DiscoveredNode, []Drop, error) {
	if d.LastFailure != "" && !d.LastFailure.Valid() {
		return nil, nil, nil, fmt.Errorf(
			"%w: discovery last_failure is not one of the enumerated values", ErrPayloadInvalid)
	}
	// A section an attempt created has no outcome, because no discovery wrote
	// a result, and it carries the failure that ended the attempt.
	if !d.Outcome.Valid() && (d.Outcome != "" || d.LastFailure == "") {
		return nil, nil, nil, fmt.Errorf("%w: discovery outcome is not one of the enumerated values", ErrPayloadInvalid)
	}
	if len(d.Nodes) > 0 && !d.Outcome.holdsNodes() {
		return nil, nil, nil, fmt.Errorf("%w: discovery lists nodes under an outcome that holds none",
			ErrPayloadInvalid)
	}
	if len(d.Nodes) > MaxNodes {
		return nil, nil, nil, fmt.Errorf("%w: discovery holds %d nodes, more than %d",
			ErrPayloadInvalid, len(d.Nodes), MaxNodes)
	}
	out := &DiscoverySection{
		DiscoveredAt:     d.DiscoveredAt.UTC(),
		NextRefreshAfter: d.NextRefreshAfter.UTC(),
		Outcome:          d.Outcome,
		LastFailure:      d.LastFailure,
		PinFailedAt:      d.PinFailedAt.UTC(),
		Quorate:          d.Quorate,
		Nodes:            make([]DiscoveredNode, 0, len(d.Nodes)),
	}
	var drops []Drop
	var known []DiscoveredNode
	names := make(map[string]struct{}, len(d.Nodes))
	for i, node := range d.Nodes {
		record := "node " + strconv.Itoa(i)
		kept, reason, err := validateNode(node)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: %s: %w", ErrPayloadInvalid, record, err)
		}
		lowered := strings.ToLower(node.Name)
		if _, dup := names[lowered]; dup {
			return nil, nil, nil, fmt.Errorf("%w: %s repeats an earlier node name", ErrPayloadInvalid, record)
		}
		names[lowered] = struct{}{}
		if reason == "" {
			known = append(known, kept)
			reason = nodeUse(kept, derive, now)
			if (reason == DropExpired || reason == DropFuture) && held != nil && held(kept) {
				reason = ""
			}
		}
		if reason != "" {
			drops = append(drops, Drop{Record: record, Reason: reason})
			continue
		}
		out.Nodes = append(out.Nodes, kept)
	}
	if d.DiscoveredAt.After(now.Add(FutureSkew)) {
		return nil, known, append(drops, Drop{Record: "discovery", Reason: DropFuture}), nil
	}
	if d.NextRefreshAfter.After(now.Add(MaxRefreshWait + FutureSkew)) {
		out.NextRefreshAfter = time.Time{}
		drops = append(drops, Drop{Record: "next_refresh_after", Reason: DropFuture})
	}
	if d.PinFailedAt.After(now.Add(FutureSkew)) {
		out.PinFailedAt = time.Time{}
		drops = append(drops, Drop{Record: "pin_failed_at", Reason: DropFuture})
	}
	return out, known, drops, nil
}

// validateNode checks one node's format and returns it in its normalized
// spelling. A non-nil error makes the file unusable. A name a resolver could
// misread drops this node alone, and it is not a node the file knows.
func validateNode(n DiscoveredNode) (DiscoveredNode, DropReason, error) {
	if err := ValidateNodeName(n.Name); err != nil {
		if errors.Is(err, ErrNodeNameAmbiguous) {
			return DiscoveredNode{}, DropAmbiguousName, nil
		}
		return DiscoveredNode{}, "", errors.New("name fails the pve-node format")
	}
	if n.Port < 1 || n.Port > 65535 {
		return DiscoveredNode{}, "", errors.New("port is out of range")
	}
	address, err := validateNodeAddress(n)
	if err != nil {
		return DiscoveredNode{}, "", err
	}
	pins, err := validatePins(n.Pins)
	if err != nil {
		return DiscoveredNode{}, "", err
	}
	return DiscoveredNode{
		Name:        n.Name,
		Address:     address,
		Port:        n.Port,
		Pins:        pins,
		Online:      n.Online,
		ConfirmedAt: n.ConfirmedAt.UTC(),
	}, "", nil
}

// nodeUse reports why a node the file knows is not one a reader may use now,
// or "" when it is. The node's address and port must be what derive gives for
// its name, that address must not be one of the key's configured endpoints,
// and its confirmation must be at most NodeTTL old and at most FutureSkew
// ahead.
func nodeUse(n DiscoveredNode, derive derivation, now time.Time) DropReason {
	if reason := derive.check(n); reason != "" {
		return reason
	}
	switch age := now.Sub(n.ConfirmedAt); {
	case n.ConfirmedAt.After(now.Add(FutureSkew)):
		return DropFuture
	case age > NodeTTL:
		return DropExpired
	}
	return ""
}

// validateNodeAddress checks that the node's address is a host that passes
// the endpoint grammar with the node's port. It returns the address in its
// normalized spelling. Whether it is the address the seed derives is
// nodeUse's check, because a failure there drops the node alone.
func validateNodeAddress(n DiscoveredNode) (string, error) {
	host, _, ok := config.NormalizeEndpoint(net.JoinHostPort(n.Address, strconv.Itoa(n.Port)))
	if !ok || !strings.EqualFold(host, n.Address) {
		return "", errors.New("address does not pass the endpoint grammar")
	}
	return host, nil
}

// derivation is the address derivation under a key's seed, which is its first
// endpoint. err is why NewDeriver refused the seed, and then no node derives.
// configured holds the key's endpoints in their normalized host:port spelling,
// so a node that derives to one of them is left out.
type derivation struct {
	deriver    *Deriver
	err        error
	configured []string
}

// keyDerivation prepares the derivation for key's seed and endpoints.
func keyDerivation(key Key) derivation {
	if len(key.Endpoints) == 0 {
		return derivation{err: ErrSeedEndpoint}
	}
	d, err := NewDeriver(key.Endpoints[0])
	configured := make([]string, 0, len(key.Endpoints))
	for _, endpoint := range key.Endpoints {
		if host, port, ok := config.NormalizeEndpoint(endpoint); ok {
			configured = append(configured, net.JoinHostPort(host, strconv.Itoa(port)))
		}
	}
	return derivation{deriver: d, err: err, configured: configured}
}

// check reports why n's address and port are not what the derivation gives
// for its name, or "" when they are. The host is compared without regard to
// case, because a node keeps its own spelling. A node whose derived address is
// one of the key's configured endpoints is DropConfiguredEndpoint, because the
// pool already holds that endpoint. That covers the seed's own node whenever
// its name derives to the seed. A seed whose host is not its node's name, such
// as an IP literal, gives the cache no way to name that node, and the
// discovery worker leaves it out by the local flag of /cluster/status.
func (d derivation) check(n DiscoveredNode) DropReason {
	err := d.err
	var address string
	if err == nil {
		address, err = d.deriver.Address(n.Name)
	}
	switch {
	case errors.Is(err, ErrPublicSuffix):
		return DropPublicSuffix
	case err != nil:
		return DropNotDerived
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, n.Address) || port != strconv.Itoa(n.Port) {
		return DropNotDerived
	}
	if normalized, ok := normalizeMarkEndpoint(address); ok && slices.Contains(d.configured, normalized) {
		return DropConfiguredEndpoint
	}
	return ""
}

// validatePins checks a node's pin set and returns it in lowercase without
// repeats.
func validatePins(pins []string) ([]string, error) {
	if len(pins) == 0 {
		return nil, errors.New("holds no pin")
	}
	if len(pins) > MaxPinsPerNode {
		return nil, fmt.Errorf("holds more than %d pins", MaxPinsPerNode)
	}
	out := make([]string, 0, len(pins))
	for _, pin := range pins {
		lowered, ok := normalizeHex(pin)
		if !ok {
			return nil, errors.New("holds a pin that is not 32 bytes of hex")
		}
		if !slices.Contains(out, lowered) {
			out = append(out, lowered)
		}
	}
	return out, nil
}

// validateMarks keeps the marks that name an endpoint, carry an enumerated
// class, are not ahead of now, and are younger than markTTL. Keys are
// normalized, and two spellings of one endpoint keep the newer mark.
func validateMarks(marks map[string]DownMark, now time.Time, drops []Drop) (map[string]DownMark, []Drop) {
	if len(marks) == 0 {
		return nil, drops
	}
	out := make(map[string]DownMark, len(marks))
	for _, endpoint := range sortedKeys(marks) {
		mark := marks[endpoint]
		normalized, ok := normalizeMarkEndpoint(endpoint)
		switch {
		case !ok || !mark.Class.Valid():
			drops = append(drops, Drop{Record: "mark", Reason: DropInvalid})
			continue
		case mark.DownAt.After(now.Add(FutureSkew)):
			drops = append(drops, Drop{Record: "mark", Reason: DropFuture})
			continue
		case now.Sub(mark.DownAt) >= markTTL:
			drops = append(drops, Drop{Record: "mark", Reason: DropExpired})
			continue
		}
		mark.DownAt = mark.DownAt.UTC()
		if prior, seen := out[normalized]; !seen || mark.DownAt.After(prior.DownAt) {
			out[normalized] = mark
		}
	}
	if len(out) == 0 {
		return nil, drops
	}
	return out, drops
}

// normalizeMarkEndpoint returns the host:port spelling a mark is keyed by.
func normalizeMarkEndpoint(endpoint string) (string, bool) {
	if _, _, err := net.SplitHostPort(endpoint); err != nil {
		return "", false
	}
	host, port, ok := config.NormalizeEndpoint(endpoint)
	if !ok {
		return "", false
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), true
}

// validateHint checks a cold-start hint. A name that fails the pve-node format
// makes the file unusable, and a name a resolver could misread, a hint that is
// ahead of now, and a hint that is hintTTL old or older drop the hint alone.
func validateHint(h ColdStartHint, now time.Time) (*ColdStartHint, []Drop, error) {
	if err := ValidateNodeName(h.Name); err != nil {
		if errors.Is(err, ErrNodeNameAmbiguous) {
			return nil, []Drop{{Record: "cold_start", Reason: DropAmbiguousName}}, nil
		}
		return nil, nil, fmt.Errorf("%w: cold_start name fails the pve-node format", ErrPayloadInvalid)
	}
	switch {
	case h.ProbedAt.After(now.Add(FutureSkew)):
		return nil, []Drop{{Record: "cold_start", Reason: DropFuture}}, nil
	case now.Sub(h.ProbedAt) >= hintTTL:
		return nil, []Drop{{Record: "cold_start", Reason: DropExpired}}, nil
	}
	return &ColdStartHint{Name: h.Name, ProbedAt: h.ProbedAt.UTC()}, nil, nil
}

// sortedKeys returns the keys of marks in order, so the same input drops and
// keeps the same records every time.
func sortedKeys[V any](marks map[string]V) []string {
	keys := make([]string, 0, len(marks))
	for k := range marks {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
