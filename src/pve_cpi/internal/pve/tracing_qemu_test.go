// tracing_qemu_test.go — success+error span-assertion matrix for the 11
// tracedQEMUService methods not already covered by the Config exemplar
// tests in tracing_test.go (Create, Status, Start, Stop, Reset, AttachDisk,
// DetachDisk, ResizeDisk, Snapshot, DeleteSnapshot, ListSnapshots).
package pve

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// qemuMethodCase describes one tracedQEMUService method under test: the
// span name it must produce, the node/vmid attribute values the call site
// carries, and how to build a fake wired for either the success or the
// error path plus how to invoke the traced method against that fake.
type qemuMethodCase struct {
	name string
	// wantSpan is the exact span name the decorator must emit.
	wantSpan string
	// wantNode is the pve.node attribute value every call in this table
	// uses.
	wantNode string
	// wantVMID is the pve.vmid attribute value, always encoded as an
	// attribute.String even though the Go parameter type is int (per the
	// vmid-stringification normalization in tracing.go). Empty string means
	// this method's span carries no pve.vmid attribute at all (Create takes
	// no vmid parameter).
	wantVMID string
	// buildSuccessFake returns a fakeQEMUService whose relevant *Fn field
	// is wired to return a canned success value.
	buildSuccessFake func() *fakeQEMUService
	// buildErrorFake returns a fakeQEMUService whose relevant *Fn field is
	// wired to return wantErr.
	buildErrorFake func(wantErr error) *fakeQEMUService
	// invoke calls the method under test against traced and returns only
	// the error, discarding any other return value — spans are asserted
	// independently of the payload.
	invoke func(traced *tracedQEMUService) error
}

