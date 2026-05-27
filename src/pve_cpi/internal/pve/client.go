// Client interface composed of per-domain service handles, mockable for tests.
package pve

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkclient "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

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
}

func (c *sdkClient) QEMU() qemu.Service                     { return c.qemuSvc }
func (c *sdkClient) Storage() storage.Service               { return c.storageSvc }
func (c *sdkClient) CloudInit() cloudinit.Service           { return c.cloudInitSvc }
func (c *sdkClient) Tasks() tasks.Service                   { return c.tasksSvc }
func (c *sdkClient) Nodes() nodes.Service                   { return c.nodesSvc }
func (c *sdkClient) Cluster() cluster.Service               { return c.clusterSvc }
func (c *sdkClient) ClusterStorage() clusterstorage.Service { return c.clusterStorageSvc }

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

	port := cfg.Port
	if port == 0 {
		port = 8006
	}

	opts := sdkclient.Options{
		Host:     cfg.Host,
		Port:     port,
		Protocol: sdkclient.ProtocolHTTPS,
		// Long timeout: stemcell uploads (multi-GB) and import tasks
		// can run for several minutes.
		Timeout: 30 * time.Minute,
	}

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
	}, nil
}
