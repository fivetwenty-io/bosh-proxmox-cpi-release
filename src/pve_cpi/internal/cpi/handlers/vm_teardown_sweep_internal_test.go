package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestBirthTag_RoundTripsAndRejectsLookalikes(t *testing.T) {
	born := time.Unix(1_790_000_000, 0)
	tag := birthTagFor(born)
	if tag != "bosh-creating-1790000000" {
		t.Fatalf("birthTagFor = %q", tag)
	}
	got, ok := birthTagTime(tag)
	if !ok || !got.Equal(born) {
		t.Fatalf("birthTagTime(%q) = %v, %v; want %v, true", tag, got, ok, born)
	}
	if !isBirthTag(" " + tag + " ") {
		t.Error("surrounding whitespace must not hide a birth tag")
	}
	for _, bad := range []string{
		"bosh-creating-",
		"bosh-creating-abc",
		"bosh-creating-12x",
		"bosh-creating--12",
		"bosh-creating-12345678901234567890",
		"bosh-creating-99999999999999999999",
		"xbosh-creating-12",
		"bosh-creating-12;custom",
		"bosh-cpi",
	} {
		if isBirthTag(bad) {
			t.Errorf("isBirthTag(%q) = true; want false", bad)
		}
	}
	if _, ok := birthTagTime("bosh-creating-9999999999999999999"); ok {
		t.Error("a birth time past int64 must not parse")
	}
}

func TestWithoutBirthTags_KeepsEverythingElseInOrder(t *testing.T) {
	got := withoutBirthTags([]string{"bosh-cpi", "bosh-creating-1", "custom", "bosh-creating-2", "bosh-creating-x"})
	want := []string{"bosh-cpi", "custom", "bosh-creating-x"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("withoutBirthTags = %v; want %v", got, want)
	}
}

func TestWithBirthTag_SitsBehindTheHeadTagsAndReplacesAnOldOne(t *testing.T) {
	tags, dropped := withBirthTag("custom;bosh-owner--dir-a;bosh-creating-5;bosh-cpi;job--web", "bosh-creating-9")
	if len(dropped) != 0 {
		t.Fatalf("nothing should drop under the cap, got %v", dropped)
	}
	entries := parseTagsField(tags)
	births := 0
	birthAt := -1
	for i, e := range entries {
		if isBirthTag(e) {
			births++
			birthAt = i
		}
	}
	if births != 1 || entries[birthAt] != "bosh-creating-9" {
		t.Fatalf("want exactly the new birth tag, got %v", entries)
	}
	for i := 0; i < birthAt; i++ {
		if entries[i] != ownershipTag && !hasHeadTagPrefix(entries[i]) {
			t.Fatalf("the birth tag must follow only head tags, got %v", entries)
		}
	}
	for i := birthAt + 1; i < len(entries); i++ {
		if entries[i] == ownershipTag || hasHeadTagPrefix(entries[i]) {
			t.Fatalf("a head tag ended up behind the birth tag: %v", entries)
		}
	}
}

func TestWithBirthTag_SurvivesTheByteCap(t *testing.T) {
	long := "bosh-cpi;bosh-owner--dir-a;" + strings.Repeat("x", maxTagLength)
	tags, dropped := withBirthTag(long, "bosh-creating-9")
	if len(dropped) == 0 {
		t.Fatal("the oversized tail tag must be reported as dropped")
	}
	if !strings.Contains(tags, "bosh-creating-9") || len(tags) > maxTagLength {
		t.Fatalf("the birth tag must survive inside the cap, got %q (%d bytes)", tags, len(tags))
	}
}

func TestAddBirthTag_SetsTheInitialTags(t *testing.T) {
	shape := &createVMShape{initialTags: "bosh-cpi;custom"}
	now := time.Unix(1_790_000_000, 0)
	addBirthTag(shape, now, log.NewNopLogger())
	if !strings.Contains(shape.initialTags, birthTagFor(now)) {
		t.Fatalf("initialTags = %q; want the birth tag", shape.initialTags)
	}
}

