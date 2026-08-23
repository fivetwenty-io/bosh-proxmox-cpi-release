// Client interface composed of per-domain service handles, mockable for tests.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/version"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/pools"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// PoolService manages PVE resource pool membership.
// PVE resource pools group VMs and containers for ACL/billing purposes.
// Besides VM-to-pool membership, the CPI uses create/delete/read-comment of a
// dedicated sentinel pool as a cluster-wide advisory mutex (see cluster_lock.go).
type PoolService interface {
	// AddVM assigns vmid to the resource pool identified by poolID.
	// Corresponds to PUT /pools/{poolid} with vms=[vmid].
	// Returns an error when the pool does not exist or the PVE API rejects the call.
	AddVM(ctx context.Context, poolID string, vmid int64) error

	// CreatePool creates a resource pool named poolID with the given comment.
	// Corresponds to POST /pools. PVE rejects a duplicate poolid with a 4xx
	// error (pmxcfs serializes user.cfg), which the cluster-lock primitive
	// treats as "lock already held". An empty comment is sent as-is.
	CreatePool(ctx context.Context, poolID, comment string) error

	// DeletePool removes the resource pool named poolID. Corresponds to
	// DELETE /pools?poolid=<poolID>. A not-found pool is reported via the
	// returned error; the cluster-lock release path treats not-found as success.
	DeletePool(ctx context.Context, poolID string) error

	// GetPoolComment returns the comment string stored on poolID. Corresponds to
	// GET /pools?poolid=<poolID>. found is false (with a nil error) when the pool
	// does not exist. A nil/absent comment is returned as the empty string.
	GetPoolComment(ctx context.Context, poolID string) (comment string, found bool, err error)

	// MoveVMToPool assigns vmid to poolID even when the guest already belongs
	// to another pool: PVE removes it from its current pool and adds it to
	// this one. Corresponds to PUT /pools/{poolid} with vms=[vmid] and
	// allow-move=1 — without allow-move, PVE rejects adding a guest that is
	// already a pool member, which is why AddVM alone cannot reconcile
	// membership. Returns an error when the pool does not exist or the PVE
	// API rejects the call.
	MoveVMToPool(ctx context.Context, poolID string, vmid int64) error

	// PoolHasVM reports whether vmid is currently a member of poolID, read
	// from GET /pools/{poolid}: the pmxcfs-backed pool object itself, not
	// the lagging /cluster/resources index. A pool that does not exist
	// reports (false, nil). Used to disambiguate PVE's "already a pool
	// member" verdict, which does not name the pool.
	PoolHasVM(ctx context.Context, poolID string, vmid int64) (bool, error)
}

// Client wraps SDK services for mockability.
type Client interface {
	QEMU() qemu.Service
	Storage() storage.Service
	CloudInit() cloudinit.Service
	Tasks() tasks.Service
	Nodes() nodes.Service
	Cluster() cluster.Service
	// ClusterStorage returns the cluster-level storage service used to list
	// all storages across the cluster (StorageLister implementation for
	// StorageInfoCache).
	ClusterStorage() clusterstorage.Service
	// Pools returns the resource pool service for pool membership management.
	Pools() PoolService
}

// sdkClient is the concrete implementation returned by NewClient.
type sdkClient struct {
	qemuSvc           qemu.Service
	storageSvc        storage.Service
	cloudInitSvc      cloudinit.Service
	tasksSvc          tasks.Service
	nodesSvc          nodes.Service
	clusterSvc        cluster.Service
	clusterStorageSvc clusterstorage.Service
	poolsSvc          PoolService
}

func (c *sdkClient) QEMU() qemu.Service                     { return c.qemuSvc }
func (c *sdkClient) Storage() storage.Service               { return c.storageSvc }
func (c *sdkClient) CloudInit() cloudinit.Service           { return c.cloudInitSvc }
func (c *sdkClient) Tasks() tasks.Service                   { return c.tasksSvc }
func (c *sdkClient) Nodes() nodes.Service                   { return c.nodesSvc }
func (c *sdkClient) Cluster() cluster.Service               { return c.clusterSvc }
func (c *sdkClient) ClusterStorage() clusterstorage.Service { return c.clusterStorageSvc }
func (c *sdkClient) Pools() PoolService                     { return c.poolsSvc }

