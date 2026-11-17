package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	rank "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageplacement"
)

// zeroNineStorageInfo is pve.StorageInfo as 0.9.7 declared it, with no JSON
// tags. The tests keep their own copy rather than reuse the production mirror,
// so a mistake in that mirror cannot hide behind itself.
type zeroNineStorageInfo struct {
	Name         string
	Type         string
	Shared       bool
	Disabled     bool
	Nodes        []string
	Path         string
	Server       string
	Export       string
	Content      string
	IsMountpoint bool
}

// tenZeroStorageInfo is pve.StorageInfo as 0.10.0 through 0.10.2 declared
// it, with the snake_case tags those releases wrote into plans. The tests keep
// their own frozen copy, because pve.StorageInfo's live tags belong to the HA
// profile cache and may change with it, while the fingerprints those releases
// stored never will.
type tenZeroStorageInfo struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Shared       bool     `json:"shared"`
	Disabled     bool     `json:"disabled"`
	Nodes        []string `json:"nodes"`
	Path         string   `json:"path"`
	Server       string   `json:"server"`
	Export       string   `json:"export"`
	Content      string   `json:"content"`
	IsMountpoint bool     `json:"is_mountpoint"`
}

// zeroNinePlan is StorageAllocationPlan as 0.9.7 declared it. Its nested
// types other than the definitions are unchanged since 0.9.7, so they are
// reused here.
type zeroNinePlan struct {
	VMExecution                                           *StorageVMExecution `json:",omitempty"`
	Version                                               int
	Namespace, AllocationKey, PolicyFingerprint, AZ, Node string
	Seed                                                  [32]byte
	HANodes                                               []string
	Targets                                               []StoragePlanTarget
	Charges                                               []inv.ChargeRecord
	Rankings                                              []rank.RankedCandidate
	Strategies                                            map[string]rank.Policy
	Definitions                                           map[string]zeroNineStorageInfo
	CapacityDomains                                       map[string][]string
	Rejections                                            []string
	Group                                                 string `json:",omitempty"`
}

var definitionFormPolicy = strings.Repeat("a", 64)

// definitionFormPlan is a plan written by hand the way 0.9.7 encodes one, with
// definitions spliced in. The definitions decide which release's key form the
// fixture carries.
func definitionFormPlan(definitions string) json.RawMessage {
	return json.RawMessage(`{"Version":1,"Namespace":"director","AllocationKey":"disk","PolicyFingerprint":"` + definitionFormPolicy + `",` +
		`"AZ":"","Node":"pve1","Seed":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],` +
		`"HANodes":null,"Targets":null,"Charges":null,"Rankings":null,"Strategies":null,` +
		`"Definitions":` + definitions + `,"CapacityDomains":null,"Rejections":null}`)
}

// The two definitions every fixture carries. "a" is the audit fixture's
// shared NFS export, and "m" is a node-local dir with the mount flag set.
const (
	zeroNineDefinitionA = `"a":{"Name":"a","Type":"nfs","Shared":true,"Disabled":false,"Nodes":null,"Path":"","Server":"nas","Export":"/a","Content":"images","IsMountpoint":false}`
	zeroNineDefinitionM = `"m":{"Name":"m","Type":"dir","Shared":false,"Disabled":false,"Nodes":["pve1"],"Path":"/mnt/m","Server":"","Export":"","Content":"images","IsMountpoint":true}`
	snakeDefinitionA    = `"a":{"name":"a","type":"nfs","shared":true,"disabled":false,"nodes":null,"path":"","server":"nas","export":"/a","content":"images","is_mountpoint":false}`
	snakeDefinitionM    = `"m":{"name":"m","type":"dir","shared":false,"disabled":false,"nodes":["pve1"],"path":"/mnt/m","server":"","export":"","content":"images","is_mountpoint":true}`
)

func definitionFormRecord(plan json.RawMessage) aj.Record {
	return aj.Record{ID: findingAllocationID, Namespace: "director", Kind: allocationKindDisk, State: aj.Observed,
		Intent: aj.Intent{PolicyFingerprint: definitionFormPolicy, PlanVersion: 1, Plan: plan}}
}

var definitionFormWant = map[string]pve.StorageInfo{
	"a": {Name: "a", Type: "nfs", Shared: true, Server: "nas", Export: "/a", Content: "images"},
	"m": {Name: "m", Type: "dir", Nodes: []string{"pve1"}, Path: "/mnt/m", Content: "images", IsMountpoint: true},
}

