package pve_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const cleanupWireUPID = "UPID:pve2:00050894:034C856A:6AA16ECE:qmclone:30880:pmx@pve!pmx:"

type cleanupTaskWireCase struct {
	name, permission, active, task string
	activeFailure                  bool
	wantErr                        bool
}

func cleanupTaskWireServer(t *testing.T, tc cleanupTaskWireCase, requests chan<- string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/", func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if r.Method != "GET" {
			t.Errorf("mutation attempted: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		body := ""
		switch {
		case r.URL.Path == "/api2/json/access/permissions":
			path := r.URL.Query().Get("path")
			if path != "/nodes/pve1" && path != "/nodes/pve2" {
				t.Errorf("wrong privilege path %q", path)
			}
			if tc.permission != "" {
				body = tc.permission
			} else {
				body = `{"data":{"` + path + `":{"Sys.Audit":0}}}`
			}
		case strings.HasSuffix(r.URL.Path, "/status"):
			if r.URL.Path != "/api2/json/nodes/pve2/tasks/"+cleanupWireUPID+"/status" {
				t.Error("task source or identity changed")
			}
			body = tc.task
			if body == "" {
				body = `{"data":{"upid":"` + cleanupWireUPID + `","node":"pve2","status":"stopped","exitstatus":"OK"}}`
			}
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			q := r.URL.Query()
			if len(q) != 3 || q.Get("source") != "active" || q.Get("start") != "0" || q.Get("limit") != "1" {
				t.Errorf("active task scan was filtered or unbounded: %v", q)
			}
			body = tc.active
			if body == "" {
				body = `{"data":[]}`
			}
			if tc.activeFailure {
				w.WriteHeader(500)
				body = `{"message":"private-task-error"}`
			}
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(404)
		}
		_, _ = w.Write([]byte(body))
	})
	return mux
}

