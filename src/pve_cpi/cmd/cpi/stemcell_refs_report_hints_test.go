package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

const hintsCacheDesc = `{"name":"a","version":"1","sha8":"aaaa1111","kind":"heavy","director_refs":["old-uuid"]}`

// hintsCLIClient has one cache template holding old-uuid, and extra guests.
func hintsCLIClient(guests ...refsCLIGuest) *refsCLIClient {
	all := make([]refsCLIGuest, 0, 1+len(guests))
	all = append(all, refsCLIGuest{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
		tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--old-uuid", desc: hintsCacheDesc})
	return &refsCLIClient{guests: append(all, guests...)}
}

func refsLine(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "old-uuid") {
			return line
		}
	}
	t.Fatalf("no old-uuid row in:\n%s", stdout)
	return ""
}

func TestStemcellRefsReportTextPairsRefWithCreateEnvDirectorVM(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := hintsCLIClient(refsCLIGuest{vmid: 4441, name: "bosh-0",
		tags: "bosh-cpi;bosh-owner--old-uuid;director--bosh-init;deployment--lab-bosh"})
	code, stdout, stderr := runStemcellRefsTest(t, client, "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	want := "owned by create-env Director VM 4441 (bosh-init, deployment lab-bosh)"
	if line := refsLine(t, stdout); !strings.Contains(line, want) {
		t.Errorf("refs row %q lacks %q", line, want)
	}
	if strings.Contains(stdout, "may be the director_id") {
		t.Errorf("a paired ref must not print the create-env note:\n%s", stdout)
	}
}

func TestStemcellRefsReportTextPairsEveryCreateEnvVM(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := hintsCLIClient(
		refsCLIGuest{vmid: 4441, name: "bosh-0", tags: "bosh-cpi;bosh-owner--old-uuid;director--bosh-init"},
		refsCLIGuest{vmid: 4450, name: "bosh-1",
			tags: "bosh-cpi;bosh-owner--old-uuid;director--bosh-init;deployment--a;deployment--b;deployment--c"})
	code, stdout, stderr := runStemcellRefsTest(t, client, "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	want := "owned by create-env Director VMs 4441 (bosh-init), 4450 (bosh-init, deployments a, b, and c)"
	if line := refsLine(t, stdout); !strings.Contains(line, want) {
		t.Errorf("refs row %q lacks %q", line, want)
	}
}

func TestStemcellRefsReportTextNoPairingForAnotherDirector(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := hintsCLIClient(refsCLIGuest{vmid: 300, name: "router",
		tags: "bosh-cpi;bosh-owner--old-uuid;director--prod-bosh;deployment--cf"})
	code, stdout, stderr := runStemcellRefsTest(t, client, "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if strings.Contains(stdout, "create-env") {
		t.Errorf("a deployed Director's ref must not mention create-env:\n%s", stdout)
	}
}

func TestStemcellRefsReportTextNotesAPossibleCreateEnvRefOnce(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	desc := `{"name":"b","version":"1","sha8":"bbbb2222","kind":"heavy","director_refs":["old-uuid","other-uuid"]}`
	client := hintsCLIClient(refsCLIGuest{vmid: 102, name: "bosh-stemcell-b-1-bbbb2222", template: true,
		tags: "bosh-stemcell-cache;bosh-stemcell-sha-bbbb2222;director--old-uuid;director--other-uuid", desc: desc})
	code, stdout, stderr := runStemcellRefsTest(t, client, "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if n := strings.Count(stdout, "may be the director_id"); n != 1 {
		t.Fatalf("note printed %d times, want once for two refs:\n%s", n, stdout)
	}
	for _, want := range []string{
		"This applies to old-uuid and other-uuid.", "state file", "`bosh env` does not print",
		"adopt-owners --director-name bosh-init", `"Stamping owner tags on older VMs"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "owned by create-env") {
		t.Errorf("no pairing expected:\n%s", stdout)
	}
}

func TestStemcellRefsReportTextNoCreateEnvNoteWhileOwnershipUnknown(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := hintsCLIClient(refsCLIGuest{vmid: 300, name: "pre-094", tags: "bosh-cpi;director--bosh;deployment--cf"})
	code, stdout, stderr := runStemcellRefsTest(t, client, "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "OWNERSHIP UNKNOWN") {
		t.Fatalf("fixture should leave ownership unknown:\n%s", stdout)
	}
	if strings.Contains(stdout, "may be the director_id") {
		t.Errorf("create-env note printed while OWNERSHIP UNKNOWN covers the case:\n%s", stdout)
	}
}

func TestStemcellRefsReportJSONCarriesTheHints(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := hintsCLIClient(refsCLIGuest{vmid: 4441, name: "bosh-0",
		tags: "bosh-cpi;bosh-owner--old-uuid;director--bosh-init;deployment--lab-bosh"})
	code, stdout, stderr := runStemcellRefsTest(t, client, "report", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	var rep handlers.StemcellRefsReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, stdout)
	}
	if len(rep.Refs) != 1 || len(rep.Refs[0].CreateEnvDirectors) != 1 || rep.Refs[0].CreateEnvDirectors[0].VMID != 4441 ||
		rep.Refs[0].CreateEnvDirectors[0].Deployments[0] != "lab-bosh" || rep.PossibleCreateEnvRefNote != "" {
		t.Fatalf("paired JSON = %+v", rep)
	}

	code, stdout, stderr = runStemcellRefsTest(t, hintsCLIClient(), "report", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	rep = handlers.StemcellRefsReport{}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, stdout)
	}
	if len(rep.Refs) != 1 || !rep.Refs[0].PossibleCreateEnvRef || rep.PossibleCreateEnvRefNote == "" {
		t.Fatalf("unmatched JSON = %+v", rep)
	}
}
