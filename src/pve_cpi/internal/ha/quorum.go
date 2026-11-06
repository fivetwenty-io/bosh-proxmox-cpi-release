package ha

import (
	"fmt"
	"sort"
)

// QDeviceStatus is what the QDevice status says about its connection.
type QDeviceStatus string

const (
	// QDeviceNone means the QDevice endpoint answered and no QDevice is configured.
	QDeviceNone QDeviceStatus = "none"

	// QDeviceConnected means a QDevice is configured and its status says it is
	// connected, so its votes participate.
	QDeviceConnected QDeviceStatus = "connected"

	// QDeviceDisconnected means a QDevice is configured and its status says it
	// is not connected, so its votes do not participate.
	QDeviceDisconnected QDeviceStatus = "disconnected"

	// QDeviceUnknown means the QDevice endpoint could not be read, or it
	// reported a QDevice whose connection state it did not state. Its votes
	// count as zero in every check.
	QDeviceUnknown QDeviceStatus = "unknown"
)

// QDevice is the profile's reading of the cluster's quorum device.
type QDevice struct {
	// Read is true when the QDevice endpoint answered. When it is false the
	// profile cannot say whether a QDevice exists, so it counts none and gives
	// no QDevice advice.
	Read bool

	// ReadErr says why the endpoint could not be read.
	ReadErr string

	// Configured is true when a QDevice is configured.
	Configured bool

	// Votes is the number of votes the QDevice carries when it is connected.
	Votes int

	// Status is the connection state of a configured QDevice.
	Status QDeviceStatus
}

// participatingVotes returns the QDevice's votes that count right now.
func (q QDevice) participatingVotes() int {
	if q.Configured && q.Status == QDeviceConnected {
		return q.Votes
	}
	return 0
}

// configuredVotes returns the QDevice's votes that count in the check that
// assumes every configured vote is present.
func (q QDevice) configuredVotes() int {
	if q.Configured {
		return q.Votes
	}
	return 0
}

// Member is one cluster node as the quorum math sees it.
type Member struct {
	// Name is the node name.
	Name string

	// Online is the online flag of the node's row in GET /cluster/status.
	Online bool

	// Votes is the node's configured quorum_votes.
	Votes int
}

// Quorum is the result of both quorum checks.
type Quorum struct {
	// Known is false when the configured votes could not be read or do not
	// cover every cluster member. Every other field is then zero, and both
	// checks fail closed.
	Known bool

	// UnknownReason says why Known is false.
	UnknownReason string

	// Expected is the total of every configured vote, the nodes' and the
	// QDevice's, which is what corosync calls expected votes.
	Expected int

	// Participating is the total of the votes that participate now, which are
	// the votes of the online nodes plus the QDevice's votes while it is
	// connected.
	Participating int

	// VotingNodes is the number of nodes whose configured votes are above zero.
	VotingNodes int

	// HeaviestNode and HeaviestVotes name the node whose loss costs the most
	// configured votes.
	HeaviestNode  string
	HeaviestVotes int

	// RemainingAfterLoss is Expected minus HeaviestVotes, the votes left after
	// the worst single node loss with every configured vote present.
	RemainingAfterLoss int

	// SurvivesOne is true when the configured votes keep quorum after the loss
	// of any one node. It is the hard check that gates every HA write.
	SurvivesOne bool

	// FragileNodes lists the online nodes whose loss would cost quorum given
	// the votes that participate now. It is empty when losing any one online
	// node leaves quorum intact.
	FragileNodes []string
}

// OneFailureFromLoss reports whether losing some online node would cost quorum
// now. A cluster that passes SurvivesOne can still be in this state, such as a
// three-node cluster that has already lost a node.
func (q Quorum) OneFailureFromLoss() bool {
	return q.Known && len(q.FragileNodes) > 0
}

// hasQuorum reports whether votes are more than half of the expected votes.
func hasQuorum(votes, expected int) bool {
	return expected > 0 && 2*votes > expected
}

// computeQuorum runs both quorum checks. Quorum needs more than half of the
// expected votes, so the hard check passes when the total minus the votes of
// the heaviest node is still more than half of the total. The second check
// tests each online node against the votes that participate now.
func computeQuorum(members []Member, qdevice QDevice) Quorum {
	q := Quorum{Known: true}
	if len(members) == 0 {
		return Quorum{UnknownReason: "the cluster lists no members"}
	}
	for _, m := range members {
		if m.Votes < 0 {
			return Quorum{UnknownReason: fmt.Sprintf("node %s has %d configured votes", m.Name, m.Votes)}
		}
		q.Expected += m.Votes
		if m.Votes > 0 {
			q.VotingNodes++
		}
		if m.Votes > q.HeaviestVotes || q.HeaviestNode == "" {
			q.HeaviestNode, q.HeaviestVotes = m.Name, m.Votes
		}
		if m.Online {
			q.Participating += m.Votes
		}
	}
	q.Expected += qdevice.configuredVotes()
	q.Participating += qdevice.participatingVotes()
	q.RemainingAfterLoss = q.Expected - q.HeaviestVotes
	q.SurvivesOne = hasQuorum(q.RemainingAfterLoss, q.Expected)

	for _, m := range members {
		if m.Online && m.Votes > 0 && !hasQuorum(q.Participating-m.Votes, q.Expected) {
			q.FragileNodes = append(q.FragileNodes, m.Name)
		}
	}
	sort.Strings(q.FragileNodes)
	return q
}

// qdeviceAdvice returns the QDevice recommendation, or "" when there is none.
// A cluster with an even number of voting nodes and no QDevice can split in
// half, and then neither half has quorum. The recommendation needs a readable
// QDevice endpoint and known votes, so it never fires on a guess, and it never
// fires for an odd number of voting nodes, because the PVE documentation
// discourages a QDevice there.
func qdeviceAdvice(q Quorum, qdevice QDevice) string {
	if !q.Known || !qdevice.Read || qdevice.Configured {
		return ""
	}
	if q.VotingNodes == 0 || q.VotingNodes%2 != 0 {
		return ""
	}
	return fmt.Sprintf("add-qdevice(%d voting nodes can split evenly)", q.VotingNodes)
}
