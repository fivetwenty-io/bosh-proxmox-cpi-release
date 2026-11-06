package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// ErrHAManagerHoldsService marks the one absence-proof refusal that time cures
// by itself. No HA resource is configured for the VMID, yet the CRM's
// service_status still lists the service, and the CRM drops a service with no
// configuration on its next round. A caller that has just removed the guest can
// wait on it with errors.Is, and every other absence refusal stays final.
var ErrHAManagerHoldsService = errors.New("the HA manager still holds the service")

// OfflineMemberTie reports whether a record that the CPI keeps outside PVE ties
// a guest to one of the members the quorate cluster reports offline. offline
// lists those members, sorted. It returns the member the record ties the guest
// to and a short description of the record, or an empty member when nothing
// ties it. An error means the record could not be read, which leaves absence
// unproven.
//
// FindVMAuthoritative consults it only when its absence proof excluded an
// offline member, because only then does the proof rest on the index for that
// member's guests. A guest created on the member just before it went dark has
// no index row yet, and a record that names the member is the only evidence of
// it the CPI can still read.
type OfflineMemberTie func(ctx context.Context, vmid int, offline []string) (member, detail string, err error)

// FindVMOption adjusts the absence proof of FindVMAuthoritative.
type FindVMOption func(*findVMOptions)

type findVMOptions struct {
	offlineTies []OfflineMemberTie
}

// WithOfflineMemberTie adds a source that can withhold the offline-member
// tolerance. A nil tie is ignored.
func WithOfflineMemberTie(tie OfflineMemberTie) FindVMOption {
	return func(o *findVMOptions) {
		if tie != nil {
			o.offlineTies = append(o.offlineTies, tie)
		}
	}
}

// proveVMAbsent is the absence proof behind FindVMAuthoritative's miss path and
// FindVMByConfigProbe. It returns a location as soon as any sweep finds the
// guest, and Found=false only when every source agrees the guest is gone:
//
//  1. Two complete sweeps found every member answering a clean not-found. The
//     second sweep closes the window in which a migration's config rename lands
//     between two probes of one sweep.
//  2. /cluster/ha/resources configures no resource for the VMID.
//  3. The CRM's service_status holds no service for the VMID.
//  4. When tolerateOffline excluded an offline member from the sweeps, no
//     OfflineMemberTie ties the guest to that member.
//
// Every source that fails to answer leaves absence unproven, and so does every
// source that still holds the guest, so the answer is then a retriable error.
// caller and why name the public function and the reason the index answer was
// set aside, for the log lines and the error text.
func proveVMAbsent(
	ctx context.Context, c Client, vmid int, caller, why string, tolerateOffline bool, opts findVMOptions,
) (VMLocation, error) {
	excluded := map[string]bool{}
	for range configProbeAbsenceSweeps {
		loc, skipped, err := probeVMConfigs(ctx, c, vmid, caller, why, tolerateOffline)
		if err != nil {
			return VMLocation{}, err
		}
		if loc.Found {
			return loc, nil
		}
		for _, n := range skipped {
			excluded[n] = true
		}
	}
	if err := requireNoHAHold(ctx, c, vmid, caller, why); err != nil {
		return VMLocation{}, err
	}
	if len(excluded) == 0 {
		return VMLocation{}, nil
	}
	offline := make([]string, 0, len(excluded))
	for n := range excluded {
		offline = append(offline, n)
	}
	sort.Strings(offline)
	for _, tie := range opts.offlineTies {
		member, detail, err := tie(ctx, vmid, offline)
		if err != nil {
			return VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"%s: VM %d %s, and the records that could tie it to offline member(s) %s could not be read; "+
					"cannot prove absence", caller, vmid, why, strings.Join(offline, ",")))
		}
		if member != "" {
			return VMLocation{}, cpierrors.Retriable(
				"%s: VM %d %s, but %s ties it to member %s, which the cluster reports offline; cannot prove absence "+
					"until %s answers again. Either bring %s back, or remove it from the cluster with pvecm delnode %s "+
					"and retry; the journal record is then closed through the retained-authority delete",
				caller, vmid, why, detail, member, member, member, member)
		}
	}
	logger := log.FromContext(ctx)
	for _, n := range offline {
		logger.Warn(caller+": excluded a member the cluster reports offline; absence rests on the index for its guests",
			log.String("node", n), log.Int("vmid", vmid))
	}
	return VMLocation{}, nil
}

// requireNoHAHold returns nil when neither the HA resource configuration nor the
// CRM's service state holds a service for vmid. PVE HA keeps both for a guest it
// manages even while the guest's config is in flight between nodes, as during a
// recovery, so either one holding it means a config sweep that saw nothing has
// not proven the guest gone. A service ID of either guest type counts, because
// the VMID is not free while a container holds it either. A source that fails
// to answer, or answers with a row it cannot attribute, is a retriable error.
func requireNoHAHold(ctx context.Context, c Client, vmid int, caller, why string) error {
	clusterAPI := c.Cluster()
	var resources *sdkcluster.ListHaResourcesResponse
	err := RetryOnTransient(ctx, nil, "find_vm_absence_ha_resources", 0, func() error {
		var inner error
		resources, inner = clusterAPI.ListHaResources(ctx, nil)
		return inner
	})
	if err != nil {
		return cpierrors.WrapAs(WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d %s, and the HA resources could not be read; cannot prove absence", caller, vmid, why))
	}
	if resources == nil {
		return cpierrors.Retriable(
			"%s: VM %d %s, and the HA resources answered with no body; cannot prove absence", caller, vmid, why)
	}
	for _, raw := range *resources {
		var row struct {
			SID string `json:"sid"`
		}
		if jsonErr := json.Unmarshal(raw, &row); jsonErr != nil {
			return cpierrors.Retriable(
				"%s: VM %d %s, and an HA resource row does not parse (%v); cannot prove absence", caller, vmid, why, jsonErr)
		}
		if haSIDNamesVMID(row.SID, vmid) {
			return cpierrors.Retriable(
				"%s: VM %d %s, but HA resource %s is still configured; cannot prove absence. If the VM is really gone, "+
					"remove the resource with ha-manager remove %s", caller, vmid, why, row.SID, row.SID)
		}
	}

	mgr, err := readHAManagerStatus(ctx, clusterAPI)
	if err != nil {
		return cpierrors.WrapAs(WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d %s, and the HA manager status could not be read; cannot prove absence", caller, vmid, why))
	}
	sids := make([]string, 0, len(mgr.services))
	for sid := range mgr.services {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		if haSIDNamesVMID(sid, vmid) {
			return cpierrors.WrapAs(ErrHAManagerHoldsService, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"%s: VM %d %s, but the HA manager still holds service %s; cannot prove absence until the CRM drops it",
				caller, vmid, why, sid))
		}
	}
	return nil
}

// haSIDNamesVMID reports whether an HA service ID names vmid, for either guest
// type.
func haSIDNamesVMID(sid string, vmid int) bool {
	kind, id, ok := strings.Cut(strings.TrimSpace(sid), ":")
	return ok && (kind == "vm" || kind == "ct") && id == strconv.Itoa(vmid)
}
