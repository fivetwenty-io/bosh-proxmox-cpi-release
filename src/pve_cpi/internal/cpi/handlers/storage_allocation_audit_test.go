package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	cs "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

type allocationAuditClient struct {
	*idFakeClient
	storageRead *allocationAuditStorage
	nodesRead   *allocationAuditNodes
}

func (c *allocationAuditClient) ClusterStorage() cs.Service { return c.storageRead }
func (c *allocationAuditClient) Nodes() ns.Service          { return c.nodesRead }

type allocationAuditStorage struct {
	cs.Service
	definitions cs.ListStorageResponse
}

func (s *allocationAuditStorage) ListStorage(context.Context, *cs.ListStorageParams) (*cs.ListStorageResponse, error) {
	return &s.definitions, nil
}

type allocationAuditNodes struct {
	ns.Service
	content ns.ListStorageContentResponse
	failure error
	mu      sync.Mutex
	listed  []string
}

// ListStorageContent records every listing it serves; the audit fans out
// across worker goroutines, so the record is guarded.
func (n *allocationAuditNodes) ListStorageContent(_ context.Context, node, storage string, _ *ns.ListStorageContentParams) (*ns.ListStorageContentResponse, error) {
	n.mu.Lock()
	n.listed = append(n.listed, node+"/"+storage)
	n.mu.Unlock()
	return &n.content, n.failure
}

func (n *allocationAuditNodes) listedStorages() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.listed)
}
func auditFixture(t *testing.T) (Deps, *aj.Journal, *allocationAuditClient) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	base := newIDFakeClient(map[int]map[string]any{})
	c := &allocationAuditClient{idFakeClient: base, storageRead: &allocationAuditStorage{definitions: cs.ListStorageResponse{json.RawMessage(`{"storage":"a","type":"nfs","server":"nas","export":"/a","content":"images","shared":1}`)}}, nodesRead: &allocationAuditNodes{Service: base.Nodes(), content: ns.ListStorageContentResponse{}}}
	identity, err := pve.ObserveStorageClusterIdentity(t.Context(), c.Nodes(), []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := aj.Initialize(context.Background(), directory, "director", aj.Enrollment{ClusterID: identity.ID(), AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})

	return Deps{Config: &config.CPIConfig{StoragePlacementNamespace: "director", StorageAllocationJournalDir: directory, Node: "pve1"}, PVE: c}, j, c
}
func TestAllocationAuditDetectsUnindexedVMWithoutWrites(t *testing.T) {
	deps, j, c := auditFixture(t)
	id := "12345678-1234-4234-8234-123456789abc"
	sum := sha256.Sum256([]byte("agent"))
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: "director", AllocationID: id, AgentSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	c.configs[123] = map[string]any{"description": marker}
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.VMScanComplete || report.Complete || len(report.Conflicts) != 1 || len(report.Evidence) != 1 {
		t.Fatalf("orphan not exposed: %+v", report)
	}
	if err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent"); err == nil {
		t.Fatal("unindexed VM allowed new generation")
	}
	records, err := j.List()
	if err != nil || len(records) != 0 {
		t.Fatal("audit wrote recovery records")
	}
	if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
		t.Fatal("audit mutated PVE")
	}
}
func TestAllocationAuditPartialStorageNeverCertifiesAbsence(t *testing.T) {
	deps, j, c := auditFixture(t)
	c.nodesRead.failure = errors.New("transport response secret-password")
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || !report.VMScanComplete || len(report.Issues) == 0 {
		t.Fatalf("partial storage hidden: %+v", report)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "secret-password") {
		t.Fatal("transport response leaked")
	}
	// An unrelated independent disk UUID needs no historical absence proof.
	if err := admitStorageAllocation(context.Background(), deps, j, []string{"pve1"}); err != nil {
		t.Fatal(err)
	}
}
func TestAllocationAuditDetectsUnindexedFreeVolume(t *testing.T) {
	deps, j, c := auditFixture(t)
	id := "12345678-1234-4234-8234-123456789abc"
	name, err := pve.AllocationVolumeName(9001, "director", id, "qcow2")
	if err != nil {
		t.Fatal(err)
	}
	c.nodesRead.content = ns.ListStorageContentResponse{planJSON(t, map[string]any{"volid": "a:9001/" + name})}
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conflicts) != 1 || report.Complete {
		t.Fatal("lost disk journal not exposed")
	}
	if err := admitStorageAllocation(context.Background(), deps, j, []string{"pve1"}); err == nil {
		t.Fatal("detected stale history admitted")
	}
}

