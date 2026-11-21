package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestNotesTags_AcceptedForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		notes string
		want  []string
	}{
		{"single tag", "tags: maintenance", []string{"maintenance"}},
		{"no space after the colon", "tags:maintenance", []string{"maintenance"}},
		{"commas", "tags: a,b,c", []string{"a", "b", "c"}},
		{"semicolons", "tags: a;b;c", []string{"a", "b", "c"}},
		{"spaces and tabs", "tags: a b\tc", []string{"a", "b", "c"}},
		{"mixed separators and runs", "tags: a, b ;; c  ,d", []string{"a", "b", "c", "d"}},
		{"surrounding whitespace", "   tags:   maintenance   ", []string{"maintenance"}},
		{"uppercase key", "TAGS: maintenance", []string{"maintenance"}},
		{"mixed case key", "Tags: Maintenance", []string{"Maintenance"}},
		{"carriage return line ending", "tags: maintenance\r\nrack 4", []string{"maintenance"}},
		{"line in the middle of the notes", "Rack 4, slot 2\n\ntags: maintenance\nOwner: platform team", []string{"maintenance"}},
		{"last line", "Rack 4\ntags: maintenance", []string{"maintenance"}},
		{"two marker lines are joined", "tags: a\nnotes\ntags: b", []string{"a", "b"}},
		{"empty list", "tags:", nil},
		{"list of separators only", "tags: , ;", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pve.NotesTags(tc.notes); !slices.Equal(got, tc.want) {
				t.Errorf("NotesTags(%q) = %q, want %q", tc.notes, got, tc.want)
			}
		})
	}
}

func TestNotesTags_RejectedForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		notes string
	}{
		{"no notes", ""},
		{"whitespace only", " \n\t\n"},
		{"prose that mentions the tag", "This node is in maintenance until Friday."},
		{"key with no colon", "tags maintenance"},
		{"equals sign instead of a colon", "tags = maintenance"},
		{"space before the colon", "tags : maintenance"},
		{"longer key", "mytags: maintenance"},
		{"key with a suffix", "tagsx: maintenance"},
		{"singular key", "tag: maintenance"},
		{"markdown list item", "- tags: maintenance"},
		{"markdown heading", "# tags: maintenance"},
		{"quoted line", "> tags: maintenance"},
		{"key later in the line", "see tags: maintenance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pve.NotesTags(tc.notes); len(got) != 0 {
				t.Errorf("NotesTags(%q) = %q, want no tags", tc.notes, got)
			}
		})
	}
}

func TestNotesMaintenanceTag_MatchRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		notes   string
		want    []string
		tag     string
		matched bool
	}{
		{"exact", "tags: maintenance", []string{"maintenance"}, "maintenance", true},
		{"notes in uppercase", "tags: MAINTENANCE", []string{"maintenance"}, "MAINTENANCE", true},
		{"configured tag in mixed case", "tags: maintenance", []string{"Maintenance"}, "maintenance", true},
		{"configured tag with whitespace", "tags: drain", []string{"  drain "}, "drain", true},
		{"one of several", "tags: rack4;drain,hw", []string{"drain"}, "drain", true},
		{"any configured tag", "tags: hw", []string{"drain", "hw"}, "hw", true},
		{"no shared tag", "tags: rack4 hw", []string{"maintenance"}, "", false},
		{"substring is not a match", "tags: maintenance-window", []string{"maintenance"}, "", false},
		{"prefix is not a match", "tags: maint", []string{"maintenance"}, "", false},
		{"no marker line", "maintenance", []string{"maintenance"}, "", false},
		{"empty configured tags", "tags: maintenance", []string{"", "  "}, "", false},
		{"no configured tags", "tags: maintenance", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tag, ok := pve.NotesMaintenanceTag(tc.notes, tc.want)
			if ok != tc.matched || tag != tc.tag {
				t.Errorf("NotesMaintenanceTag(%q, %q) = (%q, %v), want (%q, %v)",
					tc.notes, tc.want, tag, ok, tc.tag, tc.matched)
			}
		})
	}
}

