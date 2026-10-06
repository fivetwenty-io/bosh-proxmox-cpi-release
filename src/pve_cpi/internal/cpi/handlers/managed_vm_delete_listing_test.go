// managed_vm_delete_listing_test.go covers a storage content listing that
// fails while delete_vm disposes of a journal-managed VM. The presence read
// tries a transient failure again a bounded number of times, and a disposal
// that a failed listing alone stopped leaves the VM's record observed, so a
// rerun of the delete finishes the work.
package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	ce "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// listingSecret stands in for backend text, such as an endpoint URL or a
// response body, that must never reach an error or a log line.
const listingSecret = "https://token:secret@pve.invalid/api2/json"

// listingFault fails the content listings of one storage while active reports
// true. failures counts how many more listings fail, and a negative count
// fails every one. reads counts the listings of the storage while active. A
// fault with when set applies only to a listing whose context when accepts.
type listingFault struct {
	mu       sync.Mutex
	storage  string
	active   func() bool
	when     func(context.Context) bool
	failures int
	err      error
	reads    int
}

func (f *listingFault) check(ctx context.Context, storage string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if storage != f.storage || (f.active != nil && !f.active()) || (f.when != nil && !f.when(ctx)) {
		return nil
	}
	f.reads++
	if f.failures == 0 {
		return nil
	}
	if f.failures > 0 {
		f.failures--
	}
	return f.err
}

// clear lets every later listing through and resets the read count.
func (f *listingFault) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = 0
	f.reads = 0
}

func (f *listingFault) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// listingFaultNodes passes every node call to the wrapped service, except a
// listing the fault fails.
type listingFaultNodes struct {
	nodes.Service
	fault *listingFault
}

func (n listingFaultNodes) ListStorageContent(ctx context.Context, node, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	if err := n.fault.check(ctx, storage); err != nil {
		return nil, err
	}
	return n.Service.ListStorageContent(ctx, node, storage, params)
}

type listingFaultDeleteClient struct {
	*deleteManagedClient
	fault *listingFault
}

func (c listingFaultDeleteClient) Nodes() nodes.Service {
	return listingFaultNodes{Service: c.deleteManagedClient.Nodes(), fault: c.fault}
}

type listingFaultFlowPVE struct {
	*lifecycleFlowPVE
	fault *listingFault
}

func (c listingFaultFlowPVE) Nodes() nodes.Service {
	return listingFaultNodes{Service: c.lifecycleFlowPVE.Nodes(), fault: c.fault}
}

// scriptedListingNodes answers the nth listing with errs[n] when that entry
// is set, and with listing otherwise.
type scriptedListingNodes struct {
	nodes.Service
	errs    []error
	listing nodes.ListStorageContentResponse
	reads   int
	onRead  func()
}

func (n *scriptedListingNodes) ListStorageContent(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	read := n.reads
	n.reads++
	if n.onRead != nil {
		n.onRead()
	}
	if read < len(n.errs) && n.errs[read] != nil {
		return nil, n.errs[read]
	}
	return &n.listing, nil
}

