package pve

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// PVE has no tags on nodes, so an operator marks a node for maintenance with a
// line in its notes, which are the description field of GET /nodes/{node}/config
// and the text of the web interface's node Notes panel. The line is
//
//	tags: maintenance
//
// The key is matched without regard to case, and the line may sit anywhere in
// the notes, because operators keep other text there and a marker that had to
// come first would do nothing, without any warning, when it was added at the
// end. Whitespace around the line is ignored. The line must start with the
// key, so a sentence, a Markdown list item, or a heading that mentions tags
// never counts. The list after the colon is split on commas, semicolons, and
// whitespace, as PVE splits guest tags. Every marker line counts, and the tags
// of all of them are joined.
const notesTagsKey = "tags:"

// NotesTags returns the tags of every marker line in notes, in the order they
// appear, with their case kept. It returns nil when notes holds no marker line
// or every marker line has an empty list.
func NotesTags(notes string) []string {
	var tags []string
	for line := range strings.SplitSeq(notes, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < len(notesTagsKey) || !strings.EqualFold(line[:len(notesTagsKey)], notesTagsKey) {
			continue
		}
		tags = append(tags, splitNotesTags(line[len(notesTagsKey):])...)
	}
	return tags
}

// splitNotesTags splits a marker line's list on commas, semicolons, and
// whitespace, and drops the empty pieces that runs of separators leave.
func splitNotesTags(list string) []string {
	return strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\r' || r == '\v' || r == '\f'
	})
}

// NotesMaintenanceTag returns the first tag of notes' marker lines that matches
// one of want, compared without regard to case and with want's surrounding
// whitespace ignored, and true. It returns "" and false when no tag matches or
// want holds no tag.
func NotesMaintenanceTag(notes string, want []string) (string, bool) {
	wanted := normalizeMaintenanceTags(want)
	if len(wanted) == 0 {
		return "", false
	}
	for _, tag := range NotesTags(notes) {
		for _, w := range wanted {
			if strings.EqualFold(tag, w) {
				return tag, true
			}
		}
	}
	return "", false
}

// normalizeMaintenanceTags trims each configured tag and drops the empty ones.
func normalizeMaintenanceTags(want []string) []string {
	var out []string
	for _, w := range want {
		if w = strings.TrimSpace(w); w != "" {
			out = append(out, w)
		}
	}
	return out
}

type nodeNotesCacheKey struct{}

// nodeNotesReadsPerCall is how many failed reads of one node's notes a
// cache allows before it keeps the failure. Two gives the placement read its
// retry, and every later read of the call then costs nothing.
const nodeNotesReadsPerCall = 2

// nodeNotesCache keeps the notes each node answered with, for one CPI call,
// and the failure of a node whose reads failed nodeNotesReadsPerCall times.
type nodeNotesCache struct {
	mu       sync.Mutex
	notes    map[string]string
	failures map[string]int
	lastErr  map[string]error
}

// WithNodeNotesCache returns a context under which NodeHealth reads each
// node's notes at most once when the node answers, and at most twice when it
// doesn't, however many times it runs. A CPI call installs it at its start, so
// the placement read, its retry, the target_node check, and the HA profile
// share the reads, and a node that failed twice is not asked again during the
// call. The cache is keyed by node name alone, so a context must carry the
// reads of one cluster. A context that already carries a cache is returned as
// it is.
func WithNodeNotesCache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(nodeNotesCacheKey{}).(*nodeNotesCache); ok {
		return ctx
	}
	return context.WithValue(ctx, nodeNotesCacheKey{}, &nodeNotesCache{
		notes: map[string]string{}, failures: map[string]int{}, lastErr: map[string]error{},
	})
}

// lookup reports whether the cache knows node's answer, and returns its notes,
// or the error that ends its reads.
func (c *nodeNotesCache) lookup(node string) (notes string, known bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if notes, ok := c.notes[node]; ok {
		return notes, true, nil
	}
	if c.failures[node] >= nodeNotesReadsPerCall {
		return "", true, c.lastErr[node]
	}
	return "", false, nil
}