// sdkPoolService implements PoolService using the typed pools binding.
// PVE resource pool membership is managed via PUT /pools/{poolid} with a body
// containing vms=[vmid] (pools.Service.UpdatePools2).
type sdkPoolService struct {
	svc pools.Service
}

// AddVM assigns vmid to the named PVE resource pool via PUT /pools/{poolid}.
// Input validation: poolID empty → error. vmid <= 0 → error.
// PVE API error (pool not found, auth failure, etc.) → wrapped error returned.
func (s *sdkPoolService) AddVM(ctx context.Context, poolID string, vmid int64) error {
	if poolID == "" {
		return cpierrors.Cloud("PoolService.AddVM: poolID must not be empty")
	}
	if vmid <= 0 {
		return cpierrors.Cloud("PoolService.AddVM: vmid must be a positive integer, got %d", vmid)
	}
	vms := fmt.Sprintf("%d", vmid)
	if err := s.svc.UpdatePools2(ctx, poolID, &pools.UpdatePools2Params{Vms: &vms}); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("PoolService.AddVM: assign vmid %d to pool %q", vmid, poolID))
	}
	return nil
}

// MoveVMToPool assigns vmid to the named PVE resource pool with allow-move=1,
// so a guest already in another pool is moved rather than rejected.
// Input validation: poolID empty → error. vmid <= 0 → error.
// PVE API error (pool not found, auth failure, etc.) → wrapped error returned.
func (s *sdkPoolService) MoveVMToPool(ctx context.Context, poolID string, vmid int64) error {
	if poolID == "" {
		return cpierrors.Cloud("PoolService.MoveVMToPool: poolID must not be empty")
	}
	if vmid <= 0 {
		return cpierrors.Cloud("PoolService.MoveVMToPool: vmid must be a positive integer, got %d", vmid)
	}
	vms := fmt.Sprintf("%d", vmid)
	allowMove := true
	if err := s.svc.UpdatePools2(ctx, poolID, &pools.UpdatePools2Params{Vms: &vms, AllowMove: &allowMove}); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("PoolService.MoveVMToPool: move vmid %d to pool %q", vmid, poolID))
	}
	return nil
}

// CreatePool creates a resource pool via POST /pools. The raw SDK error is
// returned unwrapped so callers (cluster_lock.go) can classify duplicate-pool
// 4xx responses without a wrapper hiding the HTTP status.
func (s *sdkPoolService) CreatePool(ctx context.Context, poolID, comment string) error {
	if poolID == "" {
		return cpierrors.Cloud("PoolService.CreatePool: poolID must not be empty")
	}
	params := &pools.CreatePoolsParams{Poolid: poolID}
	if comment != "" {
		params.Comment = &comment
	}
	return s.svc.CreatePools(ctx, params)
}

// DeletePool removes a resource pool via DELETE /pools?poolid=<poolID>. The raw
// SDK error is returned unwrapped so callers can classify not-found responses.
func (s *sdkPoolService) DeletePool(ctx context.Context, poolID string) error {
	if poolID == "" {
		return cpierrors.Cloud("PoolService.DeletePool: poolID must not be empty")
	}
	return s.svc.DeletePools(ctx, &pools.DeletePoolsParams{Poolid: poolID})
}

