package pve_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const migrationTaskWireUPID = "UPID:pve1:0004D2A1:03504636:6AA1786A:qmigrate:777:root@pam:"

// migrationTaskWireServer answers the permission read and the qmigrate task
// listing of node pve1 with permission and listing, and fails the listing
// when failure is set.
func migrationTaskWireServer(t *testing.T, permission, listing string, failure bool, requests *[]string) *http.ServeMux {
	t.Helper()
	return vmTaskWireServer(t, "qmigrate", permission, listing, failure, requests)
}

// vmTaskWireServer answers the permission read and the taskType listing of
// node pve1 with permission and listing, and fails the listing when failure
// is set.
func vmTaskWireServer(
	t *testing.T, taskType, permission, listing string, failure bool, requests *[]string,
) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/", func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.URL.Path)
		if r.Method != "GET" {
			t.Errorf("mutation attempted: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		body := ""
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			body = permission
			if body == "" {
				body = `{"data":{"/nodes/pve1":{"Sys.Audit":1}}}`
			}
		case "/api2/json/nodes/pve1/tasks":
			q := r.URL.Query()
			if q.Get("source") != "all" || q.Get("typefilter") != taskType || q.Get("vmid") != "777" ||
				q.Get("start") != "0" || q.Get("limit") != "1000" || q.Get("since") != "1700000000" {
				t.Errorf("the migration task listing was filtered wrongly or unbounded: %v", q)
			}
			body = listing
			if body == "" {
				body = `{"data":[]}`
			}
			if failure {
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

// TestMigrationTasksWire lists the qmigrate tasks of VM 777 on pve1 and checks
// that a listing that could hide a task, or that names another task, is
// refused.
func TestMigrationTasksWire(t *testing.T) {
	since := time.Unix(1700000000, 0)
	done := `{"upid":"` + migrationTaskWireUPID + `","node":"pve1","type":"qmigrate","id":"777","status":"OK",` +
		`"starttime":1700000100,"endtime":1700000160}`
	running := `{"upid":"` + migrationTaskWireUPID + `","node":"pve1","type":"qmigrate","id":"777","status":"RUNNING",` +
		`"starttime":1700000100}`
	for _, tc := range []struct {
		name, permission, listing string
		failure, wantSysAudit     bool
		wantErr                   bool
		wantTasks                 int
	}{
		{name: "empty"},
		{name: "ended migration", listing: `{"data":[` + done + `]}`, wantTasks: 1},
		{name: "running migration", listing: `{"data":[` + running + `]}`, wantTasks: 1},
		{name: "no Sys.Audit", permission: `{"data":{"/nodes/pve1":{"VM.Audit":1}}}`, wantSysAudit: true,
			wantErr: true},
		{name: "null listing", listing: `{"data":null}`, wantErr: true},
		{name: "malformed UPID", listing: `{"data":[{"upid":"pending","type":"qmigrate","id":"777"}]}`, wantErr: true},
		{name: "another VM", listing: `{"data":[` + strings.Replace(done, `"id":"777"`, `"id":"778"`, 1) + `]}`,
			wantErr: true},
		{name: "another type", listing: `{"data":[` + strings.Replace(done, `"type":"qmigrate"`, `"type":"qmstart"`,
			1) + `]}`, wantErr: true},
		{name: "inaccessible node", failure: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			client := newPoolStubClient(t, migrationTaskWireServer(t, tc.permission, tc.listing, tc.failure, &requests))
			reader, ok := client.(pve.MigrationTaskReader)
			if !ok {
				t.Fatal("the PVE client cannot list migration tasks")
			}
			tasks, err := reader.MigrationTasks(t.Context(), "pve1", 777, since)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected listing: %+v %v", tasks, err)
			}
			var visibility *pve.AuditVisibilityError
			if got := errors.As(err, &visibility) && visibility.Privilege == "Sys.Audit"; got != tc.wantSysAudit {
				t.Fatalf("want a Sys.Audit refusal %v, got %v", tc.wantSysAudit, err)
			}
			if tc.wantSysAudit && len(requests) != 1 {
				t.Fatalf("the listing ran without Sys.Audit: %v", requests)
			}
			if err != nil {
				if tasks != nil || strings.Contains(err.Error(), "private-task-error") {
					t.Fatal("a partial listing or backend details escaped")
				}
				return
			}
			if len(tasks) != tc.wantTasks {
				t.Fatalf("tasks %+v, want %d", tasks, tc.wantTasks)
			}
		})
	}
}

// TestStartTasksWire lists the qmstart tasks of VM 777 on pve1 through the
// same proof and refusals as the migration listing.
func TestStartTasksWire(t *testing.T) {
	since := time.Unix(1700000000, 0)
	upid := strings.Replace(migrationTaskWireUPID, ":qmigrate:", ":qmstart:", 1)
	done := `{"upid":"` + upid + `","node":"pve1","type":"qmstart","id":"777","status":"OK",` +
		`"starttime":1700000100,"endtime":1700000105}`
	for _, tc := range []struct {
		name, permission, listing string
		wantSysAudit, wantErr     bool
		wantTasks                 int
	}{
		{name: "empty"},
		{name: "a start", listing: `{"data":[` + done + `]}`, wantTasks: 1},
		{name: "no Sys.Audit", permission: `{"data":{"/nodes/pve1":{"VM.Audit":1}}}`, wantSysAudit: true,
			wantErr: true},
		{name: "another type", listing: `{"data":[` + strings.Replace(done, `"type":"qmstart"`, `"type":"qmigrate"`,
			1) + `]}`, wantErr: true},
		{name: "another VM", listing: `{"data":[` + strings.Replace(done, `"id":"777"`, `"id":"778"`, 1) + `]}`,
			wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			client := newPoolStubClient(t, vmTaskWireServer(t, "qmstart", tc.permission, tc.listing, false,
				&requests))
			reader, ok := client.(pve.MigrationTaskReader)
			if !ok {
				t.Fatal("the PVE client cannot list start tasks")
			}
			tasks, err := reader.StartTasks(t.Context(), "pve1", 777, since)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected listing: %+v %v", tasks, err)
			}
			var visibility *pve.AuditVisibilityError
			if got := errors.As(err, &visibility) && visibility.Privilege == "Sys.Audit"; got != tc.wantSysAudit {
				t.Fatalf("want a Sys.Audit refusal %v, got %v", tc.wantSysAudit, err)
			}
			if tc.wantSysAudit && len(requests) != 1 {
				t.Fatalf("the listing ran without Sys.Audit: %v", requests)
			}
			if err == nil && len(tasks) != tc.wantTasks {
				t.Fatalf("tasks %+v, want %d", tasks, tc.wantTasks)
			}
			if err != nil && tasks != nil {
				t.Fatal("a partial listing escaped")
			}
		})
	}
}

