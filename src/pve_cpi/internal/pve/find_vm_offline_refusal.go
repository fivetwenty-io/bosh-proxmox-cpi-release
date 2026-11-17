package pve

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// configProbeOfflineRefusal is the text of the refusal FindVMByConfigProbe
// returns when its sweeps found no hit and no failed probe on an online
// member, the HA sources hold nothing, and the only evidence still missing is
// for the members the quorate cluster reports offline, because the cluster
// index read that stands in for their guests listed the VM or failed.
// requireIndexCoversOffline builds it, and proveVMAbsent reaches it only after
// every other check passed, so a failed probe, a failed HA read, and a tie
// never return this text.
var configProbeOfflineRefusal = regexp.MustCompile(`^FindVMByConfigProbe: VM ([1-9][0-9]*) contradicted by its ` +
	`node, and the cluster reports node\(s\) ([^\s,]+(?:,[^\s,]+)*) offline, so their configs cannot be probed; ` +
	`cannot prove absence$`)

// ConfigProbeOfflineMembers reports whether err is FindVMByConfigProbe's
// refusal for VM vmid whose only cause is members the quorate cluster reports
// offline, and returns those members. That refusal means the sweeps probed
// every other member, each answered that the VM's config is missing, none of
// the probes failed, and the HA sources hold nothing for the VM. What stopped
// the proof is that the cluster index, which stands in for the offline
// members' guests, still listed the VM or could not be read.
//
// It matches only the refusal itself: a retriable CPI error with no wrapped
// cause whose whole text is the offline refusal for vmid. A wrapped or
// rephrased refusal, a failed probe, a failed or empty member enumeration, a
// failed HA read, and an offline-member tie don't match, so a caller that
// tolerates offline members still refuses every other reason absence went
// unproven.
//
// The members it returns include every member the cluster began reporting
// offline during the sweeps, which can be one the caller expected to answer.
// A caller that relies on an answer from a particular member has to check
// that the member isn't among them.
func ConfigProbeOfflineMembers(err error, vmid int) ([]string, bool) {
	var refusal *cpierrors.Error
	if err == nil || !errors.As(err, &refusal) {
		return nil, false
	}
	if refusal.Unwrap() != nil || refusal.Type() != cpierrors.TypeRetriableCloud || refusal.Error() != err.Error() {
		return nil, false
	}
	match := configProbeOfflineRefusal.FindStringSubmatch(err.Error())
	if len(match) != 3 || match[1] != strconv.Itoa(vmid) {
		return nil, false
	}
	return strings.Split(match[2], ","), true
}