// requireListingStoppedDelete checks that err is the CloudError a disposal
// returns when a failed listing alone stopped it.
func requireListingStoppedDelete(t *testing.T, err error, storage, node, reason string) {
	t.Helper()
	var typed *ce.Error
	if !errors.As(err, &typed) {
		t.Fatalf("delete error %v carries no CPI error", err)
	}
	if typed.Type() != ce.TypeCloud || typed.OkToRetry() {
		t.Fatalf("delete error type %s ok_to_retry %v, want a CloudError the Director does not retry", typed.Type(), typed.OkToRetry())
	}
	message := typed.Error()
	for _, want := range []string{"storage " + storage, "node " + node, reason, "stopped before its next change, with every step it took recorded", "rerun the delete once that storage lists"} {
		if !strings.Contains(message, want) {
			t.Fatalf("delete error %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "requires reconciliation") || strings.Contains(message, "secret") {
		t.Fatalf("delete error %q names reconciliation or backend text", message)
	}
}

// requireListingStopReason checks that reason, the reason a listing stop
// saved on the VM's record, names the storage, the node, and the listing's
// reason, says what resumes the record, carries no backend text, and fits in
// the 200 bytes the journal audit's summary prints of a reason.
func requireListingStopReason(t *testing.T, reason, storage, node, listing string) {
	t.Helper()
	for _, want := range []string{"VM cleanup stopped because storage " + storage + " on node " + node + " could not be listed (" + listing + ")", "rerun delete_vm or storage-journal cleanup once that storage lists"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the record's reason %q does not contain %q", reason, want)
		}
	}
	if strings.Contains(reason, listingSecret) {
		t.Fatalf("the record's reason %q carries backend text", reason)
	}
	if len(reason) > 200 {
		t.Fatalf("the record's reason %q is %d bytes, and the audit summary would cut it", reason, len(reason))
	}
}

// TestManagedVMDeleteListingFailureLeavesRecordObservedAndRerunCompletes
// fails every content listing of the VM's storage once the VM is stopped, so
// the check of the VM's volumes before its destroy can't read the storage.
// delete_vm stops before the destroy, names the storage, the node, and the
// reason, and leaves the record observed with a reason that names them too,
// which the journal audit's summary prints. Once the storage lists again, a
// rerun destroys the VM, clears the reason, and closes the record without
// stopping it again.
func TestManagedVMDeleteListingFailureLeavesRecordObservedAndRerunCompletes(t *testing.T) {
	deps, journal, c, record := deleteManagedFixture(t)
	fault := &listingFault{storage: "a", active: func() bool { return c.stopped }, failures: -1, err: &sdkerrors.APIError{HTTPCode: 500, Message: listingSecret}}
	deps.PVE = listingFaultDeleteClient{deleteManagedClient: c, fault: fault}

	handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
	if !handled || err == nil {
		t.Fatalf("delete with a failing listing = %v, %v, want a handled failure", handled, err)
	}
	requireListingStoppedDelete(t, err, "a", "pve1", "listing_http_500")
	after, err := journal.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != aj.Observed {
		t.Fatalf("record after the failed listing is %s (%q), want observed", after.State, after.Reason)
	}
	requireListingStopReason(t, after.Reason, "a", "pve1", "listing_http_500")
	if c.stopCount != 1 || c.destroyCount != 0 || len(c.destroyed) != 0 {
		t.Fatalf("stop %d destroy %d deleted volumes %v, want one stop and nothing destroyed", c.stopCount, c.destroyCount, c.destroyed)
	}
	if reads := fault.count(); reads != 3 {
		t.Fatalf("the failing storage was listed %d times, want 3", reads)
	}

	fault.clear()
	handled, err = deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
	if !handled || err != nil {
		t.Fatalf("rerun after the storage lists again = %v, %v", handled, err)
	}
	after, err = journal.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != aj.Deleted || c.stopCount != 1 || c.destroyCount != 1 {
		t.Fatalf("rerun left state %s stop %d destroy %d, want deleted with one stop and one destroy", after.State, c.stopCount, c.destroyCount)
	}
	if strings.Contains(after.Reason, "could not be listed") {
		t.Fatalf("rerun kept the listing stop's reason %q on the closed record", after.Reason)
	}
}

// TestManagedVolumePresentRetriesTransientListing pins which listing failures
// the presence read tries again, and that it stops after three reads.
func TestManagedVolumePresentRetriesTransientListing(t *testing.T) {
	const volume = "nfs:20000/disk.qcow2"
	present := nodes.ListStorageContentResponse{[]byte(`{"volid":"` + volume + `"}`)}
	server := &sdkerrors.APIError{HTTPCode: 500, Message: listingSecret}
	for _, tc := range []struct {
		name        string
		errs        []error
		wantPresent bool
		wantReads   int
		wantReason  string
	}{
		{name: "one server error then an answer", errs: []error{server}, wantPresent: true, wantReads: 2},
		{name: "a proxy error and a dropped connection then an answer", errs: []error{&sdkerrors.APIError{HTTPCode: 596}, &sdkerrors.ConnectionError{Host: listingSecret}}, wantPresent: true, wantReads: 3},
		{name: "a server error on every read", errs: []error{server, server, server, server}, wantReads: 3, wantReason: "listing_http_500"},
		{name: "a timeout on every read", errs: []error{&sdkerrors.TimeoutError{Operation: listingSecret}, &sdkerrors.TimeoutError{}, &sdkerrors.TimeoutError{}}, wantReads: 3, wantReason: "listing_timeout"},
		{name: "not implemented", errs: []error{&sdkerrors.APIError{HTTPCode: 501}}, wantReads: 1, wantReason: "listing_http_501"},
		{name: "permission denied", errs: []error{&sdkerrors.PermissionError{What: listingSecret}}, wantReads: 1, wantReason: "listing_permission_denied"},
		{name: "authentication failed", errs: []error{&sdkerrors.AuthenticationError{Realm: listingSecret}}, wantReads: 1, wantReason: "listing_authentication_failed"},
		{name: "parameter rejected", errs: []error{&sdkerrors.ParameterError{Usage: listingSecret}}, wantReads: 1, wantReason: "listing_parameter_rejected"},
		{name: "certificate failure", errs: []error{&sdkerrors.SSLError{Host: listingSecret}}, wantReads: 1, wantReason: "listing_tls_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, observed := log.NewObservedLogger(log.LevelDebug)
			scripted := &scriptedListingNodes{errs: tc.errs, listing: present}
			deps := Deps{PVE: presencePVE{nodeClient: scripted}, Logger: logger}
			got, err := managedVolumePresent(t.Context(), deps, "pve1", volume)
			if scripted.reads != tc.wantReads {
				t.Fatalf("listings = %d, want %d", scripted.reads, tc.wantReads)
			}
			if tc.wantReason == "" {
				if err != nil || got != tc.wantPresent {
					t.Fatalf("presence = %v, %v, want %v with no error", got, err, tc.wantPresent)
				}
				if observed.Len() != 0 {
					t.Fatalf("a read that recovered logged %v", observed.All())
				}
				return
			}
			requireListingGaveUp(t, got, err, observed, tc.wantReason)
		})
	}
}