// TestStoragePlanDefinitionsReadEveryReleaseForm decodes hand-written plans
// through both plan decoders. The owner's decode is strict, so it must accept
// either key form, mixed across entries, and still refuse a key no release
// writes. The sibling decode reads the same forms and ignores unknown keys.
func TestStoragePlanDefinitionsReadEveryReleaseForm(t *testing.T) {
	for _, tc := range []struct {
		name        string
		definitions string
		want        map[string]pve.StorageInfo
		strictFails bool
		siblingFail bool
	}{
		{name: "0.9.x Go field names", definitions: "{" + zeroNineDefinitionA + "," + zeroNineDefinitionM + "}", want: definitionFormWant},
		{name: "0.10.0 to 0.10.2 snake_case", definitions: "{" + snakeDefinitionA + "," + snakeDefinitionM + "}", want: definitionFormWant},
		{name: "forms mixed across entries", definitions: "{" + snakeDefinitionA + "," + zeroNineDefinitionM + "}", want: definitionFormWant},
		{name: "Go names in another case", definitions: `{"m":{"name":"m","TYPE":"dir","nodes":["pve1"],"path":"/mnt/m","content":"images","ismountpoint":true},` + zeroNineDefinitionA + "}", want: definitionFormWant},
		{name: "null definitions", definitions: "null", want: nil},
		{name: "empty definitions", definitions: "{}", want: map[string]pve.StorageInfo{}},
		{name: "unknown key", definitions: `{"a":{"Name":"a","Type":"nfs","Shared":true,"Server":"nas","Export":"/a","Content":"images","Thinpool":"x"}}`,
			want: map[string]pve.StorageInfo{"a": definitionFormWant["a"]}, strictFails: true},
		// The strict decode refuses the shapes below, which no release writes
		// and a plain decode would settle silently. The sibling decode never
		// reads the flag, so it tolerates them rather than block a create.
		{name: "mount flag under both keys", definitions: `{"m":{"Name":"m","IsMountpoint":true,"is_mountpoint":false}}`,
			want: map[string]pve.StorageInfo{"m": {Name: "m", IsMountpoint: true}}, strictFails: true},
		{name: "null 0.9.x mount flag", definitions: `{"m":{"Name":"m","IsMountpoint":null}}`,
			want: map[string]pve.StorageInfo{"m": {Name: "m"}}, strictFails: true},
		{name: "null snake_case mount flag", definitions: `{"m":{"name":"m","is_mountpoint":null}}`,
			want: map[string]pve.StorageInfo{"m": {Name: "m"}}, strictFails: true},
		{name: "null mount flag beside the other key", definitions: `{"m":{"Name":"m","IsMountpoint":true,"is_mountpoint":null}}`,
			want: map[string]pve.StorageInfo{"m": {Name: "m", IsMountpoint: true}}, strictFails: true},
		{name: "mount flag twice in one form", definitions: `{"m":{"Name":"m","IsMountpoint":true,"ismountpoint":false}}`,
			want: map[string]pve.StorageInfo{"m": {Name: "m"}}, strictFails: true},
		{name: "snake_case mount flag twice", definitions: `{"m":{"Name":"m","is_mountpoint":true,"IS_MOUNTPOINT":false}}`,
			want: map[string]pve.StorageInfo{"m": {Name: "m"}}, strictFails: true},
		{name: "other field twice", definitions: `{"a":{"Name":"a","name":"b"}}`,
			want: map[string]pve.StorageInfo{"a": {Name: "b"}}, strictFails: true},
		{name: "null entry", definitions: `{"m":null}`, want: map[string]pve.StorageInfo{"m": {}}},
		{name: "wrong value type", definitions: `{"a":{"Name":"a","Shared":"yes"}}`, strictFails: true, siblingFail: true},
		{name: "entry not an object", definitions: `{"a":"nfs"}`, strictFails: true, siblingFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := definitionFormRecord(definitionFormPlan(tc.definitions))
			for _, decoder := range []struct {
				name   string
				decode func(aj.Record) (*StorageAllocationPlan, error)
				fails  bool
			}{
				{name: "owner", decode: activeStorageAllocationPlan, fails: tc.strictFails},
				{name: "sibling", decode: siblingStorageAllocationPlan, fails: tc.siblingFail},
			} {
				plan, err := decoder.decode(record)
				if decoder.fails {
					if err == nil {
						t.Fatalf("%s decode accepted %s", decoder.name, tc.definitions)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s decode refused %s: %v", decoder.name, tc.definitions, err)
				}
				if (plan.Definitions == nil) != (tc.want == nil) || !maps.EqualFunc(plan.Definitions, tc.want, storageInfoEqual) {
					t.Fatalf("%s decode read %+v, want %+v", decoder.name, plan.Definitions, tc.want)
				}
			}
		})
	}
}

