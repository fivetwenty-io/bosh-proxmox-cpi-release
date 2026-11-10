package endpointcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// RefreshInterval is how long a discovery that found nodes counts before the
// next one is due. A discovery that found none waits MaxRefreshWait instead,
// and so does a discovery the trust rules refused for its identity.
const RefreshInterval = time.Hour

// PinRefreshFloor is how old the last discovery attempt must be before a pin
// failure on a discovered endpoint makes a refresh due, so a spoofed DNS
// answer can force a refresh at most this often.
const PinRefreshFloor = 10 * time.Minute

// RefreshDue reports whether a discovery refresh is due for p, the payload
// ReadPayload returned, at now. pinFailed is whether a discovered endpoint
// failed its pin in this or another process since the last discovery.
//
// It is false while the stored identity holds a conflict, because discovery
// stops until the operator deletes the file. Otherwise it is true when the
// file holds no node list, which includes a nil p and a section that
// validation dropped for its time, and when next_refresh_after has passed,
// which includes one that validation dropped for being too far ahead. A pin
// failure makes a refresh due early only when the last attempt, which is the
// section's discovered_at, is more than PinRefreshFloor old. It never holds
// back a refresh that is already due.
func RefreshDue(p *Payload, now time.Time, pinFailed bool) bool {
	if p != nil && p.Cluster != nil && p.Cluster.IdentityConflict {
		return false
	}
	if p == nil || p.Discovery == nil {
		return true
	}
	if !now.Before(p.Discovery.NextRefreshAfter) {
		return true
	}
	return pinFailed && now.Sub(p.Discovery.DiscoveredAt) > PinRefreshFloor
}

// ErrUnconfiguredSource means an observation did not come through one of the
// key's configured endpoints, or did not say which endpoint it came through.
// Only a configured endpoint's answer may change what the cache trusts. The
// check reads the worker's label in Observation.Via, so the guarantee that a
// discovered node never vouches for itself also rests on the discovery
// worker sending every discovery request configured-only, as that field
// describes. It matches ErrInvalidChange, and nothing is written.
var ErrUnconfiguredSource = privatefile.Sentinel(
	"endpoint cache: discovery result did not come through a configured endpoint", ErrInvalidChange)

// Observation is what one discovery through a configured endpoint saw, before
// the trust rules judge it. The discovery worker builds it from the reads, so
// this package never depends on the client that made them.
type Observation struct {
	// Via is the configured endpoint that answered every discovery request, as
	// host:port. It must normalize to one of the key's endpoints. The decision
	// trusts this label and cannot see the transport. The label is true only
	// because the discovery worker marks every discovery request's context
	// with endpointpool.WithConfiguredOnly, so the pool walks configured
	// endpoints alone for it, and records which configured endpoint answered.
	Via string
	// RootCA is the cluster's identity, the SHA-256 of the root CA's DER bytes.
	// The zero value is refused.
	RootCA [sha256.Size]byte
	// Outcome says how the discovery ended.
	Outcome DiscoveryOutcome
	// Quorate is whether /cluster/status reported quorum.
	Quorate bool
	// Nodes are the discovered peers in the order the walk should try them.
	Nodes []ObservedNode
}

// ObservedNode is one peer an observation found.
type ObservedNode struct {
	// Name is the node's name, which passes ValidateNodeName.
	Name string
	// Address is the host:port derived from the name under the key's seed.
	Address string
	// Pins are the node's certificate fingerprints. The zero value is
	// refused.
	Pins [][sha256.Size]byte
	// Online is the flag discovery ordered the node by.
	Online bool
}

// TrustOutcome names the trust rule that decided an observation. It is an
// enumeration and never holds raw error text.
type TrustOutcome string

// Trust outcomes.
const (
	// TrustFirstUse means the file held no identity, so the observation's
	// identity and pins are recorded.
	TrustFirstUse TrustOutcome = "first_use"
	// TrustConfirmed means the identity matches the stored one and no known
	// node's pins changed, or under verify_ssl false a known node's pins only
	// grew, so it keeps its stored pins and adds the new ones. New nodes are
	// added on first use, and a known node the observation leaves out stops
	// being known.
	TrustConfirmed TrustOutcome = "confirmed"
	// TrustIdentityReplaced means the identity changed under verify_ssl true,
	// so the observation replaces the stored identity, nodes, and pins.
	TrustIdentityReplaced TrustOutcome = "identity_replaced"
	// TrustIdentityRefused means the identity changed under verify_ssl false.
	// The conflict is recorded with an attempt that moves the next refresh
	// MaxRefreshWait ahead, every discovered endpoint is dropped, and
	// discovery stops until the operator deletes the file.
	TrustIdentityRefused TrustOutcome = "identity_refused"
	// TrustPinsReplaced means a known node's pin set changed under verify_ssl
	// true, so the observation's pins replace the stored ones.
	TrustPinsReplaced TrustOutcome = "pins_replaced"
	// TrustPinsRefused means a known node's observed pins leave out a stored
	// pin under verify_ssl false. That node keeps its stored pins. It is
	// confirmed now when the observed pins share one of the stored pins, and
	// it keeps its stored confirmation when they share none, so it stops
	// loading NodeTTL after that confirmation but stays known. The rest of the
	// observation is written as observed, and the refresh is due again after
	// RefreshInterval.
	TrustPinsRefused TrustOutcome = "pins_refused"
	// TrustConflictStands means the stored identity already holds a refused
	// change, so every observation is refused until the file is deleted. The
	// decision records an attempt that moves the next refresh MaxRefreshWait
	// ahead and changes nothing else.
	TrustConflictStands TrustOutcome = "conflict_stands"
)