// requireListingGaveUp checks that a presence read gave up on a listing with
// reason, and that it logged one warning naming the reason, storage nfs, and
// node pve1 without any backend text.
func requireListingGaveUp(t *testing.T, present bool, err error, observed *log.Observer, reason string) {
	t.Helper()
	if err == nil || present {
		t.Fatalf("presence = %v, %v, want an error", present, err)
	}
	if got := pve.StorageVolumeObservationReason(err); got != reason {
		t.Fatalf("reason = %q, want %q", got, reason)
	}
	var warned []log.Entry
	for _, entry := range observed.All() {
		if entry.Level == log.LevelWarn {
			warned = append(warned, entry)
		}
	}
	if len(warned) != 1 {
		t.Fatalf("warnings = %v, want one", observed.All())
	}
	attrs := warned[0].Attrs
	if attrs["reason"] != reason || attrs["storage"] != "nfs" || attrs["node"] != "pve1" {
		t.Fatalf("warning attributes = %v, want the reason, storage nfs, and node pve1", attrs)
	}
	for key, value := range attrs {
		if text, ok := value.(string); ok && strings.Contains(text, "secret") {
			t.Fatalf("warning attribute %s carries backend text %q", key, text)
		}
	}
}

// TestManagedVolumePresentStopsWhenTheRequestEnds pins that the wait between
// reads gives way to the caller's context. The request ends while the read is
// waiting out a delay of a second, so only the wait's own check of the context
// can end it early.
func TestManagedVolumePresentStopsWhenTheRequestEnds(t *testing.T) {
	const delay = time.Second
	defer SetManagedVolumePresenceRetryDelay(delay)()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scripted := &scriptedListingNodes{errs: []error{&sdkerrors.APIError{HTTPCode: 503}, nil}}
	scripted.onRead = func() { time.AfterFunc(50*time.Millisecond, cancel) }
	deps := Deps{PVE: presencePVE{nodeClient: scripted}}
	start := time.Now()
	_, err := managedVolumePresent(ctx, deps, "pve1", "nfs:20000/disk.qcow2")
	if elapsed := time.Since(start); elapsed >= delay/2 {
		t.Fatalf("the read returned after %s, want it to end with the request, well before the %s delay", elapsed, delay)
	}
	if scripted.reads != 1 || pve.StorageVolumeObservationReason(err) != "listing_http_503" {
		t.Fatalf("reads = %d, err = %v, want one read that keeps the listing's reason", scripted.reads, err)
	}
}

