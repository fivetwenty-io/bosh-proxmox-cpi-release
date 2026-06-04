// Client interface composed of per-domain service handles, mockable for tests.
package pve

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/pools"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkclient "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// PoolService manages PVE resource pool membership.
// PVE resource pools group VMs and containers for ACL/billing purposes.
// The only operation the CPI needs is adding a VM to a named pool after creation.
type PoolService interface {
	// AddVM assigns vmid to the resource pool identified by poolID.
	// Corresponds to PUT /pools/{poolid} with vms=[vmid].
	// Returns an error when the pool does not exist or the PVE API rejects the call.
	AddVM(ctx context.Context, poolID string, vmid int64) error
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

// buildTransportOpts constructs the base sdkclient.Options from cfg, setting
// only connection, timeout, and transport-tuning fields. Auth and SSL fields
// are filled in by NewClient after this call. Extracted as a seam for unit
// testing so the Options values can be asserted without standing up a real
// PVE host.
//
// All five PVE API transport fields (DialTimeoutSec, TLSHandshakeTimeoutSec,
// MaxIdleConnsPerHost, IdleConnTimeoutSec, TCPKeepAliveSec) are assigned
// directly from cfg: 0 is the SDK's "use default" sentinel, so an unset cfg
// produces Options identical to those built before §7.30 was introduced
// (byte-identical guarantee).
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
		// PVE API transport tuning — all fields are 0 by default. Zero is the
		// SDK no-op sentinel: it leaves the transport field at the SDK internal
		// default, so an unset config is byte-identical to prior releases.
		// Direct assignment is safe: the SDK treats 0 as "use default" for each
		// field (see vendor/pkg/client/options.go). Validated >= 0 in config.Validate.
		DialTimeoutSec:         cfg.PVEDialTimeoutSec,
		TLSHandshakeTimeoutSec: cfg.PVETLSHandshakeTimeoutSec,
		MaxIdleConnsPerHost:    cfg.PVEMaxIdleConnsPerHost,
		IdleConnTimeoutSec:     cfg.PVEIdleConnTimeoutSec,
		TCPKeepAliveSec:        cfg.PVETCPKeepAliveSec,
	}
}

// NewClient constructs a Client from CPIConfig.
// Selects auth: APIToken if non-empty else User+Password+Realm.
// Honors VerifySSL (false = skip TLS verify).
func NewClient(cfg *config.CPIConfig, logger *log.Logger) (Client, error) {
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

	return &sdkClient{
		qemuSvc:           qemu.New(raw),
		storageSvc:        storage.New(raw),
		cloudInitSvc:      cloudinit.New(raw),
		tasksSvc:          tasks.New(raw),
		nodesSvc:          nodes.New(raw),
		clusterSvc:        cluster.New(raw),
		clusterStorageSvc: clusterstorage.New(raw),
		poolsSvc:          &sdkPoolService{svc: pools.New(raw)},
	}, nil
}
