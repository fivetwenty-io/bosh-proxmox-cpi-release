// storage_cleanup_tasks_clock_internal_test.go covers the clock that orders a
// storage cleanup task observation. The order is decided on the monotonic
// clock, so a wall-clock step during the check keeps good evidence, while
// evidence from outside the observation's window still refuses.
package pve

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

const cleanupClockUPID = "UPID:pve2:00050894:034C856A:6AA16ECE:qmclone:30880:pmx@pve!pmx:"

// cleanupClockReader answers the reads of one cleanup task observation for
// two nodes with no active tasks and one settled task.
type cleanupClockReader struct{}

func (cleanupClockReader) GetCtx(_ context.Context, path string, params map[string]interface{}) (interface{}, error) {
	switch {
	case path == "/access/permissions":
		return map[string]any{params["path"].(string): map[string]any{"Sys.Audit": 1}}, nil
	case strings.HasSuffix(path, "/status"):
		return map[string]any{"upid": cleanupClockUPID, "node": "pve2", "status": "stopped", "exitstatus": "OK"}, nil
	default:
		return []any{}, nil
	}
}

// cleanupClockClient makes the observation through this package's own
// observer, the way the SDK client does.
type cleanupClockClient struct {
	Client
}

func (cleanupClockClient) ObserveStorageTaskSettlement(ctx context.Context, nodes, upids []string) (StorageTaskSettlementEvidence, error) {
	return observeStorageCleanupTasks(ctx, cleanupClockReader{}, nodes, upids)
}

// scriptedCleanupClock returns its readings in order. Each reading's
// monotonic part advances, while its wall part is whatever the script says.
type scriptedCleanupClock struct {
	mu       sync.Mutex
	readings []cleanupClockReading
	next     int
}

func (c *scriptedCleanupClock) read() cleanupClockReading {
	c.mu.Lock()
	defer c.mu.Unlock()
	reading := c.readings[c.next]
	c.next++
	return reading
}

func cleanupClockReadings(walls ...time.Time) []cleanupClockReading {
	readings := make([]cleanupClockReading, len(walls))
	for i, wall := range walls {
		readings[i] = cleanupClockReading{wall: wall, mono: time.Duration(i+1) * time.Second}
	}
	return readings
}

func TestStorageCleanupTaskObservationSurvivesBackwardWallClockStep(t *testing.T) {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	back := base.Add(-30 * time.Second)
	// The readings are the caller's start, the observer's start, the
	// observer's end, and the caller's end.
	for _, tc := range []struct {
		name  string
		walls []time.Time
	}{
		{"steady", []time.Time{base, base, base.Add(time.Second), base.Add(time.Second)}},
		{"step before observer start", []time.Time{base, back, back, back}},
		{"step during observation", []time.Time{base, base, back, back}},
		{"step before caller end", []time.Time{base, base, base, back}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &scriptedCleanupClock{readings: cleanupClockReadings(tc.walls...)}
			ctx := context.WithValue(t.Context(), cleanupClockKey{}, clock.read)
			proof, err := ObserveStorageCleanupTasks(ctx, cleanupClockClient{}, []string{"pve1", "pve2"}, []string{cleanupClockUPID})
			if err != nil {
				t.Fatalf("good evidence refused: %v", err)
			}
			if clock.next != 4 {
				t.Fatalf("expected four clock readings, got %d", clock.next)
			}
			if !proof.StartedAt.Equal(tc.walls[1]) || !proof.CompletedAt.Equal(tc.walls[2]) || proof.StartedAt.Location() != time.UTC || proof.CompletedAt.Location() != time.UTC {
				t.Fatalf("stored evidence times differ from the observer's wall readings: %v %v", proof.StartedAt, proof.CompletedAt)
			}
		})
	}
}

func TestStorageCleanupTaskObservationRealClockKeepsWallTimes(t *testing.T) {
	before := time.Now()
	proof, err := ObserveStorageCleanupTasks(t.Context(), cleanupClockClient{}, []string{"pve1", "pve2"}, []string{cleanupClockUPID})
	if err != nil {
		t.Fatal(err)
	}
	if !proof.observed.set || proof.StartedAt.Location() != time.UTC || proof.CompletedAt.Location() != time.UTC {
		t.Fatalf("observation window or UTC wall times missing: %+v", proof)
	}
	if strings.Contains(proof.StartedAt.String(), "m=") || strings.Contains(proof.CompletedAt.String(), "m=") {
		t.Fatal("stored evidence times carry a monotonic reading")
	}
	if proof.observed.from.mono < before.Sub(cleanupClockOrigin) {
		t.Fatal("observation window precedes the test's own reading")
	}
}