// parkerVMIDs returns the VMs that carry the parker tag.
func parkerVMIDs(client *lifecycleFlowPVE) []int {
	var parkers []int
	for vmid, cfg := range client.state.configs {
		if tags, _ := cfg["tags"].(string); strings.Contains(tags, pve.ParkerTag) {
			parkers = append(parkers, vmid)
		}
	}
	slices.Sort(parkers)
	return parkers
}

// TestManagedVMDeletePreservationListingFailureNeverMovesDisk fails every
// listing of the persistent disk's storage while delete_vm reads the disk's
// identity before it parks the disk. The preservation stops with the disk
// still in its slot, no parker made or changed, and the disk's record as it
// was. Once the storage lists again, the preservation parks the disk.
func TestManagedVMDeletePreservationListingFailureNeverMovesDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	token := flowDiskToken(t, client)
	slot := client.state.configs[777]["scsi1"]
	volume := strings.Split(slot.(string), ",")[0]
	client.state.configs[777]["scsi0"] = "a:777/vm-777-disk-0.raw,size=10G"
	owned := map[string]bool{"a:777/vm-777-disk-0.raw": true}
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		t.Fatal(err)
	}
	before, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	parkers := parkerVMIDs(client)
	fault := &listingFault{storage: storage, failures: -1, err: &sdkerrors.APIError{HTTPCode: 500, Message: listingSecret}}
	failing := deps
	failing.PVE = listingFaultFlowPVE{lifecycleFlowPVE: client, fault: fault}

	err = detachManagedPersistentForVMDelete(context.Background(), failing, "n1", 777, owned, nil)
	if reason := pve.StorageVolumeObservationReason(err); reason != "listing_http_500" {
		t.Fatalf("preservation with a failing listing = %v (reason %q), want the listing's failure", err, reason)
	}
	if reads := fault.count(); reads != 3 {
		t.Fatalf("the disk's storage was listed %d times, want 3", reads)
	}
	if client.state.configs[777]["scsi1"] != slot || client.moves != 0 {
		t.Fatalf("scsi1 = %v after %d moves, want the disk still in its slot", client.state.configs[777]["scsi1"], client.moves)
	}
	if got := parkerVMIDs(client); !slices.Equal(got, parkers) {
		t.Fatalf("parkers = %v, want %v", got, parkers)
	}
	after, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || len(after.Steps) != len(before.Steps) || len(after.Verifications) != len(before.Verifications) {
		t.Fatalf("disk record changed from %s with %d steps to %s with %d steps", before.State, len(before.Steps), after.State, len(after.Steps))
	}

	if err := detachManagedPersistentForVMDelete(context.Background(), deps, "n1", 777, owned, nil); err != nil {
		t.Fatalf("preservation once the storage lists again: %v", err)
	}
	requireConfigEditPark(t, client, token, volume)
}