// nnNodes answers the clock like nhNodes and the node config with the notes
// it holds, and it records each node whose config was read.
type nnNodes struct {
	nhNodes
	mu         sync.Mutex
	notes      map[string]string
	notesErr   map[string]error
	notesAsked []string
	// noDescription makes the named nodes answer with no description field,
	// as PVE does for a node whose notes are empty.
	noDescription map[string]bool
}

func (n *nnNodes) ListConfig(
	_ context.Context, node string, _ *sdknodes.ListConfigParams,
) (*sdknodes.ListConfigResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notesAsked = append(n.notesAsked, node)
	if err := n.notesErr[node]; err != nil {
		return nil, err
	}
	digest := "abc"
	resp := &sdknodes.ListConfigResponse{Digest: &digest}
	if !n.noDescription[node] {
		desc := n.notes[node]
		resp.Description = &desc
	}
	return resp, nil
}

func nnRun(
	t *testing.T, ctx context.Context, c *nhCluster, n *nnNodes, opts pve.NodeHealthOptions,
) *pve.NodeHealthReport {
	t.Helper()
	if n.now == 0 {
		n.now = nhNow
	}
	r, err := pve.NodeHealth(ctx, c, n, opts)
	if err != nil {
		t.Fatalf("NodeHealth: %v", err)
	}
	return r
}

var nnTags = pve.NodeHealthOptions{MaintenanceTags: []string{"maintenance"}}

func TestNodeHealth_NotesMarkerPutsNodeInMaintenance(t *testing.T) {
	t.Parallel()
	n := &nnNodes{notes: map[string]string{
		"pve1": "Rack 4\nOwner: platform",
		"pve2": "Rack 5\nTags: Drain, MAINTENANCE\nback Friday",
	}}
	r := nnRun(t, t.Context(), nhHealthy(), n, nnTags)
	e := nhEntry(t, r, "pve2")
	if !e.Maintenance || e.Placeable || e.Health != pve.NodeHealthMaintenance || !e.ProxyTarget || e.DeadOrFenced {
		t.Errorf("pve2 carries the marker: %+v", e)
	}
	if e.NotesTag != "MAINTENANCE" || !strings.Contains(e.Reason, "notes") {
		t.Errorf("pve2 must name the tag and its notes: tag %q, reason %q", e.NotesTag, e.Reason)
	}
	if e.CRMState != "online" || e.Watchdog != pve.WatchdogArmed {
		t.Errorf("the marker must not hide what HA said: %+v", e)
	}
	for _, name := range []string{"pve1", "pve3"} {
		if e := nhEntry(t, r, name); !e.Placeable || e.Maintenance || e.NotesTag != "" {
			t.Errorf("%s carries no marker: %+v", name, e)
		}
	}
	if r.NotesErr != nil {
		t.Errorf("NotesErr = %v", r.NotesErr)
	}
	if want := []string{"pve1", "pve2", "pve3"}; !slices.Equal(n.notesAsked, want) {
		t.Errorf("notes read for %v, want %v", n.notesAsked, want)
	}
}

func TestNodeHealth_NodeWithNoNotesStaysPlaceable(t *testing.T) {
	t.Parallel()
	n := &nnNodes{noDescription: map[string]bool{"pve1": true, "pve2": true, "pve3": true}}
	r := nnRun(t, t.Context(), nhHealthy(), n, nnTags)
	for _, e := range r.Nodes {
		if !e.Placeable || e.Maintenance || e.NotesTag != "" {
			t.Errorf("%s has no notes: %+v", e.Name, e)
		}
	}
	if r.NotesErr != nil {
		t.Errorf("NotesErr = %v", r.NotesErr)
	}
}

// PVE has no tags on nodes, and the old source was a tags field on the
// cluster status rows. A row that carries one anyway must change nothing.
func TestNodeHealth_ClusterStatusTagsAreIgnored(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[2] = nhNode("pve2", 1, false, "maintenance")
	r := nnRun(t, t.Context(), c, &nnNodes{}, nnTags)
	if e := nhEntry(t, r, "pve2"); !e.Placeable || e.Maintenance {
		t.Errorf("a tags field in cluster status must not mark the node: %+v", e)
	}
}

