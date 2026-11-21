package ha

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// TestStagedCommentKeepsTheAntiAffinityHolds pins that a stage of a resource
// keeps the holds a group's planner recorded on its comment, although the
// field is part of the marker, because Stage does not write it.
func TestStagedCommentKeepsTheAntiAffinityHolds(t *testing.T) {
	t.Parallel()
	cur := stageMarker + " " + AntiAffinityHoldKey + "1.101@2000 bosh-failback-hold=9"
	got := stagedComment(stageMarker, true, cur)
	if want := stageMarker + " bosh-dlb=1 " + AntiAffinityHoldKey + "1.101@2000 bosh-failback-hold=9"; got != want {
		t.Fatalf("staged = %q; want %q", got, want)
	}
}

// commentRig is one HA resource whose comment writes PVE checks against the
// digest of resources.cfg.
type commentRig struct {
	comment  *string
	version  int
	writes   int
	noDigest bool
	missing  bool
	// readErr, when set, makes every read fail with it.
	readErr error
	// before runs before each write, so a test can change the comment the
	// way a concurrent writer would.
	before func(r *commentRig)
}

func (r *commentRig) digest() string {
	if r.noDigest {
		return ""
	}
	return "digest-" + string(rune('a'+r.version))
}

func (r *commentRig) GetHaResources(_ context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error) {
	if r.missing {
		return nil, errors.New("no such resource")
	}
	if r.readErr != nil {
		return nil, r.readErr
	}
	var c *string
	if r.comment != nil {
		v := *r.comment
		c = &v
	}
	return &sdkcluster.GetHaResourcesResponse{Sid: sid, Comment: c, Digest: r.digest()}, nil
}

func (r *commentRig) UpdateHaResources(_ context.Context, _ string, p *sdkcluster.UpdateHaResourcesParams) error {
	if r.before != nil {
		r.before(r)
	}
	if p.Digest == nil || *p.Digest != r.digest() {
		return errors.New("detected modified configuration - file changed by other user")
	}
	r.writes++
	v := *p.Comment
	r.comment = &v
	r.version++
	return nil
}

func commentOwnedByUs(c string) bool { return strings.HasPrefix(c, stageMarker) }

func addHold(c string) string { return c + " " + AntiAffinityHoldKey + "1.101@2000" }

func TestEditResourceCommentWritesAgainstTheDigestItRead(t *testing.T) {
	t.Parallel()
	c := stageMarker
	rig := &commentRig{comment: &c}
	wrote, err := EditResourceComment(context.Background(), rig, "vm:100", commentOwnedByUs, addHold)
	if err != nil || !wrote || *rig.comment != stageMarker+" "+AntiAffinityHoldKey+"1.101@2000" {
		t.Fatalf("wrote = %v, err = %v, comment %q; want the edit written", wrote, err, *rig.comment)
	}
}

// TestEditResourceCommentMergesAgainAfterAConflict pins that a token another
// writer adds between the read and the write survives, because the refused
// write is read and edited again.
func TestEditResourceCommentMergesAgainAfterAConflict(t *testing.T) {
	t.Parallel()
	c := stageMarker
	rig := &commentRig{comment: &c}
	rig.before = func(r *commentRig) {
		r.before = nil
		v := *r.comment + " bosh-failback-hold=9"
		r.comment = &v
		r.version++
	}
	wrote, err := EditResourceComment(context.Background(), rig, "vm:100", commentOwnedByUs, addHold)
	want := stageMarker + " bosh-failback-hold=9 " + AntiAffinityHoldKey + "1.101@2000"
	if err != nil || !wrote || *rig.comment != want {
		t.Fatalf("wrote = %v, err = %v, comment %q; want %q", wrote, err, *rig.comment, want)
	}
}

func TestEditResourceCommentGivesUpAfterItsAttempts(t *testing.T) {
	t.Parallel()
	c := stageMarker
	rig := &commentRig{comment: &c}
	rig.before = func(r *commentRig) { r.version++ }
	wrote, err := EditResourceComment(context.Background(), rig, "vm:100", commentOwnedByUs, addHold)
	if err == nil || wrote || rig.writes != 0 || !strings.Contains(err.Error(), "changed under 4 writes") {
		t.Fatalf("wrote = %v, err = %v, writes %d; want no write after %d conflicts", wrote, err, rig.writes,
			CommentWriteAttempts)
	}
}

func TestEditResourceCommentLeavesSomeResourcesAlone(t *testing.T) {
	t.Parallel()
	other := "bosh-cpi:v1 owner=them deployment=cf group=router"
	plain := stageMarker
	for name, rig := range map[string]*commentRig{
		"missing":        {missing: true},
		"no comment":     {},
		"another owner":  {comment: &other},
		"nothing to add": {comment: &plain},
	} {
		edit := addHold
		if name == "nothing to add" {
			edit = func(c string) string { return c }
		}
		wrote, err := EditResourceComment(context.Background(), rig, "vm:100", commentOwnedByUs, edit)
		if err != nil || wrote || rig.writes != 0 {
			t.Errorf("%s: wrote = %v, err = %v, writes %d; want the resource left alone", name, wrote, err,
				rig.writes)
		}
	}
}

func TestEditResourceCommentNeedsADigest(t *testing.T) {
	t.Parallel()
	c := stageMarker
	rig := &commentRig{comment: &c, noDigest: true}
	wrote, err := EditResourceComment(context.Background(), rig, "vm:100", commentOwnedByUs, addHold)
	if err == nil || wrote || rig.writes != 0 {
		t.Fatalf("wrote = %v, err = %v, writes %d; want no write without a digest", wrote, err, rig.writes)
	}
}

// TestEditResourceCommentNamesItsOwnFeature pins that a failed read during a
// comment edit, such as an anti-affinity hold write, is reported as a comment
// edit and not as the failback hold's read.
func TestEditResourceCommentNamesItsOwnFeature(t *testing.T) {
	t.Parallel()
	rig := &commentRig{readErr: errors.New("connection refused")}
	_, err := EditResourceComment(context.Background(), rig, "vm:100", commentOwnedByUs, addHold)
	if err == nil || !strings.Contains(err.Error(), "ha comment: read the HA resource vm:100") ||
		strings.Contains(err.Error(), "failback") {
		t.Fatalf("err = %v; want a comment edit's read error that names no failback", err)
	}
}
