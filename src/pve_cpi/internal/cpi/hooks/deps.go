package hooks

import (
	"context"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// Deps carries everything a built-in hook constructor needs. It is assembled in
// cmd/cpi/main.go from the resolved CPIConfig and passed to every registry
// constructor. A constructor reads only the fields it needs; unused fields stay
// nil/zero. Keeping the per-hook config types in this package (rather than
// importing internal/config) avoids an import cycle: internal/config already
// imports this package for hook-name validation, so this package must not
// import internal/config.
type Deps struct {
	// Logger is the process logger. Always set.
	Logger *log.Logger

	// LBRegister configures the lb_register hook. Nil when the operator did not
	// supply an lb_register block; the hook then constructs inert.
	LBRegister *LBRegisterConfig

	// ExternalCommand configures the external_command hook. Nil (or an empty
	// allowlist) makes the hook inert — it never executes anything.
	ExternalCommand *ExternalCommandConfig

	// Annotator writes guest Notes for the notes_audit hook. Satisfied by an
	// adapter in internal/cpi/handlers that decodes the VM CID and resolves the
	// node via the PVE API. Nil disables Notes writing (hook logs and no-ops).
	Annotator VMAnnotator

	// Metrics configures the metrics hook. Nil when the operator did not supply
	// a metrics block or Enabled is false. MetricsConfig lives in this package
	// (not internal/config) to avoid the import cycle: internal/config imports
	// this package for hook-name validation.
	Metrics *MetricsConfig
}

// LBRegisterConfig holds the HAProxy Data Plane API target for the lb_register
// hook. Field JSON tags match the manifest sub-block pve.lb_register.
type LBRegisterConfig struct {
	// Endpoint is the HAProxy Data Plane API base URL, e.g. https://lb:5555.
	Endpoint string `json:"endpoint,omitempty"`
	// Backend is the HAProxy backend name servers are added to/removed from.
	Backend string `json:"backend,omitempty"`
	// Port is the server port registered for each VM (e.g. the CF router port).
	Port int `json:"port,omitempty"`
	// User / Password are Data Plane API basic-auth credentials.
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	// CACertPEM optionally pins the Data Plane API server certificate chain.
	CACertPEM string `json:"ca_cert,omitempty"`
	// AllowPrivateIP permits an Endpoint that resolves to a private/loopback
	// address. Default false rejects such endpoints (SSRF guard).
	AllowPrivateIP bool `json:"allow_private_ip,omitempty"`
	// TimeoutMS bounds each Data Plane API call. Zero uses a built-in default.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// ExternalCommandConfig holds the allowlisted external_command hook settings.
// The allowlist is the safety boundary: Command must be an exact allowlist
// member, paths must be absolute, and the child runs with no shell and a
// scrubbed environment.
type ExternalCommandConfig struct {
	// Command is the absolute executable path to run. Must be in Allowlist.
	Command string `json:"command,omitempty"`
	// Args are passed verbatim as discrete argv (no shell interpretation).
	Args []string `json:"args,omitempty"`
	// Allowlist enumerates the absolute executable paths permitted to run. An
	// empty allowlist makes the hook inert.
	Allowlist []string `json:"allowlist,omitempty"`
	// EnvPasslist names environment variables passed through from the CPI
	// process to the child. Everything else is scrubbed.
	EnvPasslist []string `json:"env_passlist,omitempty"`
	// TimeoutMS bounds each invocation. Zero uses a built-in default.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// Methods names the CPI methods that trigger the command. Empty defaults to
	// create_vm and delete_vm.
	Methods []string `json:"methods,omitempty"`
}

// VMAnnotator writes operator-facing Notes onto a created VM. Implemented by an
// adapter in internal/cpi/handlers (structural satisfaction — that package does
// not import this one). vmid is the integer VM CID.
type VMAnnotator interface {
	AnnotateNotes(ctx context.Context, vmid int, notes string) error
}
