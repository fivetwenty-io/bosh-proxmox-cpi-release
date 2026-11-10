package endpointcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// RefreshInterval is how long a discovery that found nodes counts before the
// next one is due. A discovery that found none waits MaxRefreshWait instead.
const RefreshInterval = time.Hour

// ErrUnconfiguredSource means an observation did not come through one of the
// key's configured endpoints, or did not say which endpoint it came through.
// Only a configured endpoint's answer may change what the cache trusts, so a
// discovered node can never vouch for itself. It matches ErrInvalidChange, and
// nothing is written.
var ErrUnconfiguredSource = privatefile.Sentinel(
	"endpoint cache: discovery result did not come through a configured endpoint", ErrInvalidChange)

// Observation is what one discovery through a configured endpoint saw, before
// the trust rules judge it. The discovery worker builds it from the reads, so
// this package never depends on the client that made them.
type Observation struct {
	// Via is the configured endpoint that answered every discovery request, as
	// host:port. It must normalize to one of the key's endpoints.
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
	// node's pins changed. New nodes are added on first use, and a known
	// node the observation leaves out stops being known.
	TrustConfirmed TrustOutcome = "confirmed"
	// TrustIdentityReplaced means the identity changed under verify_ssl true,
	// so the observation replaces the stored identity, nodes, and pins.
	TrustIdentityReplaced TrustOutcome = "identity_replaced"
	// TrustIdentityRefused means the identity changed under verify_ssl false.
	// The conflict is recorded, every discovered endpoint is dropped, and
	// discovery stops until the operator deletes the file.
	TrustIdentityRefused TrustOutcome = "identity_refused"
	// TrustPinsReplaced means a known node's pin set changed under verify_ssl
	// true, so the observation's pins replace the stored ones.
	TrustPinsReplaced TrustOutcome = "pins_replaced"
	// TrustPinsRefused means a known node's pin set changed under verify_ssl
	// false. Nothing is written, so every known node keeps its stored pins.
	TrustPinsRefused TrustOutcome = "pins_refused"
	// TrustConflictStands means the stored identity already holds a refused
	// change, so every observation is refused until the file is deleted.
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
	// accepted, sets Conflict when a changed identity is refused under
	// verify_ssl false, and is empty when the decision records nothing. Merge
	// accepts every Change a decision carries for the stored state it was
	// decided against. Inside UpdatePayload an empty Change writes the stored
	// file back as it was read, which changes no trust record, and a caller
	// that wants no write at all returns an error from its build function.
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
	// PinsChanged names the known nodes whose pin sets the observation
	// changes, in the observation's order. Under verify_ssl true the new sets
	// replace the old ones, and under verify_ssl false they are refused. It is
	// empty when the identity changed, because the pins are then not compared.
	PinsChanged []string
}

// Writes reports whether the decision has something to record.
func (d TrustDecision) Writes() bool { return d.Change.Result != nil || d.Change.Conflict }

// DecideTrust applies the trust rules to an observation and returns what to
// record. stored is the view UpdatePayload hands its build function, so the
// decision and the merge read one state under the key's lock. key is the
// store's key, and its VerifySSL is the entry's effective verify_ssl. now is
// the time UpdatePayload was given, and it stamps the discovery, every node's
// confirmation, and an identity seen for the first time.
//
// Only a configured endpoint writes trust. An observation that names no
// endpoint, or one outside key.Endpoints, is refused with
// ErrUnconfiguredSource. An observation Merge could not write, such as a node
// name that fails the pve-node format, a zero identity or pin, or nodes under
// an outcome that holds none, is refused with ErrInvalidChange, and so is a
// stored view that belongs to another key. A node whose name is ambiguous or
// whose address the seed does not derive is left out, as Merge leaves it out.
//
// While the stored identity holds a conflict, every observation is refused,
// the discovered endpoints are dropped, and the decision logs at Warn. With
// verify_ssl true an identity change replaces the stored identity, nodes, and
// pins, drops the discovered endpoints the pool loaded, and logs at Error, and
// a changed pin set for a known node replaces the old one. With verify_ssl
// false the first observation records the identity and the pins. A later
// identity change is recorded as a conflict, drops the discovered endpoints,
// and logs at Error. A changed pin set for a known node, including a node past
// NodeTTL that the pool no longer loads, refuses the whole observation and
// logs at Warn, so the node keeps its stored pins. A node the file does not
// know is added on first use, and a known node the observation leaves out is
// removed, so it stops being known and is added on first use if it returns.
func DecideTrust(stored Stored, key Key, obs Observation, now time.Time) (TrustDecision, error) {
	if now.IsZero() {
		return TrustDecision{}, fmt.Errorf("%w: trust decision needs a clock", ErrInvalidChange)
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
	decision := TrustDecision{ObservedRootCA: cluster.RootCASHA256}
	if prior != nil {
		decision.StoredRootCA = prior.RootCASHA256
		if prior.IdentityConflict {
			decision.Outcome = TrustConflictStands
			decision.Log = TrustLogWarn
			decision.DropDiscovered = true
			return decision, nil
		}
	}

	if prior != nil && prior.RootCASHA256 != cluster.RootCASHA256 {
		decision.Log = TrustLogError
		decision.DropDiscovered = true
		if !key.VerifySSL {
			decision.Outcome = TrustIdentityRefused
			decision.Change = Change{Conflict: true}
			return decision, nil
		}
		decision.Outcome = TrustIdentityReplaced
		decision.Change = Change{Result: &DiscoveryResult{Cluster: cluster, Discovery: built}}
		return decision, nil
	}

	decision.PinsChanged = changedPins(stored.Known, section.Nodes)
	switch {
	case len(decision.PinsChanged) > 0 && !key.VerifySSL:
		decision.Outcome = TrustPinsRefused
		decision.Log = TrustLogWarn
		return decision, nil
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
	section, _, _, err = validateDiscovery(&built, keyDerivation(key), at)
	if err != nil {
		return ClusterIdentity{}, DiscoverySection{}, nil, fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
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

// changedPins returns the names of the nodes in observed whose pin sets differ
// from those of the known node with the same name, compared without regard to
// case, in observed's order. A node known does not hold is new, not changed.
func changedPins(known, observed []DiscoveredNode) []string {
	pins := make(map[string][]string, len(known))
	for _, node := range known {
		pins[strings.ToLower(node.Name)] = node.Pins
	}
	var changed []string
	for _, node := range observed {
		if stored, ok := pins[strings.ToLower(node.Name)]; ok && !samePins(stored, node.Pins) {
			changed = append(changed, node.Name)
		}
	}
	return changed
}