func storageInfoEqual(a, b pve.StorageInfo) bool {
	return reflect.DeepEqual(a, b)
}

// TestStoragePlanDefinitionsWriteTheZeroNineForm encodes a real plan and
// decodes it strictly the way 0.9.7 does, into 0.9.7's own declaration. It
// also pins the definition bytes to what an untagged pve.StorageInfo encodes.
func TestStoragePlanDefinitionsWriteTheZeroNineForm(t *testing.T) {
	_, plan, intent := runtimePlanFixture(t)
	if len(plan.Definitions) == 0 {
		t.Fatal("fixture plan froze no definitions")
	}
	id := slices.Sorted(maps.Keys(plan.Definitions))[0]
	flagged := plan.Definitions[id]
	flagged.IsMountpoint = true
	flagged.Nodes = []string{"pve1"}
	plan.Definitions[id] = flagged

	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	var old zeroNinePlan
	if err := d.Decode(&old); err != nil {
		t.Fatalf("0.9.7 strict decode refused the plan: %v\n%s", err, payload)
	}
	if !old.Definitions[id].IsMountpoint || !slices.Equal(old.Definitions[id].Nodes, []string{"pve1"}) {
		t.Fatalf("0.9.7 decode lost the definition: %+v", old.Definitions[id])
	}

	var fields struct{ Definitions json.RawMessage }
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	untagged := map[string]zeroNineStorageInfo{}
	for key, info := range plan.Definitions {
		untagged[key] = zeroNineStorageInfo(info)
	}
	want, err := json.Marshal(untagged)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fields.Definitions, want) {
		t.Fatalf("definitions are not in the 0.9.x form:\ngot  %s\nwant %s", fields.Definitions, want)
	}
	if strings.Contains(string(payload), `"is_mountpoint"`) || strings.Contains(string(payload), `"name"`) {
		t.Fatalf("plan carries snake_case definition keys: %s", payload)
	}

	// The plan the journal intent stores decodes strictly in 0.9.7 too, and a
	// nil map stays null as it did when the field was a plain map.
	d = json.NewDecoder(bytes.NewReader(intent.Plan))
	d.DisallowUnknownFields()
	if err := d.Decode(new(zeroNinePlan)); err != nil {
		t.Fatalf("0.9.7 strict decode refused the journal intent plan: %v\n%s", err, intent.Plan)
	}
	if got := mustMarshal(t, StorageAllocationPlan{}); !bytes.Contains(got, []byte(`"Definitions":null`)) {
		t.Fatalf("nil definitions encode as %s", got)
	}
}

// TestStorageFrozenInputsFingerprintForms recomputes the frozen-inputs
// fingerprint independently. A new intent hashes the 0.9.x form, which is
// what 0.9.x stored, and the retry comparison accepts that form and the
// snake_case form 0.10.0 through 0.10.2 stored, but nothing else.
func TestStorageFrozenInputsFingerprintForms(t *testing.T) {
	req, plan, intent := runtimePlanFixture(t)
	frozen, err := storageJournalFrozenInputs(req.Selection, req.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen.Definitions) == 0 {
		t.Fatal("fixture froze no definitions")
	}
	hash := func(definitions any) string {
		raw := mustMarshal(t, struct {
			Nodes             []string
			Memberships       map[string][]string
			Definitions       any
			PolicyFingerprint string
		}{frozen.Nodes, frozen.Memberships, definitions, frozen.PolicyFingerprint})
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	untagged := map[string]zeroNineStorageInfo{}
	tagged := map[string]tenZeroStorageInfo{}
	for id := range frozen.Definitions {
		untagged[id] = zeroNineStorageInfo(frozen.Definitions[id])
		tagged[id] = tenZeroStorageInfo(frozen.Definitions[id])
	}
	zeroNine := hash(untagged)
	snake := hash(tagged)
	if zeroNine == snake {
		t.Fatal("the two forms hash alike, so the test proves nothing")
	}
	if intent.FrozenInputsFingerprint != zeroNine {
		t.Fatalf("intent fingerprint %s is not the 0.9.x form %s", intent.FrozenInputsFingerprint, zeroNine)
	}
	for _, tc := range []struct {
		name   string
		stored string
		want   bool
	}{
		{name: "0.9.x form", stored: zeroNine, want: true},
		{name: "0.10.0 to 0.10.2 form", stored: snake, want: true},
		{name: "different inputs", stored: strings.Repeat("f", 64), want: false},
		{name: "empty", stored: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := storageJournalFrozenInputsMatch(tc.stored, req.Selection, req.Inventory, plan)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("match(%s) = %t, want %t", tc.stored, got, tc.want)
			}
		})
	}
}