func TestAllocationVerificationRetainsFactsWithoutGrantingAuthority(t *testing.T) {
	now := time.Now()
	report := StorageAllocationAudit{Complete: true, VMScanComplete: true, StartedAt: now, CompletedAt: now.Add(time.Millisecond), Records: []aj.Record{{Reason: "recursive-history-must-not-be-embedded"}}}
	proof, err := storageAllocationVerification(report, map[string]any{"operation": "readback", "owned_volumes": []string{"a:123/disk.qcow2"}})
	if err != nil {
		t.Fatal(err)
	}
	if !proof.Complete || proof.OwnershipVerified || proof.AbsenceVerified || proof.ArtifactDispositionVerified {
		t.Fatal("inventory automatically granted resource authority")
	}
	sum := sha256.Sum256([]byte(proof.EvidenceJSON))
	if proof.EvidenceID != hex.EncodeToString(sum[:]) || !strings.Contains(proof.EvidenceJSON, "a:123/disk.qcow2") || strings.Contains(proof.EvidenceJSON, "recursive-history") {
		t.Fatal("durable evidence lost facts or recursively copied history")
	}
	if _, err := storageAllocationVerification(report, map[string]any{"request_body": "secret"}); err == nil {
		t.Fatal("unapproved evidence field accepted")
	}
	report.Complete = false
	if _, err := storageAllocationVerification(report, nil); err == nil {
		t.Fatal("partial audit certified")
	}
}
func TestAllocationAuditPreservesLegacyAttachedCIDMap(t *testing.T) {
	deps, j, c := auditFixture(t)
	description, err := pve.RenderSentinel("legacy VM", map[string]json.RawMessage{"bosh_attached_disks": json.RawMessage(`{"scsi1":"bpd-0123456789abcdef"}`)})
	if err != nil {
		t.Fatal(err)
	}
	c.configs[123] = map[string]any{"description": description}
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || len(report.Issues) != 0 || len(report.Conflicts) != 0 {
		t.Fatalf("legacy CID map misinterpreted: %+v", report)
	}
}

func (c *allocationAuditClient) StorageAuditVisibility(context.Context) error { return nil }

type deniedAuditClient struct{ *allocationAuditClient }

func (*deniedAuditClient) StorageAuditVisibility(context.Context) error {
	return errors.New("token visibility denied: secret")
}
func TestAllocationAuditFilteredListingCannotProveAbsence(t *testing.T) {
	deps, j, c := auditFixture(t)
	deps.PVE = &deniedAuditClient{c}
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || report.VMScanComplete {
		t.Fatal("successful empty filtered listing certified global absence")
	}
	if _, err := storageAllocationVerification(report, nil); err == nil {
		t.Fatal("restricted visibility granted durable absence proof")
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("visibility API leaked transport error")
	}
	if err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent"); err == nil {
		t.Fatal("filtered VM listing admitted duplicate generation")
	}
}

