package pve_test

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const moveTaskWireUPID = "UPID:pve1:0004D2A1:03504636:6AA1786A:qmmove:777-unused0>90030-scsi0:bosh@pve!cpi:"

type moveTaskWireCase struct {
	name, permission, active string
	activeFailure            bool
	wantTasks                int
	wantSysAudit, wantErr    bool
}

func moveTaskWireServer(t *testing.T, tc moveTaskWireCase, requests *[]string) *http.ServeMux {
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
			if path := r.URL.Query().Get("path"); path != "/nodes/pve1" {
				t.Errorf("wrong privilege path %q", path)
			}
			body = tc.permission
			if body == "" {
				body = `{"data":{"/nodes/pve1":{"Sys.Audit":1}}}`
			}
		case "/api2/json/nodes/pve1/tasks":
			q := r.URL.Query()
			if len(q) != 4 || q.Get("source") != "active" || q.Get("typefilter") != "qmmove" || q.Get("start") != "0" || q.Get("limit") != "1000" {
				t.Errorf("the move task listing was filtered or unbounded: %v", q)
			}
			if q.Has("vmid") {
				t.Error("the listing used PVE's vmid filter, which misses a move to another VM")
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

// fullMoveTaskListing is a listing with as many rows as the limit allows,
// which may have hidden one more.
func fullMoveTaskListing() string {
	rows := make([]string, 1000)
	for i := range rows {
		rows[i] = `{"upid":"UPID:pve1:0004D2A1:03504636:6AA1786A:qmmove:` + strconv.Itoa(1000+i) + `:bosh@pve!cpi:","node":"pve1","type":"qmmove","id":"` + strconv.Itoa(1000+i) + `","status":"RUNNING"}`
	}
	return `{"data":[` + strings.Join(rows, ",") + `]}`
}

func TestActiveMoveTasksWire(t *testing.T) {
	for _, tc := range []moveTaskWireCase{
		{name: "empty"},
		{name: "running move", active: `{"data":[{"upid":"` + moveTaskWireUPID + `","node":"pve1","type":"qmmove","id":"777-unused0>90030-scsi0","status":"RUNNING"}]}`, wantTasks: 1},
		{name: "no Sys.Audit", permission: `{"data":{"/nodes/pve1":{"VM.Audit":1}}}`, wantSysAudit: true, wantErr: true},
		{name: "null permissions", permission: `{"data":null}`, wantErr: true},
		{name: "malformed propagation", permission: `{"data":{"/nodes/pve1":{"Sys.Audit":"yes"}}}`, wantErr: true},
		{name: "null listing", active: `{"data":null}`, wantErr: true},
		{name: "missing listing", active: `{}`, wantErr: true},
		{name: "object listing", active: `{"data":{}}`, wantErr: true},
		{name: "malformed listing", active: `broken`, wantErr: true},
		{name: "malformed UPID", active: `{"data":[{"upid":"pending","id":"777"}]}`, wantErr: true},
		{name: "full listing", active: fullMoveTaskListing(), wantErr: true},
		{name: "inaccessible node", activeFailure: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) { checkMoveTaskWireCase(t, tc) })
	}
	var requests []string
	client := newPoolStubClient(t, moveTaskWireServer(t, moveTaskWireCase{}, &requests))
	reader, ok := client.(pve.ActiveMoveTaskReader)
	if !ok {
		t.Fatal("the PVE client cannot list active move tasks")
	}
	if _, err := reader.ActiveMoveTasks(t.Context(), "pve1/../x"); err == nil || len(requests) != 0 {
		t.Fatalf("a malformed node reached PVE: %v %v", err, requests)
	}
}

// checkMoveTaskWireCase lists node pve1's active move tasks against a server
// that answers as tc says, and checks the listing or the refusal.
func checkMoveTaskWireCase(t *testing.T, tc moveTaskWireCase) {
	t.Helper()
	var requests []string
	client := newPoolStubClient(t, moveTaskWireServer(t, tc, &requests))
	reader, ok := client.(pve.ActiveMoveTaskReader)
	if !ok {
		t.Fatal("the PVE client cannot list active move tasks")
	}
	tasks, err := reader.ActiveMoveTasks(t.Context(), "pve1")
	if (err != nil) != tc.wantErr {
		t.Fatalf("unexpected listing: %+v %v", tasks, err)
	}
	if err != nil {
		requireMoveTaskRefusal(t, tc, tasks, err, requests)
		return
	}
	if len(tasks) != tc.wantTasks || len(requests) != 2 {
		t.Fatalf("want %d tasks from one permission read and one listing, got %+v from %v", tc.wantTasks, tasks, requests)
	}
	if tc.wantTasks == 1 && (tasks[0].UPID != moveTaskWireUPID || tasks[0].ID != "777-unused0>90030-scsi0") {
		t.Fatalf("the task was not decoded: %+v", tasks[0])
	}
}

// requireMoveTaskRefusal checks a refused listing. It must be a Sys.Audit
// refusal exactly when tc wants one, return no partial listing, keep PVE's
// text out of its own text, and, without Sys.Audit, never reach the listing.
func requireMoveTaskRefusal(t *testing.T, tc moveTaskWireCase, tasks []pve.ActiveTask, err error, requests []string) {
	t.Helper()
	var visibility *pve.AuditVisibilityError
	if isSysAudit := errors.As(err, &visibility) && visibility.Privilege == "Sys.Audit" && visibility.Path == "/nodes/pve1"; isSysAudit != tc.wantSysAudit {
		t.Fatalf("want a Sys.Audit refusal %v, got %v", tc.wantSysAudit, err)
	}
	if tasks != nil || strings.Contains(err.Error(), "private-task-error") {
		t.Fatal("a partial listing or backend details escaped")
	}
	if tc.activeFailure {
		want := "could not read active move tasks at /nodes/pve1/tasks (HTTP 500: private-task-error)"
		if got := pve.DescribeAuditError(err); got != want {
			t.Fatalf("description %q, want %q", got, want)
		}
	}
	if tc.wantSysAudit && len(requests) != 1 {
		t.Fatalf("the listing ran without Sys.Audit: %v", requests)
	}
}

func TestMoveTaskIDNamesVM(t *testing.T) {
	for id, want := range map[string]bool{
		"777":                     true,
		"777-unused0>90030-scsi0": true,
		"777-scsi1>888-scsi2":     true,
		"7770":                    false,
		"7770-unused0>9-scsi0":    false,
		"77-unused0>9-scsi0":      false,
		"1777-scsi1>777-scsi0":    false,
		"":                        false,
	} {
		if got := pve.MoveTaskIDNamesVM(id, 777); got != want {
			t.Errorf("MoveTaskIDNamesVM(%q, 777) = %v, want %v", id, got, want)
		}
	}
	if pve.MoveTaskIDNamesVM("0", 0) || pve.MoveTaskIDNamesVM("-1", -1) {
		t.Error("a VMID that is not positive matched")
	}
}