func TestStorageCleanupTaskSettlementWire(t *testing.T) {
	for _, tc := range []cleanupTaskWireCase{
		{name: "complete"},
		{name: "filtered permissions", permission: `{"data":{"/nodes/pve1":{"VM.Audit":1}}}`, wantErr: true},
		{name: "null permissions", permission: `{"data":null}`, wantErr: true},
		{name: "malformed propagation", permission: `{"data":{"/nodes/pve1":{"Sys.Audit":"yes"}}}`, wantErr: true},
		{name: "null active", active: `{"data":null}`, wantErr: true},
		{name: "missing active", active: `{}`, wantErr: true},
		{name: "object active", active: `{"data":{}}`, wantErr: true},
		{name: "malformed active", active: `broken`, wantErr: true},
		{name: "active task", active: `{"data":[{"upid":"pending"}]}`, wantErr: true},
		{name: "inaccessible node", activeFailure: true, wantErr: true},
		{name: "null recorded", task: `{"data":null}`, wantErr: true},
		{name: "running recorded", task: `{"data":{"upid":"` + cleanupWireUPID + `","node":"pve2","status":"running"}}`, wantErr: true},
		{name: "failed recorded", task: `{"data":{"upid":"` + cleanupWireUPID + `","node":"pve2","status":"stopped","exitstatus":"failure"}}`, wantErr: true},
		{name: "wrong source node", task: `{"data":{"upid":"` + cleanupWireUPID + `","node":"pve1","status":"stopped","exitstatus":"OK"}}`, wantErr: true},
		{name: "wrong task", task: `{"data":{"upid":"other","node":"pve2","status":"stopped","exitstatus":"OK"}}`, wantErr: true},
		{name: "missing exit status", task: `{"data":{"upid":"` + cleanupWireUPID + `","node":"pve2","status":"stopped"}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 16)
			mux := cleanupTaskWireServer(t, tc, requests)
			client := newPoolStubClient(t, mux)
			nodes := []string{"pve2", "pve1", "pve2"}
			proof, err := pve.ObserveStorageCleanupTasks(t.Context(), client, nodes, []string{cleanupWireUPID, cleanupWireUPID})
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected proof: %+v %v", proof, err)
			}
			if nodes[0] != "pve2" || len(nodes) != 3 {
				t.Fatal("caller inventory modified")
			}
			if err != nil {
				if proof.Version != 0 || proof.ActiveTasksEmpty || strings.Contains(err.Error(), "private-task-error") {
					t.Fatal("partial proof or backend details escaped")
				}
				return
			}
			if proof.Version != 1 || !proof.TaskVisibilityVerified || !proof.ActiveTasksEmpty || len(proof.Nodes) != 2 || len(proof.Tasks) != 1 || proof.Tasks[0].UPID != cleanupWireUPID || proof.Tasks[0].Node != "pve2" || proof.CompletedAt.Before(proof.StartedAt) {
				t.Fatalf("incomplete evidence: %+v", proof)
			}
			if _, err := json.Marshal(proof); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 5 {
				t.Fatalf("expected complete two-node scan, got %d requests", len(requests))
			}
		})
	}
}

type cleanupTaskEvidenceClient struct {
	pve.Client
	mutate func(*pve.StorageTaskSettlementEvidence)
}

func (c cleanupTaskEvidenceClient) ObserveStorageTaskSettlement(_ context.Context, nodes, upids []string) (pve.StorageTaskSettlementEvidence, error) {
	proof := pve.StorageTaskSettlementEvidence{Version: 1, StartedAt: time.Now().UTC(), Nodes: nodes, Tasks: []pve.StorageSettledTask{}, TaskVisibilityVerified: true, ActiveTasksEmpty: true}
	for _, upid := range upids {
		proof.Tasks = append(proof.Tasks, pve.StorageSettledTask{UPID: upid, Node: "pve2", Status: "stopped", ExitStatus: "OK"})
	}
	proof.CompletedAt = time.Now().UTC()
	if c.mutate != nil {
		c.mutate(&proof)
	}
	return proof, nil
}

func TestStorageCleanupTaskSettlementRejectsIncompleteCapabilityProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*pve.StorageTaskSettlementEvidence)
	}{
		{"zero", func(p *pve.StorageTaskSettlementEvidence) { *p = pve.StorageTaskSettlementEvidence{} }},
		{"missing visibility", func(p *pve.StorageTaskSettlementEvidence) { p.TaskVisibilityVerified = false }},
		{"active unknown", func(p *pve.StorageTaskSettlementEvidence) { p.ActiveTasksEmpty = false }},
		{"missing node", func(p *pve.StorageTaskSettlementEvidence) { p.Nodes = p.Nodes[:1] }},
		{"changed node", func(p *pve.StorageTaskSettlementEvidence) { p.Nodes[0] = "other" }},
		{"missing task", func(p *pve.StorageTaskSettlementEvidence) { p.Tasks = nil }},
		{"wrong task", func(p *pve.StorageTaskSettlementEvidence) { p.Tasks[0].UPID = "wrong" }},
		{"wrong task node", func(p *pve.StorageTaskSettlementEvidence) { p.Tasks[0].Node = "pve1" }},
		{"running task", func(p *pve.StorageTaskSettlementEvidence) { p.Tasks[0].Status = "running" }},
		{"failed task", func(p *pve.StorageTaskSettlementEvidence) { p.Tasks[0].ExitStatus = "failed" }},
		{"stale", func(p *pve.StorageTaskSettlementEvidence) { p.StartedAt = p.StartedAt.Add(-time.Minute) }},
		{"future", func(p *pve.StorageTaskSettlementEvidence) { p.CompletedAt = p.CompletedAt.Add(time.Minute) }},
		{"reversed", func(p *pve.StorageTaskSettlementEvidence) { p.CompletedAt = p.StartedAt.Add(-time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof, err := pve.ObserveStorageCleanupTasks(t.Context(), cleanupTaskEvidenceClient{mutate: tc.mutate}, []string{"pve1", "pve2"}, []string{cleanupWireUPID})
			if err == nil || proof.Version != 0 {
				t.Fatal("incomplete capability proof accepted")
			}
		})
	}
	if _, err := pve.ObserveStorageCleanupTasks(t.Context(), cleanupTaskEvidenceClient{}, []string{"pve1", "pve2"}, []string{cleanupWireUPID}); err != nil {
		t.Fatal(err)
	}
}

func TestStorageCleanupTaskSettlementRejectsInputs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { t.Error("invalid input reached API"); w.WriteHeader(500) })
	client := newPoolStubClient(t, mux)
	for _, tc := range []struct{ nodes, upids []string }{
		{},
		{nodes: []string{"bad/node"}},
		{nodes: []string{"pve1"}, upids: []string{cleanupWireUPID}},
		{nodes: []string{"pve2"}, upids: []string{"UPID:pve2:short"}},
		{nodes: []string{"pve2"}, upids: []string{cleanupWireUPID + "\n"}},
	} {
		if _, err := pve.ObserveStorageCleanupTasks(t.Context(), client, tc.nodes, tc.upids); err == nil {
			t.Fatal("invalid task identity accepted")
		}
	}
	if _, err := pve.ObserveStorageCleanupTasks(t.Context(), nil, []string{"pve1"}, nil); err == nil {
		t.Fatal("missing observation capability accepted")
	}
}

func TestStorageCleanupFailedAllocationTaskIsNarrowAndReadOnly(t *testing.T) {
	for _, mode := range []string{"failed allocation", "default strict", "running allocation", "empty exit", "active peer task", "delete allowance", "upload allowance"} {
		t.Run(mode, func(t *testing.T) {
			tc := cleanupTaskWireCase{task: `{"data":{"upid":"` + cleanupWireUPID + `","node":"pve2","status":"stopped","exitstatus":"allocation failed"}}`}
			if mode == "running allocation" {
				tc.task = strings.Replace(tc.task, `"stopped"`, `"running"`, 1)
			}
			if mode == "empty exit" {
				tc.task = strings.Replace(tc.task, "allocation failed", "", 1)
			}
			if mode == "active peer task" {
				tc.active = `{"data":[{"upid":"busy"}]}`
			}
			requests := make(chan string, 16)
			client := newPoolStubClient(t, cleanupTaskWireServer(t, tc, requests))
			upid := cleanupWireUPID
			if mode == "delete allowance" {
				upid = strings.Replace(upid, "qmclone", "imgdel", 1)
			}
			if mode == "upload allowance" {
				upid = strings.Replace(upid, "qmclone", "imgcopy", 1)
			}
			var err error
			if mode == "default strict" {
				_, err = pve.ObserveStorageCleanupTasks(t.Context(), client, []string{"pve1", "pve2"}, []string{upid})
			} else {
				_, err = pve.ObserveStorageCleanupAllocationTasks(t.Context(), client, []string{"pve1", "pve2"}, []string{upid}, []string{upid})
			}
			if (err == nil) != (mode == "failed allocation") {
				t.Fatalf("wrong terminal-task policy: %v", err)
			}
		})
	}
}