func TestAllocationAuditRejectsRemoteProvenanceAfterTombstone(t *testing.T) {
	deps, j, c, record, _ := resumeVMFixture(t)
	h, err := j.Acquire(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	record = h.Record()
	record.State = aj.Deleted
	record.Verifications = append(record.Verifications, aj.Verification{EvidenceID: "external-audit", Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true})
	if err = h.Save(record); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || len(report.Conflicts) == 0 || !strings.Contains(strings.Join(report.Conflicts, " "), "terminal allocation") {
		t.Fatalf("resurrected resource trusted: %+v", report)
	}
	if len(c.destroyed) != 0 {
		t.Fatal("audit deleted resurrected resource")
	}
}

func TestAllocationAuditRejectsAmbiguousDefinitionsAndSentinels(t *testing.T) {
	for _, scenario := range []string{"definitions", "duplicate sentinel", "duplicate field", "malformed sentinel"} {
		t.Run(scenario, func(t *testing.T) {
			deps, j, c := auditFixture(t)
			switch scenario {
			case "definitions":
				c.storageRead.definitions = append(c.storageRead.definitions, json.RawMessage(`{"storage":"a","type":"nfs","server":"other","export":"/a","content":"images","shared":1}`))
			case "duplicate sentinel":
				c.configs[123] = map[string]any{"description": "<!--BOSH:{}--> <!--BOSH:{}-->"}
			case "duplicate field":
				c.configs[123] = map[string]any{"description": `<!--BOSH:{"bosh_parked_disks":{},"bosh_parked_disks":{}}-->`}
			case "malformed sentinel":
				c.configs[123] = map[string]any{"description": "<!--BOSH:{not-json-->"}
			}
			report, err := AuditStorageAllocations(t.Context(), deps, j, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			if report.Complete || len(report.Issues) == 0 {
				t.Fatalf("ambiguous observations certified absence: %+v", report)
			}
			if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
				t.Fatal("audit mutated PVE")
			}
		})
	}
}
func TestAllocationAuditVolumeMatchingRequiresPhysicalScope(t *testing.T) {
	local := pve.StorageInfo{Name: "a", Type: "dir", Path: "/local", Content: "images"}
	shared := pve.StorageInfo{Name: "a", Type: "nfs", Server: "nas", Export: "/a", Content: "images", Shared: true}
	volume := "a:123/vm-123-disk-0.raw"
	for _, tc := range []struct {
		name, node, backing, storage string
		definition                   pve.StorageInfo
		want                         bool
	}{
		{"local same node", "n1", local.BackingKey(), "a", local, true},
		{"local other node", "n2", local.BackingKey(), "a", local, false},
		{"shared other node", "n2", shared.BackingKey(), "a", shared, true},
		{"changed backing", "n1", "nfs://other/a", "a", shared, false},
		{"different logical storage", "n1", shared.BackingKey(), "b", shared, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := aj.Step{Target: aj.Target{Node: "n1", Storage: tc.storage, Backing: tc.backing}, VolIDs: []string{volume}}
			if got := storageAuditVolumeTargetMatches(aj.Record{}, step, map[string]pve.StorageInfo{"a": tc.definition}, tc.node, volume); got != tc.want {
				t.Fatalf("match=%t expected=%t", got, tc.want)
			}
		})
	}
}

func (n *allocationAuditNodes) ListCertificatesInfo(context.Context, string) (*ns.ListCertificatesInfoResponse, error) {
	raw, err := json.Marshal(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.TrimSuffix(strings.Repeat("ab:", 32), ":")})
	if err != nil {
		return nil, err
	}
	response := ns.ListCertificatesInfoResponse{raw}
	return &response, nil
}

