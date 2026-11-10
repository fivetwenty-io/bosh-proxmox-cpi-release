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
// key, at time now. It returns a copy that holds only what a reader may trust,
// along with the records it dropped. It does not look at the reader's az_map,
// because a writer keeps a hint that another entry sharing the key wrote. See
// ForReader for that check.
//
// A payload is unusable, and Validate returns ErrPayloadInvalid, when its key
// differs from key (ErrKeyMismatch), when a node name fails the pve-node
// format, when a node's address does not pass the endpoint grammar or does not
// begin with the node's name, when its port is out of range, when a pin is not
// 32 bytes of hex or a node holds no pin or more than MaxPinsPerNode, when the
// file holds more than MaxNodes nodes or two nodes with one name, when the
// cluster identity is not 32 bytes of hex or a discovery lists nodes without
// one, and when an outcome or a hint's name is not valid.
//
// A single record is dropped, and the rest of the file stands, when a node name
// is all digits, a 0x hex spelling, or a loopback alias, when a node's
// confirmation is more than NodeTTL old or more than FutureSkew ahead, when a
// mark is older than MarkTTL, ahead, or names something that is not an
// endpoint or a class that is not enumerated, and when the hint is older than
// HintTTL or ahead. A discovery whose own time is ahead is dropped whole, and
// a next-refresh time further ahead than MaxRefreshWait is dropped so the
// refresh is due now.
func Validate(p *Payload, key Key, now time.Time) (*Payload, []Drop, error) {
	if p == nil {
		return nil, nil, fmt.Errorf("%w: no payload", ErrPayloadInvalid)
	}
	if !p.Key.Equal(key) {
		return nil, nil, ErrKeyMismatch
	}
	out := &Payload{Key: key}
	out.Key.Endpoints = slices.Clone(key.Endpoints)
	var drops []Drop

	if p.Cluster != nil {
		cluster, err := validateCluster(*p.Cluster)
		if err != nil {
			return nil, nil, err
		}
		out.Cluster = &cluster
	}
	if p.Discovery != nil {
		section, found, err := validateDiscovery(p.Discovery, now)
		if err != nil {
			return nil, nil, err
		}
		drops = append(drops, found...)
		if section != nil && len(section.Nodes) > 0 && out.Cluster == nil {
			return nil, nil, fmt.Errorf("%w: discovery lists nodes without a cluster identity", ErrPayloadInvalid)
		}
		out.Discovery = section
	}
	out.Marks, drops = validateMarks(p.Marks, now, drops)
	if p.ColdStart != nil {
		hint, found, err := validateHint(*p.ColdStart, now)
		if err != nil {
			return nil, nil, err
		}
		drops = append(drops, found...)
		out.ColdStart = hint
	}
	return out, drops, nil
}

// ForReader applies the one check that depends on the reader: a hint whose
// node the reader's az_map does not name is dropped and the rest of the file
// stands, because two entries that reach one cluster share a key and can carry
// different placement. azNodes are the node names of the reader's effective
// az_map, compared exactly. p is not changed.
func ForReader(p *Payload, azNodes []string) (*Payload, []Drop) {
	if p == nil || p.ColdStart == nil || slices.Contains(azNodes, p.ColdStart.Name) {
		return p, nil
	}
	out := p.clone()
	out.ColdStart = nil
	return out, []Drop{{Record: "cold_start", Reason: DropNotInAZMap}}
}

