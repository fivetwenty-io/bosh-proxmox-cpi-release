package ha

import (
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// RuleWriteGate returns nil when the CPI may write HA rules on this cluster,
// and an error that names the reason when it may not. HA rules exist only on
// PVE 9 and later, and a rule written to a mixed cluster could be rejected or
// ignored by a node below that floor, so the gate requires the version floor
// to hold on every online node, the rules endpoint to answer, and no node whose
// version is known to sit below PVE 9. A nil profile refuses, because the
// writer then has no evidence about the cluster.
func (p *Profile) RuleWriteGate() error {
	if p == nil {
		return cpierrors.Cloud("HA rule writes need a cluster profile, and none was available")
	}
	var below []string
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if v, ok := parseVersion(n.Version); ok && !v.AtLeast(minHAVersionMajor, minHAVersionMinor) {
			below = append(below, n.Name+" ("+n.Version+")")
		}
	}
	if len(below) > 0 {
		return cpierrors.Cloud("HA rule writes need PVE %d or later on every node, and %s is below it",
			minHAVersionMajor, strings.Join(below, ", "))
	}
	if !p.HAVersionOK {
		return cpierrors.Cloud("HA rule writes need PVE %d or later on every node, and the profile does not confirm it",
			minHAVersionMajor)
	}
	return nil
}
