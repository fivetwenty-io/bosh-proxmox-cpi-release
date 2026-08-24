// tracing_tasks_test.go — success+error span-assertion matrix for
// tracedTasksService (Wait, GetStatus). Mirrors the QEMU exemplar tests in
// tracing_test.go: one child span per call, correct name, correct
// attributes, Error status with a scrubbed message on failure.
package pve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// --------------------------------------------------------------------------
// Wait
// --------------------------------------------------------------------------

func TestTracedTasksService_Wait_Success(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	wantStatus := &tasks.Status{Status: "stopped", ExitStatus: "OK", UpID: "UPID:pve1:wait"}
	fake := &fakeTasksService{
		waitFn: func(_ context.Context, node, upid string, opts *tasks.WaitOptions) (*tasks.Status, error) {
			if node != "pve1" || upid != "UPID:pve1:wait" {
				t.Errorf("Wait called with node=%q upid=%q, want pve1/UPID:pve1:wait", node, upid)
			}
			if opts == nil || opts.TimeoutSeconds != 30 {
				t.Errorf("Wait called with opts=%+v, want TimeoutSeconds=30", opts)
			}
			return wantStatus, nil
		},
	}
	traced := &tracedTasksService{Service: fake, tracer: tracer}

	status, err := traced.Wait(context.Background(), "pve1", "UPID:pve1:wait", &tasks.WaitOptions{TimeoutSeconds: 30})
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if status != wantStatus {
		t.Fatalf("Wait returned %+v, want %+v", status, wantStatus)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "pve.tasks.wait" {
		t.Fatalf("span name = %q, want pve.tasks.wait", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	var sawNode, sawUPID bool
	for _, kv := range span.Attributes {
		switch string(kv.Key) {
		case "pve.node":
			sawNode = kv.Value.AsString() == "pve1"
		case "pve.upid":
			sawUPID = kv.Value.AsString() == "UPID:pve1:wait"
		}
	}
	if !sawNode {
		t.Error("span missing/incorrect pve.node attribute")
	}
	if !sawUPID {
		t.Error("span missing/incorrect pve.upid attribute")
	}
}

func TestTracedTasksService_Wait_Error_SpanRecordsScrubbedError(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	rawErr := errors.New("task poll failed: GET https://bosh:s3cretpw@blob.lab.internal/status?X-Amz-Signature=deadbeef1234 returned 403")
	fake := &fakeTasksService{
		waitFn: func(context.Context, string, string, *tasks.WaitOptions) (*tasks.Status, error) {
			return nil, rawErr
		},
	}
	traced := &tracedTasksService{Service: fake, tracer: tracer}

	_, err := traced.Wait(context.Background(), "pve1", "UPID:pve1:wait", nil)
	if !errors.Is(err, rawErr) {
		t.Fatalf("Wait returned err=%v, want the caller to still receive the raw %v", err, rawErr)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "pve.tasks.wait" {
		t.Fatalf("span name = %q, want pve.tasks.wait", span.Name)
	}
	if span.Status.Code != codes.Error {
		t.Fatalf("span status code = %v, want Error", span.Status.Code)
	}
	if len(span.Events) == 0 {
		t.Fatal("expected span.RecordError to add an exception event, got none")
	}
	var sawNode, sawUPID bool
	for _, kv := range span.Attributes {
		switch string(kv.Key) {
		case "pve.node":
			sawNode = kv.Value.AsString() == "pve1"
		case "pve.upid":
			sawUPID = kv.Value.AsString() == "UPID:pve1:wait"
		}
	}
	if !sawNode || !sawUPID {
		t.Error("span missing/incorrect pve.node or pve.upid attribute")
	}

	for _, secret := range []string{"s3cretpw", "deadbeef1234"} {
		if strings.Contains(span.Status.Description, secret) {
			t.Errorf("span status description leaks credential %q: %q", secret, span.Status.Description)
		}
		for _, ev := range span.Events {
			for _, attr := range ev.Attributes {
				if v := attr.Value.AsString(); strings.Contains(v, secret) {
					t.Errorf("span event attribute %s leaks credential %q: %q", attr.Key, secret, v)
				}
			}
		}
	}
	if !strings.Contains(span.Status.Description, log.RedactedPlaceholder) {
		t.Errorf("span status description not scrubbed, want %q marker: %q", log.RedactedPlaceholder, span.Status.Description)
	}
}

// --------------------------------------------------------------------------
// GetStatus
// --------------------------------------------------------------------------

func TestTracedTasksService_GetStatus_Success(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	wantStatus := &tasks.Status{Status: "running", Progress: 0.5, UpID: "UPID:pve2:getstatus"}
	fake := &fakeTasksService{
		getStatusFn: func(_ context.Context, node, upid string) (*tasks.Status, error) {
			if node != "pve2" || upid != "UPID:pve2:getstatus" {
				t.Errorf("GetStatus called with node=%q upid=%q, want pve2/UPID:pve2:getstatus", node, upid)
			}
			return wantStatus, nil
		},
	}
	traced := &tracedTasksService{Service: fake, tracer: tracer}

	status, err := traced.GetStatus(context.Background(), "pve2", "UPID:pve2:getstatus")
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if status != wantStatus {
		t.Fatalf("GetStatus returned %+v, want %+v", status, wantStatus)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "pve.tasks.get_status" {
		t.Fatalf("span name = %q, want pve.tasks.get_status", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	var sawNode, sawUPID bool
	for _, kv := range span.Attributes {
		switch string(kv.Key) {
		case "pve.node":
			sawNode = kv.Value.AsString() == "pve2"
		case "pve.upid":
			sawUPID = kv.Value.AsString() == "UPID:pve2:getstatus"
		}
	}
	if !sawNode {
		t.Error("span missing/incorrect pve.node attribute")
	}
	if !sawUPID {
		t.Error("span missing/incorrect pve.upid attribute")
	}
}

func TestTracedTasksService_GetStatus_Error_SpanRecordsScrubbedError(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	rawErr := errors.New("get task status failed: GET https://bosh:s3cretpw@blob.lab.internal/status?X-Amz-Signature=deadbeef1234 returned 403")
	fake := &fakeTasksService{
		getStatusFn: func(context.Context, string, string) (*tasks.Status, error) {
			return nil, rawErr
		},
	}
	traced := &tracedTasksService{Service: fake, tracer: tracer}

	_, err := traced.GetStatus(context.Background(), "pve2", "UPID:pve2:getstatus")
	if !errors.Is(err, rawErr) {
		t.Fatalf("GetStatus returned err=%v, want the caller to still receive the raw %v", err, rawErr)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "pve.tasks.get_status" {
		t.Fatalf("span name = %q, want pve.tasks.get_status", span.Name)
	}
	if span.Status.Code != codes.Error {
		t.Fatalf("span status code = %v, want Error", span.Status.Code)
	}
	if len(span.Events) == 0 {
		t.Fatal("expected span.RecordError to add an exception event, got none")
	}
	var sawNode, sawUPID bool
	for _, kv := range span.Attributes {
		switch string(kv.Key) {
		case "pve.node":
			sawNode = kv.Value.AsString() == "pve2"
		case "pve.upid":
			sawUPID = kv.Value.AsString() == "UPID:pve2:getstatus"
		}
	}
	if !sawNode || !sawUPID {
		t.Error("span missing/incorrect pve.node or pve.upid attribute")
	}

	for _, secret := range []string{"s3cretpw", "deadbeef1234"} {
		if strings.Contains(span.Status.Description, secret) {
			t.Errorf("span status description leaks credential %q: %q", secret, span.Status.Description)
		}
		for _, ev := range span.Events {
			for _, attr := range ev.Attributes {
				if v := attr.Value.AsString(); strings.Contains(v, secret) {
					t.Errorf("span event attribute %s leaks credential %q: %q", attr.Key, secret, v)
				}
			}
		}
	}
	if !strings.Contains(span.Status.Description, log.RedactedPlaceholder) {
		t.Errorf("span status description not scrubbed, want %q marker: %q", log.RedactedPlaceholder, span.Status.Description)
	}
}
