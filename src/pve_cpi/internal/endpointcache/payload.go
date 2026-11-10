package endpointcache

import (
	"time"
)

const (
	// MaxNodes is the most discovered nodes a cache file holds.
	MaxNodes = 64
	// MaxPinsPerNode is the most pins one node holds. The set is the /nodes
	// fingerprint plus the pve-ssl.pem and pveproxy-ssl.pem fingerprints.
	MaxPinsPerNode = 3

	// MarkTTL is how long a down mark counts. A mark that is this old or older
	// is dropped.
	MarkTTL = 2 * time.Minute
	// HintTTL is how long a cold-start hint counts, measured from its probe. A
	// hint that is this old or older is dropped.
	HintTTL = time.Hour
	// NodeTTL is how long a discovered node stays loaded without a discovery
	// through a configured endpoint confirming it.
	NodeTTL = 24 * time.Hour
	// FutureSkew is how far ahead of the reader's clock a record's timestamp
	// may be before the record is dropped. A record further ahead is dropped,
	// not clamped, so a forged or skewed time cannot make it last forever.
	FutureSkew = 30 * time.Second
	// MaxRefreshWait is the longest wait the next-refresh time may ask for. It
	// is the 24-hour back-off for a result with no candidates.
	MaxRefreshWait = 24 * time.Hour
)

// DiscoveryOutcome says how the last discovery ended. It is an enumeration and
// never holds raw error text.
type DiscoveryOutcome string

// Discovery outcomes.
const (
	// OutcomeOK means discovery found nodes.
	OutcomeOK DiscoveryOutcome = "ok"
	// OutcomeNoStatus means discovery found nodes without a usable
	// /cluster/status answer, so the nodes are in name order.
	OutcomeNoStatus DiscoveryOutcome = "no_status"
	// OutcomeNoCandidates means no derived name resolved.
	OutcomeNoCandidates DiscoveryOutcome = "no_candidates"
	// OutcomeStandalone means a configured endpoint reported no peers.
	OutcomeStandalone DiscoveryOutcome = "standalone"
	// OutcomePublicSuffix means the seed's domain is a public suffix, so no
	// address was derived.
	OutcomePublicSuffix DiscoveryOutcome = "public_suffix"
)

// Valid reports whether o is one of the enumerated outcomes.
func (o DiscoveryOutcome) Valid() bool {
	switch o {
	case OutcomeOK, OutcomeNoStatus, OutcomeNoCandidates, OutcomeStandalone, OutcomePublicSuffix:
		return true
	}
	return false
}

// MarkClass says why an endpoint was marked down. It is an enumeration and
// never holds raw error text.
type MarkClass string

// Mark classes.
const (
	MarkDial         MarkClass = "dial"
	MarkDNS          MarkClass = "dns"
	MarkTLSHandshake MarkClass = "tls_handshake"
	MarkTLSVerify    MarkClass = "tls_verify"
	MarkPin          MarkClass = "pin"
)

// Valid reports whether c is one of the enumerated classes.
func (c MarkClass) Valid() bool {
	switch c {
	case MarkDial, MarkDNS, MarkTLSHandshake, MarkTLSVerify, MarkPin:
		return true
	}
	return false
}

// Payload is the content of one cache file. The envelope that carries it
// holds the HMAC. A file holds either a discovery section or a cold-start
// hint in practice, and marks alongside either.
type Payload struct {
	// Key names the configuration and credential the file belongs to. A
	// reader refuses a file whose key differs from its own.
	Key Key `json:"key"`
	// Cluster is the identity of the cluster the configured endpoints reached.
	Cluster *ClusterIdentity `json:"cluster,omitempty"`
	// Discovery is the last discovery through a configured endpoint.
	Discovery *DiscoverySection `json:"discovery,omitempty"`
	// Marks are the endpoints recently seen down, by normalized host:port.
	Marks map[string]DownMark `json:"marks,omitempty"`
	// ColdStart is an ordering hint from a cold start that succeeded.
	ColdStart *ColdStartHint `json:"cold_start,omitempty"`
}

