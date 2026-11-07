package pve

import (
	"context"
	"errors"
	"sort"
	"strings"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// ParkerRecordedVolumes returns the volumes the provenance records in a
// parker's description name, sorted and distinct. A record names its volume
// while the disk sits on the parker and also while an unfinished transfer is
// still moving the disk in, which is when no slot of the parker names it yet.
// The HA registration of a parker counts both, because either volume is one
// the parker holds once the transfer ends.
func ParkerRecordedVolumes(vmCfg map[string]any) []string {
	_, records, _ := parseParkerSentinel(DescriptionFromConfig(vmCfg))
	seen := map[string]bool{}
	var out []string
	for key := range records {
		volid := strings.TrimSpace(provEntryVolid(key, records[key]))
		if volid == "" || !strings.Contains(volid, ":") || seen[volid] {
			continue
		}
		seen[volid] = true
		out = append(out, volid)
	}
	sort.Strings(out)
	return out
}

// ParkerHasRecords reports whether a parker's description carries any
// provenance record, finished or not.
func ParkerHasRecords(vmCfg map[string]any) bool {
	_, records, _ := parseParkerSentinel(DescriptionFromConfig(vmCfg))
	return len(records) > 0
}

// ParkerOwnedBy reports whether a VM with these tags and this name is a
// parker of cfg: inside its band, tagged as a parker and not as a mover, and
// of its director and prefix. It is the filter ListParkersForNode applies to
// a listing row.
func ParkerOwnedBy(vmid int, tags, name string, cfg ParkerConfig) bool {
	return IsParkerVM(vmid, tags, cfg) && !TagsMarkDiskMover(tags) &&
		parkerBelongsToDirector(tags, cfg.DirectorID) && parkerBelongsToPrefix(tags, name, cfg.Prefix)
}

// SetParkerProtection sets or clears the PVE protection flag of a parker,
// with the bounded retry every write inside a parker window uses. The caller
// must hold the parker's VMID lock, which is the lock the protection window
// takes, so no park or unpark can find the flag in a state it did not leave.
// The parker retirement clears it immediately before the destroy, because PVE
// refuses to destroy a protected VM.
func SetParkerProtection(
	ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, on bool,
) error {
	if c == nil {
		return cpierrors.Cloud("SetParkerProtection: client must not be nil")
	}
	if node == "" || parkerVMID <= 0 {
		return cpierrors.Cloud("SetParkerProtection: node and parker VMID are required")
	}
	return setParkerProtection(ctx, c, logger, node, parkerVMID, on)
}

// IsPermissionDenied reports whether err is PVE refusing the call for a grant
// the token lacks, an HTTP 403. No retry adds a grant, so a caller that sees
// one names the privilege rather than retrying.
func IsPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sdkerrors.ErrForbidden) {
		return true
	}
	var permErr *sdkerrors.PermissionError
	if errors.As(err, &permErr) {
		return true
	}
	code, ok := apiHTTPCode(err)
	return ok && code == 403
}

// IsRequestVerdict reports whether err is PVE's verdict about the request
// itself, an HTTP 4xx other than 404 and 429, which no retry of the same
// request changes. A 404 answers that the object is missing, a 429 is
// pushback, and both are left to the caller.
func IsRequestVerdict(err error) bool {
	if err == nil || IsNotFound(err) {
		return false
	}
	code, ok := apiHTTPCode(err)
	return ok && code >= 400 && code < 500 && code != 429
}
