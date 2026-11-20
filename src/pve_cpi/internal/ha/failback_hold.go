package ha

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// FailbackHoldKey opens the token an attach appends to the comment of a VM's
// HA resource when it turns failback off before PVE HA moves the VM into its
// admissible set. The token names the Unix time of that write. While it is
// fresh, no reconcile turns failback on, because the VM's pin still prefers
// the node the VM leaves, and failback would pull the VM back there. The
// attach drops the token once the pin names the set.
const FailbackHoldKey = "bosh-failback-hold="

// FailbackHoldTTL is how long a hold token keeps failback off. It covers the
// move, the barrier, and the attach many times over, and it bounds what a
// token left by a call that stopped part way costs: a VM whose failback stays
// off until a call after that time sets it.
const FailbackHoldTTL = 30 * time.Minute

// WithFailbackHold returns comment with a hold token that names at, replacing
// any hold token it carried. The rest of the comment, which starts with the
// ownership marker, is kept in order.
func WithFailbackHold(comment string, at time.Time) string {
	return strings.Join(append(withoutFailbackHold(comment), FailbackHoldKey+strconv.FormatInt(at.Unix(), 10)),
		" ")
}

// StripFailbackHold returns comment without its hold token, and whether it
// carried one. The rest of the comment is kept in order.
func StripFailbackHold(comment string) (string, bool) {
	kept := withoutFailbackHold(comment)
	return strings.Join(kept, " "), len(kept) != len(strings.Fields(comment))
}

func withoutFailbackHold(comment string) []string {
	fields := strings.Fields(comment)
	kept := fields[:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, FailbackHoldKey) {
			kept = append(kept, f)
		}
	}
	return kept
}

// FailbackHoldOf returns the hold token comment carries, and false when it
// carries none.
func FailbackHoldOf(comment *string) (string, bool) {
	if comment == nil {
		return "", false
	}
	for _, f := range strings.Fields(*comment) {
		if v, ok := strings.CutPrefix(f, FailbackHoldKey); ok {
			return f, v != ""
		}
	}
	return "", false
}

// FailbackHeld reports whether comment carries a hold token that is still
// fresh at now: one written less than FailbackHoldTTL ago. A token that names
// a time more than FailbackHoldTTL ahead of now, which only a broken clock
// writes, and one whose time does not parse, hold nothing, so no token keeps
// failback off for longer than FailbackHoldTTL after the clock reads it.
func FailbackHeld(comment *string, now time.Time) bool {
	tok, ok := FailbackHoldOf(comment)
	if !ok {
		return false
	}
	unix, err := strconv.ParseInt(strings.TrimPrefix(tok, FailbackHoldKey), 10, 64)
	if err != nil {
		return false
	}
	at := time.Unix(unix, 0)
	return now.Before(at.Add(FailbackHoldTTL)) && at.Before(now.Add(FailbackHoldTTL))
}

// HoldFailback turns failback off on the resource sid and puts a hold token
// that names now on its comment, in one write, and returns the token. It
// writes even when failback already reads off, because the write changes the
// digest of resources.cfg, so a raise that a reconcile decided from an earlier
// read is refused (see RaiseFailback). The write names the digest of the read
// the comment was changed from, so a token another writer adds in between
// makes PVE refuse it rather than lose the token, and a refused write is read
// and merged again, up to CommentWriteAttempts times. A resource that is
// missing, and one whose comment owned does not accept, are left alone, and
// the token is then empty. A read or a write that fails is returned.
//
// A hold that runs out of attempts, or whose read names no digest, cannot put
// its token on the comment without risking another writer's. It then turns
// failback off with a write of the flag alone, which leaves the comment as it
// is (see ReconcileFailback), and returns a retriable error with an empty
// token, so failback is off and every token stays, and the caller asks for no
// move that the hold would have covered.
func HoldFailback(ctx context.Context, api FailbackAPI, sid string, now time.Time, owned func(string) bool) (
	string, error,
) {
	if api == nil || owned == nil {
		return "", cpierrors.Cloud("ha failback: the resource client and the ownership check must be set")
	}
	var why string
	for attempt := 1; ; attempt++ {
		got, err := readOwnedResource(ctx, api, sid, owned)
		if err != nil || got == nil {
			return "", err
		}
		if got.Digest == "" {
			why = "the read of the HA resource " + sid + " named no digest"
			break
		}
		off, digest := false, got.Digest
		comment := WithFailbackHold(*got.Comment, now)
		err = api.UpdateHaResources(ctx, sid, &sdkcluster.UpdateHaResourcesParams{Failback: &off,
			Comment: &comment, Digest: &digest})
		switch {
		case err == nil:
			tok, _ := FailbackHoldOf(&comment)
			return tok, nil
		case !IsDigestConflict(err):
			return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ha failback: set failback off on the HA resource %s", sid))
		case attempt < CommentWriteAttempts:
			continue
		}
		why = fmt.Sprintf("resources.cfg changed under %d writes of the hold on the HA resource %s", attempt, sid)
		break
	}
	if _, err := ReconcileFailback(ctx, api, sid, false, owned); err != nil {
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("ha failback: %s, and "+
			"failback could not be turned off without the hold either", why))
	}
	return "", cpierrors.Retriable("ha failback: %s, so failback is off without the hold token that keeps a "+
		"reconcile from turning it on; retry", why)
}