func qemuMethodCases() []qemuMethodCase {
	const node = "pve1"
	const vmid = 42

	return []qemuMethodCase{
		{
			name:     "Create",
			wantSpan: "pve.qemu.create",
			wantNode: node,
			wantVMID: "", // Create has no vmid parameter, so no pve.vmid attribute.
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{createFn: func(context.Context, string, map[string]interface{}) (string, error) {
					return "UPID:pve1:qmcreate:", nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{createFn: func(context.Context, string, map[string]interface{}) (string, error) {
					return "", wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.Create(context.Background(), node, map[string]interface{}{"vmid": vmid})
				return err
			},
		},
		{
			name:     "Status",
			wantSpan: "pve.qemu.status",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{statusFn: func(context.Context, string, int) (map[string]interface{}, error) {
					return map[string]interface{}{"status": "running"}, nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{statusFn: func(context.Context, string, int) (map[string]interface{}, error) {
					return nil, wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.Status(context.Background(), node, vmid)
				return err
			},
		},
		{
			name:     "Start",
			wantSpan: "pve.qemu.start",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{startFn: func(context.Context, string, int) (string, error) {
					return "UPID:pve1:qmstart:", nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{startFn: func(context.Context, string, int) (string, error) {
					return "", wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.Start(context.Background(), node, vmid)
				return err
			},
		},
		{
			name:     "Stop",
			wantSpan: "pve.qemu.stop",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{stopFn: func(context.Context, string, int) (string, error) {
					return "UPID:pve1:qmstop:", nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{stopFn: func(context.Context, string, int) (string, error) {
					return "", wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.Stop(context.Background(), node, vmid)
				return err
			},
		},
		{
			name:     "Reset",
			wantSpan: "pve.qemu.reset",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{resetFn: func(context.Context, string, int) (string, error) {
					return "UPID:pve1:qmreset:", nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{resetFn: func(context.Context, string, int) (string, error) {
					return "", wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.Reset(context.Background(), node, vmid)
				return err
			},
		},
		{
			name:     "AttachDisk",
			wantSpan: "pve.qemu.attach_disk",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{attachDiskFn: func(context.Context, string, int, string, string, *qemu.AttachOpts) (string, error) {
					return "scsi1", nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{attachDiskFn: func(context.Context, string, int, string, string, *qemu.AttachOpts) (string, error) {
					return "", wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.AttachDisk(context.Background(), node, vmid, "vol-0001", "scsi", &qemu.AttachOpts{})
				return err
			},
		},
		{
			name:     "DetachDisk",
			wantSpan: "pve.qemu.detach_disk",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{detachDiskFn: func(context.Context, string, int, string) error {
					return nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{detachDiskFn: func(context.Context, string, int, string) error {
					return wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				return traced.DetachDisk(context.Background(), node, vmid, "scsi1")
			},
		},
		{
			name:     "ResizeDisk",
			wantSpan: "pve.qemu.resize_disk",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{resizeDiskFn: func(context.Context, string, int, string, int) (string, error) {
					return "UPID:pve1:qmresize:", nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{resizeDiskFn: func(context.Context, string, int, string, int) (string, error) {
					return "", wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.ResizeDisk(context.Background(), node, vmid, "scsi1", 20)
				return err
			},
		},
		{
			name:     "Snapshot",
			wantSpan: "pve.qemu.snapshot",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{snapshotFn: func(context.Context, string, int, string, map[string]interface{}) (string, error) {
					return "UPID:pve1:qmsnapshot:", nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{snapshotFn: func(context.Context, string, int, string, map[string]interface{}) (string, error) {
					return "", wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.Snapshot(context.Background(), node, vmid, "snap1", map[string]interface{}{"description": "pre-upgrade"})
				return err
			},
		},
		{
			name:     "DeleteSnapshot",
			wantSpan: "pve.qemu.delete_snapshot",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{deleteSnapshotFn: func(context.Context, string, int, string) error {
					return nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{deleteSnapshotFn: func(context.Context, string, int, string) error {
					return wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				return traced.DeleteSnapshot(context.Background(), node, vmid, "snap1")
			},
		},
		{
			name:     "ListSnapshots",
			wantSpan: "pve.qemu.list_snapshots",
			wantNode: node,
			wantVMID: "42",
			buildSuccessFake: func() *fakeQEMUService {
				return &fakeQEMUService{listSnapshotsFn: func(context.Context, string, int) ([]map[string]interface{}, error) {
					return []map[string]interface{}{{"name": "snap1"}}, nil
				}}
			},
			buildErrorFake: func(wantErr error) *fakeQEMUService {
				return &fakeQEMUService{listSnapshotsFn: func(context.Context, string, int) ([]map[string]interface{}, error) {
					return nil, wantErr
				}}
			},
			invoke: func(traced *tracedQEMUService) error {
				_, err := traced.ListSnapshots(context.Background(), node, vmid)
				return err
			},
		},
	}
}

// assertQEMUSpanAttributes checks the span carries the wanted pve.node
// attribute and, when tc.wantVMID is non-empty, the wanted pve.vmid
// attribute as an attribute.String (vmid is always stringified onto the
// span regardless of the method parameter's Go type).
func assertQEMUSpanAttributes(t *testing.T, tc qemuMethodCase, attrs []attribute.KeyValue) {
	t.Helper()

	var sawNode, sawVMID bool
	var gotNode, gotVMID string
	for _, kv := range attrs {
		switch string(kv.Key) {
		case "pve.node":
			sawNode = true
			gotNode = kv.Value.AsString()
		case "pve.vmid":
			sawVMID = true
			gotVMID = kv.Value.AsString()
		}
	}

	if !sawNode || gotNode != tc.wantNode {
		t.Errorf("%s: pve.node attribute = (present=%v, value=%q), want (true, %q)", tc.name, sawNode, gotNode, tc.wantNode)
	}

	if tc.wantVMID == "" {
		if sawVMID {
			t.Errorf("%s: unexpected pve.vmid attribute %q on a method with no vmid parameter", tc.name, gotVMID)
		}
		return
	}

	if !sawVMID || gotVMID != tc.wantVMID {
		t.Errorf("%s: pve.vmid attribute = (present=%v, value=%q), want (true, %q)", tc.name, sawVMID, gotVMID, tc.wantVMID)
	}
}

func TestTracedQEMUService_Matrix_AllMethodsSuccessAndError(t *testing.T) {
	t.Parallel()

	for _, tc := range qemuMethodCases() {
		t.Run(tc.name+"_success", func(t *testing.T) {
			t.Parallel()
			tracer, exporter := newTestTracer(t)
			traced := &tracedQEMUService{Service: tc.buildSuccessFake(), tracer: tracer}

			if err := tc.invoke(traced); err != nil {
				t.Fatalf("%s returned err=%v, want nil", tc.name, err)
			}

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d exported spans, want exactly 1", len(spans))
			}
			span := spans[0]

			if span.Name != tc.wantSpan {
				t.Fatalf("span name = %q, want %q", span.Name, tc.wantSpan)
			}
			if span.Status.Code == codes.Error {
				t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
			}
			assertQEMUSpanAttributes(t, tc, span.Attributes)
		})

		t.Run(tc.name+"_error", func(t *testing.T) {
			t.Parallel()
			tracer, exporter := newTestTracer(t)
			wantErr := errors.New(tc.name + " failed: pve unreachable")
			traced := &tracedQEMUService{Service: tc.buildErrorFake(wantErr), tracer: tracer}

			err := tc.invoke(traced)
			if !errors.Is(err, wantErr) {
				t.Fatalf("%s returned err=%v, want %v", tc.name, err, wantErr)
			}

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d exported spans, want exactly 1", len(spans))
			}
			span := spans[0]

			if span.Name != tc.wantSpan {
				t.Fatalf("span name = %q, want %q", span.Name, tc.wantSpan)
			}
			if span.Status.Code != codes.Error {
				t.Fatalf("span status code = %v, want Error", span.Status.Code)
			}
			if span.Status.Description != wantErr.Error() {
				t.Errorf("span status description = %q, want %q (scrubbing only rewrites credential-bearing text, and this error carries none)", span.Status.Description, wantErr.Error())
			}
			if len(span.Events) == 0 {
				t.Errorf("%s: expected span.RecordError to add an exception event, got none", tc.name)
			}
			assertQEMUSpanAttributes(t, tc, span.Attributes)
		})
	}
}
