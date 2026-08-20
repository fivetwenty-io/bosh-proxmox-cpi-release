package pve

// This file wraps the PVE SDK service handles the CPI actually calls with a
// tracing decorator: one struct per service, embedding the raw SDK interface
// and overriding only the methods CPI code calls (see NewClientWithTracer in
// client.go for the wiring point). The rule is: overridden = on the actual
// production call surface (found by grepping every call site, including
// indirection through a local var such as `svc := c.Cluster()` — a plain
// `.Cluster().X(` grep alone UNDERCOUNTS this); everything else (ACME, apt,
// and the handful of other genuinely-uncalled generated methods) passes
// through unoverridden via Go's interface embedding. HA and SDN methods on
// Cluster(), and the firewall/PCI/network methods on Nodes(), ARE called in
// production (placement_*.go, sdn.go, create_network.go, create_vm_firewall.go,
// create_vm_pci.go, create_vm_vip.go, delete_network.go) and ARE overridden
// below — do not assume an unfamiliar service name is uncalled without
// grepping for local-var indirection first.
//
// Each override follows the same shape: start a child span named
// "pve.<service>.<method>" from the ctx handed in by the caller (so it
// parents correctly under whatever root span the dispatcher started),
// attach node/vmid/storage attributes when the method signature carries
// them, call through to the embedded raw service, and record an error
// status on the span when the call fails. finishSpan centralizes the
// record-and-end step so each override is a two-line wrapper around the
// real call.
//
// CloudInit has zero production call sites (grep of internal/ and cmd/
// confirms nothing calls Client.CloudInit()), so no decorator exists for it
// — writing one would be dead code with no caller to exercise it.

import (
	"context"
	"errors"
	"io"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// finishSpan records err (if non-nil) as an error status on span, then ends
// the span. Called via defer with err bound through a named return so it
// captures the call's final error on every return path.
//
// The error text is scrubbed before it leaves the process: PVE-returned
// errors can embed token-bearing URLs (userinfo, presigned query params),
// and handlers scrub exactly these values before logging them — the span
// exporter is one more external sink and must not bypass that scrubbing.
func finishSpan(span trace.Span, err error) {
	if err != nil {
		msg := log.ScrubMessage(err.Error())
		span.RecordError(errors.New(msg))
		span.SetStatus(codes.Error, msg)
	}
	span.End()
}

// -----------------------------------------------------------------------
// QEMU (12 overridden methods: Config, Stop, DetachDisk, Create, ResizeDisk,
// AttachDisk, Start, DeleteSnapshot, ListSnapshots, Status, Reset, Snapshot)
// -----------------------------------------------------------------------

type tracedQEMUService struct {
	qemu.Service
	tracer trace.Tracer
}

func (t *tracedQEMUService) Create(ctx context.Context, node string, params map[string]interface{}) (upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.create", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.Create(ctx, node, params)
}

func (t *tracedQEMUService) Config(ctx context.Context, node string, vmid int) (cfg map[string]interface{}, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.config", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.Config(ctx, node, vmid)
}

func (t *tracedQEMUService) Status(ctx context.Context, node string, vmid int) (status map[string]interface{}, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.status", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.Status(ctx, node, vmid)
}

func (t *tracedQEMUService) Start(ctx context.Context, node string, vmid int) (upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.start", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.Start(ctx, node, vmid)
}

func (t *tracedQEMUService) Stop(ctx context.Context, node string, vmid int) (upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.stop", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.Stop(ctx, node, vmid)
}

func (t *tracedQEMUService) Reset(ctx context.Context, node string, vmid int) (upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.reset", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.Reset(ctx, node, vmid)
}

func (t *tracedQEMUService) AttachDisk(ctx context.Context, node string, vmid int, volid string, bus string, opts *qemu.AttachOpts) (diskID string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.attach_disk", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.AttachDisk(ctx, node, vmid, volid, bus, opts)
}

func (t *tracedQEMUService) DetachDisk(ctx context.Context, node string, vmid int, diskID string) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.detach_disk", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.DetachDisk(ctx, node, vmid, diskID)
}