func TestStorageCleanupTaskObservationRefusesEvidenceOutsideWindow(t *testing.T) {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	reading := func(wall time.Time, mono time.Duration) cleanupClockReading {
		return cleanupClockReading{wall: wall, mono: mono}
	}
	started, completed := reading(base, 10*time.Second), reading(base.Add(time.Second), 20*time.Second)
	nodes, upids := []string{"pve1", "pve2"}, []string{cleanupClockUPID}
	good := func() StorageTaskSettlementEvidence {
		from, until := reading(base, 12*time.Second), reading(base, 18*time.Second)
		return StorageTaskSettlementEvidence{
			Version: 1, StartedAt: from.wall, CompletedAt: until.wall, Nodes: nodes,
			Tasks:                  []StorageSettledTask{{UPID: cleanupClockUPID, Node: "pve2", Status: taskStatusStopped, ExitStatus: "OK"}},
			TaskVisibilityVerified: true, ActiveTasksEmpty: true,
			observed: cleanupObservationWindow{set: true, from: from, until: until},
		}
	}
	if err := validateCleanupTaskEvidence(good(), nodes, upids, started, completed); err != nil {
		t.Fatalf("good evidence refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*StorageTaskSettlementEvidence)
	}{
		{"observed before caller start", func(p *StorageTaskSettlementEvidence) { p.observed.from.mono = 9 * time.Second }},
		{"observed after caller end", func(p *StorageTaskSettlementEvidence) { p.observed.until.mono = 21 * time.Second }},
		{"reversed observation", func(p *StorageTaskSettlementEvidence) { p.observed.until.mono = 11 * time.Second }},
		{"stored start differs", func(p *StorageTaskSettlementEvidence) { p.StartedAt = p.StartedAt.Add(-time.Minute) }},
		{"stored end differs", func(p *StorageTaskSettlementEvidence) { p.CompletedAt = p.CompletedAt.Add(time.Minute) }},
		{"wall only stale", func(p *StorageTaskSettlementEvidence) {
			p.observed = cleanupObservationWindow{}
			p.StartedAt = base.Add(-time.Minute)
		}},
		{"wall only future", func(p *StorageTaskSettlementEvidence) {
			p.observed = cleanupObservationWindow{}
			p.CompletedAt = base.Add(time.Minute)
		}},
		{"wall only reversed", func(p *StorageTaskSettlementEvidence) {
			p.observed = cleanupObservationWindow{}
			p.StartedAt, p.CompletedAt = base.Add(time.Second), base
		}},
		{"wrong version", func(p *StorageTaskSettlementEvidence) { p.Version = 2 }},
		{"visibility unverified", func(p *StorageTaskSettlementEvidence) { p.TaskVisibilityVerified = false }},
		{"active tasks unknown", func(p *StorageTaskSettlementEvidence) { p.ActiveTasksEmpty = false }},
		{"wrong nodes", func(p *StorageTaskSettlementEvidence) { p.Nodes = []string{"pve1"} }},
		{"wrong task", func(p *StorageTaskSettlementEvidence) { p.Tasks[0].UPID = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof := good()
			tc.mutate(&proof)
			if err := validateCleanupTaskEvidence(proof, nodes, upids, started, completed); err == nil {
				t.Fatal("evidence outside the observation's scope or window accepted")
			}
		})
	}
}

func TestStorageCleanupTaskObservationRefusesReplayedEvidence(t *testing.T) {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	clock := &scriptedCleanupClock{readings: cleanupClockReadings(base, base, base, base, base, base)}
	ctx := context.WithValue(t.Context(), cleanupClockKey{}, clock.read)
	earlier, err := ObserveStorageCleanupTasks(ctx, cleanupClockClient{}, []string{"pve1", "pve2"}, []string{cleanupClockUPID})
	if err != nil {
		t.Fatal(err)
	}
	// A later check that receives the earlier proof, even with an identical
	// wall clock, refuses it on the monotonic clock.
	started, completed := clock.read(), clock.read()
	if err := validateCleanupTaskEvidence(earlier, []string{"pve1", "pve2"}, []string{cleanupClockUPID}, started, completed); err == nil {
		t.Fatal("evidence from an earlier observation accepted")
	}
}
