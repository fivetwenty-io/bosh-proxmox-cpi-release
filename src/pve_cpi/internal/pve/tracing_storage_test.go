// tracing_storage_test.go — span-assertion matrix for the 6 overridden
// tracedStorageService methods (CreateVolume, Exists, DeleteVolumeAsync,
// DeleteVolumeIfExists, DeleteVolumeIfExistsAsync, Upload). Mirrors the
// in-memory-exporter pattern established for QEMU in tracing_test.go: a real
// SDK tracer + tracetest.InMemoryExporter, one exported span per call,
// asserted on name, status, and attributes.
package pve

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
)

// storageCredentialErr is the shared error used across every error-path
// subtest below. It embeds a userinfo credential and a presigned-URL query
// secret so each test proves the span's recorded error/status is scrubbed by
// finishSpan, not just that some error string made it onto the span.
func storageCredentialErr() error {
	return errors.New("upstream call failed: https://svc:s3cretpw@pve1.lab.internal/upload?X-Amz-Signature=deadbeef1234 returned 500")
}

// assertStorageSpanAttrs looks up the single attribute value for key on span
// and fails the test unless it exists and equals want.
func assertStorageSpanAttrs(t *testing.T, span tracetest.SpanStub, want map[string]string) {
	t.Helper()
	got := make(map[string]string, len(span.Attributes))
	for _, kv := range span.Attributes {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	for k, wantV := range want {
		gotV, ok := got[k]
		if !ok {
			t.Errorf("span %q missing attribute %s (want %q); got attrs %v", span.Name, k, wantV, got)
			continue
		}
		if gotV != wantV {
			t.Errorf("span %q attribute %s = %q, want %q", span.Name, k, gotV, wantV)
		}
	}
}

// requireSingleSpan asserts exactly one span was exported and returns it.
func requireSingleSpan(t *testing.T, exporter *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1: %+v", len(spans), spans)
	}
	return spans[0]
}

// assertScrubbedErrorSpan asserts span carries an Error status whose
// description is the scrubbed form of rawErr, with the raw secrets absent
// and the redaction marker present.
func assertScrubbedErrorSpan(t *testing.T, span tracetest.SpanStub, rawErr error, secrets ...string) {
	t.Helper()
	if span.Status.Code != codes.Error {
		t.Fatalf("span %q status code = %v, want Error", span.Name, span.Status.Code)
	}
	wantDesc := log.ScrubMessage(rawErr.Error())
	if span.Status.Description != wantDesc {
		t.Errorf("span %q status description = %q, want %q", span.Name, span.Status.Description, wantDesc)
	}
	for _, secret := range secrets {
		if strings.Contains(span.Status.Description, secret) {
			t.Errorf("span %q status description leaks credential %q: %q", span.Name, secret, span.Status.Description)
		}
	}
	if !strings.Contains(span.Status.Description, log.RedactedPlaceholder) {
		t.Errorf("span %q status description not scrubbed, want %q marker: %q", span.Name, log.RedactedPlaceholder, span.Status.Description)
	}
	if len(span.Events) == 0 {
		t.Errorf("span %q: expected span.RecordError to add an exception event, got none", span.Name)
	}
}

// --------------------------------------------------------------------------
// CreateVolume — span pve.storage.create_volume, attrs pve.node/pve.storage/
// pve.vmid (vmid always attribute.String, per tracing.go's CreateVolume
// override).
// --------------------------------------------------------------------------