func (t *tracedQEMUService) ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.resize_disk", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.ResizeDisk(ctx, node, vmid, diskID, sizeGiB)
}

func (t *tracedQEMUService) Snapshot(ctx context.Context, node string, vmid int, name string, opts map[string]interface{}) (upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.snapshot", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.Snapshot(ctx, node, vmid, name, opts)
}

func (t *tracedQEMUService) DeleteSnapshot(ctx context.Context, node string, vmid int, name string) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.delete_snapshot", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteSnapshot(ctx, node, vmid, name)
}

func (t *tracedQEMUService) ListSnapshots(ctx context.Context, node string, vmid int) (snaps []map[string]interface{}, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.qemu.list_snapshots", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListSnapshots(ctx, node, vmid)
}

// -----------------------------------------------------------------------
// Storage (6 overridden methods: CreateVolume, Exists, DeleteVolumeAsync,
// DeleteVolumeIfExists, DeleteVolumeIfExistsAsync, Upload)
// -----------------------------------------------------------------------

type tracedStorageService struct {
	storage.Service
	tracer trace.Tracer
}

func (t *tracedStorageService) CreateVolume(ctx context.Context, node, storageName string, sizeGiB int, format string, vmid int, name string) (volid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.storage.create_volume", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.storage", storageName), attribute.String("pve.vmid", strconv.Itoa(vmid))))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateVolume(ctx, node, storageName, sizeGiB, format, vmid, name)
}

func (t *tracedStorageService) Exists(ctx context.Context, node, storageName, volume string) (exists bool, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.storage.exists", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.storage", storageName)))
	defer func() { finishSpan(span, err) }()
	return t.Service.Exists(ctx, node, storageName, volume)
}

func (t *tracedStorageService) DeleteVolumeAsync(ctx context.Context, node, storageName, volume string) (upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.storage.delete_volume_async", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.storage", storageName)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteVolumeAsync(ctx, node, storageName, volume)
}

func (t *tracedStorageService) DeleteVolumeIfExists(ctx context.Context, node, storageName, volume string) (existed bool, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.storage.delete_volume_if_exists", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.storage", storageName)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteVolumeIfExists(ctx, node, storageName, volume)
}

func (t *tracedStorageService) DeleteVolumeIfExistsAsync(ctx context.Context, node, storageName, volume string) (existed bool, upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.storage.delete_volume_if_exists_async", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.storage", storageName)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteVolumeIfExistsAsync(ctx, node, storageName, volume)
}

func (t *tracedStorageService) Upload(ctx context.Context, node, storageName, content, filename string, body io.Reader) (upid string, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.storage.upload", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.storage", storageName)))
	defer func() { finishSpan(span, err) }()
	return t.Service.Upload(ctx, node, storageName, content, filename, body)
}

// -----------------------------------------------------------------------
// Tasks (2 overridden methods: Wait, GetStatus)
// -----------------------------------------------------------------------

type tracedTasksService struct {
	tasks.Service
	tracer trace.Tracer
}

func (t *tracedTasksService) Wait(ctx context.Context, node, upid string, opts *tasks.WaitOptions) (status *tasks.Status, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.tasks.wait", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.upid", upid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.Wait(ctx, node, upid, opts)
}

func (t *tracedTasksService) GetStatus(ctx context.Context, node, upid string) (status *tasks.Status, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.tasks.get_status", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.upid", upid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.GetStatus(ctx, node, upid)
}

// -----------------------------------------------------------------------
// Nodes (25 overridden methods: 18 QEMU-lifecycle/storage/version methods
// plus 7 network/firewall/PCI methods reached only through local-var
// indirection — e.g. create_vm_firewall.go's `nodeSvc := deps.PVE.Nodes()`
// then `nodeSvc.CreateQemuFirewallRules(...)` — which a naive
// `.Nodes().X(` grep misses. The generated nodes.Service interface has 60+
// methods covering ACME/SDN/HA/apt/etc.; only the methods CPI code actually
// calls are overridden here, per the call-surface audit.)
// -----------------------------------------------------------------------

type tracedNodesService struct {
	nodes.Service
	tracer trace.Tracer
}