// TrustLog is the level the trust rules give a decision's own log line.
type TrustLog uint8

// Trust log outcomes.
const (
	// TrustLogNone means the decision adds no line of its own. The caller's
	// discovery lines still apply.
	TrustLogNone TrustLog = iota
	// TrustLogWarn means the caller logs the decision at Warn.
	TrustLogWarn
	// TrustLogError means the caller logs the decision at Error.
	TrustLogError
)

// Level returns the slog level of l, and false for TrustLogNone.
func (l TrustLog) Level() (slog.Level, bool) {
	switch l {
	case TrustLogWarn:
		return log.LevelWarn, true
	case TrustLogError:
		return log.LevelError, true
	}
	return log.LevelDebug, false
}

// TrustDecision is what the trust rules make of one observation.
type TrustDecision struct {
	// Outcome names the rule that decided.
	Outcome TrustOutcome
	// Change is what to record. It carries a Result when the observation is
	// written, which includes a refused pin change, and sets Conflict with an
	// Attempt when a changed identity is refused under verify_ssl false. It
	// carries only an Attempt while a conflict stands. Merge accepts every
	// Change a decision carries for the stored state it was decided against
	// at the same clock. Inside UpdatePayload an empty Change leaves the
	// identity, the discovery section, and the pins as stored, while the merge
	// still removes expired marks and an expired hint. A caller that wants no
	// write at all returns an error from its build function.
	Change Change
	// Log is the level of the decision's own line.
	Log TrustLog
	// DropDiscovered is set when the pool must stop using every discovered
	// endpoint for the rest of the call, because they belong to an identity
	// the configured endpoints no longer report.
	DropDiscovered bool
	// StoredRootCA is the stored identity in lowercase hex, or empty when the
	// file held none.
	StoredRootCA string
	// ObservedRootCA is the observation's identity in lowercase hex.
	ObservedRootCA string
	// Via is the configured endpoint the observation came through, in the
	// key's spelling, for the caller's log line. It is set on every outcome.
	Via string
	// PinsChanged names the known nodes whose pin sets the observation
	// changes, in the observation's order. Under verify_ssl true it names
	// every node whose set differs, and the new sets replace the old ones.
	// Under verify_ssl false it names every node whose observed set leaves out
	// a stored pin, and those nodes keep their stored pins, while a set that
	// only grew is accepted and not named. It is empty when the identity
	// changed, because the pins are then not compared.
	PinsChanged []string
}

// Writes reports whether the decision has something to record.
func (d TrustDecision) Writes() bool {
	return d.Change.Result != nil || d.Change.Conflict || d.Change.Attempt != nil
}

