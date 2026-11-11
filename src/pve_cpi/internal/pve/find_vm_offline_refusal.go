package pve

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// configProbeOfflineRefusal is the text of the refusal FindVMByConfigProbe
// returns when a sweep reached its end with no hit and no failed probe, and
// the only members it could not prove are the ones the quorate cluster reports
// offline. probeVMConfigs builds it, and it checks the failed probes first, so
// a sweep with a failed probe never returns this text.
var configProbeOfflineRefusal = regexp.MustCompile(`^FindVMByConfigProbe: VM ([1-9][0-9]*) contradicted by its ` +
	`node, and the cluster reports node\(s\) ([^\s,]+(?:,[^\s,]+)*) offline, so their configs cannot be probed; ` +
	`cannot prove absence$`)

// ConfigProbeOfflineMembers reports whether err is FindVMByConfigProbe's
// refusal for VM vmid whose only cause is members the quorate cluster reports
// offline, and returns those members. That refusal means the sweep probed
// every other member, each answered that the VM's config is missing, and none
// of the probes failed.
//
// It matches only the refusal itself: a retriable CPI error with no wrapped
// cause whose whole text is the offline refusal for vmid. A wrapped or
// rephrased refusal, a failed probe, a failed or empty member enumeration, and
// a failed HA read don't match, so a caller that tolerates offline members
// still refuses every other reason absence went unproven.
//
// The members it returns can include every member of the cluster, because the
// probe names every offline member whether or not it probed any other. A
// caller that relies on an answer from a particular member has to check that
// the member isn't among them.
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