func (t *tracedNodesService) CreateNetwork(ctx context.Context, node string, params *nodes.CreateNetworkParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_network", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateNetwork(ctx, node, params)
}

func (t *tracedNodesService) UpdateNetwork(ctx context.Context, node string, params *nodes.UpdateNetworkParams) (resp *nodes.UpdateNetworkResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.update_network", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.UpdateNetwork(ctx, node, params)
}

func (t *tracedNodesService) ListQemu(ctx context.Context, node string, params *nodes.ListQemuParams) (resp *nodes.ListQemuResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_qemu", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListQemu(ctx, node, params)
}

func (t *tracedNodesService) DeleteQemu(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (resp *nodes.DeleteQemuResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.delete_qemu", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteQemu(ctx, node, vmid, params)
}

func (t *tracedNodesService) CreateQemuAgentExec(ctx context.Context, node string, vmid string, params *nodes.CreateQemuAgentExecParams) (resp *nodes.CreateQemuAgentExecResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_qemu_agent_exec", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateQemuAgentExec(ctx, node, vmid, params)
}

func (t *tracedNodesService) ListQemuAgentExecStatus(ctx context.Context, node string, vmid string, params *nodes.ListQemuAgentExecStatusParams) (resp *nodes.ListQemuAgentExecStatusResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_qemu_agent_exec_status", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListQemuAgentExecStatus(ctx, node, vmid, params)
}

func (t *tracedNodesService) ListQemuAgentNetworkGetInterfaces(ctx context.Context, node string, vmid string) (resp *nodes.ListQemuAgentNetworkGetInterfacesResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_qemu_agent_network_get_interfaces", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListQemuAgentNetworkGetInterfaces(ctx, node, vmid)
}

func (t *tracedNodesService) CreateQemuAgentPing(ctx context.Context, node string, vmid string) (resp *nodes.CreateQemuAgentPingResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_qemu_agent_ping", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateQemuAgentPing(ctx, node, vmid)
}

func (t *tracedNodesService) CreateQemuClone(ctx context.Context, node string, vmid string, params *nodes.CreateQemuCloneParams) (resp *nodes.CreateQemuCloneResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_qemu_clone", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateQemuClone(ctx, node, vmid, params)
}

func (t *tracedNodesService) UpdateQemuConfig(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.update_qemu_config", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.UpdateQemuConfig(ctx, node, vmid, params)
}

func (t *tracedNodesService) ListQemuStatusCurrent(ctx context.Context, node string, vmid string) (resp *nodes.ListQemuStatusCurrentResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_qemu_status_current", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListQemuStatusCurrent(ctx, node, vmid)
}

func (t *tracedNodesService) CreateQemuStatusReboot(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusRebootParams) (resp *nodes.CreateQemuStatusRebootResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_qemu_status_reboot", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateQemuStatusReboot(ctx, node, vmid, params)
}

func (t *tracedNodesService) CreateQemuTemplate(ctx context.Context, node string, vmid string, params *nodes.CreateQemuTemplateParams) (resp *nodes.CreateQemuTemplateResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_qemu_template", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateQemuTemplate(ctx, node, vmid, params)
}

func (t *tracedNodesService) UpdateQemuUnlink(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuUnlinkParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.update_qemu_unlink", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.UpdateQemuUnlink(ctx, node, vmid, params)
}

func (t *tracedNodesService) ListStorage(ctx context.Context, node string, params *nodes.ListStorageParams) (resp *nodes.ListStorageResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_storage", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListStorage(ctx, node, params)
}

func (t *tracedNodesService) ListStorageContent(ctx context.Context, node string, storageName string, params *nodes.ListStorageContentParams) (resp *nodes.ListStorageContentResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_storage_content", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.storage", storageName)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListStorageContent(ctx, node, storageName, params)
}

//nolint:revive // method name must match the SDK interface (SDK uses Url not URL)
func (t *tracedNodesService) CreateStorageDownloadUrl(ctx context.Context, node string, storageName string, params *nodes.CreateStorageDownloadUrlParams) (resp *nodes.CreateStorageDownloadUrlResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_storage_download_url", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.storage", storageName)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateStorageDownloadUrl(ctx, node, storageName, params)
}

