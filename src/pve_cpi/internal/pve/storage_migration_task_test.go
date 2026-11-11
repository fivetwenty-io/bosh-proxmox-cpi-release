package pve_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const migrationWireUPID = "UPID:pve2:0001A2B3:034C856A:6AA16ECE:qmigrate:90144:pmx@pve!pmx:"

// settledMigrationTaskWire answers the reads ObserveSettledMigrationTask
// makes: the token's privileges on the UPID's node and the task's status
// there. It records every path it serves and fails the test on any write.
type settledMigrationTaskWire struct {
	permission string
	status     string
	statusCode int
	mu         sync.Mutex
	paths      []string
}

func (w *settledMigrationTaskWire) mux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/", func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.paths = append(w.paths, r.URL.Path)
		w.mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("mutation attempted: %s %s", r.Method, r.URL.Path)
		}
		rw.Header().Set("Content-Type", "application/json")
		body := ""
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			if path := r.URL.Query().Get("path"); path != "/nodes/pve2" {
				t.Errorf("privilege read for %q, want the UPID's node /nodes/pve2", path)
			}
			body = w.permission
			if body == "" {
				body = `{"data":{"/nodes/pve2":{"Sys.Audit":1}}}`
			}
		case "/api2/json/nodes/pve2/tasks/" + migrationWireUPID + "/status":
			if w.statusCode != 0 {
				rw.WriteHeader(w.statusCode)
				body = `{"message":"private-task-error"}`
				break
			}
			body = w.status
			if body == "" {
				body = `{"data":{"upid":"` + migrationWireUPID +
					`","node":"pve2","status":"stopped","exitstatus":"OK","type":"qmigrate","id":"90144"}}`
			}
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			rw.WriteHeader(http.StatusNotFound)
		}
		_, _ = rw.Write([]byte(body))
	})
	return mux
}

func (w *settledMigrationTaskWire) served() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.paths...)
}

// TestObserveSettledMigrationTaskAcceptsAStoppedOKMigration reads a qmigrate
// task of the named VM that stopped with exit status OK, on the node its
// UPID names, after proving Sys.Audit there. It reads nothing else, and it
// never lists the node's active tasks.
func TestObserveSettledMigrationTaskAcceptsAStoppedOKMigration(t *testing.T) {
	wire := &settledMigrationTaskWire{}
	client := newPoolStubClient(t, wire.mux(t))
	task, err := pve.ObserveSettledMigrationTask(t.Context(), client, migrationWireUPID, 90144)
	if err != nil {
		t.Fatalf("ObserveSettledMigrationTask = %v, want the task", err)
	}
	if task.UPID != migrationWireUPID || task.Node != "pve2" || task.Status != "stopped" || task.ExitStatus != "OK" {
		t.Fatalf("task = %+v, want the stopped OK migration on pve2", task)
	}
	want := []string{"/api2/json/access/permissions", "/api2/json/nodes/pve2/tasks/" + migrationWireUPID + "/status"}
	if got := wire.served(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("reads = %v, want %v", got, want)
	}
}

// migrationStatus is a task status answer for the wire UPID from node with
// the given status and exit fields, which are left out when empty.
func migrationStatus(upid, node, status, exit string) string {
	body := `{"data":{"upid":"` + upid + `","node":"` + node + `","status":"` + status + `"`
	if exit != "" {
		body += `,"exitstatus":"` + exit + `"`
	}
	return body + "}}"
}