// TestManagedVMDeleteLifecycleFailureStillRequiresReconciliation pins that
// only a failed listing that is the whole failure, on a record whose steps are
// all settled, leaves the record as it was. Any other failure, including a
// listing failure that comes with a lifecycle's uncertainty or with another
// failure, still requires reconciliation.
func TestManagedVMDeleteLifecycleFailureStillRequiresReconciliation(t *testing.T) {
	listing := listingFailureFor(t, "pve1", "a:123/vm-123-disk-0.qcow2")
	uncertain := ce.Cloud("allocation 1 requires reconciliation at lifecycle delete_vm.preserve_disk; no alternate allocation was attempted")
	for _, tc := range []struct {
		name      string
		err       error
		unsettled bool
		clean     bool
	}{
		{name: "the listing alone", err: listing, clean: true},
		{name: "the listing behind an untyped wrap", err: wrapUntyped(listing), clean: true},
		{name: "the listing in a join of one member", err: errors.Join(listing, nil, nil), clean: true},
		{name: "the listing in a join inside a wrap", err: wrapUntyped(errors.Join(wrapUntyped(listing))), clean: true},
		{name: "the listing in a join of two members", err: errors.Join(listing, nil, errors.New("handle close failed"))},
		{name: "the listing joined with a lifecycle's uncertainty", err: errors.Join(listing, uncertain)},
		{name: "the listing behind a typed wrap", err: ce.Wrap(listing, "VM cleanup stopped")},
		{name: "the listing joined with another failure", err: errors.Join(listing, errors.New("parker write failed"))},
		{name: "the listing with a planned step", err: listing, unsettled: true},
		{name: "a failure without a listing", err: errors.New("config read failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := createdManagedVM(t)
			if tc.unsettled {
				record := m.handle.Record()
				if _, err := storageMutationIntent(m.handle, "vm.delete.destroy", aj.Target{Node: record.Steps[0].Target.Node, VMID: record.Steps[0].Target.VMID}, nil); err != nil {
					t.Fatal(err)
				}
			}
			before := m.handle.Record().State
			got := managedVMCleanupFailure(m.handle, tc.err)
			state := m.handle.Record().State
			if tc.clean {
				if state != before || state == aj.ReconciliationRequired {
					t.Fatalf("state = %s, want %s", state, before)
				}
				requireListingStoppedDelete(t, got, "a", "pve1", "listing_http_500")
				return
			}
			if state != aj.ReconciliationRequired {
				t.Fatalf("state = %s, want reconciliation_required", state)
			}
			if got == nil || strings.Contains(got.Error(), "rerun the delete") {
				t.Fatalf("error = %v, want the reconciliation path", got)
			}
		})
	}
}