func TestTracedStorageService_CreateVolume_Success(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	fake := &fakeStorageService{
		createVolumeFn: func(_ context.Context, node, storageName string, sizeGiB int, format string, vmid int, name string) (string, error) {
			if node != "pve1" || storageName != "local-lvm" || sizeGiB != 20 || format != "qcow2" || vmid != 100 || name != "disk-1" {
				t.Errorf("CreateVolume called with node=%q storage=%q sizeGiB=%d format=%q vmid=%d name=%q, want pve1/local-lvm/20/qcow2/100/disk-1",
					node, storageName, sizeGiB, format, vmid, name)
			}
			return "local-lvm:100/vm-100-disk-0.qcow2", nil
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}
	// Compile-time proof the decorator still satisfies storage.Service.
	var _ storage.Service = traced

	volid, err := traced.CreateVolume(context.Background(), "pve1", "local-lvm", 20, "qcow2", 100, "disk-1")
	if err != nil {
		t.Fatalf("CreateVolume returned error: %v", err)
	}
	if volid != "local-lvm:100/vm-100-disk-0.qcow2" {
		t.Fatalf("CreateVolume returned volid=%q, want local-lvm:100/vm-100-disk-0.qcow2", volid)
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.create_volume" {
		t.Fatalf("span name = %q, want pve.storage.create_volume", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
		"pve.vmid":    "100",
	})
}

func TestTracedStorageService_CreateVolume_Error(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)
	rawErr := storageCredentialErr()

	fake := &fakeStorageService{
		createVolumeFn: func(context.Context, string, string, int, string, int, string) (string, error) {
			return "", rawErr
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	volid, err := traced.CreateVolume(context.Background(), "pve1", "local-lvm", 20, "qcow2", 100, "disk-1")
	if !errors.Is(err, rawErr) {
		t.Fatalf("CreateVolume returned err=%v, want %v", err, rawErr)
	}
	if volid != "" {
		t.Fatalf("CreateVolume returned volid=%q on error, want empty", volid)
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.create_volume" {
		t.Fatalf("span name = %q, want pve.storage.create_volume", span.Name)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
		"pve.vmid":    "100",
	})
	assertScrubbedErrorSpan(t, span, rawErr, "s3cretpw", "deadbeef1234")
}

// --------------------------------------------------------------------------
// Exists — span pve.storage.exists, attrs pve.node/pve.storage (no vmid: the
// method signature carries no vmid param).
// --------------------------------------------------------------------------

func TestTracedStorageService_Exists_Success(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	fake := &fakeStorageService{
		existsFn: func(_ context.Context, node, storageName, volume string) (bool, error) {
			if node != "pve1" || storageName != "local-lvm" || volume != "vm-100-disk-0.qcow2" {
				t.Errorf("Exists called with node=%q storage=%q volume=%q, want pve1/local-lvm/vm-100-disk-0.qcow2", node, storageName, volume)
			}
			return true, nil
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	exists, err := traced.Exists(context.Background(), "pve1", "local-lvm", "vm-100-disk-0.qcow2")
	if err != nil {
		t.Fatalf("Exists returned error: %v", err)
	}
	if !exists {
		t.Fatal("Exists returned false, want true")
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.exists" {
		t.Fatalf("span name = %q, want pve.storage.exists", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
}

func TestTracedStorageService_Exists_Error(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)
	rawErr := storageCredentialErr()

	fake := &fakeStorageService{
		existsFn: func(context.Context, string, string, string) (bool, error) {
			return false, rawErr
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	exists, err := traced.Exists(context.Background(), "pve1", "local-lvm", "vm-100-disk-0.qcow2")
	if !errors.Is(err, rawErr) {
		t.Fatalf("Exists returned err=%v, want %v", err, rawErr)
	}
	if exists {
		t.Fatal("Exists returned true on error, want false")
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.exists" {
		t.Fatalf("span name = %q, want pve.storage.exists", span.Name)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
	assertScrubbedErrorSpan(t, span, rawErr, "s3cretpw", "deadbeef1234")
}

// --------------------------------------------------------------------------
// DeleteVolumeAsync — span pve.storage.delete_volume_async, attrs pve.node/
// pve.storage.
// --------------------------------------------------------------------------

func TestTracedStorageService_DeleteVolumeAsync_Success(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	fake := &fakeStorageService{
		deleteVolumeAsyncFn: func(_ context.Context, node, storageName, volume string) (string, error) {
			if node != "pve1" || storageName != "local-lvm" || volume != "vm-100-disk-0.qcow2" {
				t.Errorf("DeleteVolumeAsync called with node=%q storage=%q volume=%q, want pve1/local-lvm/vm-100-disk-0.qcow2", node, storageName, volume)
			}
			return "UPID:pve1:imgdel", nil
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	upid, err := traced.DeleteVolumeAsync(context.Background(), "pve1", "local-lvm", "vm-100-disk-0.qcow2")
	if err != nil {
		t.Fatalf("DeleteVolumeAsync returned error: %v", err)
	}
	if upid != "UPID:pve1:imgdel" {
		t.Fatalf("DeleteVolumeAsync returned upid=%q, want UPID:pve1:imgdel", upid)
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.delete_volume_async" {
		t.Fatalf("span name = %q, want pve.storage.delete_volume_async", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
}

func TestTracedStorageService_DeleteVolumeAsync_Error(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)
	rawErr := storageCredentialErr()

	fake := &fakeStorageService{
		deleteVolumeAsyncFn: func(context.Context, string, string, string) (string, error) {
			return "", rawErr
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	upid, err := traced.DeleteVolumeAsync(context.Background(), "pve1", "local-lvm", "vm-100-disk-0.qcow2")
	if !errors.Is(err, rawErr) {
		t.Fatalf("DeleteVolumeAsync returned err=%v, want %v", err, rawErr)
	}
	if upid != "" {
		t.Fatalf("DeleteVolumeAsync returned upid=%q on error, want empty", upid)
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.delete_volume_async" {
		t.Fatalf("span name = %q, want pve.storage.delete_volume_async", span.Name)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
	assertScrubbedErrorSpan(t, span, rawErr, "s3cretpw", "deadbeef1234")
}

// --------------------------------------------------------------------------
// DeleteVolumeIfExists — span pve.storage.delete_volume_if_exists, attrs
// pve.node/pve.storage.
// --------------------------------------------------------------------------

func TestTracedStorageService_DeleteVolumeIfExists_Success(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	fake := &fakeStorageService{
		deleteVolumeIfExistsFn: func(_ context.Context, node, storageName, volume string) (bool, error) {
			if node != "pve1" || storageName != "local-lvm" || volume != "vm-100-disk-0.qcow2" {
				t.Errorf("DeleteVolumeIfExists called with node=%q storage=%q volume=%q, want pve1/local-lvm/vm-100-disk-0.qcow2", node, storageName, volume)
			}
			return true, nil
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	existed, err := traced.DeleteVolumeIfExists(context.Background(), "pve1", "local-lvm", "vm-100-disk-0.qcow2")
	if err != nil {
		t.Fatalf("DeleteVolumeIfExists returned error: %v", err)
	}
	if !existed {
		t.Fatal("DeleteVolumeIfExists returned existed=false, want true")
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.delete_volume_if_exists" {
		t.Fatalf("span name = %q, want pve.storage.delete_volume_if_exists", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
}

func TestTracedStorageService_DeleteVolumeIfExists_Error(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)
	rawErr := storageCredentialErr()

	fake := &fakeStorageService{
		deleteVolumeIfExistsFn: func(context.Context, string, string, string) (bool, error) {
			return false, rawErr
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	existed, err := traced.DeleteVolumeIfExists(context.Background(), "pve1", "local-lvm", "vm-100-disk-0.qcow2")
	if !errors.Is(err, rawErr) {
		t.Fatalf("DeleteVolumeIfExists returned err=%v, want %v", err, rawErr)
	}
	if existed {
		t.Fatal("DeleteVolumeIfExists returned existed=true on error, want false")
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.delete_volume_if_exists" {
		t.Fatalf("span name = %q, want pve.storage.delete_volume_if_exists", span.Name)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
	assertScrubbedErrorSpan(t, span, rawErr, "s3cretpw", "deadbeef1234")
}

// --------------------------------------------------------------------------
// DeleteVolumeIfExistsAsync — span pve.storage.delete_volume_if_exists_async,
// attrs pve.node/pve.storage.
// --------------------------------------------------------------------------

func TestTracedStorageService_DeleteVolumeIfExistsAsync_Success(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	fake := &fakeStorageService{
		deleteVolumeIfExistsAsyncFn: func(_ context.Context, node, storageName, volume string) (bool, string, error) {
			if node != "pve1" || storageName != "local-lvm" || volume != "vm-100-disk-0.qcow2" {
				t.Errorf("DeleteVolumeIfExistsAsync called with node=%q storage=%q volume=%q, want pve1/local-lvm/vm-100-disk-0.qcow2", node, storageName, volume)
			}
			return true, "UPID:pve1:imgdel", nil
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	existed, upid, err := traced.DeleteVolumeIfExistsAsync(context.Background(), "pve1", "local-lvm", "vm-100-disk-0.qcow2")
	if err != nil {
		t.Fatalf("DeleteVolumeIfExistsAsync returned error: %v", err)
	}
	if !existed || upid != "UPID:pve1:imgdel" {
		t.Fatalf("DeleteVolumeIfExistsAsync returned existed=%v upid=%q, want true/UPID:pve1:imgdel", existed, upid)
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.delete_volume_if_exists_async" {
		t.Fatalf("span name = %q, want pve.storage.delete_volume_if_exists_async", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
}

func TestTracedStorageService_DeleteVolumeIfExistsAsync_Error(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)
	rawErr := storageCredentialErr()

	fake := &fakeStorageService{
		deleteVolumeIfExistsAsyncFn: func(context.Context, string, string, string) (bool, string, error) {
			return false, "", rawErr
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	existed, upid, err := traced.DeleteVolumeIfExistsAsync(context.Background(), "pve1", "local-lvm", "vm-100-disk-0.qcow2")
	if !errors.Is(err, rawErr) {
		t.Fatalf("DeleteVolumeIfExistsAsync returned err=%v, want %v", err, rawErr)
	}
	if existed || upid != "" {
		t.Fatalf("DeleteVolumeIfExistsAsync returned existed=%v upid=%q on error, want false/empty", existed, upid)
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.delete_volume_if_exists_async" {
		t.Fatalf("span name = %q, want pve.storage.delete_volume_if_exists_async", span.Name)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
	assertScrubbedErrorSpan(t, span, rawErr, "s3cretpw", "deadbeef1234")
}

// --------------------------------------------------------------------------
// Upload — span pve.storage.upload, attrs pve.node/pve.storage.
// --------------------------------------------------------------------------

func TestTracedStorageService_Upload_Success(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	body := strings.NewReader("iso-bytes")
	fake := &fakeStorageService{
		uploadFn: func(_ context.Context, node, storageName, content, filename string, gotBody io.Reader) (string, error) {
			if node != "pve1" || storageName != "local-lvm" || content != "iso" || filename != "stemcell.iso" || gotBody != body {
				t.Errorf("Upload called with node=%q storage=%q content=%q filename=%q, want pve1/local-lvm/iso/stemcell.iso (and same body reader)",
					node, storageName, content, filename)
			}
			return "UPID:pve1:upload", nil
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	upid, err := traced.Upload(context.Background(), "pve1", "local-lvm", "iso", "stemcell.iso", body)
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if upid != "UPID:pve1:upload" {
		t.Fatalf("Upload returned upid=%q, want UPID:pve1:upload", upid)
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.upload" {
		t.Fatalf("span name = %q, want pve.storage.upload", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
}

func TestTracedStorageService_Upload_Error(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)
	rawErr := storageCredentialErr()

	fake := &fakeStorageService{
		uploadFn: func(context.Context, string, string, string, string, io.Reader) (string, error) {
			return "", rawErr
		},
	}
	traced := &tracedStorageService{Service: fake, tracer: tracer}

	upid, err := traced.Upload(context.Background(), "pve1", "local-lvm", "iso", "stemcell.iso", strings.NewReader("iso-bytes"))
	if !errors.Is(err, rawErr) {
		t.Fatalf("Upload returned err=%v, want %v", err, rawErr)
	}
	if upid != "" {
		t.Fatalf("Upload returned upid=%q on error, want empty", upid)
	}

	span := requireSingleSpan(t, exporter)
	if span.Name != "pve.storage.upload" {
		t.Fatalf("span name = %q, want pve.storage.upload", span.Name)
	}
	assertStorageSpanAttrs(t, span, map[string]string{
		"pve.node":    "pve1",
		"pve.storage": "local-lvm",
	})
	assertScrubbedErrorSpan(t, span, rawErr, "s3cretpw", "deadbeef1234")
}