func TestDeferredTeardownQueue_Queued(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	age := 10 * time.Minute
	expired := birthTagFor(now.Add(-age - time.Second))
	fresh := birthTagFor(now.Add(-age + time.Second))
	cases := []struct {
		name       string
		tags       string
		keepFailed bool
		want       bool
	}{
		{"deleting", "bosh-cpi;bosh-deleting", false, true},
		{"deleting upper case", "bosh-cpi;BOSH-DELETING", false, true},
		{"expired birth", "bosh-cpi;" + expired, false, true},
		{"fresh birth", "bosh-cpi;" + fresh, false, false},
		{"no queue tag", "bosh-cpi;custom", false, false},
		{"ha readd wins over deleting", "bosh-cpi;bosh-deleting;bosh-ha-readd-1", false, false},
		{"ha readd wins over expired birth", "bosh-cpi;" + expired + ";bosh-ha-readd-1", false, false},
		{"keep failed with create failed", "bosh-cpi;bosh-deleting;bosh-create-failed", true, false},
		{"keep failed with birth only", "bosh-cpi;" + expired, true, false},
		{"keep failed with deleting", "bosh-cpi;bosh-deleting", true, true},
		{"create failed without keep failed", "bosh-cpi;" + expired + ";bosh-create-failed", false, true},
		{"empty", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := deferredTeardownQueue{now: now, age: age, keepFailed: tc.keepFailed}
			if got := q.queued(tc.tags); got != tc.want {
				t.Fatalf("queued(%q) = %v; want %v", tc.tags, got, tc.want)
			}
		})
	}
}

func TestStragglerRow_Candidate(t *testing.T) {
	q := deferredTeardownQueue{now: time.Now(), age: time.Minute}
	const uuid = "dir-a"
	cases := []struct {
		name string
		row  stragglerRow
		want bool
	}{
		{"queued VM", stragglerRow{Type: "qemu", VMID: sdkclient.PVEInt(100), Node: "pve01", Tags: "bosh-cpi;bosh-deleting"}, true},
		{"own owner", stragglerRow{Type: "qemu", VMID: 100, Node: "pve01", Tags: "bosh-owner--dir-a;bosh-deleting"}, true},
		{"container", stragglerRow{Type: "lxc", VMID: 100, Node: "pve01", Tags: "bosh-deleting"}, false},
		{"no vmid", stragglerRow{Type: "qemu", Node: "pve01", Tags: "bosh-deleting"}, false},
		{"no node", stragglerRow{Type: "qemu", VMID: 100, Tags: "bosh-deleting"}, false},
		{"not queued", stragglerRow{Type: "qemu", VMID: 100, Node: "pve01", Tags: "bosh-cpi"}, false},
		{"foreign owner", stragglerRow{Type: "qemu", VMID: 100, Node: "pve01", Tags: "bosh-owner--dir-b;bosh-deleting"}, false},
		{"parker", stragglerRow{Type: "qemu", VMID: 100, Node: "pve01", Tags: pve.ParkerTag + ";bosh-deleting"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.candidate(q, uuid); got != tc.want {
				t.Fatalf("candidate(%+v) = %v; want %v", tc.row, got, tc.want)
			}
		})
	}
}

func TestSweepContext_LeavesHalfTheRequestDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sweepCtx, sweepCancel := sweepContext(ctx)
	defer sweepCancel()
	dl, ok := sweepCtx.Deadline()
	if !ok || time.Until(dl) > 1100*time.Millisecond {
		t.Fatalf("the sweep must take at most half of the request's deadline; deadline in %v", time.Until(dl))
	}
}

func birthTagRemovalDeps(nodes *lrNodesStub, q *lrQEMUStub) Deps {
	return Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}
}

