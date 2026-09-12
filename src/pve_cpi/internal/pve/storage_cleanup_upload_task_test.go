package pve_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const cleanupUploadWireUPID = "UPID:lab-pve-cpi-0:00243A13:0421971E:6AA39043:imgcopy::pmx@pve!pmx:"

type cleanupUploadWireServer struct {
	t            *testing.T
	mode         string
	historyCalls int
	statusCalls  int
}

func (s *cleanupUploadWireServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.t.Error("mutation attempted")
		http.Error(w, "mutation refused", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var data any
	switch {
	case r.URL.Path == "/api2/json/access/permissions":
		data = map[string]any{r.URL.Query().Get("path"): map[string]any{"Sys.Audit": 0}}
	case strings.HasSuffix(r.URL.Path, "/status"):
		s.statusCalls++
		if r.URL.Path != "/api2/json/nodes/lab-pve-cpi-0/tasks/"+cleanupUploadWireUPID+"/status" {
			s.t.Error("task status used destination instead of worker endpoint")
		}
		data = cleanupUploadWireStatus(s.mode)
	case strings.HasSuffix(r.URL.Path, "/tasks") && r.URL.Query().Get("source") == "all":
		data = s.history(w, r)
	case strings.HasSuffix(r.URL.Path, "/tasks"):
		if r.URL.Query().Get("source") != "active" {
			s.t.Error("unexpected task scan")
		}
		data = []any{}
	default:
		s.t.Errorf("unexpected request %s", r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		s.t.Error(err)
	}
}

func cleanupUploadWireStatus(mode string) map[string]any {
	row := map[string]any{"upid": cleanupUploadWireUPID, "node": "lab-pve-cpi-0", "status": "stopped", "exitstatus": "OK", "type": "imgcopy", "id": "", "starttime": 1789104195}
	switch mode {
	case "wrong status start":
		row["starttime"] = 1789104194
	case "wrong status type":
		row["type"] = "qmclone"
	case "wrong status id":
		row["id"] = "8073"
	}
	return row
}

func cleanupUploadWireHistoryRow(mode string) map[string]any {
	row := map[string]any{"upid": cleanupUploadWireUPID, "node": "lab-pve-cpi-0", "type": "imgcopy", "id": "", "status": "OK", "starttime": 1789104195, "endtime": 1789104196}
	switch mode {
	case "wrong node":
		row["node"] = "lab-pve-cpi-1"
	case "wrong type":
		row["type"] = "qmclone"
	case "wrong id":
		row["id"] = "8073"
	case "missing id":
		delete(row, "id")
	case "failed history":
		row["status"] = "ERROR"
	case "wrong start":
		row["starttime"] = 1789104194
	case "clock inversion":
		row["endtime"] = 1789104194
	case "missing end":
		delete(row, "endtime")
	case "future end":
		row["endtime"] = 9999999999
	}
	return row
}

func cleanupUploadWireHistoryRows(mode string, offset int) []any {
	row := cleanupUploadWireHistoryRow(mode)
	rows := []any{row}
	switch mode {
	case "missing history":
		return []any{}
	case "duplicate", "conflicting duplicate":
		other := maps.Clone(row)
		if mode == "conflicting duplicate" {
			other["endtime"] = 1789104197
		}
		return append(rows, other)
	}
	if (mode == "second page" || mode == "cross-page duplicate") && offset == 0 || mode == "page limit" {
		rows = make([]any, 100)
		for i := range rows {
			rows[i] = map[string]any{"upid": fmt.Sprintf("UPID:lab-pve-cpi-0:%08X:0421971E:6AA39043:imgcopy::pmx@pve!pmx:", offset+i+1)}
		}
	}
	if mode == "cross-page duplicate" && offset == 0 {
		rows[0] = row
	}
	return rows
}

func (s *cleanupUploadWireServer) history(w http.ResponseWriter, r *http.Request) any {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("start"))
	s.historyCalls++
	if r.URL.Path != "/api2/json/nodes/lab-pve-cpi-0/tasks" || len(q) != 6 || q.Get("since") != "1789104195" || q.Get("until") != "1789104195" || q.Get("typefilter") != "imgcopy" || q.Get("limit") != "100" || offset != (s.historyCalls-1)*100 {
		s.t.Error("history scan identity or bounded pagination differs")
	}
	switch s.mode {
	case "null history":
		return nil
	case "malformed history":
		return map[string]any{}
	case "history unavailable":
		w.WriteHeader(http.StatusForbidden)
		return nil
	default:
		return cleanupUploadWireHistoryRows(s.mode, offset)
	}
}

func (s *cleanupUploadWireServer) checkObservation(proof pve.StorageTaskSettlementEvidence, err error) {
	s.t.Helper()
	success := s.mode == "actual interval" || s.mode == "second page"
	if (err == nil) != success {
		s.t.Fatalf("proof=%+v error=%v", proof, err)
	}
	if success && (len(proof.Tasks) != 1 || proof.Tasks[0].StartTime != 1789104195 || proof.Tasks[0].EndTime != 1789104196) {
		s.t.Fatal("exact retained task interval lost")
	}
	if s.mode == "second page" && s.historyCalls != 2 {
		s.t.Fatal("history not completely paginated")
	}
	if s.mode == "page limit" && s.historyCalls != 128 {
		s.t.Fatal("history exceeded or skipped bounded scan")
	}
	if s.mode == "foreign node" && (s.statusCalls != 0 || s.historyCalls != 0) {
		s.t.Fatal("foreign task node was queried")
	}
}

func TestStorageCleanupUploadTaskIntervalWire(t *testing.T) {
	for _, mode := range []string{"actual interval", "second page", "cross-page duplicate", "missing history", "duplicate", "conflicting duplicate", "wrong node", "wrong type", "wrong id", "missing id", "failed history", "wrong start", "clock inversion", "missing end", "future end", "wrong status start", "wrong status type", "wrong status id", "null history", "malformed history", "history unavailable", "page limit", "foreign node"} {
		t.Run(mode, func(t *testing.T) {
			server := &cleanupUploadWireServer{t: t, mode: mode}
			mux := http.NewServeMux()
			mux.HandleFunc("/api2/json/", server.serveHTTP)
			client := newPoolStubClient(t, mux)
			nodes := []string{"lab-pve-cpi-0", "lab-pve-cpi-1"}
			if mode == "foreign node" {
				nodes = []string{"lab-pve-cpi-1"}
			}
			proof, err := pve.ObserveStorageCleanupTasks(t.Context(), client, nodes, []string{cleanupUploadWireUPID})
			server.checkObservation(proof, err)
		})
	}
}