// TestObserveSettledMigrationTaskRefusesAnUnsettledOrForeignTask refuses each
// way the task can fail to prove a finished migration of the VM. A UPID that
// isn't a qmigrate of the VM refuses before any read.
func TestObserveSettledMigrationTaskRefusesAnUnsettledOrForeignTask(t *testing.T) {
	const settledText = "stopped with exit status OK"
	for _, tc := range []struct {
		name       string
		upid       string
		vmid       int
		permission string
		status     string
		statusCode int
		wantText   string
		wantReads  int
	}{
		{name: "running", status: migrationStatus(migrationWireUPID, "pve2", "running", ""),
			wantText: settledText, wantReads: 2},
		{name: "failed", status: migrationStatus(migrationWireUPID, "pve2", "stopped", "migration aborted"),
			wantText: settledText, wantReads: 2},
		{name: "warnings", status: migrationStatus(migrationWireUPID, "pve2", "stopped", "WARNINGS: 1"),
			wantText: settledText, wantReads: 2},
		{name: "empty exit", status: migrationStatus(migrationWireUPID, "pve2", "stopped", ""),
			wantText: settledText, wantReads: 2},
		{name: "other node answers", status: migrationStatus(migrationWireUPID, "pve1", "stopped", "OK"),
			wantText: settledText, wantReads: 2},
		{name: "other task answers", status: migrationStatus("UPID:pve2:other", "pve2", "stopped", "OK"),
			wantText: settledText, wantReads: 2},
		{name: "null status", status: `{"data":null}`, wantText: settledText, wantReads: 2},
		{name: "status unreadable", statusCode: http.StatusInternalServerError, wantText: settledText, wantReads: 2},
		{name: "no Sys.Audit", permission: `{"data":{"/nodes/pve2":{"VM.Audit":1}}}`,
			wantText: "Sys.Audit on node pve2", wantReads: 1},
		{name: "malformed permissions", permission: `{"data":{"/nodes/pve2":{"Sys.Audit":"yes"}}}`,
			wantText: "visibility", wantReads: 1},
		{name: "not a migration", upid: strings.Replace(migrationWireUPID, "qmigrate", "qmclone", 1),
			wantText: "not a qmigrate task"},
		{name: "another VM", vmid: 90145, wantText: "does not name VM 90145"},
		{name: "no VM", vmid: -1, wantText: "does not name VM"},
		{name: "malformed UPID", upid: "UPID:pve2:qmigrate:90144", wantText: "malformed"},
		{name: "path in UPID", upid: "UPID:pve2:0001A2B3:034C856A:6AA16ECE:qmigrate:90144/../x:pmx@pve!pmx:",
			wantText: "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := &settledMigrationTaskWire{permission: tc.permission, status: tc.status, statusCode: tc.statusCode}
			client := newPoolStubClient(t, wire.mux(t))
			upid, vmid := migrationWireUPID, 90144
			if tc.upid != "" {
				upid = tc.upid
			}
			if tc.vmid != 0 {
				vmid = tc.vmid
			}
			task, err := pve.ObserveSettledMigrationTask(t.Context(), client, upid, vmid)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("ObserveSettledMigrationTask = %+v, %v; want a refusal containing %q",
					task, err, tc.wantText)
			}
			if strings.Contains(err.Error(), "private-task-error") {
				t.Fatalf("the refusal carries the backend's text: %v", err)
			}
			if task != (pve.StorageSettledTask{}) {
				t.Fatalf("a refusal returned task %+v", task)
			}
			if got := wire.served(); len(got) != tc.wantReads {
				t.Fatalf("reads = %v, want %d", got, tc.wantReads)
			}
		})
	}
}

// migrationTaskStub answers ObserveMigrationTaskSettlement with a fixed task,
// the way a decorated client forwards the read.
type migrationTaskStub struct {
	pve.Client
	task  pve.StorageSettledTask
	err   error
	calls int
}

func (s *migrationTaskStub) ObserveMigrationTaskSettlement(context.Context, string) (pve.StorageSettledTask, error) {
	s.calls++
	return s.task, s.err
}

// TestObserveSettledMigrationTaskChecksTheReadersAnswer checks the answer of
// whatever client supplies the read again, so a reader that returns another
// task or an unfinished one can't settle the migration, and refuses a client
// that can't read task status at all.
func TestObserveSettledMigrationTaskChecksTheReadersAnswer(t *testing.T) {
	settled := pve.StorageSettledTask{UPID: migrationWireUPID, Node: "pve2", Status: "stopped", ExitStatus: "OK"}
	task, err := pve.ObserveSettledMigrationTask(t.Context(), &migrationTaskStub{task: settled}, migrationWireUPID, 90144)
	if err != nil || task != settled {
		t.Fatalf("a settled answer = %+v, %v; want it accepted", task, err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*pve.StorageSettledTask)
		err    error
	}{
		{name: "another task", mutate: func(task *pve.StorageSettledTask) {
			task.UPID = strings.Replace(task.UPID, "90144", "90145", 1)
		}},
		{name: "another node", mutate: func(task *pve.StorageSettledTask) { task.Node = "pve1" }},
		{name: "running", mutate: func(task *pve.StorageSettledTask) { task.Status = "running" }},
		{name: "failed", mutate: func(task *pve.StorageSettledTask) { task.ExitStatus = "failed" }},
		{name: "read error", err: errors.New("status read failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := settled
			if tc.mutate != nil {
				tc.mutate(&task)
			}
			stub := &migrationTaskStub{task: task, err: tc.err}
			if got, err := pve.ObserveSettledMigrationTask(t.Context(), stub, migrationWireUPID, 90144); err == nil {
				t.Fatalf("answer %+v accepted as %+v", task, got)
			}
			if stub.calls != 1 {
				t.Fatalf("reader called %d times, want once", stub.calls)
			}
		})
	}
	if _, err := pve.ObserveSettledMigrationTask(t.Context(), nil, migrationWireUPID, 90144); err == nil {
		t.Fatal("a client that can't read task status was accepted")
	}
	var missing context.Context
	stub := &migrationTaskStub{task: settled}
	if _, err := pve.ObserveSettledMigrationTask(missing, stub, migrationWireUPID, 90144); err == nil {
		t.Fatal("a nil context was accepted")
	}
}
