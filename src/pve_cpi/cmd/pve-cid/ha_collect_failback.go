package main

import (
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
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
// The check leaves alone a resource it cannot judge: one the barrier holds, a
// VM whose config, storages, rules, or replication jobs could not be read, a
// volume on a storage the index does not list, and a strict pin that names one
// node, under which failback has nowhere to move the VM.
func (c *haCollector) failbackFindings() {
	if c.off() || !c.guestsOK || !c.resourcesOK {
		return
	}
	for _, g := range c.guests {
		if !g.owned || g.ref.Template || g.resource == nil || !c.judgesVM(g) || !c.ownsFailbackOf(g.resource) {
			continue
		}
		have := g.resource.Failback == nil || *g.resource.Failback
		facts, reason, ok := c.failbackFacts(g)
		if !ok {
			continue
		}
		want, known := ha.ExpectedFailback(facts)
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
// resource carries this Director's marker, and the narrowing barrier does not
// hold it.
func (c *haCollector) ownsFailbackOf(res *HAResourceRow) bool {
	if _, marked := haMarkerOwner(res.Comment); !marked || c.foreign(res.Comment) {
		return false
	}
	_, held := ha.NarrowMarkOf(&res.Comment)
	return !held
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
	facts.Replicate = g.hasTag(haReplicaTag) || c.replicates(g.ref.VMID, pin)
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

// onOff spells a failback flag the way the doctor's findings do.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