// TestAllocationAuditReadsEveryPlanDefinitionForm runs the audit over a
// journal record whose plan an earlier release wrote. Before the plan carried
// its own definition form, a 0.9.x plan failed the strict decode, and every
// such record raised "could not be decoded" and left the audit incomplete.
func TestAllocationAuditReadsEveryPlanDefinitionForm(t *testing.T) {
	volume := "a:123/vm-123-disk-0.qcow2"
	sharedBacking := definitionFormWant["a"].BackingKey()
	for _, tc := range []struct {
		name        string
		definitions string
		undecodable bool
	}{
		{name: "0.9.x Go field names", definitions: "{" + zeroNineDefinitionA + "}"},
		{name: "0.10.0 to 0.10.2 snake_case", definitions: "{" + snakeDefinitionA + "}"},
		{name: "unknown key", definitions: `{"a":{"Name":"a","Type":"nfs","Shared":true,"Server":"nas","Export":"/a","Content":"images","Thinpool":"x"}}`, undecodable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, _ := auditFixture(t)
			record := findingDisk(sharedBacking, volume)
			record.Intent = aj.Intent{PolicyFingerprint: definitionFormPolicy, PlanVersion: 1, Plan: definitionFormPlan(tc.definitions)}
			report, err := auditStorageAllocationRecords(context.Background(), deps, []aj.Record{record}, nil, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			undecodable := slices.ContainsFunc(report.Issues, func(issue string) bool { return strings.Contains(issue, "could not be decoded") })
			if tc.undecodable {
				if !undecodable || report.Complete {
					t.Fatalf("audit accepted a plan with an unknown definition key:\n%s", strings.Join(report.Issues, "\n"))
				}
				return
			}
			if undecodable || !report.Complete || len(report.Issues) != 0 || len(report.Conflicts) != 0 {
				t.Fatalf("audit of a %s plan is not clean (complete=%t):\nissues: %s\nconflicts: %s", tc.name, report.Complete,
					strings.Join(report.Issues, "\n"), strings.Join(report.Conflicts, "\n"))
			}
		})
	}
}

// TestAllocationAuditReadsZeroNineVMRecordFromJournal writes a VM record whose
// plan carries 0.9.x definitions through a real on-disk journal, lists it
// back, and audits the journal. The plan is encoded from 0.9.7's own
// declaration, so its bytes are the ones a 0.9.x director left on disk.
func TestAllocationAuditReadsZeroNineVMRecordFromJournal(t *testing.T) {
	deps, j, c := auditFixture(t)
	c.nodesRead.nodeNames = []string{"pve1"}
	def, err := pve.ParseStorageEntry(c.storageRead.definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	plan := zeroNinePlan{Version: 1, Namespace: "director", AllocationKey: "agent", PolicyFingerprint: definitionFormPolicy, Node: "pve1",
		Definitions: map[string]zeroNineStorageInfo{"a": zeroNineStorageInfo(def)},
		Targets:     []StoragePlanTarget{{Role: "root", Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30}}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(`"IsMountpoint":false`)) {
		t.Fatalf("fixture plan is not in the 0.9.x form: %s", payload)
	}
	args := []json.RawMessage{json.RawMessage(`"agent"`)}
	fingerprint, err := storageCallerIntentFingerprint("create_vm", args)
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: fingerprint, PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	records, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Kind != "vm" {
		t.Fatalf("journal lists %d records, want the one VM record", len(records))
	}
	decoded, err := activeStorageAllocationPlan(records[0])
	if err != nil {
		t.Fatalf("owner decode refused the 0.9.x plan read back from disk: %v", err)
	}
	if !storageInfoEqual(decoded.Definitions["a"], def) {
		t.Fatalf("decoded definition %+v, want %+v", decoded.Definitions["a"], def)
	}
	report, err := AuditStorageAllocations(t.Context(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || len(report.Issues) != 0 || len(report.Conflicts) != 0 {
		t.Fatalf("audit of a 0.9.x VM record is not clean (complete=%t):\nissues: %s\nconflicts: %s", report.Complete,
			strings.Join(report.Issues, "\n"), strings.Join(report.Conflicts, "\n"))
	}
}