func TestAllocationAuditRetainedVMAllowsOnlyDisposedSurvivors(t *testing.T) {
	deps, j, c, record, _ := resumeVMFixture(t)
	h, err := j.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained := record.Steps[1].Target
	retained.IntendedVolume = record.Steps[1].VolIDs[0]
	id, payload, err := aj.VerificationEvidence(aj.VMRetentionEvidence{VMID: 123, RetainedArtifacts: []aj.Target{retained}})
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	r.State = aj.VMDeletedRetained
	r.Verifications = append(r.Verifications, aj.Verification{EvidenceID: id, EvidenceJSON: payload, Complete: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true})
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	delete(c.configs, 123)
	c.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"a:123/vm-123-disk-1.qcow2","size":1073741824,"content":"images"}`)}
	report, err := AuditStorageAllocations(t.Context(), deps, j, []string{"pve1"})
	if err != nil || !report.Complete || len(report.Conflicts) > 0 {
		t.Fatalf("retained audit: %+v %v", report, err)
	}
	c.nodesRead.content = append(c.nodesRead.content, json.RawMessage(`{"volid":"a:123/vm-123-disk-0.qcow2","size":1073741824,"content":"images"}`))
	report, err = AuditStorageAllocations(t.Context(), deps, j, []string{"pve1"})
	if err != nil || report.Complete || len(report.Conflicts) == 0 {
		t.Fatalf("reappeared disposed root accepted: %+v %v", report, err)
	}
}

func TestAllocationAuditExternalTargetsNeverEstablishOwnership(t *testing.T) {
	def := pve.StorageInfo{Name: "a", Type: "nfs", Server: "nas", Export: "/a", Shared: true}
	target := aj.Target{External: true, Node: "pve1", VMID: 123, Storage: "a", Backing: def.BackingKey(), IntendedVolume: "a:123/vm-123-disk-0.raw"}
	step := aj.Step{Target: target, VolIDs: []string{target.IntendedVolume}}
	if storageAuditVMTargetMatches(aj.Record{}, step, "pve1", 123) || storageAuditVolumeTargetMatches(aj.Record{}, step, map[string]pve.StorageInfo{"a": def}, "pve1", target.IntendedVolume) {
		t.Fatal("external preservation target authorized allocation ownership")
	}
}

func TestAllocationAuditPreservationTargetsDoNotRequireVMMarker(t *testing.T) {
	for _, entry := range []aj.Step{
		{Kind: "legacy-preservation", Target: aj.Target{External: true, Node: "pve1", VMID: 999}},
		{Kind: "lifecycle_delete_vm_retain_ephemeral_QEMU.Create", Target: aj.Target{Node: "pve1", VMID: 999}},
	} {
		report := StorageAllocationAudit{Complete: true, VMScanComplete: true}
		record := aj.Record{ID: "owned-vm", Namespace: "director", Kind: "vm", State: aj.Observed, Steps: []aj.Step{entry}}
		collectAuditVMProvenance(&report, []aj.Record{record}, "director", "pve1", 999, "")
		if len(report.Conflicts) > 0 || !report.Complete {
			t.Fatalf("preservation target became owned VM: %+v", report)
		}
	}
}

func TestAllocationAuditSkipsAndDisclosesDisabledStorage(t *testing.T) {
	deps, j, c := auditFixture(t)
	c.storageRead.definitions = append(c.storageRead.definitions,
		json.RawMessage(`{"storage":"local-lvm","type":"lvmthin","vgname":"pve","thinpool":"data","content":"images,rootdir","disable":1}`),
		json.RawMessage(`{"storage":"local","type":"dir","path":"/var/lib/vz","content":"iso,vztmpl,backup","disable":1}`))
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || !report.VMScanComplete || len(report.Issues) != 0 {
		t.Fatalf("disabled storage counted as an inspection failure: %+v", report)
	}
	if strings.Join(report.SkippedDisabledStorages, ",") != "local,local-lvm" {
		t.Fatalf("disabled storages not disclosed: %+v", report.SkippedDisabledStorages)
	}
	listed := c.nodesRead.listedStorages()
	for _, entry := range listed {
		if strings.HasSuffix(entry, "/local") || strings.HasSuffix(entry, "/local-lvm") {
			t.Fatalf("disabled storage was listed: %v", listed)
		}
	}
	if !slices.Contains(listed, "pve1/a") {
		t.Fatalf("enabled storage was not listed: %v", listed)
	}
}

func TestAllocationAuditDisabledHistoricalStorageStaysAudited(t *testing.T) {
	deps, _, c := auditFixture(t)
	c.storageRead.definitions = append(c.storageRead.definitions,
		json.RawMessage(`{"storage":"local-lvm","type":"lvmthin","vgname":"pve","thinpool":"data","content":"images","disable":1}`))
	c.nodesRead.failure = errors.New("storage 'local-lvm' is disabled")
	records := []aj.Record{{ID: "old-disk", Namespace: "director", Kind: "disk", State: aj.Observed, Steps: []aj.Step{{ID: "owned", State: aj.Observed, Target: aj.Target{Node: "pve1", Storage: "local-lvm"}, VolIDs: []string{"local-lvm:vm-100-disk-0"}}}}}
	report, err := auditStorageAllocationRecords(context.Background(), deps, records, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || len(report.SkippedDisabledStorages) != 0 {
		t.Fatalf("historical disabled storage was skipped: %+v", report)
	}
}