// GetPoolComment reads a pool's comment via GET /pools/{poolid}. This uses the
// single-object endpoint (not the list endpoint) so the response is decoded into
// the correct shape: {comment, members:[...]}. A pool that does not exist yields
// ("", false, nil) so the caller can distinguish "absent" from "present with
// empty comment". Any other API error propagates.
func (s *sdkPoolService) GetPoolComment(ctx context.Context, poolID string) (string, bool, error) {
	if poolID == "" {
		return "", false, cpierrors.Cloud("PoolService.GetPoolComment: poolID must not be empty")
	}
	resp, err := s.svc.GetPools(ctx, poolID, nil)
	if err != nil {
		// A not-found response means the pool does not exist — caller treats as absent.
		if isPoolNotFound(err) {
			return "", false, nil
		}
		return "", false, cpierrors.Wrap(err, fmt.Sprintf("PoolService.GetPoolComment: read pool %q", poolID))
	}
	if resp == nil {
		// Should not happen per SDK contract, but guard defensively.
		return "", false, nil
	}
	if resp.Comment == nil {
		return "", true, nil
	}
	return *resp.Comment, true, nil
}

// PoolHasVM implements PoolService.PoolHasVM via GET /pools/{poolid},
// decoding the members list and matching on vmid. See the interface doc.
func (s *sdkPoolService) PoolHasVM(ctx context.Context, poolID string, vmid int64) (bool, error) {
	if poolID == "" || vmid <= 0 {
		return false, cpierrors.Cloud("PoolService.PoolHasVM: poolID must not be empty and vmid must be positive")
	}
	resp, err := s.svc.GetPools(ctx, poolID, nil)
	if err != nil {
		if isPoolNotFound(err) {
			return false, nil
		}
		return false, cpierrors.Wrap(err, fmt.Sprintf("PoolService.PoolHasVM: read pool %q", poolID))
	}
	if resp == nil {
		return false, nil
	}
	for _, raw := range resp.Members {
		var item struct {
			Vmid *int64 `json:"vmid"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Vmid == nil {
			continue
		}
		if *item.Vmid == vmid {
			return true, nil
		}
	}
	return false, nil
}

// isPoolNotFound reports whether err indicates the queried pool does not exist.
// Fail-closed: only returns true when we POSITIVELY identify the pool as
// absent. Unknown errors propagate as failures rather than being treated as
// absent (fail-open would let a transient error be mistaken for an available
// slot).
//
// Two positive shapes are recognised. A genuine 404 (SDK sentinel or APIError)
// covers the documented contract. Live PVE 9.2 does not use it: GET
// /pools/{poolid} for a missing pool answers HTTP 500 with the body text
// "pool 'x' does not exist" — the same 500-with-text convention every other
// pool error follows — so IsPoolNotFound's text matcher is consulted too.
// Without it every first boot against the shipped default pools logged a
// spurious "could not confirm pool access" warning, because a pool that does
// not exist yet is the normal pre-lazy-creation state, not a fault.
func isPoolNotFound(err error) bool {
	if err == nil {
		return false
	}
	// Prefer SDK sentinel — errors.Is traverses the Unwrap chain.
	if errors.Is(err, sdkerrors.ErrNotFound) {
		return true
	}
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) && apiErr.IsNotFound() {
		return true
	}
	return IsPoolNotFound(err)
}

// buildUserAgent returns the User-Agent string for all PVE API requests.
// Format: "BOSH-PVE-CPI/<version>" with an optional " pid-<operator_id>"
// suffix when cfg.OperatorID is non-empty.
//
// version.Short() is used (not version.String()) to avoid embedding commit SHA
// and build-date noise into the header. When OperatorID is empty the result
// contains no trailing space — byte-identical to any prior release that did not
// set a User-Agent.
func buildUserAgent(cfg *config.CPIConfig) string {
	ua := "BOSH-PVE-CPI/" + version.Short()
	if cfg.OperatorID != "" {
		ua += " pid-" + cfg.OperatorID
	}
	return ua
}

// buildTransportOpts constructs the base sdkclient.Options from cfg, setting
// only connection, timeout, and transport-tuning fields. Auth and SSL fields
// are filled in by NewClient after this call. Extracted as a seam for unit
// testing so the Options values can be asserted without standing up a real
// PVE host.
//
// All five PVE API transport fields (DialTimeoutSec, TLSHandshakeTimeoutSec,
// MaxIdleConnsPerHost, IdleConnTimeoutSec, TCPKeepAliveSec) are assigned
// directly from cfg: 0 is the SDK's "use default" sentinel. Four of the five
// therefore produce Options identical to those built before §7.30 when
// unset; IdleConnTimeoutSec is the exception, because ApplyDefaults rewrites
// its 0 to 15 before this function runs (see the paragraph below).
func buildTransportOpts(cfg *config.CPIConfig) sdkclient.Options {
	port := cfg.Port
	if port == 0 {
		port = 8006
	}
	return sdkclient.Options{
		Host:     cfg.Host,
		Port:     port,
		Protocol: sdkclient.ProtocolHTTPS,
		// Long timeout: stemcell uploads (multi-GB) and import tasks
		// can run for several minutes.
		Timeout: 30 * time.Minute,
		// PVE API transport tuning — zero is the SDK no-op sentinel: it leaves
		// the transport field at the SDK internal default. Direct assignment is
		// safe: the SDK treats 0 as "use default" for each field (see
		// vendor/pkg/client/options.go). Validated >= 0 in config.Validate.
		// IdleConnTimeoutSec arrives as 15 for an unset config (see
		// config.ApplyDefaults): pveproxy's keep-alive window is shorter than
		// the SDK's 90s default, so retiring idle connections first avoids the
		// reused-dead-connection race.
		DialTimeoutSec:         cfg.PVEDialTimeoutSec,
		TLSHandshakeTimeoutSec: cfg.PVETLSHandshakeTimeoutSec,
		MaxIdleConnsPerHost:    cfg.PVEMaxIdleConnsPerHost,
		IdleConnTimeoutSec:     cfg.PVEIdleConnTimeoutSec,
		TCPKeepAliveSec:        cfg.PVETCPKeepAliveSec,
	}
}

// NewClientWithTracer constructs a Client from CPIConfig.
// Selects auth: APIToken if non-empty else User+Password+Realm.
// Honors VerifySSL (false = skip TLS verify).
//
// Additionally decorates each PVE service call site actually used by CPI
// code (see tracing.go) with a child span started from tracer when tracer is
// non-nil. A nil tracer yields raw, undecorated services with zero added
// overhead.
func NewClientWithTracer(cfg *config.CPIConfig, logger *log.Logger, tracer trace.Tracer) (Client, error) {
	return newClient(cfg, logger, tracer)
}

// newClient is the shared construction path for NewClientWithTracer.
// tracer == nil means "no tracing".
func newClient(cfg *config.CPIConfig, logger *log.Logger, tracer trace.Tracer) (Client, error) {
	if cfg == nil {
		return nil, cpierrors.Cloud("pve client init: cfg must not be nil")
	}
	if cfg.Host == "" {
		return nil, cpierrors.Cloud("pve client init: host is required")
	}

	hasToken := cfg.APIToken != ""
	hasPassword := cfg.Password != ""
	if !hasToken && !hasPassword {
		return nil, cpierrors.Cloud("pve client init: one of api_token or password is required")
	}

	opts := buildTransportOpts(cfg)

	if hasToken {
		opts.APIToken = cfg.APIToken
		logger.Debug("pve client: using API token auth")
	} else {
		realm := cfg.Realm
		if realm == "" {
			realm = "pam"
		}
		// Only append @realm if the user didn't already supply one
		// (e.g. cfg.User = "root@pam" should not become "root@pam@pam").
		if strings.Contains(cfg.User, "@") {
			opts.Username = cfg.User
		} else {
			opts.Username = fmt.Sprintf("%s@%s", cfg.User, realm)
		}
		opts.Password = cfg.Password
		opts.AutoLogin = true
		// username intentionally omitted: auth-event probe surface; emit at scope of auth-failure event instead
		logger.Debug("pve client: using password auth")
	}

	if !cfg.VerifySSLValue() {
		opts.SSLOptions = &sdkclient.SSLOptions{
			VerifyMode:     sdkclient.SSLVerifyNone,
			VerifyHostname: false,
		}
		logger.Warn("pve client: TLS verification disabled")
	} else if cfg.PVECACertPEM != "" {
		// verify_ssl=true and a custom CA PEM is supplied. The SDK accepts a CA
		// cert as a file path only; write the PEM to a secure temp file, pass the
		// path during construction, then delete the file immediately. The TLS
		// config is built synchronously inside sdkclient.NewClient — the pool is
		// baked into *tls.Config before the call returns and the file is no longer
		// referenced after that point.
		f, tmpErr := os.CreateTemp("", "pve-ca-*.pem")
		if tmpErr != nil {
			return nil, cpierrors.Cloud("pve client init: create temp CA file: %s", tmpErr.Error())
		}
		tmpPath := f.Name()
		// Ensure the temp file is removed whether the write, SDK init, or any
		// subsequent step fails. The defer fires after sdkclient.NewClient returns.
		defer func() { _ = os.Remove(tmpPath) }()

		if _, writeErr := f.WriteString(cfg.PVECACertPEM); writeErr != nil {
			_ = f.Close()
			return nil, cpierrors.Cloud("pve client init: write temp CA file: %s", writeErr.Error())
		}
		if closeErr := f.Close(); closeErr != nil {
			return nil, cpierrors.Cloud("pve client init: close temp CA file: %s", closeErr.Error())
		}

		opts.SSLOptions = &sdkclient.SSLOptions{
			VerifyMode:     sdkclient.SSLVerifyPeer,
			VerifyHostname: true,
			CACert:         tmpPath,
		}
		logger.Debug("pve client: custom CA cert pool in use")
	}

	raw, err := sdkclient.NewClient(opts)
	if err != nil {
		return nil, cpierrors.Wrap(err, "pve client init")
	}
	// Set User-Agent on every outgoing PVE API request. The SDK's
	// applyCustomHeaders runs after standard headers, so SetHeader overrides
	// the SDK default "proxmox-apiclient-go/1.0". Both request paths (regular and
	// upload) call applyCustomHeaders, so a single SetHeader covers all calls.
	// SetHeader is part of the public client.Client interface as of upstream
	// v3.2.10, so no vendor patch is required.
	raw.SetHeader("User-Agent", buildUserAgent(cfg))

	qemuSvc := qemu.New(raw)
	storageSvc := storage.New(raw)
	tasksSvc := tasks.New(raw)
	nodesSvc := nodes.New(raw)
	clusterSvc := cluster.New(raw)
	clusterStorageSvc := clusterstorage.New(raw)
	var poolsSvc PoolService = &sdkPoolService{svc: pools.New(raw)}
	// cloudInitSvc is never decorated: the call-surface audit (see tracing.go)
	// found zero production call sites for Client.CloudInit(), so there is no
	// span-worthy call path to instrument.
	cloudInitSvc := cloudinit.New(raw)

	if tracer != nil {
		qemuSvc = &tracedQEMUService{Service: qemuSvc, tracer: tracer}
		storageSvc = &tracedStorageService{Service: storageSvc, tracer: tracer}
		tasksSvc = &tracedTasksService{Service: tasksSvc, tracer: tracer}
		nodesSvc = &tracedNodesService{Service: nodesSvc, tracer: tracer}
		clusterSvc = &tracedClusterService{Service: clusterSvc, tracer: tracer}
		clusterStorageSvc = &tracedClusterStorageService{Service: clusterStorageSvc, tracer: tracer}
		poolsSvc = &tracedPoolService{PoolService: poolsSvc, tracer: tracer}
	}

	return &sdkClient{
		qemuSvc:           qemuSvc,
		storageSvc:        storageSvc,
		cloudInitSvc:      cloudInitSvc,
		tasksSvc:          tasksSvc,
		nodesSvc:          nodesSvc,
		clusterSvc:        clusterSvc,
		clusterStorageSvc: clusterStorageSvc,
		poolsSvc:          poolsSvc,
	}, nil
}
