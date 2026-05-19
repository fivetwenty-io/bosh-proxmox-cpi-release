package agent

import (
	"fmt"
	"strconv"
)

func vmNameDefault(vmid int) string { return fmt.Sprintf("vm-%d", vmid) }
func vmidString(vmid int) string    { return strconv.Itoa(vmid) }

// settingsJSON is the BOSH agent settings.json payload. ConfigDrive writes
// it as raw bytes at /ec2/latest/user-data; RegistryAgent PUTs it to the
// /instances/{vmid} record. The struct shape and tag set are part of the
// BOSH agent contract — all fields must be present even when zero-valued.
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
// (so JSON renders {}/[] not null), VM.Name fallback "vm-{vmid}", VM.ID
// fallback to vmid as string, and the MBus-from-blobstore fallback (when
// cfg.MBus is empty and the blobstore endpoint host is non-loopback).
//
// The returned bool is true when the MBus fallback was applied — callers log
// it so operators can spot the derivation in director logs.
func buildSettings(cfg AgentConfig, vmid int) (settingsJSON, bool) {
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

	fallbackApplied := false
	if s.MBus == "" {
		if derived := deriveMBusFromBlobstore(s.Blobstore); derived != "" {
			s.MBus = derived
			fallbackApplied = true
		}
	}
	return s, fallbackApplied
}