// DecideTrust applies the trust rules to an observation and returns what to
// record. stored is the view UpdatePayload hands its build function, so the
// decision and the merge read one state under the key's lock. key is the
// store's key, and its VerifySSL is the entry's effective verify_ssl. now is
// stored.Now, the clock UpdatePayload was given, and it stamps the discovery,
// every node's confirmation, and an identity seen for the first time. A view
// whose Now is set and differs from now is refused with ErrInvalidChange,
// because the merge would check the decision's stamps against another clock.
//
// Only a configured endpoint writes trust. An observation that names no
// endpoint, or one outside key.Endpoints, is refused with
// ErrUnconfiguredSource. An observation Merge could not write, such as a node
// name that fails the pve-node format, a zero identity or pin, or nodes under
// an outcome that holds none, is refused with ErrInvalidChange, and so is a
// stored view that belongs to another key. A node whose name is ambiguous,
// whose address the seed does not derive, or whose derived address is one of
// the key's endpoints is left out, as Merge leaves it out.
//
// While the stored identity holds a conflict, every observation is refused,
// the discovered endpoints are dropped, the decision logs at Warn, and it
// records an attempt that moves the next refresh MaxRefreshWait ahead. With
// verify_ssl true an identity change replaces the stored identity, nodes, and
// pins, drops the discovered endpoints the pool loaded, and logs at Error, and
// a changed pin set for a known node replaces the old one. With verify_ssl
// false the first observation records the identity and the pins. A later
// identity change is recorded as a conflict with the same attempt, drops the
// discovered endpoints, and logs at Error.
//
// Under verify_ssl false the pin rule applies to each known node on its own,
// including a node past NodeTTL that the pool no longer loads, and the rest of
// the observation is written as observed. A node whose observed pins equal its
// stored pins, or hold every stored pin and more, is confirmed now with the
// observed pins. A node whose observed pins leave out a stored pin keeps its
// stored pins and is named in PinsChanged. It is confirmed now when the two
// sets share a pin, because the endpoint still reports a stored one, and it
// keeps its stored confirmation when they share none, so it stops loading
// NodeTTL after that confirmation but stays known. Any named node makes the
// outcome pins_refused, logged at Warn, and the result still advances the
// refresh by RefreshInterval. A node the file does not know is added on first
// use, and a known node the observation leaves out is removed, so it stops
// being known and is added on first use if it returns.
func DecideTrust(stored Stored, key Key, obs Observation, now time.Time) (TrustDecision, error) {
	if now.IsZero() {
		return TrustDecision{}, fmt.Errorf("%w: trust decision needs a clock", ErrInvalidChange)
	}
	if !stored.Now.IsZero() && !stored.Now.Equal(now) {
		return TrustDecision{}, fmt.Errorf("%w: trust decision clock differs from the stored view's", ErrInvalidChange)
	}
	if stored.Usable != nil && !stored.Usable.Key.Equal(key) {
		return TrustDecision{}, fmt.Errorf("%w: %w", ErrInvalidChange, ErrKeyMismatch)
	}
	cluster, built, section, err := observedResult(obs, key, now)
	if err != nil {
		return TrustDecision{}, err
	}
	var prior *ClusterIdentity
	if stored.Usable != nil {
		prior = stored.Usable.Cluster
	}
	decision := TrustDecision{ObservedRootCA: cluster.RootCASHA256, Via: cluster.ObservedVia}
	// A refusal for the identity records the attempt, so the refresh is not
	// due again on every call while the refusal stands.
	attempt := &DiscoveryAttempt{At: now.UTC(), NextRefreshAfter: now.UTC().Add(MaxRefreshWait)}
	if prior != nil {
		decision.StoredRootCA = prior.RootCASHA256
		if prior.IdentityConflict {
			decision.Outcome = TrustConflictStands
			decision.Log = TrustLogWarn
			decision.DropDiscovered = true
			decision.Change = Change{Attempt: attempt}
			return decision, nil
		}
	}

	if prior != nil && prior.RootCASHA256 != cluster.RootCASHA256 {
		decision.Log = TrustLogError
		decision.DropDiscovered = true
		if !key.VerifySSL {
			decision.Outcome = TrustIdentityRefused
			decision.Change = Change{Conflict: true, Attempt: attempt}
			return decision, nil
		}
		decision.Outcome = TrustIdentityReplaced
		decision.Change = Change{Result: &DiscoveryResult{Cluster: cluster, Discovery: built}}
		return decision, nil
	}

	decision.PinsChanged = applyPinRule(stored.Known, section, &built, key.VerifySSL)
	switch {
	case len(decision.PinsChanged) > 0 && !key.VerifySSL:
		decision.Outcome = TrustPinsRefused
		decision.Log = TrustLogWarn
	case len(decision.PinsChanged) > 0:
		decision.Outcome = TrustPinsReplaced
	case prior == nil:
		decision.Outcome = TrustFirstUse
	default:
		decision.Outcome = TrustConfirmed
	}
	if prior != nil {
		// The identity is unchanged, and observed_at and observed_via record
		// when and where it was first seen.
		cluster.ObservedAt = prior.ObservedAt
		cluster.ObservedVia = prior.ObservedVia
	}
	decision.Change = Change{Result: &DiscoveryResult{Cluster: cluster, Discovery: built}}
	return decision, nil
}

