package agent

import (
	"fmt"
	"strconv"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

func vmNameDefault(vmid int) string { return fmt.Sprintf("vm-%d", vmid) }
func vmidString(vmid int) string    { return strconv.Itoa(vmid) }

// settingsJSON is the BOSH agent settings.json payload. ConfigDrive writes
// it as raw bytes at /ec2/latest/user-data. The struct shape and tag set
// are part of the BOSH agent contract — all fields must be present even
// when zero-valued.
type settingsJSON struct {
	AgentID   string                 `json:"agent_id"`
	VM        VMSpec                 `json:"vm"`
	Networks  map[string]NetworkSpec `json:"networks"`
	Disks     DisksSpec              `json:"disks"`
	Env       map[string]any         `json:"env"`
	MBus      string                 `json:"mbus"`
	Blobstore BlobstoreSpec          `json:"blobstore"`
	NTP       []string               `json:"ntp"`
}

// buildSettings packs an AgentConfig into a fully populated settingsJSON,
// applying the agent-mode-independent defaults: non-nil networks/disks/env/ntp
// (so JSON renders {}/[] not null), VM.Name fallback "vm-{vmid}", and VM.ID
// fallback to vmid as string.
//
// MBus handling: cfg.MBus must be explicitly set. If cfg.MBus is empty and
// deriveMBusFromBlobstore can extract a host from the blobstore endpoint, this
// function returns an error rather than silently producing a credential-less
// nats:// URL that will fail NATS authentication. Operators must supply mbus
// explicitly in the director manifest. If cfg.MBus is empty AND no blobstore
// host is available, the settings are returned with an empty mbus field — the
// BOSH agent will fail to connect, surfacing the misconfiguration at runtime.
func buildSettings(cfg AgentConfig, vmid int) (settingsJSON, error) {
	networks := cfg.Networks
	if networks == nil {
		networks = map[string]NetworkSpec{}
	}
	disks := cfg.Disks
	if disks.Persistent == nil {
		disks.Persistent = map[string]string{}
	}
	env := cfg.Env
	if env == nil {
		env = map[string]any{}
	}
	ntp := cfg.NTP
	if ntp == nil {
		ntp = []string{}
	}

	s := settingsJSON{
		AgentID:   cfg.AgentID,
		VM:        VMSpec{Name: cfg.VM.Name, ID: cfg.VM.ID},
		Networks:  networks,
		Disks:     disks,
		Env:       env,
		MBus:      cfg.MBus,
		Blobstore: cfg.Blobstore,
		NTP:       ntp,
	}
	if s.VM.Name == "" {
		s.VM.Name = vmNameDefault(vmid)
	}
	if s.VM.ID == "" {
		s.VM.ID = vmidString(vmid)
	}

	if s.MBus == "" {
		if derived := deriveMBusFromBlobstore(s.Blobstore); derived != "" {
			// Refuse to use the derived URL: it carries no credentials and no TLS,
			// so the BOSH agent would fail authentication against a production NATS
			// server. Require an explicit mbus in the director manifest instead.
			return settingsJSON{}, cpierrors.Cloud(
				"agent settings vm %d: mbus is empty; derived credential-less URL %q from blobstore "+
					"endpoint — set mbus explicitly in the director manifest",
				vmid, derived,
			)
		}
	}
	return s, nil
}