func TestNodeHealth_NoNotesReadWithoutMaintenanceTags(t *testing.T) {
	t.Parallel()
	for _, opts := range []pve.NodeHealthOptions{{}, {MaintenanceTags: []string{}}, {MaintenanceTags: []string{"", " "}}} {
		n := &nnNodes{notes: map[string]string{"pve2": "tags: maintenance"}}
		r := nnRun(t, t.Context(), nhHealthy(), n, opts)
		if len(n.notesAsked) != 0 {
			t.Errorf("opts %+v read notes for %v", opts, n.notesAsked)
		}
		if !r.Placeable("pve2") {
			t.Errorf("opts %+v: the marker must mean nothing without configured tags", opts)
		}
	}
}

// Only a node that would otherwise take new work has its notes read, because
// the marker can only take a node out of placement. An offline node could not
// answer anyway, since PVE proxies the read to the node itself.
func TestNodeHealth_NotesReadOnlyForPlaceableNodes(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[3] = nhNode("pve3", 0, false, "")
	c.manager = nhManager(nhNow-5,
		map[string]string{"pve1": "online", "pve2": "maintenance", "pve3": "online"},
		map[string]map[string]any{
			"pve1": nhActive(nhNow), "pve2": nhLRM("maintenance", "maintenance", nhNow), "pve3": nhActive(nhNow - 600),
		})
	n := &nnNodes{notesErr: map[string]error{"pve2": errors.New("boom"), "pve3": errors.New("boom")}}
	r := nnRun(t, t.Context(), c, n, nnTags)
	if !slices.Equal(n.notesAsked, []string{"pve1"}) {
		t.Errorf("notes read for %v, want only pve1", n.notesAsked)
	}
	if r.NotesErr != nil {
		t.Errorf("a node that takes no work anyway must not fail the read: %v", r.NotesErr)
	}
	if e := nhEntry(t, r, "pve2"); e.NotesTag != "" || e.NotesUnread {
		t.Errorf("pve2 is in HA maintenance and its notes were not read: %+v", e)
	}
}

func TestNodeHealth_FailedNotesReadFailsClosed(t *testing.T) {
	t.Parallel()
	n := &nnNodes{notesErr: map[string]error{"pve2": errors.New("595 no route to host")}}
	r := nnRun(t, t.Context(), nhHealthy(), n, nnTags)
	e := nhEntry(t, r, "pve2")
	if e.Placeable || !e.NotesUnread || e.Maintenance || !e.ProxyTarget {
		t.Errorf("pve2's notes could not be read, so it must not take work: %+v", e)
	}
	if !strings.Contains(e.Reason, "notes") {
		t.Errorf("the reason must name the notes read: %q", e.Reason)
	}
	if r.NotesErr == nil || !strings.Contains(r.NotesErr.Error(), "pve2") ||
		!strings.Contains(r.NotesErr.Error(), "595 no route to host") {
		t.Errorf("NotesErr must name the node and the cause: %v", r.NotesErr)
	}
	if !r.Placeable("pve1") || !r.Placeable("pve3") {
		t.Error("the other nodes' notes were read, so they stay placeable")
	}
}

func TestNodeHealth_NotesWithoutHASources(t *testing.T) {
	t.Parallel()
	c := &nhCluster{rows: []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "")}}
	n := &nnNodes{notes: map[string]string{"pve1": "tags: maintenance"}}
	r := nnRun(t, t.Context(), c, n, nnTags)
	if r.HA != pve.HAViewNotConfigured {
		t.Fatalf("HA = %q", r.HA)
	}
	if e := nhEntry(t, r, "pve1"); e.Placeable || !e.Maintenance || !e.ProxyTarget || e.NotesTag != "maintenance" {
		t.Errorf("pve1: %+v", e)
	}
}

