package main

import (
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// failbackFindings reports each of this Director's HA resources whose failback
// flag differs from the one the CPI writes for its VM. The CPI turns failback
// on when every volume of the VM is on shared storage and pve.ha.failback is
// not false, and turns it off otherwise. A replicate VM with failback on is a
// problem, because PVE moves such a VM back to its preferred node once that
// node returns and reverses the replication job, and the first run of the
// reversed job overwrites the volumes on that node. Any other difference is
// advice, because the next call that touches the VM sets the flag.
//
// The CPI's HA decision settles the flag of a VM whose facts call for it on,
// and of a VM whose flag reads on under a strict pin that names one node (see
// decidedFailback), so a VM that only one node can take is reported when its
// flag reads on, because the next call turns it off.
//
// The check leaves alone a resource it cannot judge: one the barrier or a move
// into the VM's admissible set holds, a VM whose config, storages, rules, or
// replication jobs could not be read, a volume on a storage the index does not
// list, a strict pin that names one node while the flag reads off, and a VM
// whose decision fails or refuses HA for a reason that may pass. It also
// leaves parkers alone, because a parker stays stopped, so failback never
// moves it, and the Director never calls set_vm_metadata or reboot_vm on one,
// so a finding about it would never clear.
func (c *haCollector) failbackFindings() {
	if c.off() || !c.guestsOK || !c.resourcesOK {
		return
	}
	for _, g := range c.guests {
		if !g.owned || g.ref.Template || g.resource == nil || g.hasTag(pve.ParkerTag) || !c.judgesVM(g) ||
			!c.ownsFailbackOf(g.resource) {
			continue
		}
		have := g.resource.Failback == nil || *g.resource.Failback
		facts, reason, ok := c.failbackFacts(g)
		if !ok {
			continue
		}
		want, known := ha.ExpectedFailback(facts)
		switch {
		case known && want, !known && have && facts.SelfPinned && !facts.Unknown:
			// The facts call for failback on, or the flag reads on under a
			// strict pin that names one node. Either way the CPI's HA
			// decision settles it, and the reconcile writes what it says.
			var why string
			want, known, why = c.decidedFailback(g)
			if known && !want {
				reason = why
			}
		}
		if !known || want == have {
			continue
		}
		if facts.Replicate {
			c.rep.problem(haKindFailbackOnReplica, g.sid,
				"%s replicates its volumes to a partner node, and its HA resource has failback on. Once the VM's "+
					"preferred node is back after a recovery or a maintenance, PVE moves the VM back to it and "+
					"reverses the replication job, and the first run of the reversed job overwrites the volumes on "+
					"that node with zfs recv -F, which loses every write made since the move. The next "+
					"set_vm_metadata or reboot_vm on the VM turns failback off, and so does "+
					"ha-manager set %s --failback 0",
				g.sid, g.sid)
			continue
		}
		c.rep.advise(haKindFailbackDrift, g.sid,
			"the HA resource of %s has failback %s, and the CPI writes failback %s for it because %s. The next "+
				"set_vm_metadata or reboot_vm on the VM sets the flag",
			g.sid, onOff(have), onOff(want), reason)
	}
}

// ownsFailbackOf reports whether the CPI sets the failback flag of res: the
// resource carries this Director's marker, the narrowing barrier does not
// hold it, and no fresh hold token says an attach is moving the VM into its
// admissible set, which the attach finishes by setting the flag itself.
func (c *haCollector) ownsFailbackOf(res *HAResourceRow) bool {
	if _, marked := haMarkerOwner(res.Comment); !marked || c.foreign(res.Comment) {
		return false
	}
	_, held := ha.NarrowMarkOf(&res.Comment)
	return !held && !ha.FailbackHeld(&res.Comment, c.now)
}

// failbackFacts gathers what the expected failback flag of g rests on, and
// says why the flag is off or on. ok is false when a read the facts need
// failed, so the doctor cannot tell whether g replicates.
func (c *haCollector) failbackFacts(g *haGuest) (facts ha.FailbackFacts, reason string, ok bool) {
	var pin *ha.Rule
	if c.rulesOK {
		pin = c.pinOf(g.ref.VMID)
	}
	facts.Enabled = c.cfg.HAFailbackEnabled()
	facts.Replicate = g.hasTag(haReplicaTag) || c.replicates(g.ref.VMID, pin) || replicaChangePending(pin)
	if facts.Replicate {
		return facts, "the VM replicates its volumes to a partner node", true
	}
	if !c.rulesOK || !c.jobsOK || g.cfg == nil || !c.storagesOK {
		return facts, "", false
	}
	if !facts.Enabled {
		return facts, "pve.ha.failback is false", true
	}
	if local := c.localVolumes(g); len(local) > 0 {
		facts.Local = true
		return facts, "the VM has volumes on node-local storage: " + strings.Join(local, ", "), true
	}
	for _, v := range g.volumes {
		name, _, found := strings.Cut(v.Volume, ":")
		if _, known := c.storages[name]; v.Kind != ha.VolumeISO && (!found || !known) {
			facts.Unknown = true
		}
	}
	facts.SelfPinned = pin != nil && pin.Strict && !pin.Disabled && len(pin.NodeNames()) == 1
	return facts, "every volume is on shared storage, so PVE moves the VM back to its preferred node once that " +
		"node returns", true
}

// replicaChangePending reports whether pin records a change to the VM's
// replicated volume set, which the CPI writes on a replicate VM's pin before
// the pin names a partner again. The CPI keeps failback off while the token is
// there. The token is left out of replicates, because a pin that carries it
// names the VM's node alone, and the maintenance check must still report that
// VM as one that cannot move.
func replicaChangePending(pin *ha.Rule) bool {
	if pin == nil {
		return false
	}
	_, changed := ha.ReplicaChanged(pin.Comment)
	return changed
}

// decidedFailback runs the CPI's HA decision for g the way the failback
// reconcile does, and returns the flag the reconcile writes for g and, when
// that flag is off, why. The reconcile writes the decision's flag for a VM the
// decision registers, which is off for a VM that only one node can take, such
// as one under a strict pin that names one node, one with a PCI passthrough
// device, or one with a host CPU type that one node alone runs. It writes off
// for a VM the decision gives no HA. A decision that fails, or that refuses
// HA for a reason that may pass, leaves the flag unknown, because the
// reconcile then leaves the flag as it is.
func (c *haCollector) decidedFailback(g *haGuest) (want, known bool, reason string) {
	if g.cfg == nil {
		return false, false, ""
	}
	cpu, _ := pve.ConfigString(g.cfg, "cpu")
	in := ha.VMInput{
		Config: c.cfg, VMID: g.ref.VMID, Node: g.ref.Node, Volumes: g.volumes,
		CPUType: ha.EffectiveCPUType(c.cfg, nil, cpu), HAManaged: true,
	}
	for key := range g.cfg {
		if strings.HasPrefix(key, "hostpci") {
			in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: "PCI passthrough devices",
				Nodes: []string{g.ref.Node}})
			break
		}
	}
	if pin := c.pinOf(g.ref.VMID); pin != nil && pin.Strict && !pin.Disabled {
		if _, marked := haMarkerOwner(pin.Comment); marked && !c.foreign(pin.Comment) {
			in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: "the VM's pin " + pin.ID,
				Nodes: pin.NodeNames(), Pin: true})
		}
	}
	d, err := c.p.DecideVM(in)
	switch {
	case err != nil, d.Transient, d.PlaceableShortfall:
		return false, false, ""
	case d.Registers() && d.Failback:
		return true, true, ""
	case d.Registers():
		return false, true, "the CPI's HA decision registers the VM with failback off (" + d.Reason + ")"
	}
	return false, true, "the CPI's HA decision gives the VM no HA (" + d.Reason + ")"
}

// onOff spells a failback flag the way the doctor's findings do.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