// TestManagedVMRollbackListingFailureNamesTheStorage pins that a create_vm
// rollback a failed listing stopped still requires reconciliation, and that
// its error names the storage, the node, and the reason.
func TestManagedVMRollbackListingFailureNamesTheStorage(t *testing.T) {
	t.Parallel()
	m := createdManagedVM(t)
	got := managedVMRollbackFailure(m.handle, listingFailureFor(t, "pve1", "a:123/vm-123-disk-0.qcow2"))
	if state := m.handle.Record().State; state != aj.ReconciliationRequired {
		t.Fatalf("state = %s, want reconciliation_required", state)
	}
	var typed *ce.Error
	if !errors.As(got, &typed) {
		t.Fatalf("rollback error %v carries no CPI error", got)
	}
	message := typed.Error()
	for _, want := range []string{"storage a", "node pve1", "listing_http_500"} {
		if !strings.Contains(message, want) {
			t.Fatalf("rollback error %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "rerun the delete") || strings.Contains(message, "secret") {
		t.Fatalf("rollback error %q offers a rerun or carries backend text", message)
	}
}

// listingFailureFor returns the error a content listing that PVE answered
// with a server error gives for volume on node.
func listingFailureFor(t *testing.T, node, volume string) error {
	t.Helper()
	failing := presencePVE{nodeClient: presenceNodes{err: &sdkerrors.APIError{HTTPCode: 500, Message: listingSecret}}}
	_, err := pve.ObserveStorageVolumePresence(t.Context(), failing, node, volume)
	if pve.StorageVolumeObservationReason(err) != "listing_http_500" {
		t.Fatalf("setup: listing failure = %v", err)
	}
	return err
}

// untypedWrap wraps an error the way a caller's fmt.Errorf with %w does.
type untypedWrap struct{ err error }

func (w untypedWrap) Error() string { return "presence check: " + w.err.Error() }
func (w untypedWrap) Unwrap() error { return w.err }

func wrapUntyped(err error) error { return untypedWrap{err: err} }

// TestManagedVMDeletePersistentDiskListingFailureUnderLockLeavesRecordObserved
// fails the content listing that the persistent disk's lifecycle makes after
// it takes the allocation lock, while the listing before the lock succeeds.
// The lifecycle's early failure comes back joined with the results of closing
// its handle and journal, which are nil, and delete_vm still leaves the VM's
// record observed and names the storage.
func TestManagedVMDeletePersistentDiskListingFailureUnderLockLeavesRecordObserved(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	slot := client.state.configs[777]["scsi1"]
	volume := strings.Split(slot.(string), ",")[0]
	client.state.configs[777]["scsi0"] = "a:777/vm-777-disk-0.raw,size=10G"
	owned := map[string]bool{"a:777/vm-777-disk-0.raw": true}
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		t.Fatal(err)
	}
	fault := &listingFault{
		storage: storage, failures: -1, err: &sdkerrors.APIError{HTTPCode: 500, Message: listingSecret},
		when: func(ctx context.Context) bool { return holderHealFor(ctx) == holderHealWrite },
	}
	failing := deps
	failing.PVE = listingFaultFlowPVE{lifecycleFlowPVE: client, fault: fault}
	handle, err := journal.AcquireVM(context.Background(), "vm-agent", record.Intent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	step, err := storageMutationIntent(handle, "vm_create", aj.Target{Node: "n1", VMID: 777}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, nil, false); err != nil {
		t.Fatal(err)
	}
	vm := handle.Record()
	vm.State = aj.Observed
	vm.CID = "777"
	if err := handle.Save(vm); err != nil {
		t.Fatal(err)
	}

	err = detachManagedPersistentForVMDelete(context.Background(), failing, "n1", 777, owned, handle)
	if reason := pve.StorageVolumeObservationReason(err); reason != "listing_http_500" {
		t.Fatalf("preservation with a failing listing under the lock = %v (reason %q), want the listing's failure", err, reason)
	}
	if reads := fault.count(); reads != 3 {
		t.Fatalf("the listing under the lock read %d times, want 3", reads)
	}
	got := managedVMCleanupFailure(handle, err)
	requireListingStoppedDelete(t, got, storage, "n1", "listing_http_500")
	if state := handle.Record().State; state != aj.Observed {
		t.Fatalf("record state = %s, want observed", state)
	}
	requireListingStopReason(t, handle.Record().Reason, storage, "n1", "listing_http_500")
	if client.state.configs[777]["scsi1"] != slot || client.moves != 0 {
		t.Fatalf("scsi1 = %v after %d moves, want the disk still in its slot", client.state.configs[777]["scsi1"], client.moves)
	}
}

// TestStorageAllocationDecisionFailureNamesAListingStop pins what the
// storage-journal cleanup command prints when a failed listing alone stopped
// the VM's disposal. It names the storage, the node, and the reason, and it
// says to run cleanup again.
func TestStorageAllocationDecisionFailureNamesAListingStop(t *testing.T) {
	t.Parallel()
	m := createdManagedVM(t)
	stopped := managedVMCleanupFailure(m.handle, listingFailureFor(t, "pve1", "a:123/vm-123-disk-0.qcow2"))
	got := StorageAllocationDecisionFailure(stopped)
	for _, want := range []string{"storage a", "node pve1", "listing_http_500", "run cleanup again once that storage lists"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cleanup text %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "unclassified") || strings.Contains(got, "secret") {
		t.Fatalf("cleanup text %q is unclassified or carries backend text", got)
	}
}