// TestMigrationTaskOutcome reads a task's outcome the way PVE reports it.
func TestMigrationTaskOutcome(t *testing.T) {
	for _, tc := range []struct {
		task               pve.VMTask
		running, succeeded bool
	}{
		{pve.VMTask{Status: "OK", EndTime: 10}, false, true},
		{pve.VMTask{Status: "WARNINGS: 2", EndTime: 10}, false, true},
		{pve.VMTask{Status: "WARNINGS: 2"}, true, false},
		{pve.VMTask{Status: "WARNINGS: two", EndTime: 10}, false, false},
		{pve.VMTask{Status: "WARNINGS: 2 extra", EndTime: 10}, false, false},
		{pve.VMTask{Status: "migration aborted", EndTime: 10}, false, false},
		{pve.VMTask{Status: "RUNNING", EndTime: 10}, true, false},
		{pve.VMTask{Status: "RUNNING"}, true, false},
		{pve.VMTask{}, true, false},
	} {
		if got := tc.task.Running(); got != tc.running {
			t.Errorf("%+v Running() = %v, want %v", tc.task, got, tc.running)
		}
		if got := tc.task.Succeeded(); got != tc.succeeded {
			t.Errorf("%+v Succeeded() = %v, want %v", tc.task, got, tc.succeeded)
		}
	}
	if !(pve.VMTask{EndTime: 10}).End().Equal(time.Unix(10, 0)) || !(pve.VMTask{}).End().IsZero() {
		t.Error("End does not read the end time")
	}
}