// validateCluster checks the cluster identity.
func validateCluster(c ClusterIdentity) (ClusterIdentity, error) {
	root, ok := normalizeHex(c.RootCASHA256)
	if !ok {
		return ClusterIdentity{}, fmt.Errorf("%w: cluster root CA digest is not 32 bytes of hex", ErrPayloadInvalid)
	}
	c.RootCASHA256 = root
	c.ObservedAt = c.ObservedAt.UTC()
	if c.ObservedVia != "" {
		if _, _, ok := config.NormalizeEndpoint(c.ObservedVia); !ok {
			return ClusterIdentity{}, fmt.Errorf("%w: cluster observed_via is not an endpoint", ErrPayloadInvalid)
		}
	}
	return c, nil
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

// validateDiscovery checks a discovery section. It returns a nil section, with
// no error, when the section's own time is ahead of now.
func validateDiscovery(d *DiscoverySection, now time.Time) (*DiscoverySection, []Drop, error) {
	if !d.Outcome.Valid() {
		return nil, nil, fmt.Errorf("%w: discovery outcome is not one of the enumerated values", ErrPayloadInvalid)
	}
	if len(d.Nodes) > MaxNodes {
		return nil, nil, fmt.Errorf("%w: discovery holds %d nodes, more than %d", ErrPayloadInvalid, len(d.Nodes), MaxNodes)
	}
	out := &DiscoverySection{
		DiscoveredAt:     d.DiscoveredAt.UTC(),
		NextRefreshAfter: d.NextRefreshAfter.UTC(),
		Outcome:          d.Outcome,
		Quorate:          d.Quorate,
		Nodes:            make([]DiscoveredNode, 0, len(d.Nodes)),
	}
	var drops []Drop
	names := make(map[string]struct{}, len(d.Nodes))
	for i, node := range d.Nodes {
		record := "node " + strconv.Itoa(i)
		kept, reason, err := validateNode(node, now)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %w", ErrPayloadInvalid, record, err)
		}
		lowered := strings.ToLower(node.Name)
		if _, dup := names[lowered]; dup {
			return nil, nil, fmt.Errorf("%w: %s repeats an earlier node name", ErrPayloadInvalid, record)
		}
		names[lowered] = struct{}{}
		if reason != "" {
			drops = append(drops, Drop{Record: record, Reason: reason})
			continue
		}
		out.Nodes = append(out.Nodes, kept)
	}
	if d.DiscoveredAt.After(now.Add(FutureSkew)) {
		return nil, append(drops, Drop{Record: "discovery", Reason: DropFuture}), nil
	}
	if d.NextRefreshAfter.After(now.Add(MaxRefreshWait + FutureSkew)) {
		out.NextRefreshAfter = time.Time{}
		drops = append(drops, Drop{Record: "next_refresh_after", Reason: DropFuture})
	}
	return out, drops, nil
}

// validateNode checks one node. A non-nil error makes the file unusable. A
// non-empty reason drops this node alone.
func validateNode(n DiscoveredNode, now time.Time) (DiscoveredNode, DropReason, error) {
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
	kept := DiscoveredNode{
		Name:        n.Name,
		Address:     address,
		Port:        n.Port,
		Pins:        pins,
		Online:      n.Online,
		ConfirmedAt: n.ConfirmedAt.UTC(),
	}
	switch age := now.Sub(n.ConfirmedAt); {
	case n.ConfirmedAt.After(now.Add(FutureSkew)):
		return DiscoveredNode{}, DropFuture, nil
	case age > NodeTTL:
		return DiscoveredNode{}, DropExpired, nil
	}
	return kept, "", nil
}

// validateNodeAddress checks that the node's address passes the endpoint
// grammar with the node's port, and that it is the node's name or begins with
// the name and a dot, which is all the derivation produces. It returns the
// address in its normalized spelling.
func validateNodeAddress(n DiscoveredNode) (string, error) {
	host, _, ok := config.NormalizeEndpoint(net.JoinHostPort(n.Address, strconv.Itoa(n.Port)))
	if !ok || !strings.EqualFold(host, n.Address) {
		return "", errors.New("address does not pass the endpoint grammar")
	}
	name := strings.ToLower(n.Name)
	if host != name && !strings.HasPrefix(host, name+".") {
		return "", errors.New("address does not begin with the node name")
	}
	return host, nil
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
// class, are not ahead of now, and are younger than MarkTTL. Keys are
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
		case now.Sub(mark.DownAt) >= MarkTTL:
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
// ahead of now, and a hint that is HintTTL old or older drop the hint alone.
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
	case now.Sub(h.ProbedAt) >= HintTTL:
		return nil, []Drop{{Record: "cold_start", Reason: DropExpired}}, nil
	}
	return &ColdStartHint{Name: h.Name, ProbedAt: h.ProbedAt.UTC()}, nil, nil
}

// sortedKeys returns the keys of marks in order, so the same input drops and
// keeps the same records every time.
func sortedKeys(marks map[string]DownMark) []string {
	keys := make([]string, 0, len(marks))
	for k := range marks {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
