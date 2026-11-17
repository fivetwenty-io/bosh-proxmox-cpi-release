package pve

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// statusOnly serves /cluster/status rows and counts the reads.
type statusOnly struct {
	rows  []string
	err   error
	reads int
}

func (s *statusOnly) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	out := make(sdkcluster.ListStatusResponse, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, json.RawMessage(r))
	}
	return &out, nil
}

func TestReadClusterQuorum(t *testing.T) {
	t.Parallel()
	lost := &statusOnly{rows: []string{
		`{"type":"cluster","name":"az2","quorate":0,"nodes":3}`,
		`{"type":"node","name":"lab-pve-cpi-az2-0","online":0}`,
		`{"type":"node","name":"lab-pve-cpi-az2-1","online":1,"local":1}`,
		`{"type":"node","name":"lab-pve-cpi-az2-2","online":0}`,
	}}
	q, err := ReadClusterQuorum(context.Background(), lost)
	if err != nil || !q.Lost() || q.EntryNode != "lab-pve-cpi-az2-1" {
		t.Fatalf("a cluster row with quorate 0 must prove lost quorum: %+v %v", q, err)
	}
	if !q.Offline["lab-pve-cpi-az2-0"] || !q.Offline["lab-pve-cpi-az2-2"] || q.Offline["lab-pve-cpi-az2-1"] {
		t.Fatalf("offline members = %v", q.Offline)
	}

	standalone := &statusOnly{rows: []string{`{"type":"node","name":"pve","online":1,"local":1}`}}
	if q, err := ReadClusterQuorum(context.Background(), standalone); err != nil || q.Lost() {
		t.Fatalf("a standalone host never proves lost quorum: %+v %v", q, err)
	}

	quorate := &statusOnly{rows: []string{`{"type":"cluster","quorate":1}`, `{"type":"node","name":"a","online":1}`}}
	if q, err := ReadClusterQuorum(context.Background(), quorate); err != nil || q.Lost() {
		t.Fatalf("a quorate cluster does not prove lost quorum: %+v %v", q, err)
	}

	failing := &statusOnly{err: errors.New("read tcp: connection reset by peer")}
	if _, err := ReadClusterQuorum(context.Background(), failing); err == nil || failing.reads != 1 {
		t.Fatalf("a failed read must come back after a single attempt: reads=%d err=%v", failing.reads, err)
	}
}
