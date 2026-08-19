// tracing_nodes_test.go — table-driven success+error span-assertion matrix
// for all 25 tracedNodesService methods.
// Mirrors the TestTracedPoolService_AllMethods_Traced
// pattern: one table entry per method, wiring a single fakeNodesService *Fn
// field, invoking the traced method, and asserting exactly one span with the
// expected name, attributes, and (on the error path) Error status carrying
// the scrubbed message.
package pve

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// nodesMethodCase describes one tracedNodesService method under test.
// makeFake wires the single *Fn field the method needs: retErr nil means
// "succeed", non-nil means "fail with retErr". invoke calls the traced
// method with fixed arguments matching wantAttrs and returns only the error,
// since span assertions never need the response payload.
type nodesMethodCase struct {
	name      string
	wantSpan  string
	wantAttrs map[string]string
	makeFake  func(retErr error) *fakeNodesService
	invoke    func(traced *tracedNodesService) error
}

//nolint:gocognit // Flat test-case table, one entry per traced method; the complexity score is repetition of an identical wire/invoke shape, not branching logic.
func nodesMethodCases() []nodesMethodCase {
	const (
		node        = "pve1"
		vmid        = "100"
		storageName = "local-zfs"
		iface       = "vmbr100"
		ipsetName   = "ipset0"
	)

	return []nodesMethodCase{
		{
			name:      "CreateNetwork",
			wantSpan:  "pve.nodes.create_network",
			wantAttrs: map[string]string{"pve.node": node},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createNetworkFn: func(context.Context, string, *nodes.CreateNetworkParams) error {
					return retErr
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				return traced.CreateNetwork(context.Background(), node, &nodes.CreateNetworkParams{})
			},
		},
		{
			name:      "UpdateNetwork",
			wantSpan:  "pve.nodes.update_network",
			wantAttrs: map[string]string{"pve.node": node},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{updateNetworkFn: func(context.Context, string, *nodes.UpdateNetworkParams) (*nodes.UpdateNetworkResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.UpdateNetworkResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.UpdateNetwork(context.Background(), node, &nodes.UpdateNetworkParams{})
				return err
			},
		},
		{
			name:      "ListQemu",
			wantSpan:  "pve.nodes.list_qemu",
			wantAttrs: map[string]string{"pve.node": node},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listQemuFn: func(context.Context, string, *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListQemuResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListQemu(context.Background(), node, &nodes.ListQemuParams{})
				return err
			},
		},
		{
			name:      "DeleteQemu",
			wantSpan:  "pve.nodes.delete_qemu",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{deleteQemuFn: func(context.Context, string, string, *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.DeleteQemuResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.DeleteQemu(context.Background(), node, vmid, &nodes.DeleteQemuParams{})
				return err
			},
		},
		{
			name:      "CreateQemuAgentExec",
			wantSpan:  "pve.nodes.create_qemu_agent_exec",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createQemuAgentExecFn: func(context.Context, string, string, *nodes.CreateQemuAgentExecParams) (*nodes.CreateQemuAgentExecResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.CreateQemuAgentExecResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.CreateQemuAgentExec(context.Background(), node, vmid, &nodes.CreateQemuAgentExecParams{})
				return err
			},
		},
		{
			name:      "ListQemuAgentExecStatus",
			wantSpan:  "pve.nodes.list_qemu_agent_exec_status",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listQemuAgentExecStatusFn: func(context.Context, string, string, *nodes.ListQemuAgentExecStatusParams) (*nodes.ListQemuAgentExecStatusResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListQemuAgentExecStatusResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListQemuAgentExecStatus(context.Background(), node, vmid, &nodes.ListQemuAgentExecStatusParams{})
				return err
			},
		},
		{
			name:      "ListQemuAgentNetworkGetInterfaces",
			wantSpan:  "pve.nodes.list_qemu_agent_network_get_interfaces",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listQemuAgentNetworkGetInterfacesFn: func(context.Context, string, string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListQemuAgentNetworkGetInterfacesResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListQemuAgentNetworkGetInterfaces(context.Background(), node, vmid)
				return err
			},
		},
		{
			name:      "CreateQemuAgentPing",
			wantSpan:  "pve.nodes.create_qemu_agent_ping",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createQemuAgentPingFn: func(context.Context, string, string) (*nodes.CreateQemuAgentPingResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.CreateQemuAgentPingResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.CreateQemuAgentPing(context.Background(), node, vmid)
				return err
			},
		},
		{
			name:      "CreateQemuClone",
			wantSpan:  "pve.nodes.create_qemu_clone",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createQemuCloneFn: func(context.Context, string, string, *nodes.CreateQemuCloneParams) (*nodes.CreateQemuCloneResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.CreateQemuCloneResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.CreateQemuClone(context.Background(), node, vmid, &nodes.CreateQemuCloneParams{})
				return err
			},
		},
		{
			name:      "UpdateQemuConfig",
			wantSpan:  "pve.nodes.update_qemu_config",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
					return retErr
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				return traced.UpdateQemuConfig(context.Background(), node, vmid, &nodes.UpdateQemuConfigParams{})
			},
		},
		{
			name:      "ListQemuStatusCurrent",
			wantSpan:  "pve.nodes.list_qemu_status_current",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listQemuStatusCurrentFn: func(context.Context, string, string) (*nodes.ListQemuStatusCurrentResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListQemuStatusCurrentResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListQemuStatusCurrent(context.Background(), node, vmid)
				return err
			},
		},
		{
			name:      "CreateQemuStatusReboot",
			wantSpan:  "pve.nodes.create_qemu_status_reboot",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createQemuStatusRebootFn: func(context.Context, string, string, *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.CreateQemuStatusRebootResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.CreateQemuStatusReboot(context.Background(), node, vmid, &nodes.CreateQemuStatusRebootParams{})
				return err
			},
		},
		{
			name:      "CreateQemuTemplate",
			wantSpan:  "pve.nodes.create_qemu_template",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createQemuTemplateFn: func(context.Context, string, string, *nodes.CreateQemuTemplateParams) (*nodes.CreateQemuTemplateResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.CreateQemuTemplateResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.CreateQemuTemplate(context.Background(), node, vmid, &nodes.CreateQemuTemplateParams{})
				return err
			},
		},
		{
			name:      "UpdateQemuUnlink",
			wantSpan:  "pve.nodes.update_qemu_unlink",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{updateQemuUnlinkFn: func(context.Context, string, string, *nodes.UpdateQemuUnlinkParams) error {
					return retErr
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				return traced.UpdateQemuUnlink(context.Background(), node, vmid, &nodes.UpdateQemuUnlinkParams{})
			},
		},
		{
			name:      "ListStorage",
			wantSpan:  "pve.nodes.list_storage",
			wantAttrs: map[string]string{"pve.node": node},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListStorageResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListStorage(context.Background(), node, &nodes.ListStorageParams{})
				return err
			},
		},
		{
			name:      "ListStorageContent",
			wantSpan:  "pve.nodes.list_storage_content",
			wantAttrs: map[string]string{"pve.node": node, "pve.storage": storageName},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listStorageContentFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListStorageContentResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListStorageContent(context.Background(), node, storageName, &nodes.ListStorageContentParams{})
				return err
			},
		},
		{
			name:      "CreateStorageDownloadUrl",
			wantSpan:  "pve.nodes.create_storage_download_url",
			wantAttrs: map[string]string{"pve.node": node, "pve.storage": storageName},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createStorageDownloadURLFn: func(context.Context, string, string, *nodes.CreateStorageDownloadUrlParams) (*nodes.CreateStorageDownloadUrlResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.CreateStorageDownloadUrlResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.CreateStorageDownloadUrl(context.Background(), node, storageName, &nodes.CreateStorageDownloadUrlParams{})
				return err
			},
		},
		{
			name:      "ListVersion",
			wantSpan:  "pve.nodes.list_version",
			wantAttrs: map[string]string{"pve.node": node},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listVersionFn: func(context.Context, string) (*nodes.ListVersionResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListVersionResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListVersion(context.Background(), node)
				return err
			},
		},
		{
			name:      "ListNetwork",
			wantSpan:  "pve.nodes.list_network",
			wantAttrs: map[string]string{"pve.node": node},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listNetworkFn: func(context.Context, string, *nodes.ListNetworkParams) (*nodes.ListNetworkResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListNetworkResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListNetwork(context.Background(), node, &nodes.ListNetworkParams{})
				return err
			},
		},
		{
			name:      "DeleteNetwork2",
			wantSpan:  "pve.nodes.delete_network2",
			wantAttrs: map[string]string{"pve.node": node},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{deleteNetwork2Fn: func(context.Context, string, string) error {
					return retErr
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				return traced.DeleteNetwork2(context.Background(), node, iface)
			},
		},
		{
			name:      "ListHardwarePci",
			wantSpan:  "pve.nodes.list_hardware_pci",
			wantAttrs: map[string]string{"pve.node": node},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{listHardwarePciFn: func(context.Context, string, *nodes.ListHardwarePciParams) (*nodes.ListHardwarePciResponse, error) {
					if retErr != nil {
						return nil, retErr
					}
					return &nodes.ListHardwarePciResponse{}, nil
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				_, err := traced.ListHardwarePci(context.Background(), node, &nodes.ListHardwarePciParams{})
				return err
			},
		},
		{
			name:      "CreateQemuFirewallIpset",
			wantSpan:  "pve.nodes.create_qemu_firewall_ipset",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createQemuFirewallIpsetFn: func(context.Context, string, string, *nodes.CreateQemuFirewallIpsetParams) error {
					return retErr
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				return traced.CreateQemuFirewallIpset(context.Background(), node, vmid, &nodes.CreateQemuFirewallIpsetParams{})
			},
		},
		{
			name:      "CreateQemuFirewallIpset2",
			wantSpan:  "pve.nodes.create_qemu_firewall_ipset2",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createQemuFirewallIpset2Fn: func(context.Context, string, string, string, *nodes.CreateQemuFirewallIpset2Params) error {
					return retErr
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				return traced.CreateQemuFirewallIpset2(context.Background(), node, vmid, ipsetName, &nodes.CreateQemuFirewallIpset2Params{})
			},
		},
		{
			name:      "UpdateQemuFirewallOptions",
			wantSpan:  "pve.nodes.update_qemu_firewall_options",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{updateQemuFirewallOptionsFn: func(context.Context, string, string, *nodes.UpdateQemuFirewallOptionsParams) error {
					return retErr
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				return traced.UpdateQemuFirewallOptions(context.Background(), node, vmid, &nodes.UpdateQemuFirewallOptionsParams{})
			},
		},
		{
			name:      "CreateQemuFirewallRules",
			wantSpan:  "pve.nodes.create_qemu_firewall_rules",
			wantAttrs: map[string]string{"pve.node": node, "pve.vmid": vmid},
			makeFake: func(retErr error) *fakeNodesService {
				return &fakeNodesService{createQemuFirewallRulesFn: func(context.Context, string, string, *nodes.CreateQemuFirewallRulesParams) error {
					return retErr
				}}
			},
			invoke: func(traced *tracedNodesService) error {
				return traced.CreateQemuFirewallRules(context.Background(), node, vmid, &nodes.CreateQemuFirewallRulesParams{})
			},
		},
	}
}

func TestTracedNodesService_AllRemainingMethods_Traced(t *testing.T) {
	t.Parallel()

	for _, tc := range nodesMethodCases() {
		t.Run(tc.name+"_success", func(t *testing.T) {
			t.Parallel()
			tracer, exporter := newTestTracer(t)
			traced := &tracedNodesService{Service: tc.makeFake(nil), tracer: tracer}

			if err := tc.invoke(traced); err != nil {
				t.Fatalf("%s returned err=%v, want nil", tc.name, err)
			}

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d exported spans, want 1", len(spans))
			}
			span := spans[0]
			if span.Name != tc.wantSpan {
				t.Fatalf("span name = %q, want %q", span.Name, tc.wantSpan)
			}
			if span.Status.Code == codes.Error {
				t.Errorf("success span carries Error status: %+v", span.Status)
			}
			assertNodesSpanAttrs(t, span.Attributes, tc.wantAttrs)
		})

		t.Run(tc.name+"_error", func(t *testing.T) {
			t.Parallel()
			tracer, exporter := newTestTracer(t)
			wantErr := errors.New(tc.name + " failed")
			traced := &tracedNodesService{Service: tc.makeFake(wantErr), tracer: tracer}

			if err := tc.invoke(traced); !errors.Is(err, wantErr) {
				t.Fatalf("%s returned err=%v, want %v", tc.name, err, wantErr)
			}

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d exported spans, want 1", len(spans))
			}
			span := spans[0]
			if span.Name != tc.wantSpan {
				t.Fatalf("span name = %q, want %q", span.Name, tc.wantSpan)
			}
			if span.Status.Code != codes.Error {
				t.Fatalf("error span status code = %v, want Error", span.Status.Code)
			}
			if span.Status.Description != wantErr.Error() {
				t.Errorf("span status description = %q, want %q (scrubbing must not alter a message with no embedded credentials)",
					span.Status.Description, wantErr.Error())
			}
			assertNodesSpanAttrs(t, span.Attributes, tc.wantAttrs)
		})
	}
}

// assertNodesSpanAttrs fails the test if span attrs do not contain every
// key/value pair in want as a string-valued attribute.
func assertNodesSpanAttrs(t *testing.T, got []attribute.KeyValue, want map[string]string) {
	t.Helper()
	for wantKey, wantVal := range want {
		found := false
		for _, kv := range got {
			if string(kv.Key) == wantKey {
				found = true
				if kv.Value.AsString() != wantVal {
					t.Errorf("attribute %s = %q, want %q", wantKey, kv.Value.AsString(), wantVal)
				}
			}
		}
		if !found {
			t.Errorf("span missing attribute %s (want value %q); got attrs: %v", wantKey, wantVal, got)
		}
	}
}