// record keeps the outcome of one read of node's notes.
func (c *nodeNotesCache) record(node, notes string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.failures[node]++
		c.lastErr[node] = err
		return
	}
	c.notes[node] = notes
}

// readNodeNotes returns node's notes, from the context's cache when it knows
// them or has given up on the node, and otherwise from GET /nodes/{node}/config
// with the transient retry every other node health read uses. PVE proxies the
// read to the node itself.
func readNodeNotes(ctx context.Context, nodesAPI NodeHealthNodes, node string) (string, error) {
	cache, _ := ctx.Value(nodeNotesCacheKey{}).(*nodeNotesCache)
	if cache == nil {
		return fetchNodeNotes(ctx, nodesAPI, node)
	}
	if notes, known, err := cache.lookup(node); known {
		return notes, err
	}
	notes, err := fetchNodeNotes(ctx, nodesAPI, node)
	if err != nil && ctx.Err() != nil {
		// A read the call's own end cut short says nothing about the node.
		return "", err
	}
	cache.record(node, notes, err)
	return notes, err
}

// fetchNodeNotes reads node's notes from PVE.
func fetchNodeNotes(ctx context.Context, nodesAPI NodeHealthNodes, node string) (string, error) {
	var resp *sdknodes.ListConfigResponse
	err := RetryOnTransient(ctx, nil, "node_health_node_notes", 0, func() error {
		var inner error
		resp, inner = nodesAPI.ListConfig(ctx, node, nil)
		return inner
	})
	if err != nil {
		return "", WrapError(err)
	}
	if resp == nil {
		return "", cpierrors.Cloud("nil response from the node config")
	}
	notes := ""
	if resp.Description != nil {
		notes = *resp.Description
	}
	return notes, nil
}

// notesUnreadReasonPrefix starts the reason of a node whose notes could not
// be read.
const notesUnreadReasonPrefix = "its notes could not be read to check them for a maintenance tag"

// NotesUnreadReason is the reason node health gives a node whose notes read
// failed with err.
func NotesUnreadReason(err error) string {
	return notesUnreadReasonPrefix + ": " + err.Error()
}

// IsNotesUnreadReason reports whether reason, or a rejection that quotes it,
// says a node's notes could not be read.
func IsNotesUnreadReason(reason string) bool {
	return strings.Contains(reason, notesUnreadReasonPrefix)
}

// applyNodeNotes reads the notes of every placeable node and takes out of
// placement each one whose notes carry a tag of want. Only a placeable node is
// read, because the marker can only take a node out of placement, and a node
// that cannot take work now, such as one that is offline, in HA maintenance,
// or fenced, needs no read that could fail. A node whose notes cannot be read
// is taken out of placement too, and r.NotesErr records why, so a caller that
// must know whether a node is in maintenance can treat the report as a failed
// read. It reads nothing when opts names no tag, and when opts.NotesNodes
// names nodes it reads only theirs.
func applyNodeNotes(ctx context.Context, nodesAPI NodeHealthNodes, r *NodeHealthReport, opts NodeHealthOptions) {
	wanted := normalizeMaintenanceTags(opts.MaintenanceTags)
	if len(wanted) == 0 {
		return
	}
	var errs []error
	for i := range r.Nodes {
		e := &r.Nodes[i]
		if !e.Placeable {
			continue
		}
		if len(opts.NotesNodes) > 0 && !slices.Contains(opts.NotesNodes, e.Name) {
			continue
		}
		notes, err := readNodeNotes(ctx, nodesAPI, e.Name)
		if err != nil {
			e.Placeable = false
			e.NotesUnread = true
			e.Reason = NotesUnreadReason(err)
			errs = append(errs, fmt.Errorf("node %s: %w", e.Name, err))
			continue
		}
		tag, ok := NotesMaintenanceTag(notes, wanted)
		if !ok {
			continue
		}
		e.Placeable = false
		e.Maintenance = true
		e.NotesTag = tag
		e.Health = NodeHealthMaintenance
		e.Reason = fmt.Sprintf("its notes carry the maintenance tag %q", tag)
	}
	if len(errs) > 0 {
		r.NotesErr = cpierrors.Wrap(errors.Join(errs...), "node health: read node notes")
	}
}
