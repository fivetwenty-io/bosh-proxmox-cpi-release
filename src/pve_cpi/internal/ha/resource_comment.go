package ha

import (
	"context"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// The comment of a CPI HA resource is a list of tokens separated by spaces,
// and several writers share it. Its first token is the ownership marker's
// prefix (MarkerPrefix), and the marker's fields follow it as key=value tokens
// whose keys do not start with "bosh-". Every other writer adds a token of its
// own whose key starts with "bosh-", such as the narrowing barrier's
// bosh-narrow-from, the attach's bosh-failback-hold, and the DLB record
// bosh-dlb. The dead-node completion's dead-node-from token is the one
// exception to that prefix, because it came before the rule.
//
// Each writer replaces only the tokens it owns and keeps every other token
// where it was. A writer keeps a bosh- token it does not know too, because a
// later release may own it. Stage owns the marker, its fields, and the DLB
// record (see stagedComment). A writer that reads the comment, changes it, and
// writes it back names the digest of its read, so a token another writer adds
// in between makes PVE refuse the write rather than lose the token, and the
// writer then reads and merges again, up to CommentWriteAttempts times.

// CommentWriteAttempts is how many times a writer of a CPI HA resource's
// comment reads the resource, changes the comment, and writes it against the
// digest of that read, before it gives up.
const CommentWriteAttempts = 4

// commentTokenPrefix opens the key of every token a writer other than the
// marker adds to a CPI HA resource's comment.
const commentTokenPrefix = "bosh-"

// AntiAffinityHoldKey opens the marker field that records the anti-affinity
// holds of a group, hold=<index>.<vmid>@<unix-until>, on a group rule or on
// the HA resource of a group member. A delete writes it so every plan of the
// group keeps the deleted member's place free for the recreate of its index.
// The field has no bosh- prefix, because it is part of the marker, but the
// planner writes it, not Stage, so Stage keeps it (see stageOwnsToken).
const AntiAffinityHoldKey = "hold="

// commentTokenKey returns the key a token is replaced by: its text up to and
// including the first '=', or the whole token when it has none.
func commentTokenKey(token string) string {
	if key, _, ok := strings.Cut(token, "="); ok {
		return key + "="
	}
	return token
}

// stageOwnsToken reports whether token, one of the tokens after the marker's
// prefix, belongs to Stage: a field of the marker, which is any token whose
// key does not start with "bosh-" other than the dead-node token and the
// anti-affinity holds, or the DLB record.
func stageOwnsToken(token string) bool {
	key := commentTokenKey(token)
	switch key {
	case DLBRecordKey:
		return true
	case DeadNodeSourceKey + "=", AntiAffinityHoldKey:
		return false
	}
	return !strings.HasPrefix(key, commentTokenPrefix)
}

// stagedComment returns the comment Stage writes over cur, the comment of a
// resource that is already there: marker with the DLB record dlb, followed by
// every token of cur that another writer owns, in cur's order. A token of cur
// replaces a token with the same key that marker carries, because marker may
// be a comment a re-registration recorded earlier, and the resource holds the
// newer token. A cur that holds no marker adds nothing.
func stagedComment(marker string, dlb bool, cur string) string {
	staged := WithDLBRecord(marker, dlb)
	fields, _, ok := MarkerFields(cur)
	if !ok {
		return staged
	}
	var kept []string
	keys := map[string]bool{}
	for _, f := range fields {
		if !stageOwnsToken(f) {
			kept = append(kept, f)
			keys[commentTokenKey(f)] = true
		}
	}
	if len(kept) == 0 {
		return staged
	}
	tokens := strings.Fields(staged)
	out := append([]string{tokens[0]}, deleteKeys(tokens[1:], keys)...)
	return strings.Join(append(out, kept...), " ")
}

// deleteKeys returns tokens without those whose key is in keys.
func deleteKeys(tokens []string, keys map[string]bool) []string {
	out := tokens[:0]
	for _, t := range tokens {
		if !keys[commentTokenKey(t)] {
			out = append(out, t)
		}
	}
	return out
}

// EditResourceComment rewrites the comment of the HA resource sid through
// edit, which gets the comment as it reads now and returns the comment to
// write. It reports whether it wrote. The write names the digest of the read
// it changed, and a write PVE refuses because resources.cfg changed is read
// and edited again, up to CommentWriteAttempts times. A resource that is
// missing, and one whose comment owned does not accept, are left alone, as is
// a comment that edit leaves as it is. A read or a write that fails is
// returned, and so is a read that names no digest, because a write without one
// could drop a token another writer added after the read.
func EditResourceComment(
	ctx context.Context, api FailbackAPI, sid string, owned func(string) bool, edit func(string) string,
) (bool, error) {
	if api == nil || owned == nil || edit == nil {
		return false, cpierrors.Cloud("ha comment: the resource client, the ownership check, and the edit must be set")
	}
	for attempt := 1; ; attempt++ {
		got, err := readOwnedResource(ctx, api, "ha comment", sid, owned)
		if err != nil || got == nil {
			return false, err
		}
		comment := edit(*got.Comment)
		if comment == *got.Comment {
			return false, nil
		}
		if got.Digest == "" {
			return false, cpierrors.Retriable("ha comment: the read of the HA resource %s named no digest, so its "+
				"comment is left as it is", sid)
		}
		digest := got.Digest
		err = api.UpdateHaResources(ctx, sid, &sdkcluster.UpdateHaResourcesParams{Comment: &comment,
			Digest: &digest})
		switch {
		case err == nil:
			return true, nil
		case !IsDigestConflict(err):
			return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ha comment: write the comment of the HA resource %s", sid))
		case attempt < CommentWriteAttempts:
			continue
		}
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("ha comment: resources.cfg "+
			"changed under %d writes of the comment of the HA resource %s", attempt, sid))
	}
}