func TestNodeHealth_NotesWithSkipHA(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	n := &nnNodes{notes: map[string]string{"pve3": "tags: maintenance"}}
	r := nnRun(t, t.Context(), c, n, pve.NodeHealthOptions{SkipHA: true, MaintenanceTags: []string{"maintenance"}})
	if c.managerCalls != 0 || c.currentCalls != 0 || len(n.asked) != 0 {
		t.Fatalf("read an HA source: manager %d, status rows %d, clock %v", c.managerCalls, c.currentCalls, n.asked)
	}
	if e := nhEntry(t, r, "pve3"); !e.Maintenance || e.Placeable || !e.ProxyTarget {
		t.Errorf("pve3 carries the marker: %+v", e)
	}
}

func TestNodeHealth_NotesWithTextFallback(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.manager = json.RawMessage(`{}`)
	idle, _ := json.Marshal(map[string]any{"type": "lrm", "node": "pve3", "status": "pve3 (idle, Mon Jan  1 2001)"})
	c.current = []json.RawMessage{idle}
	n := &nnNodes{notes: map[string]string{"pve2": "tags: maintenance"}}
	r := nnRun(t, t.Context(), c, n, nnTags)
	if r.HA != pve.HAViewLRMText {
		t.Fatalf("HA = %q", r.HA)
	}
	if e := nhEntry(t, r, "pve2"); !e.Maintenance || e.Placeable || !e.ProxyTarget {
		t.Errorf("pve2: %+v", e)
	}
}

// A non-quorate entry node leaves every node unknown and unplaceable, so the
// notes can change nothing and are not read.
func TestNodeHealth_NonQuorateReadsNoNotes(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[0] = nhClusterRow(0)
	n := &nnNodes{notes: map[string]string{"pve2": "tags: maintenance"}}
	r := nnRun(t, t.Context(), c, n, nnTags)
	if len(n.notesAsked) != 0 {
		t.Errorf("notes read through a non-quorate entry node: %v", n.notesAsked)
	}
	for _, e := range r.Nodes {
		if e.Placeable || e.Health != pve.NodeHealthUnknown {
			t.Errorf("%s: %+v", e.Name, e)
		}
	}
}

func TestNodeHealth_NotesCacheReadsEachNodeOncePerContext(t *testing.T) {
	t.Parallel()
	ctx := pve.WithNodeNotesCache(t.Context())
	if pve.WithNodeNotesCache(ctx) != ctx {
		t.Fatal("a context that already carries a cache must be returned as it is")
	}
	n := &nnNodes{notes: map[string]string{"pve2": "tags: maintenance"}}
	for range 3 {
		r := nnRun(t, ctx, nhHealthy(), n, nnTags)
		if e := nhEntry(t, r, "pve2"); !e.Maintenance || e.Placeable {
			t.Fatalf("pve2 carries the marker: %+v", e)
		}
	}
	if want := []string{"pve1", "pve2", "pve3"}; !slices.Equal(n.notesAsked, want) {
		t.Errorf("notes read for %v, want each node once: %v", n.notesAsked, want)
	}

	// A context without the cache reads the notes on every call.
	plain := &nnNodes{}
	nnRun(t, t.Context(), nhHealthy(), plain, nnTags)
	nnRun(t, t.Context(), nhHealthy(), plain, nnTags)
	if len(plain.notesAsked) != 6 {
		t.Errorf("without a cache every call reads every node: %v", plain.notesAsked)
	}
}

// A failed read is never kept, so the read that follows asks the node again
// and a node that answers then is placeable.
func TestNodeHealth_NotesCacheKeepsNoFailure(t *testing.T) {
	t.Parallel()
	ctx := pve.WithNodeNotesCache(t.Context())
	n := &nnNodes{notesErr: map[string]error{"pve2": errors.New("boom")}}
	r := nnRun(t, ctx, nhHealthy(), n, nnTags)
	if r.NotesErr == nil || r.Placeable("pve2") {
		t.Fatalf("the first read must fail closed: %v %+v", r.NotesErr, r.Nodes)
	}
	n.notesErr = nil
	r = nnRun(t, ctx, nhHealthy(), n, nnTags)
	if r.NotesErr != nil || !r.Placeable("pve2") {
		t.Errorf("the second read must ask pve2 again: %v %+v", r.NotesErr, r.Nodes)
	}
	if want := []string{"pve1", "pve2", "pve3", "pve2"}; !slices.Equal(n.notesAsked, want) {
		t.Errorf("notes read for %v, want %v", n.notesAsked, want)
	}
}