// taskLogWireServer answers the permission read and the log of task
// migrationTaskWireUPID on node pve1 with permission and the lines of log,
// paged by the start and limit the read sends. A line holds its number and
// its text, unless renumber changes the number PVE reports.
func taskLogWireServer(
	t *testing.T, permission string, log []string, renumber func(int) int, requests *[]string,
) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/", func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.URL.Path)
		if r.Method != "GET" {
			t.Errorf("mutation attempted: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			if permission == "" {
				permission = `{"data":{"/nodes/pve1":{"Sys.Audit":1}}}`
			}
			_, _ = w.Write([]byte(permission))
		case "/api2/json/nodes/pve1/tasks/" + migrationTaskWireUPID + "/log":
			start, _ := strconv.Atoi(r.URL.Query().Get("start"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit != 1000 {
				t.Errorf("the task log read asked for %d lines a page", limit)
			}
			page := []map[string]any{}
			for i := start; i < len(log) && i < start+limit; i++ {
				n := i + 1
				if renumber != nil {
					n = renumber(n)
				}
				page = append(page, map[string]any{"n": n, "t": log[i]})
			}
			body, _ := json.Marshal(map[string]any{"data": page, "total": len(log)})
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	return mux
}

// TestTaskLogWire reads the log of a qmigrate task on pve1 and checks that a
// log PVE could not open, an empty log, a log with a missing line, a log
// longer than the read allows, and a task of another node are refused.
func TestTaskLogWire(t *testing.T) {
	long := make([]string, 2500)
	for i := range long {
		long[i] = fmt.Sprintf("line %d", i+1)
	}
	tooLong := make([]string, 50001)
	for i := range tooLong {
		tooLong[i] = "x"
	}
	for _, tc := range []struct {
		name, permission, node string
		log                    []string
		renumber               func(int) int
		wantSysAudit, wantErr  bool
		wantLines              int
	}{
		{name: "a short log", log: []string{"starting migration", "TASK OK"}, wantLines: 2},
		{name: "a log over three pages", log: long, wantLines: 2500},
		{name: "no Sys.Audit", log: []string{"TASK OK"}, permission: `{"data":{"/nodes/pve1":{"VM.Audit":1}}}`,
			wantSysAudit: true, wantErr: true},
		{name: "a log PVE could not open", log: []string{"unable to open file - No such file or directory"},
			wantErr: true},
		{name: "an empty log", log: []string{"no content"}, wantErr: true},
		{name: "a line missing", log: []string{"a", "b", "c"},
			renumber: func(n int) int { return map[bool]int{true: n + 1, false: n}[n > 1] }, wantErr: true},
		{name: "a log longer than the read allows", log: tooLong, wantErr: true},
		{name: "a task of another node", node: "pve2", log: []string{"TASK OK"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			client := newPoolStubClient(t, taskLogWireServer(t, tc.permission, tc.log, tc.renumber, &requests))
			reader, ok := client.(pve.MigrationTaskReader)
			if !ok {
				t.Fatal("the PVE client cannot read task logs")
			}
			node := tc.node
			if node == "" {
				node = "pve1"
			}
			lines, err := reader.TaskLog(t.Context(), node, migrationTaskWireUPID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected read: %d lines, %v", len(lines), err)
			}
			var visibility *pve.AuditVisibilityError
			if got := errors.As(err, &visibility) && visibility.Privilege == "Sys.Audit"; got != tc.wantSysAudit {
				t.Fatalf("want a Sys.Audit refusal %v, got %v", tc.wantSysAudit, err)
			}
			if tc.wantSysAudit && len(requests) != 1 {
				t.Fatalf("the log read ran without Sys.Audit: %v", requests)
			}
			if err != nil {
				if lines != nil {
					t.Fatal("a partial log escaped")
				}
				return
			}
			if len(lines) != tc.wantLines || lines[0] != tc.log[0] || lines[len(lines)-1] != tc.log[len(tc.log)-1] {
				t.Fatalf("read %d lines, want %d in order", len(lines), tc.wantLines)
			}
		})
	}
}