// observedResult turns obs into the identity and the discovery section a
// Result stores, stamped with now, after the checks Merge applies to a Result.
// built is the section as the Result carries it, so Merge reports the nodes it
// leaves out. section is built as Merge reads it, holding only the nodes Merge
// writes, so the pin comparison sees what Merge sees.
func observedResult(
	obs Observation, key Key, now time.Time,
) (cluster ClusterIdentity, built DiscoverySection, section *DiscoverySection, err error) {
	if strings.TrimSpace(obs.Via) == "" {
		return ClusterIdentity{}, DiscoverySection{}, nil, ErrUnconfiguredSource
	}
	if obs.RootCA == ([sha256.Size]byte{}) {
		return ClusterIdentity{}, DiscoverySection{}, nil,
			fmt.Errorf("%w: observation carries no cluster identity", ErrInvalidChange)
	}
	at := now.UTC()
	// The digest is hex this function wrote, so validateCluster can refuse
	// only observed_via, either for not being an endpoint or, through a drop,
	// for naming one outside the key.
	cluster, found, err := validateCluster(ClusterIdentity{
		RootCASHA256: hex.EncodeToString(obs.RootCA[:]),
		ObservedAt:   at,
		ObservedVia:  obs.Via,
	}, key)
	if err != nil || len(found) > 0 {
		return ClusterIdentity{}, DiscoverySection{}, nil, ErrUnconfiguredSource
	}

	wait := RefreshInterval
	if !obs.Outcome.holdsNodes() {
		wait = MaxRefreshWait
	}
	built = DiscoverySection{
		DiscoveredAt:     at,
		NextRefreshAfter: at.Add(wait),
		Outcome:          obs.Outcome,
		Quorate:          obs.Quorate,
		Nodes:            make([]DiscoveredNode, 0, len(obs.Nodes)),
	}
	for i, node := range obs.Nodes {
		stamped, err := observedNode(node, at)
		if err != nil {
			return ClusterIdentity{}, DiscoverySection{}, nil,
				fmt.Errorf("%w: observed node %d %w", ErrInvalidChange, i, err)
		}
		built.Nodes = append(built.Nodes, stamped)
	}
	section, _, _, err = validateDiscovery(&built, keyDerivation(key), at, nil)
	if err != nil {
		return ClusterIdentity{}, DiscoverySection{}, nil, fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
	// This guard is unreachable. The section is stamped with now, and
	// validateDiscovery returns nil only for a section dated more than
	// FutureSkew ahead of now, so no test can reach it. It stays so that a
	// later change to the stamping fails closed here and never hands Merge a
	// result it would refuse.
	if section == nil {
		return ClusterIdentity{}, DiscoverySection{}, nil,
			fmt.Errorf("%w: discovery time is in the future", ErrInvalidChange)
	}
	return cluster, built, section, nil
}

// observedNode turns one observed node into the record a Result stores,
// confirmed at at. The format checks are validateDiscovery's.
func observedNode(node ObservedNode, at time.Time) (DiscoveredNode, error) {
	host, portText, err := net.SplitHostPort(node.Address)
	if err != nil {
		return DiscoveredNode{}, errors.New("address is not host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return DiscoveredNode{}, errors.New("port is not a number")
	}
	pins := make([]string, 0, len(node.Pins))
	for _, pin := range node.Pins {
		if pin == ([sha256.Size]byte{}) {
			return DiscoveredNode{}, errors.New("holds a zero pin")
		}
		pins = append(pins, hex.EncodeToString(pin[:]))
	}
	return DiscoveredNode{
		Name:        node.Name,
		Address:     host,
		Port:        port,
		Pins:        pins,
		Online:      node.Online,
		ConfirmedAt: at,
	}, nil
}

// applyPinRule compares each node of section, which holds the nodes Merge
// writes, with the known node of the same name, compared without regard to
// case, and returns the names of the known nodes whose pin sets change, in
// section's order. A node known does not hold is new, not changed.
//
// Under verify_ssl true every set that differs is a change, and built keeps
// the observed sets. Under verify_ssl false a set that holds every stored pin
// is accepted as observed, and any other set is a change. built then carries
// that node with its stored pins, confirmed now when the observed set shares
// a stored pin and with its stored confirmation when it shares none. built is
// the section the Result carries, and its nodes are in section's order with
// the nodes Merge leaves out between them.
func applyPinRule(known []DiscoveredNode, section *DiscoverySection, built *DiscoverySection, verify bool) []string {
	stored := make(map[string]DiscoveredNode, len(known))
	for _, node := range known {
		stored[strings.ToLower(node.Name)] = node
	}
	index := make(map[string]int, len(built.Nodes))
	for i, node := range built.Nodes {
		index[strings.ToLower(node.Name)] = i
	}
	var changed []string
	for _, node := range section.Nodes {
		name := strings.ToLower(node.Name)
		prior, ok := stored[name]
		if !ok || samePins(prior.Pins, node.Pins) {
			continue
		}
		if !verify && holdsPins(node.Pins, prior.Pins) {
			continue
		}
		changed = append(changed, node.Name)
		if verify {
			continue
		}
		kept := &built.Nodes[index[name]]
		kept.Pins = slices.Clone(prior.Pins)
		if !sharesPin(node.Pins, prior.Pins) {
			kept.ConfirmedAt = prior.ConfirmedAt
		}
	}
	return changed
}

// sharesPin reports whether a and b hold at least one pin in common.
func sharesPin(a, b []string) bool {
	for _, pin := range a {
		if slices.Contains(b, pin) {
			return true
		}
	}
	return false
}