// A node whose notes failed on two reads keeps failing for the rest of the
// call without another read, so one unreachable node costs each later health
// read of the call nothing.
func TestNodeHealth_NotesCacheKeepsAFailureAfterTwoReads(t *testing.T) {
	t.Parallel()
	ctx := pve.WithNodeNotesCache(t.Context())
	n := &nnNodes{notesErr: map[string]error{"pve2": errors.New("595 no route to host")}}
	for i := range 4 {
		r := nnRun(t, ctx, nhHealthy(), n, nnTags)
		e := nhEntry(t, r, "pve2")
		if r.Placeable("pve2") || !e.NotesUnread || r.NotesErr == nil ||
			!strings.Contains(r.NotesErr.Error(), "595 no route to host") {
			t.Fatalf("read %d: pve2 must stay unread with its cause: %v %+v", i+1, r.NotesErr, e)
		}
		if !r.Placeable("pve1") || !r.Placeable("pve3") {
			t.Fatalf("read %d: the other nodes answered: %+v", i+1, r.Nodes)
		}
	}
	if want := []string{"pve1", "pve2", "pve3", "pve2"}; !slices.Equal(n.notesAsked, want) {
		t.Errorf("notes read for %v, want %v", n.notesAsked, want)
	}
}

// NotesNodes limits the notes reads to the nodes a caller asks about.
func TestNodeHealth_NotesNodesReadsOnlyTheNamedNodes(t *testing.T) {
	t.Parallel()
	n := &nnNodes{
		notes:    map[string]string{"pve2": "tags: maintenance"},
		notesErr: map[string]error{"pve3": errors.New("boom")},
	}
	opts := nnTags
	opts.NotesNodes = []string{"pve2"}
	r := nnRun(t, t.Context(), nhHealthy(), n, opts)
	if !slices.Equal(n.notesAsked, []string{"pve2"}) {
		t.Errorf("notes read for %v, want only pve2", n.notesAsked)
	}
	if e := nhEntry(t, r, "pve2"); !e.Maintenance || e.Placeable {
		t.Errorf("pve2 carries the marker: %+v", e)
	}
	if r.NotesErr != nil {
		t.Errorf("a node that was not asked about must not fail the read: %v", r.NotesErr)
	}
}

// Every reader of one call shares the cache, and the CPI's reads can run on
// several goroutines, so the cache must hold up under concurrent use. Run it
// with -race.
func TestNodeHealth_NotesCacheIsSafeForConcurrentReads(t *testing.T) {
	t.Parallel()
	ctx := pve.WithNodeNotesCache(t.Context())
	n := &nnNodes{
		notes:    map[string]string{"pve1": "tags: maintenance"},
		notesErr: map[string]error{"pve3": errors.New("boom")},
	}
	c := nhHealthy()
	opts := pve.NodeHealthOptions{SkipHA: true, MaintenanceTags: []string{"maintenance"}}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			r, err := pve.NodeHealth(ctx, c, n, opts)
			switch {
			case err != nil:
				errs <- err
			case r.Placeable("pve1") || !r.Placeable("pve2") || r.Placeable("pve3"):
				errs <- errors.New("a concurrent read gave the wrong answer")
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	n.mu.Lock()
	asked := len(n.notesAsked)
	n.mu.Unlock()
	// Every node has now answered, or failed at least twice, so a further
	// read of the call asks no node again.
	nnRun(t, ctx, c, n, opts)
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.notesAsked) != asked {
		t.Errorf("a read after the burst asked again: %v", n.notesAsked[asked:])
	}
}