func (t *tracedNodesService) ListVersion(ctx context.Context, node string) (resp *nodes.ListVersionResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_version", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListVersion(ctx, node)
}

func (t *tracedNodesService) ListNetwork(ctx context.Context, node string, params *nodes.ListNetworkParams) (resp *nodes.ListNetworkResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_network", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListNetwork(ctx, node, params)
}

func (t *tracedNodesService) DeleteNetwork2(ctx context.Context, node string, iface string) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.delete_network2", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteNetwork2(ctx, node, iface)
}

func (t *tracedNodesService) ListHardwarePci(ctx context.Context, node string, params *nodes.ListHardwarePciParams) (resp *nodes.ListHardwarePciResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.list_hardware_pci", trace.WithAttributes(attribute.String("pve.node", node)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListHardwarePci(ctx, node, params)
}

func (t *tracedNodesService) CreateQemuFirewallIpset(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallIpsetParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_qemu_firewall_ipset", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateQemuFirewallIpset(ctx, node, vmid, params)
}

func (t *tracedNodesService) CreateQemuFirewallIpset2(ctx context.Context, node string, vmid string, name string, params *nodes.CreateQemuFirewallIpset2Params) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_qemu_firewall_ipset2", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateQemuFirewallIpset2(ctx, node, vmid, name, params)
}

func (t *tracedNodesService) CreateQemuFirewallRules(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallRulesParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.create_qemu_firewall_rules", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateQemuFirewallRules(ctx, node, vmid, params)
}