func TestRemoveBirthTag_WritesTheRestWithTheDigest(t *testing.T) {
	removed := false
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		if removed {
			return map[string]any{"tags": "bosh-cpi;custom", "digest": "d2"}, nil
		}
		return map[string]any{"tags": "bosh-cpi;bosh-creating-123;custom", "digest": "d1"}, nil
	}}
	nodes := &lrNodesStub{onUpdate: func() { removed = true }}

	if err := removeBirthTag(crhCtx(), birthTagRemovalDeps(nodes, q), "pve01", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes.updateParams) != 1 {
		t.Fatalf("want one tag write, got %d", len(nodes.updateParams))
	}
	p := nodes.updateParams[0]
	if p.Tags == nil || *p.Tags != "bosh-cpi;custom" {
		t.Fatalf("tags written = %v; want bosh-cpi;custom", p.Tags)
	}
	if p.Digest == nil || *p.Digest != "d1" {
		t.Fatalf("digest = %v; want the digest the write read", p.Digest)
	}
}

func TestRemoveBirthTag_DeletesTagsWhenNothingRemains(t *testing.T) {
	removed := false
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		if removed {
			return map[string]any{}, nil
		}
		return map[string]any{"tags": "bosh-creating-123"}, nil
	}}
	nodes := &lrNodesStub{onUpdate: func() { removed = true }}

	if err := removeBirthTag(crhCtx(), birthTagRemovalDeps(nodes, q), "pve01", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes.updateParams) != 1 {
		t.Fatalf("want one write, got %d", len(nodes.updateParams))
	}
	p := nodes.updateParams[0]
	if p.Delete == nil || *p.Delete != jsonKeyTags || p.Tags != nil {
		t.Fatalf("want a delete of the tags key, got %+v", p)
	}
}

func TestRemoveBirthTag_NoBirthTagNoWrite(t *testing.T) {
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		return map[string]any{"tags": "bosh-cpi;custom"}, nil
	}}
	nodes := &lrNodesStub{}
	if err := removeBirthTag(crhCtx(), birthTagRemovalDeps(nodes, q), "pve01", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes.updateParams) != 0 {
		t.Fatalf("a config without a birth tag needs no write, got %d", len(nodes.updateParams))
	}
}

func TestRemoveBirthTag_ReadFailureIsACPIError(t *testing.T) {
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		return nil, errors.New("500 internal error")
	}}
	nodes := &lrNodesStub{}
	err := removeBirthTag(crhCtx(), birthTagRemovalDeps(nodes, q), "pve01", 100, log.NewNopLogger())
	if err == nil {
		t.Fatal("want an error")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) {
		t.Fatalf("want a CPI error, got %T %v", err, err)
	}
	if len(nodes.updateParams) != 0 {
		t.Fatal("an unreadable config must not be written")
	}
}

func TestKeepFailedVM_DropsTheBirthTag(t *testing.T) {
	nodes := &kfNodes{}
	q := &kfQEMU{existingTag: "bosh-cpi;bosh-creating-123;custom"}
	keepFailedVM(context.Background(), kfDeps(true, nodes, q), "pve01", 100, kfEnv(), log.NewNopLogger())
	if nodes.updatedTags == nil {
		t.Fatal("want a tag write")
	}
	if strings.Contains(*nodes.updatedTags, birthTagPrefix) {
		t.Fatalf("a kept VM must lose its birth tag, got %q", *nodes.updatedTags)
	}
	if !strings.Contains(*nodes.updatedTags, tagCreateFailed) || !strings.Contains(*nodes.updatedTags, "custom") {
		t.Fatalf("tags = %q; want the failure tag and the operator's tags", *nodes.updatedTags)
	}
}

func TestTagFailedVM_KeepsTheBirthTag(t *testing.T) {
	nodes := &kfNodes{}
	q := &kfQEMU{existingTag: "bosh-cpi;bosh-creating-123;custom"}
	tagFailedVM(context.Background(), kfDeps(false, nodes, q), "pve01", 100, kfEnv(), log.NewNopLogger())
	if nodes.updatedTags == nil || !strings.Contains(*nodes.updatedTags, "bosh-creating-123") {
		t.Fatalf("a VM tagged for a later teardown keeps its birth tag, got %v", nodes.updatedTags)
	}
}