// ClusterIdentity is the cluster's identity as the last discovery observed it.
type ClusterIdentity struct {
	// RootCASHA256 is the full SHA-256 of the root CA's DER bytes, in hex.
	RootCASHA256 string `json:"root_ca_sha256"`
	// ObservedAt is when the identity was read. It is informational and gates
	// nothing, so the future-timestamp rule does not apply to it.
	ObservedAt time.Time `json:"observed_at"`
	// ObservedVia is the configured endpoint that answered.
	ObservedVia string `json:"observed_via"`
	// IdentityConflict is set when a changed identity was refused under
	// verify_ssl false. It stays set until the operator deletes the file.
	IdentityConflict bool `json:"identity_conflict"`
}

// DiscoverySection is the last discovery's result.
type DiscoverySection struct {
	// DiscoveredAt is when the discovery ran.
	DiscoveredAt time.Time `json:"discovered_at"`
	// NextRefreshAfter is the earliest time the next discovery is due.
	NextRefreshAfter time.Time `json:"next_refresh_after"`
	// Outcome says how the discovery ended.
	Outcome DiscoveryOutcome `json:"outcome"`
	// Quorate is whether /cluster/status reported quorum, which decides
	// whether the online flags are trustworthy.
	Quorate bool `json:"quorate"`
	// Nodes are the discovered nodes, in the order the walk should try them.
	Nodes []DiscoveredNode `json:"nodes"`
}

// DiscoveredNode is one cluster node discovery found.
type DiscoveredNode struct {
	// Name is the node's name, which passes ValidateNodeName.
	Name string `json:"name"`
	// Address is the host derived from the name, with no port.
	Address string `json:"address"`
	// Port is the seed's port.
	Port int `json:"port"`
	// Pins are the node's certificate fingerprints, each 32 bytes in hex.
	Pins []string `json:"pins"`
	// Online is whether a quorate /cluster/status reported the node online.
	Online bool `json:"online"`
	// ConfirmedAt is when a discovery through a configured endpoint last
	// saw the node.
	ConfirmedAt time.Time `json:"confirmed_at"`
}

// DownMark records that an endpoint failed in the connect phase.
type DownMark struct {
	// DownAt is when the failure happened.
	DownAt time.Time `json:"down_at"`
	// Class says which kind of failure it was.
	Class MarkClass `json:"class"`
}

// ColdStartHint records the node a cold start accepted. It holds a name and a
// time only, never an address, a port, a pin, or an identity, because a reader
// derives the address and port from the name, so a hint cannot name an address
// the derivation would not produce.
type ColdStartHint struct {
	// Name is the node's name, which passes ValidateNodeName.
	Name string `json:"name"`
	// ProbedAt is when the node passed its probe.
	ProbedAt time.Time `json:"probed_at"`
}

// clone returns a deep copy of p.
func (p *Payload) clone() *Payload {
	if p == nil {
		return nil
	}
	out := *p
	out.Key.Endpoints = append([]string(nil), p.Key.Endpoints...)
	if p.Cluster != nil {
		cluster := *p.Cluster
		out.Cluster = &cluster
	}
	if p.Discovery != nil {
		out.Discovery = p.Discovery.clone()
	}
	if p.Marks != nil {
		out.Marks = make(map[string]DownMark, len(p.Marks))
		for endpoint, mark := range p.Marks {
			out.Marks[endpoint] = mark
		}
	}
	if p.ColdStart != nil {
		hint := *p.ColdStart
		out.ColdStart = &hint
	}
	return &out
}

// clone returns a deep copy of d.
func (d *DiscoverySection) clone() *DiscoverySection {
	if d == nil {
		return nil
	}
	out := *d
	out.Nodes = make([]DiscoveredNode, len(d.Nodes))
	for i, node := range d.Nodes {
		node.Pins = append([]string(nil), node.Pins...)
		out.Nodes[i] = node
	}
	return &out
}

// empty reports whether p holds nothing beyond its key.
func (p *Payload) empty() bool {
	return p.Cluster == nil && p.Discovery == nil && len(p.Marks) == 0 && p.ColdStart == nil
}