func (t *tracedNodesService) UpdateQemuFirewallOptions(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuFirewallOptionsParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.nodes.update_qemu_firewall_options", trace.WithAttributes(
		attribute.String("pve.node", node), attribute.String("pve.vmid", vmid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.UpdateQemuFirewallOptions(ctx, node, vmid, params)
}

// -----------------------------------------------------------------------
// Cluster (24 overridden methods: 4 original cluster-wide read methods
// plus 12 SDN + 8 HA/Options methods reached only through local-var
// indirection — e.g. sdn.go's `svc := c.Cluster()` then `svc.CreateSdnZones(...)`
// or placement_dlb.go's `svc := deps.PVE.Cluster()` then `svc.UpdateOptions(...)`
// — which a naive `.Cluster().X(` grep misses. All cluster-wide calls: no
// node/vmid in most signatures, zone/vnet/sid/rule names attached where the
// signature carries them.)
// -----------------------------------------------------------------------

type tracedClusterService struct {
	cluster.Service
	tracer trace.Tracer
}

func (t *tracedClusterService) ListResources(ctx context.Context, params *cluster.ListResourcesParams) (resp *cluster.ListResourcesResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_resources")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListResources(ctx, params)
}

func (t *tracedClusterService) ListStatus(ctx context.Context) (resp *cluster.ListStatusResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_status")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListStatus(ctx)
}

func (t *tracedClusterService) ListFirewallGroups(ctx context.Context) (resp *cluster.ListFirewallGroupsResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_firewall_groups")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListFirewallGroups(ctx)
}

func (t *tracedClusterService) ListConfigNodes(ctx context.Context) (resp *cluster.ListConfigNodesResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_config_nodes")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListConfigNodes(ctx)
}

// --- SDN zones ---

func (t *tracedClusterService) ListSdnZones(ctx context.Context, params *cluster.ListSdnZonesParams) (resp *cluster.ListSdnZonesResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_sdn_zones")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListSdnZones(ctx, params)
}

func (t *tracedClusterService) GetSdnZones(ctx context.Context, zone string, params *cluster.GetSdnZonesParams) (resp *cluster.GetSdnZonesResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.get_sdn_zones", trace.WithAttributes(attribute.String("pve.sdn_zone", zone)))
	defer func() { finishSpan(span, err) }()
	return t.Service.GetSdnZones(ctx, zone, params)
}

func (t *tracedClusterService) CreateSdnZones(ctx context.Context, params *cluster.CreateSdnZonesParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.create_sdn_zones")
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateSdnZones(ctx, params)
}

func (t *tracedClusterService) DeleteSdnZones(ctx context.Context, zone string, params *cluster.DeleteSdnZonesParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.delete_sdn_zones", trace.WithAttributes(attribute.String("pve.sdn_zone", zone)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteSdnZones(ctx, zone, params)
}

// --- SDN vnets ---

func (t *tracedClusterService) ListSdnVnets(ctx context.Context, params *cluster.ListSdnVnetsParams) (resp *cluster.ListSdnVnetsResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_sdn_vnets")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListSdnVnets(ctx, params)
}

func (t *tracedClusterService) GetSdnVnets(ctx context.Context, vnet string, params *cluster.GetSdnVnetsParams) (resp *cluster.GetSdnVnetsResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.get_sdn_vnets", trace.WithAttributes(attribute.String("pve.sdn_vnet", vnet)))
	defer func() { finishSpan(span, err) }()
	return t.Service.GetSdnVnets(ctx, vnet, params)
}

func (t *tracedClusterService) CreateSdnVnets(ctx context.Context, params *cluster.CreateSdnVnetsParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.create_sdn_vnets")
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateSdnVnets(ctx, params)
}

func (t *tracedClusterService) DeleteSdnVnets(ctx context.Context, vnet string, params *cluster.DeleteSdnVnetsParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.delete_sdn_vnets", trace.WithAttributes(attribute.String("pve.sdn_vnet", vnet)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteSdnVnets(ctx, vnet, params)
}

// --- SDN vnet subnets ---

func (t *tracedClusterService) ListSdnVnetsSubnets(ctx context.Context, vnet string, params *cluster.ListSdnVnetsSubnetsParams) (resp *cluster.ListSdnVnetsSubnetsResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_sdn_vnets_subnets", trace.WithAttributes(attribute.String("pve.sdn_vnet", vnet)))
	defer func() { finishSpan(span, err) }()
	return t.Service.ListSdnVnetsSubnets(ctx, vnet, params)
}

func (t *tracedClusterService) CreateSdnVnetsSubnets(ctx context.Context, vnet string, params *cluster.CreateSdnVnetsSubnetsParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.create_sdn_vnets_subnets", trace.WithAttributes(attribute.String("pve.sdn_vnet", vnet)))
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateSdnVnetsSubnets(ctx, vnet, params)
}

func (t *tracedClusterService) DeleteSdnVnetsSubnets(ctx context.Context, vnet string, subnet string, params *cluster.DeleteSdnVnetsSubnetsParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.delete_sdn_vnets_subnets", trace.WithAttributes(
		attribute.String("pve.sdn_vnet", vnet), attribute.String("pve.sdn_subnet", subnet)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteSdnVnetsSubnets(ctx, vnet, subnet, params)
}

// --- SDN apply ---

func (t *tracedClusterService) UpdateSdn(ctx context.Context, params *cluster.UpdateSdnParams) (resp *cluster.UpdateSdnResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.update_sdn")
	defer func() { finishSpan(span, err) }()
	return t.Service.UpdateSdn(ctx, params)
}

// --- HA resources/rules ---

func (t *tracedClusterService) CreateHaResources(ctx context.Context, params *cluster.CreateHaResourcesParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.create_ha_resources")
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateHaResources(ctx, params)
}

func (t *tracedClusterService) UpdateHaResources(ctx context.Context, sid string, params *cluster.UpdateHaResourcesParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.update_ha_resources", trace.WithAttributes(attribute.String("pve.ha_sid", sid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.UpdateHaResources(ctx, sid, params)
}

func (t *tracedClusterService) DeleteHaResources(ctx context.Context, sid string, params *cluster.DeleteHaResourcesParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.delete_ha_resources", trace.WithAttributes(attribute.String("pve.ha_sid", sid)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteHaResources(ctx, sid, params)
}

func (t *tracedClusterService) ListHaRules(ctx context.Context, params *cluster.ListHaRulesParams) (resp *cluster.ListHaRulesResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_ha_rules")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListHaRules(ctx, params)
}

func (t *tracedClusterService) CreateHaRules(ctx context.Context, params *cluster.CreateHaRulesParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.create_ha_rules")
	defer func() { finishSpan(span, err) }()
	return t.Service.CreateHaRules(ctx, params)
}

func (t *tracedClusterService) DeleteHaRules(ctx context.Context, rule string) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.delete_ha_rules", trace.WithAttributes(attribute.String("pve.ha_rule", rule)))
	defer func() { finishSpan(span, err) }()
	return t.Service.DeleteHaRules(ctx, rule)
}