// ReleaseFailbackHold drops the hold token tok from the comment of the
// resource sid when the comment still carries exactly that token, so a
// release never drops a later hold that another call wrote. It reports
// whether it wrote. The write names the digest of the read it dropped the
// token from, and a write PVE refuses because resources.cfg changed is read
// and judged again, up to CommentWriteAttempts times. A resource that is
// missing, or whose comment owned does not accept, is left alone. A read or a
// write that fails is returned.
//
// A release that runs out of attempts, or whose read names no digest, writes
// nothing and returns a retriable error. The token then stays, and it keeps
// failback off until it expires (see FailbackHoldTTL), so a release that fails
// never turns failback on and never drops another writer's token.
func ReleaseFailbackHold(ctx context.Context, api FailbackAPI, sid, tok string, owned func(string) bool) (
	bool, error,
) {
	if tok == "" {
		return false, nil
	}
	if api == nil || owned == nil {
		return false, cpierrors.Cloud("ha failback: the resource client and the ownership check must be set")
	}
	for attempt := 1; ; attempt++ {
		got, err := readOwnedResource(ctx, api, sid, owned)
		if err != nil || got == nil {
			return false, err
		}
		if have, _ := FailbackHoldOf(got.Comment); have != tok {
			return false, nil
		}
		if got.Digest == "" {
			return false, cpierrors.Retriable("ha failback: the read of the HA resource %s named no digest, so the "+
				"failback hold stays until it expires", sid)
		}
		comment, _ := StripFailbackHold(*got.Comment)
		digest := got.Digest
		err = api.UpdateHaResources(ctx, sid, &sdkcluster.UpdateHaResourcesParams{Comment: &comment,
			Digest: &digest})
		switch {
		case err == nil:
			return true, nil
		case !IsDigestConflict(err):
			return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ha failback: drop the failback hold from the HA resource %s", sid))
		case attempt < CommentWriteAttempts:
			continue
		}
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("ha failback: resources.cfg "+
			"changed under %d writes that drop the failback hold from the HA resource %s, so the hold stays until "+
			"it expires", attempt, sid))
	}
}

// readOwnedResource reads the resource sid for a writer of its comment. It
// returns nil and no error for a resource that is missing or whose comment
// owned does not accept, which the writer leaves alone.
func readOwnedResource(ctx context.Context, api FailbackAPI, sid string, owned func(string) bool) (
	*sdkcluster.GetHaResourcesResponse, error,
) {
	got, err := api.GetHaResources(ctx, sid)
	switch {
	case err != nil && isResourceNotFound(err):
		return nil, nil
	case err != nil:
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha failback: read the HA resource %s", sid))
	case got == nil:
		return nil, cpierrors.Retriable("ha failback: the read of the HA resource %s returned nothing", sid)
	case got.Comment == nil || !owned(*got.Comment):
		return nil, nil
	}
	return got, nil
}