// --- Cluster options (DLB placement reads/updates the datacenter.cfg
// options blob to persist a chosen scheduler mode) ---

func (t *tracedClusterService) ListOptions(ctx context.Context) (resp *cluster.ListOptionsResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.list_options")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListOptions(ctx)
}

func (t *tracedClusterService) UpdateOptions(ctx context.Context, params *cluster.UpdateOptionsParams) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.cluster.update_options")
	defer func() { finishSpan(span, err) }()
	return t.Service.UpdateOptions(ctx, params)
}

// -----------------------------------------------------------------------
// ClusterStorage (1 overridden method: ListStorage)
// -----------------------------------------------------------------------

type tracedClusterStorageService struct {
	clusterstorage.Service
	tracer trace.Tracer
}

func (t *tracedClusterStorageService) ListStorage(ctx context.Context, params *clusterstorage.ListStorageParams) (resp *clusterstorage.ListStorageResponse, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.clusterstorage.list_storage")
	defer func() { finishSpan(span, err) }()
	return t.Service.ListStorage(ctx, params)
}

// -----------------------------------------------------------------------
// Pools (5 overridden methods: AddVM, CreatePool, DeletePool,
// GetPoolComment, MoveVMToPool — decorates the CPI-owned PoolService
// interface the same way the 6 SDK-generated services above are decorated,
// for one consistent span-naming convention across every PVE service call)
// -----------------------------------------------------------------------

type tracedPoolService struct {
	PoolService
	tracer trace.Tracer
}

func (t *tracedPoolService) AddVM(ctx context.Context, poolID string, vmid int64) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.pools.add_vm", trace.WithAttributes(
		attribute.String("pve.pool_id", poolID), attribute.String("pve.vmid", strconv.FormatInt(vmid, 10))))
	defer func() { finishSpan(span, err) }()
	return t.PoolService.AddVM(ctx, poolID, vmid)
}

func (t *tracedPoolService) MoveVMToPool(ctx context.Context, poolID string, vmid int64) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.pools.move_vm_to_pool", trace.WithAttributes(
		attribute.String("pve.pool_id", poolID), attribute.String("pve.vmid", strconv.FormatInt(vmid, 10))))
	defer func() { finishSpan(span, err) }()
	return t.PoolService.MoveVMToPool(ctx, poolID, vmid)
}

func (t *tracedPoolService) CreatePool(ctx context.Context, poolID, comment string) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.pools.create_pool", trace.WithAttributes(attribute.String("pve.pool_id", poolID)))
	defer func() { finishSpan(span, err) }()
	return t.PoolService.CreatePool(ctx, poolID, comment)
}

func (t *tracedPoolService) DeletePool(ctx context.Context, poolID string) (err error) {
	ctx, span := t.tracer.Start(ctx, "pve.pools.delete_pool", trace.WithAttributes(attribute.String("pve.pool_id", poolID)))
	defer func() { finishSpan(span, err) }()
	return t.PoolService.DeletePool(ctx, poolID)
}

func (t *tracedPoolService) GetPoolComment(ctx context.Context, poolID string) (comment string, found bool, err error) {
	ctx, span := t.tracer.Start(ctx, "pve.pools.get_pool_comment", trace.WithAttributes(attribute.String("pve.pool_id", poolID)))
	defer func() { finishSpan(span, err) }()
	return t.PoolService.GetPoolComment(ctx, poolID)
}
