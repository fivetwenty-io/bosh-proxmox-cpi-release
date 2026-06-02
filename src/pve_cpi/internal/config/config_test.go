package config_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// mustLoad is a test helper that calls Load from a JSON string.
func mustLoad(t *testing.T, jsonStr string) (*config.CPIConfig, error) {
	t.Helper()
	return config.Load(strings.NewReader(jsonStr))
}

// boolPtr returns a pointer to b, for constructing *bool fields in literals.
//
//nolint:modernize // helper supports non-zero bool values; new(bool) only gives false
func boolPtr(b bool) *bool { return &b }

// assertCloudError asserts err is a *cpierrors.Error with TypeCloud and that
// its message contains the given substring.
func assertCloudError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", contains)
	}
	// errors.As walks the wrap chain so future fmt.Errorf-wrapping inside
	// config.Load won't silently turn this assertion into a false negative.
	var ce *cpierrors.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if ce.Type() != cpierrors.TypeCloud {
		t.Errorf("expected TypeCloud, got %s", ce.Type())
	}
	if !strings.Contains(err.Error(), contains) {
		t.Errorf("error %q does not contain %q", err.Error(), contains)
	}
}

// --------------------------------------------------------------------------
// TestLoad_Valid
// --------------------------------------------------------------------------

func TestLoad_Valid(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadFile("testdata/valid.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Supplied fields preserved.
	if cfg.Host != "pve.example.com" {
		t.Errorf("Host = %q, want %q", cfg.Host, "pve.example.com")
	}
	if cfg.User != "root" {
		t.Errorf("User = %q, want %q", cfg.User, "root")
	}
	if cfg.VMStorage != "local-lvm" {
		t.Errorf("VMStorage = %q, want %q", cfg.VMStorage, "local-lvm")
	}

	// Defaults applied.
	if cfg.Port != 8006 {
		t.Errorf("Port = %d, want 8006", cfg.Port)
	}
	if cfg.Realm != "pam" {
		t.Errorf("Realm = %q, want %q", cfg.Realm, "pam")
	}
	if !cfg.VerifySSLValue() {
		t.Errorf("VerifySSLValue() = false, want true")
	}
	if cfg.AgentMode != "cloudinit" {
		t.Errorf("AgentMode = %q, want %q", cfg.AgentMode, "cloudinit")
	}
	if cfg.VMDiskFormat != "qcow2" {
		t.Errorf("VMDiskFormat = %q, want %q", cfg.VMDiskFormat, "qcow2")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.VMIDRangeStart != 100 {
		t.Errorf("VMIDRangeStart = %d, want 100", cfg.VMIDRangeStart)
	}
	// StemcellStorage explicitly set in testdata/valid.json.
	if cfg.StemcellStorage != "local" {
		t.Errorf("StemcellStorage = %q, want %q", cfg.StemcellStorage, "local")
	}
}

// --------------------------------------------------------------------------
// TestLoad_MissingRequired
// --------------------------------------------------------------------------

func TestLoad_MissingRequired(t *testing.T) {
	t.Parallel()
	_, err := config.LoadFile("testdata/invalid_missing_host.json")
	assertCloudError(t, err, "host is required")
}

// --------------------------------------------------------------------------
// TestValidate_AgentModeInvalid
// --------------------------------------------------------------------------

func TestValidate_AgentModeInvalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"agent_mode": "magic"
	}`)
	assertCloudError(t, err, "agent_mode must be one of")
}

// --------------------------------------------------------------------------
// TestValidate_AuthMissing
// --------------------------------------------------------------------------

func TestValidate_AuthMissing(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	assertCloudError(t, err, "one of password or api_token is required")
}

// --------------------------------------------------------------------------
// TestValidate_AuthBoth
// --------------------------------------------------------------------------

func TestValidate_AuthBoth(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u",
		"password": "pw", "api_token": "tok",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	// When both password and api_token are supplied, the api_token wins and
	// the password is cleared so downstream code authenticates with the token.
	if err != nil {
		t.Fatalf("expected no error when both credentials set, got %v", err)
	}
	if cfg.APIToken != "tok" {
		t.Errorf("expected api_token preserved, got %q", cfg.APIToken)
	}
	if cfg.Password != "" {
		t.Errorf("expected password cleared when api_token present, got %q", cfg.Password)
	}
}

// --------------------------------------------------------------------------
// TestValidate_RegistryRequiresFields
// --------------------------------------------------------------------------

func TestValidate_RegistryRequiresFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		json    string
		wantMsg string
	}{
		{
			name: "missing endpoint",
			json: `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"agent_mode":"registry",
				"registry_user":"ru","registry_password":"rp"
			}`,
			wantMsg: "registry_endpoint is required",
		},
		{
			name: "missing registry_user",
			json: `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"agent_mode":"registry",
				"registry_endpoint":"http://r:25777","registry_password":"rp"
			}`,
			wantMsg: "registry_user is required",
		},
		{
			name: "missing registry_password",
			json: `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"agent_mode":"registry",
				"registry_endpoint":"http://r:25777","registry_user":"ru"
			}`,
			wantMsg: "registry_password is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mustLoad(t, tc.json)
			assertCloudError(t, err, tc.wantMsg)
		})
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_AllFields
// --------------------------------------------------------------------------

func TestApplyDefaults_AllFields(t *testing.T) {
	t.Parallel()
	// Construct a zeroed config; ApplyDefaults fills everything.
	var cfg config.CPIConfig
	// VMStorage must be non-empty for StemcellStorage fallback to land on it.
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.Port != 8006 {
		t.Errorf("Port = %d, want 8006", cfg.Port)
	}
	if cfg.Realm != "pam" {
		t.Errorf("Realm = %q, want %q", cfg.Realm, "pam")
	}
	if !cfg.VerifySSLValue() {
		t.Errorf("VerifySSLValue() = false, want true (nil → default true)")
	}
	if cfg.AgentMode != "cloudinit" {
		t.Errorf("AgentMode = %q, want %q", cfg.AgentMode, "cloudinit")
	}
	if cfg.VMDiskFormat != "qcow2" {
		t.Errorf("VMDiskFormat = %q, want %q", cfg.VMDiskFormat, "qcow2")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.VMIDRangeStart != 100 {
		t.Errorf("VMIDRangeStart = %d, want 100", cfg.VMIDRangeStart)
	}
	if cfg.CreateEnvDeployment != "create-env" {
		t.Errorf("CreateEnvDeployment = %q, want %q", cfg.CreateEnvDeployment, "create-env")
	}
	if cfg.VMPrefix != "" {
		t.Errorf("VMPrefix = %q, want empty (no default prefix)", cfg.VMPrefix)
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_VerifySSLExplicitFalse
// --------------------------------------------------------------------------

// TestApplyDefaults_VerifySSLExplicitFalse ensures ApplyDefaults does not
// overwrite an explicitly-set *false — the historical silent-overwrite bug.
func TestApplyDefaults_VerifySSLExplicitFalse(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.VerifySSL = boolPtr(false)
	cfg.ApplyDefaults()

	if cfg.VerifySSLValue() {
		t.Error("VerifySSLValue() = true, want false — ApplyDefaults must not overwrite explicit *false")
	}
	if cfg.VerifySSL == nil {
		t.Error("VerifySSL became nil after ApplyDefaults, want *false")
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_StemcellFallback
// --------------------------------------------------------------------------

func TestApplyDefaults_StemcellFallback(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "local-lvm"
	// StemcellStorage deliberately left empty.
	cfg.ApplyDefaults()

	if cfg.StemcellStorage != "local-lvm" {
		t.Errorf("StemcellStorage = %q, want %q (fallback to VMStorage)", cfg.StemcellStorage, "local-lvm")
	}
}

// TestApplyDefaults_StemcellNotOverwritten ensures an explicit StemcellStorage is preserved.
func TestApplyDefaults_StemcellNotOverwritten(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "local-lvm"
	cfg.StemcellStorage = "local"
	cfg.ApplyDefaults()

	if cfg.StemcellStorage != "local" {
		t.Errorf("StemcellStorage = %q, want %q", cfg.StemcellStorage, "local")
	}
}

// --------------------------------------------------------------------------
// TestLoadFile_FileNotFound
// --------------------------------------------------------------------------

func TestLoadFile_FileNotFound(t *testing.T) {
	t.Parallel()
	_, err := config.LoadFile("/nonexistent/path/config.json")
	assertCloudError(t, err, "config: open")
}

// --------------------------------------------------------------------------
// TestLoadFile_MalformedJSON
// --------------------------------------------------------------------------

func TestLoadFile_MalformedJSON(t *testing.T) {
	t.Parallel()
	_, err := config.Load(strings.NewReader(`{this is not json}`))
	assertCloudError(t, err, "config: decode failed")
}

// --------------------------------------------------------------------------
// TestLoad_ExceedsMaxBytes
// --------------------------------------------------------------------------

// TestLoad_ExceedsMaxBytes verifies the 1 MiB cap on config input. The cap
// is defense-in-depth against a malformed or attacker-controlled config that
// would otherwise drive an unbounded io.ReadAll allocation.
func TestLoad_ExceedsMaxBytes(t *testing.T) {
	t.Parallel()
	oversize := strings.Repeat("a", config.MaxConfigBytes+1)
	_, err := config.Load(strings.NewReader(oversize))
	assertCloudError(t, err, "config: input exceeds")
}

// --------------------------------------------------------------------------
// TestApplyDefaults_StemcellFetchTimeouts
// --------------------------------------------------------------------------

// TestApplyDefaults_StemcellFetchTimeouts verifies the stemcell-fetch transport
// timeout defaults are applied when fields are zero.
func TestApplyDefaults_StemcellFetchTimeouts(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.ApplyDefaults()

	if cfg.StemcellFetchDialTimeoutSec != 30 {
		t.Errorf("StemcellFetchDialTimeoutSec = %d, want 30", cfg.StemcellFetchDialTimeoutSec)
	}
	if cfg.StemcellFetchTLSHandshakeTimeoutSec != 15 {
		t.Errorf("StemcellFetchTLSHandshakeTimeoutSec = %d, want 15", cfg.StemcellFetchTLSHandshakeTimeoutSec)
	}
	if cfg.StemcellFetchResponseHeaderTimeoutSec != 120 {
		t.Errorf("StemcellFetchResponseHeaderTimeoutSec = %d, want 120", cfg.StemcellFetchResponseHeaderTimeoutSec)
	}
	if cfg.StemcellFetchIdleConnTimeoutSec != 90 {
		t.Errorf("StemcellFetchIdleConnTimeoutSec = %d, want 90", cfg.StemcellFetchIdleConnTimeoutSec)
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_StemcellFetchTimeoutsExplicit
// --------------------------------------------------------------------------

// TestApplyDefaults_StemcellFetchTimeoutsExplicit ensures operator-supplied
// values are preserved through ApplyDefaults.
func TestApplyDefaults_StemcellFetchTimeoutsExplicit(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		StemcellFetchDialTimeoutSec:           45,
		StemcellFetchTLSHandshakeTimeoutSec:   20,
		StemcellFetchResponseHeaderTimeoutSec: 300,
		StemcellFetchIdleConnTimeoutSec:       60,
	}
	cfg.ApplyDefaults()

	if cfg.StemcellFetchDialTimeoutSec != 45 {
		t.Errorf("StemcellFetchDialTimeoutSec = %d, want 45", cfg.StemcellFetchDialTimeoutSec)
	}
	if cfg.StemcellFetchTLSHandshakeTimeoutSec != 20 {
		t.Errorf("StemcellFetchTLSHandshakeTimeoutSec = %d, want 20", cfg.StemcellFetchTLSHandshakeTimeoutSec)
	}
	if cfg.StemcellFetchResponseHeaderTimeoutSec != 300 {
		t.Errorf("StemcellFetchResponseHeaderTimeoutSec = %d, want 300", cfg.StemcellFetchResponseHeaderTimeoutSec)
	}
	if cfg.StemcellFetchIdleConnTimeoutSec != 60 {
		t.Errorf("StemcellFetchIdleConnTimeoutSec = %d, want 60", cfg.StemcellFetchIdleConnTimeoutSec)
	}
}

// --------------------------------------------------------------------------
// TestValidate_StemcellFetchTimeoutOutOfRange
// --------------------------------------------------------------------------

// TestValidate_StemcellFetchTimeoutOutOfRange verifies the 1-3600 cap is
// enforced when an operator sets an explicit non-zero invalid value. Zero is
// accepted (treated as "use default at ApplyDefaults time").
func TestValidate_StemcellFetchTimeoutOutOfRange(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:                                  "h",
		User:                                  "u",
		Password:                              "p",
		VMStorage:                             "s",
		DiskStorage:                           "s",
		NetworkBridge:                         "br",
		Port:                                  8006,
		VerifySSL:                             boolPtr(true),
		AgentMode:                             "cloudinit",
		VMDiskFormat:                          "qcow2",
		LogLevel:                              "info",
		VMIDRangeStart:                        100,
		VMIDRangeEnd:                          5999,
		RebootMode:                            "soft",
		RebootTimeout:                         60,
		NetworkMode:                           "auto",
		SDNZoneType:                           "simple",
		StemcellFetchResponseHeaderTimeoutSec: 7200,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for response_header_timeout_sec=7200")
	}
	if !strings.Contains(err.Error(), "stemcell_fetch_response_header_timeout_sec") {
		t.Errorf("error message missing field name: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestValidate_VMDiskFormatInvalid
// --------------------------------------------------------------------------

func TestValidate_VMDiskFormatInvalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vm_disk_format":"iso9660"
	}`)
	assertCloudError(t, err, "vm_disk_format must be one of")
}

// --------------------------------------------------------------------------
// TestValidate_LogLevelInvalid
// --------------------------------------------------------------------------

func TestValidate_LogLevelInvalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"log_level":"verbose"
	}`)
	assertCloudError(t, err, "log_level must be one of")
}

// --------------------------------------------------------------------------
// TestValidate_VMIDRangeStartTooLow
// --------------------------------------------------------------------------

func TestValidate_VMIDRangeStartTooLow(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vmid_range_start":50
	}`)
	assertCloudError(t, err, "vmid_range_start must be")
}

// --------------------------------------------------------------------------
// TestValidate_PortInvalid
// --------------------------------------------------------------------------

func TestValidate_PortInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		port int
	}{
		{"negative", -1},
		{"zero after forced-set via manual Validate", 99999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.CPIConfig{
				Host:           "h",
				User:           "u",
				Password:       "p",
				VMStorage:      "s",
				DiskStorage:    "s",
				NetworkBridge:  "br",
				AgentMode:      "cloudinit",
				VMDiskFormat:   "qcow2",
				LogLevel:       "info",
				VMIDRangeStart: 100,
				VerifySSL:      boolPtr(true),
				Port:           tc.port,
			}
			err := cfg.Validate()
			assertCloudError(t, err, "port must be")
		})
	}
}

// --------------------------------------------------------------------------
// TestLoad_APIToken
// --------------------------------------------------------------------------

func TestLoad_APIToken(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"pve.example.com","user":"root@pam","api_token":"PVEAPIToken=root@pam!mytoken=abc123",
		"vm_storage":"local-lvm","disk_storage":"local-lvm","network_bridge":"vmbr0"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIToken == "" {
		t.Error("APIToken should be set")
	}
	if cfg.Password != "" {
		t.Error("Password should be empty")
	}
}

// --------------------------------------------------------------------------
// TestValidate_MultipleErrors
// --------------------------------------------------------------------------

func TestValidate_MultipleErrors(t *testing.T) {
	t.Parallel()
	// Craft a config with multiple simultaneous failures.
	cfg := &config.CPIConfig{
		// Host, User, VMStorage, DiskStorage, NetworkBridge all empty.
		// No auth.
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
		Port:           8006,
		VerifySSL:      boolPtr(true),
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"host is required", "user is required", "vm_storage is required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_RebootFields
// --------------------------------------------------------------------------

// TestApplyDefaults_RebootFields verifies that ApplyDefaults fills in the
// reboot_mode and reboot_timeout defaults when both fields are absent.
func TestApplyDefaults_RebootFields(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.RebootModeValue() != "soft" {
		t.Errorf("RebootModeValue() = %q, want %q", cfg.RebootModeValue(), "soft")
	}
	if cfg.RebootTimeoutValue() != 60 {
		t.Errorf("RebootTimeoutValue() = %d, want 60", cfg.RebootTimeoutValue())
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_RebootFieldsExplicit
// --------------------------------------------------------------------------

// TestApplyDefaults_RebootFieldsExplicit ensures explicit values survive ApplyDefaults.
func TestApplyDefaults_RebootFieldsExplicit(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.RebootMode = "hard"
	cfg.RebootTimeout = 120
	cfg.ApplyDefaults()

	if cfg.RebootModeValue() != "hard" {
		t.Errorf("RebootModeValue() = %q, want %q (must not overwrite explicit hard)", cfg.RebootModeValue(), "hard")
	}
	if cfg.RebootTimeoutValue() != 120 {
		t.Errorf("RebootTimeoutValue() = %d, want 120 (must not overwrite explicit 120)", cfg.RebootTimeoutValue())
	}
}

// --------------------------------------------------------------------------
// TestValidate_RebootModeInvalid
// --------------------------------------------------------------------------

func TestValidate_RebootModeInvalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"reboot_mode":"graceful"
	}`)
	assertCloudError(t, err, "reboot_mode must be one of soft|hard")
}

// --------------------------------------------------------------------------
// TestValidate_RebootTimeoutZeroDefaulted
// --------------------------------------------------------------------------

// TestValidate_RebootTimeoutZeroDefaulted confirms reboot_timeout=0 (absent from JSON)
// is defaulted to 60 before Validate runs, so no validation error fires.
func TestValidate_RebootTimeoutZeroDefaulted(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RebootTimeoutValue() != 60 {
		t.Errorf("RebootTimeoutValue() = %d, want 60 after default", cfg.RebootTimeoutValue())
	}
}

// --------------------------------------------------------------------------
// TestValidate_RebootTimeoutTooLarge
// --------------------------------------------------------------------------

func TestValidate_RebootTimeoutTooLarge(t *testing.T) {
	t.Parallel()
	// Must construct manually and call Validate after ApplyDefaults to force
	// an out-of-range value through without JSON decode clamping it.
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
		Port:           8006,
		VerifySSL:      boolPtr(true),
		RebootMode:     "soft",
		RebootTimeout:  3601,
	}
	err := cfg.Validate()
	assertCloudError(t, err, "reboot_timeout must be 1-3600 seconds")
}

// --------------------------------------------------------------------------
// TestLoad_RegistryMode_Valid
// --------------------------------------------------------------------------

func TestLoad_RegistryMode_Valid(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"agent_mode":"registry",
		"registry_endpoint":"https://registry:25777",
		"registry_user":"admin",
		"registry_password":"secret"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RegistryEndpoint != "https://registry:25777" {
		t.Errorf("RegistryEndpoint = %q", cfg.RegistryEndpoint)
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_VMIDRangeEnd
// --------------------------------------------------------------------------

// TestApplyDefaults_VMIDRangeEnd verifies VMIDRangeEnd defaults to 8999 when absent.
func TestApplyDefaults_VMIDRangeEnd(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.VMIDRangeEnd != 8999 {
		t.Errorf("VMIDRangeEnd = %d, want 8999 (default)", cfg.VMIDRangeEnd)
	}
}

// TestApplyDefaults_VMIDRangeEndNotOverwritten ensures an explicit VMIDRangeEnd
// is preserved by ApplyDefaults.
func TestApplyDefaults_VMIDRangeEndNotOverwritten(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.VMIDRangeEnd = 3000
	cfg.ApplyDefaults()

	if cfg.VMIDRangeEnd != 3000 {
		t.Errorf("VMIDRangeEnd = %d, want 3000 (must not overwrite explicit value)", cfg.VMIDRangeEnd)
	}
}

// --------------------------------------------------------------------------
// TestLoad_VMIDRangeEndOverride
// --------------------------------------------------------------------------

// TestLoad_VMIDRangeEndOverride confirms that vmid_range_end from JSON is honored.
func TestLoad_VMIDRangeEndOverride(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vmid_range_end":2500
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VMIDRangeEnd != 2500 {
		t.Errorf("VMIDRangeEnd = %d, want 2500", cfg.VMIDRangeEnd)
	}
}

// --------------------------------------------------------------------------
// TestValidate_VMIDRangeEnd
// --------------------------------------------------------------------------

// TestValidate_VMIDRangeEndEqualToStart confirms end == start is rejected.
func TestValidate_VMIDRangeEndEqualToStart(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		Port:           8006,
		VerifySSL:      boolPtr(true),
		VMIDRangeStart: 100,
		VMIDRangeEnd:   100, // equal to start — invalid
		RebootMode:     "soft",
		RebootTimeout:  60,
	}
	assertCloudError(t, cfg.Validate(), "vmid_range_end must be > vmid_range_start")
}

// TestValidate_VMIDRangeEndBelowStart confirms end < start is rejected.
func TestValidate_VMIDRangeEndBelowStart(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		Port:           8006,
		VerifySSL:      boolPtr(true),
		VMIDRangeStart: 500,
		VMIDRangeEnd:   200, // below start — invalid
		RebootMode:     "soft",
		RebootTimeout:  60,
	}
	assertCloudError(t, cfg.Validate(), "vmid_range_end must be > vmid_range_start")
}

// TestValidate_VMRange_OverlapsDefaultDiskRange confirms that a VM range
// extending into the default disk band (9000+) is rejected as an overlap. With
// the disk range configurable there is no hard VM ceiling; the guard is the
// pairwise overlap check against the effective disk range.
func TestValidate_VMRange_OverlapsDefaultDiskRange(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		Port:           8006,
		VerifySSL:      boolPtr(true),
		VMIDRangeStart: 100,
		VMIDRangeEnd:   9500, // reaches into the default disk band [9000,29999]
		RebootMode:     "soft",
		RebootTimeout:  60,
	}
	assertCloudError(t, cfg.Validate(), "overlaps VM VMID range")
}

// TestValidate_VMIDRangeEndAt8999 confirms end == 8999 (the design default and
// the highest legal VM VMID, one below the disk range floor of 9000) is valid.
func TestValidate_VMIDRangeEndAt8999(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vmid_range_end":8999
	}`)
	if err != nil {
		t.Fatalf("unexpected error for vmid_range_end=8999: %v", err)
	}
	if cfg.VMIDRangeEnd != 8999 {
		t.Errorf("VMIDRangeEnd = %d, want 8999", cfg.VMIDRangeEnd)
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_SnapshotGuardBools
// --------------------------------------------------------------------------

// TestApplyDefaults_SnapshotGuardBools confirms both bool fields default to false
// (Go zero-value; ApplyDefaults has no explicit step for them — this test verifies
// that no accidental default-to-true was introduced).
func TestApplyDefaults_SnapshotGuardBools(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.AllowDiskOpsWithSnapshots {
		t.Error("AllowDiskOpsWithSnapshots = true after ApplyDefaults, want false")
	}
	if cfg.RequireSnapshotCheckPass {
		t.Error("RequireSnapshotCheckPass = true after ApplyDefaults, want false")
	}
}

// --------------------------------------------------------------------------
// TestLoad_SnapshotGuardBools
// --------------------------------------------------------------------------

// --------------------------------------------------------------------------
// TestApplyDefaults_NetworkMode_DefaultsToAuto
// --------------------------------------------------------------------------

// TestApplyDefaults_NetworkMode_DefaultsToAuto verifies that a zero-value
// CPIConfig gets NetworkMode="auto" after ApplyDefaults.
func TestApplyDefaults_NetworkMode_DefaultsToAuto(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.NetworkMode != "auto" {
		t.Errorf("NetworkMode = %q, want %q", cfg.NetworkMode, "auto")
	}
}

// --------------------------------------------------------------------------
// TestApplyDefaults_SDNZoneType_DefaultsToSimple
// --------------------------------------------------------------------------

// TestApplyDefaults_SDNZoneType_DefaultsToSimple verifies that a zero-value
// CPIConfig gets SDNZoneType="simple" after ApplyDefaults.
func TestApplyDefaults_SDNZoneType_DefaultsToSimple(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.SDNZoneType != "simple" {
		t.Errorf("SDNZoneType = %q, want %q", cfg.SDNZoneType, "simple")
	}
}

// --------------------------------------------------------------------------
// TestValidate_NetworkMode_InvalidEnum
// --------------------------------------------------------------------------

func TestValidate_NetworkMode_InvalidEnum(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"network_mode":"invalid"
	}`)
	assertCloudError(t, err, "network_mode must be one of sdn|bridge|auto")
}

// --------------------------------------------------------------------------
// TestValidate_NetworkMode_ValidValues
// --------------------------------------------------------------------------

func TestValidate_NetworkMode_ValidValues(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"sdn", "bridge", "auto"} {
		t.Run(mode, func(t *testing.T) {
			_, err := mustLoad(t, `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"network_mode":"`+mode+`"
			}`)
			if err != nil {
				t.Errorf("network_mode=%q: unexpected error: %v", mode, err)
			}
		})
	}
}

// --------------------------------------------------------------------------
// TestValidate_SDNZoneType_InvalidEnum_WhenSDNMode
// --------------------------------------------------------------------------

func TestValidate_SDNZoneType_InvalidEnum_WhenSDNMode(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"network_mode":"sdn","sdn_zone_type":"bogus"
	}`)
	assertCloudError(t, err, "sdn_zone_type must be one of simple|vlan|qinq|vxlan|evpn")
}

// --------------------------------------------------------------------------
// TestValidate_SDNZoneType_NotValidated_WhenBridgeMode
// --------------------------------------------------------------------------

// TestValidate_SDNZoneType_NotValidated_WhenBridgeMode confirms that an invalid
// sdn_zone_type is not rejected when network_mode="bridge" (SDN path unreachable).
func TestValidate_SDNZoneType_NotValidated_WhenBridgeMode(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"network_mode":"bridge","sdn_zone_type":"bogus"
	}`)
	if err != nil {
		t.Errorf("expected no error for sdn_zone_type with network_mode=bridge, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestLoad_NetworkFields_RoundTrip
// --------------------------------------------------------------------------

// TestLoad_NetworkFields_RoundTrip verifies all four SDN config fields parse
// correctly from JSON.
func TestLoad_NetworkFields_RoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"network_mode":"sdn",
		"sdn_zone":"boshzone",
		"sdn_zone_type":"vxlan",
		"sdn_auto_manage_zone":true
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NetworkMode != "sdn" {
		t.Errorf("NetworkMode = %q, want %q", cfg.NetworkMode, "sdn")
	}
	if cfg.SDNZone != "boshzone" {
		t.Errorf("SDNZone = %q, want %q", cfg.SDNZone, "boshzone")
	}
	if cfg.SDNZoneType != "vxlan" {
		t.Errorf("SDNZoneType = %q, want %q", cfg.SDNZoneType, "vxlan")
	}
	if !cfg.SDNAutoManageZone {
		t.Error("SDNAutoManageZone = false, want true")
	}
}

// --------------------------------------------------------------------------
// TestLoad_NetworkMode_Omitted_GetsAuto
// --------------------------------------------------------------------------

// --------------------------------------------------------------------------
// TestLoad_UnknownFields_LogsWarn_StillLoads
// --------------------------------------------------------------------------

// TestLoad_UnknownFields_LogsWarn_StillLoads verifies that a JSON payload
// containing an unrecognized field ("future_field") decodes successfully
// and returns a valid config. The unknown field is ignored (forward-compat).
func TestLoad_UnknownFields_LogsWarn_StillLoads(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"future_field":"x"
	}`)
	if err != nil {
		t.Fatalf("expected no error for unknown field, got: %v", err)
	}
	// Verify that the rest of the config decoded correctly.
	if cfg.Host != "h" {
		t.Errorf("Host = %q, want %q", cfg.Host, "h")
	}
	if cfg.VMStorage != "s" {
		t.Errorf("VMStorage = %q, want %q", cfg.VMStorage, "s")
	}
}

// TestLoad_NetworkMode_Omitted_GetsAuto confirms that omitting network_mode
// from JSON results in NetworkMode="auto" after Load applies defaults.
func TestLoad_NetworkMode_Omitted_GetsAuto(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NetworkMode != "auto" {
		t.Errorf("NetworkMode = %q, want %q (default when omitted)", cfg.NetworkMode, "auto")
	}
}

// --------------------------------------------------------------------------
// TestLoad_SnapshotGuardBools
// --------------------------------------------------------------------------

// TestLoad_SnapshotGuardBools confirms that explicit true values are honored
// through JSON decode and that absent fields remain false.
func TestLoad_SnapshotGuardBools(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		json        string
		wantAllow   bool
		wantRequire bool
	}{
		{
			name: "both absent — both false",
			json: `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br"
			}`,
			wantAllow:   false,
			wantRequire: false,
		},
		{
			name: "allow_disk_ops_with_snapshots true",
			json: `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"allow_disk_ops_with_snapshots":true
			}`,
			wantAllow:   true,
			wantRequire: false,
		},
		{
			name: "require_snapshot_check_pass true",
			json: `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"require_snapshot_check_pass":true
			}`,
			wantAllow:   false,
			wantRequire: true,
		},
		{
			name: "both true",
			json: `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"allow_disk_ops_with_snapshots":true,
				"require_snapshot_check_pass":true
			}`,
			wantAllow:   true,
			wantRequire: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := mustLoad(t, tc.json)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.AllowDiskOpsWithSnapshots != tc.wantAllow {
				t.Errorf("AllowDiskOpsWithSnapshots = %v, want %v", cfg.AllowDiskOpsWithSnapshots, tc.wantAllow)
			}
			if cfg.RequireSnapshotCheckPass != tc.wantRequire {
				t.Errorf("RequireSnapshotCheckPass = %v, want %v", cfg.RequireSnapshotCheckPass, tc.wantRequire)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Registry scheme guard
// --------------------------------------------------------------------------

// registryBaseCfg returns a CPIConfig with all required non-registry fields
// populated and AgentMode=registry. Caller sets RegistryEndpoint per test.
func registryBaseCfg() *config.CPIConfig {
	return &config.CPIConfig{
		Host:             "h",
		User:             "u",
		Password:         "p",
		VMStorage:        "s",
		DiskStorage:      "s",
		NetworkBridge:    "br",
		Port:             8006,
		VerifySSL:        boolPtr(true),
		AgentMode:        "registry",
		VMDiskFormat:     "qcow2",
		LogLevel:         "info",
		VMIDRangeStart:   100,
		VMIDRangeEnd:     5999,
		RebootMode:       "soft",
		RebootTimeout:    60,
		NetworkMode:      "auto",
		SDNZoneType:      "simple",
		RegistryUser:     "ru",
		RegistryPassword: "rp",
	}
}

// TestValidate_DoesNotMutatePassword guards the read-only contract of Validate.
// The "both password and api_token supplied" normalization moved into
// ApplyDefaults; Validate must observe whatever the caller passed in without
// rewriting fields. A regression that re-introduces the in-Validate mutation
// would cause repeated Validate calls in diagnostic code to silently clear a
// caller-provided password, which is surprising and hides bugs.
func TestValidate_DoesNotMutatePassword(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "foo",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		Port:           8006,
		VerifySSL:      boolPtr(true),
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
		VMIDRangeEnd:   5999,
		RebootMode:     "soft",
		RebootTimeout:  60,
		NetworkMode:    "auto",
		SDNZoneType:    "simple",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned unexpected error: %v", err)
	}
	if cfg.Password != "foo" {
		t.Errorf("Validate mutated Password: got %q, want %q", cfg.Password, "foo")
	}

	// Also assert the both-credentials path: Validate must not clear Password
	// when APIToken is also set. ApplyDefaults owns that normalization now.
	cfg2 := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "foo",
		APIToken:       "tok",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		Port:           8006,
		VerifySSL:      boolPtr(true),
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
		VMIDRangeEnd:   5999,
		RebootMode:     "soft",
		RebootTimeout:  60,
		NetworkMode:    "auto",
		SDNZoneType:    "simple",
	}
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("Validate (both creds) returned unexpected error: %v", err)
	}
	if cfg2.Password != "foo" {
		t.Errorf("Validate cleared Password despite read-only contract: got %q, want %q",
			cfg2.Password, "foo")
	}
	if cfg2.APIToken != "tok" {
		t.Errorf("Validate mutated APIToken: got %q, want %q", cfg2.APIToken, "tok")
	}
}

// TestApplyDefaults_BothCredentialsTokenWins covers the mutation moved from
// Validate to ApplyDefaults: when both password and api_token are supplied,
// the token wins and the password is cleared so downstream code never sends
// stale Basic Auth credentials alongside a token.
func TestApplyDefaults_BothCredentialsTokenWins(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.Password = "foo"
	cfg.APIToken = "tok"
	cfg.ApplyDefaults()

	if cfg.APIToken != "tok" {
		t.Errorf("APIToken = %q, want %q (api_token must survive)", cfg.APIToken, "tok")
	}
	if cfg.Password != "" {
		t.Errorf("Password = %q, want empty (cleared when api_token also present)", cfg.Password)
	}
}

func TestValidate_RegistryHTTPRejected(t *testing.T) {
	t.Parallel()
	cfg := registryBaseCfg()
	cfg.RegistryEndpoint = "http://registry.example.com:25777"
	// RegistryAllowInsecure left false (default).
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "registry_allow_insecure") {
		t.Errorf("error %q must mention registry_allow_insecure for operator clarity", err.Error())
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error %q should name the required scheme (https)", err.Error())
	}
}

func TestValidate_RegistryHTTPAllowedWithOptIn(t *testing.T) {
	t.Parallel()
	cfg := registryBaseCfg()
	cfg.RegistryEndpoint = "http://registry.example.com:25777"
	cfg.RegistryAllowInsecure = true

	logger, obs := log.NewObservedLogger(log.LevelWarn)
	if err := cfg.ValidateWithLogger(logger); err != nil {
		t.Fatalf("expected nil error with opt-in true, got %v", err)
	}

	// Exactly one warn entry mentioning the opt-in flag must be emitted.
	entries := obs.All()
	if len(entries) == 0 {
		t.Fatal("expected at least one warn log entry, got none")
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Message, "registry_allow_insecure=true") &&
			strings.Contains(e.Message, "cleartext") {
			found = true
			// Endpoint attribute must be present and must not leak userinfo.
			ep, ok := e.Attrs["endpoint"]
			if !ok {
				t.Errorf("warn entry missing 'endpoint' attribute: %+v", e)
			}
			if epStr, _ := ep.(string); !strings.Contains(epStr, "registry.example.com") {
				t.Errorf("endpoint attr %q lost host", epStr)
			}
		}
	}
	if !found {
		t.Errorf("no warn entry matched the expected message; entries=%+v", entries)
	}
}

func TestValidate_RegistryHTTPSAccepted(t *testing.T) {
	t.Parallel()
	cfg := registryBaseCfg()
	cfg.RegistryEndpoint = "https://registry.example.com:25777"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected nil error for https endpoint, got %v", err)
	}
}

func TestValidate_NonRegistryModeIgnoresInsecureFlag(t *testing.T) {
	t.Parallel()
	cfg := registryBaseCfg()
	cfg.AgentMode = "cloudinit"
	// Registry endpoint left set with http:// — must be ignored entirely because
	// the scheme guard only fires in registry mode.
	cfg.RegistryEndpoint = "http://registry.example.com:25777"
	cfg.RegistryAllowInsecure = false

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected nil error in cloudinit mode despite http registry endpoint, got %v", err)
	}
}

func TestValidate_RegistryRejectsUnknownSchemeEvenWithOptIn(t *testing.T) {
	t.Parallel()
	cfg := registryBaseCfg()
	cfg.RegistryEndpoint = "ftp://registry.example.com"
	cfg.RegistryAllowInsecure = true

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected rejection for ftp scheme even with opt-in, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error %q should explain unsupported scheme", err.Error())
	}
}

func TestValidate_RegistryRejectsMissingScheme(t *testing.T) {
	t.Parallel()
	cfg := registryBaseCfg()
	cfg.RegistryEndpoint = "registry.example.com:25777"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected rejection for scheme-less endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error %q should mention missing scheme", err.Error())
	}
}

func TestValidateWithLogger_NilLoggerStillSucceedsOnHTTPSPath(t *testing.T) {
	t.Parallel()
	// Regression: ValidateWithLogger(nil) must behave identically to Validate()
	// for the https success path (no warning to emit, so no logger needed).
	cfg := registryBaseCfg()
	cfg.RegistryEndpoint = "https://registry.example.com:25777"
	if err := cfg.ValidateWithLogger(nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// --------------------------------------------------------------------------
// Hotplug / NUMA accessors
// --------------------------------------------------------------------------

// TestHotplugValue_DefaultsAndOverrides covers all three states for the
// Hotplug *string field: nil pointer (returns canonical default), explicit
// override survives ApplyDefaults, and the empty-string "disabled" override
// is honored verbatim.
func TestHotplugValue_DefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	t.Run("nil pointer returns canonical default", func(t *testing.T) {
		var cfg config.CPIConfig
		if got := cfg.HotplugValue(); got != "network,disk,cpu,memory" {
			t.Errorf("HotplugValue() = %q, want %q", got, "network,disk,cpu,memory")
		}
	})

	t.Run("ApplyDefaults populates pointer when nil", func(t *testing.T) {
		var cfg config.CPIConfig
		cfg.VMStorage = "vm-store"
		cfg.ApplyDefaults()
		if cfg.Hotplug == nil {
			t.Fatal("Hotplug pointer remained nil after ApplyDefaults")
		}
		if got := cfg.HotplugValue(); got != "network,disk,cpu,memory" {
			t.Errorf("HotplugValue() after ApplyDefaults = %q, want %q", got, "network,disk,cpu,memory")
		}
	})

	t.Run("explicit override survives ApplyDefaults", func(t *testing.T) {
		var cfg config.CPIConfig
		cfg.VMStorage = "vm-store"
		custom := "network,disk"
		cfg.Hotplug = &custom
		cfg.ApplyDefaults()
		if got := cfg.HotplugValue(); got != "network,disk" {
			t.Errorf("HotplugValue() = %q, want %q (explicit override must survive)", got, "network,disk")
		}
	})

	t.Run("explicit disabled (\"0\") survives ApplyDefaults", func(t *testing.T) {
		var cfg config.CPIConfig
		cfg.VMStorage = "vm-store"
		disabled := "0"
		cfg.Hotplug = &disabled
		cfg.ApplyDefaults()
		if got := cfg.HotplugValue(); got != "0" {
			t.Errorf("HotplugValue() = %q, want %q (\"0\" must survive)", got, "0")
		}
	})

	t.Run("explicit empty string survives ApplyDefaults", func(t *testing.T) {
		// Empty string is a legitimate caller-supplied value distinct from
		// nil — the pointer is non-nil but its target is empty. ApplyDefaults
		// must not overwrite it.
		var cfg config.CPIConfig
		cfg.VMStorage = "vm-store"
		empty := ""
		cfg.Hotplug = &empty
		cfg.ApplyDefaults()
		if got := cfg.HotplugValue(); got != "" {
			t.Errorf("HotplugValue() = %q, want empty (caller-supplied empty must survive)", got)
		}
	})

	t.Run("JSON omission produces default via Load", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br"
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := cfg.HotplugValue(); got != "network,disk,cpu,memory" {
			t.Errorf("HotplugValue() from Load = %q, want default", got)
		}
	})

	t.Run("JSON explicit override survives Load", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"hotplug":"cpu,memory"
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := cfg.HotplugValue(); got != "cpu,memory" {
			t.Errorf("HotplugValue() from Load = %q, want %q", got, "cpu,memory")
		}
	})
}

// TestNUMAValue_DefaultTrue verifies NUMAValue defaults to true (memory hotplug
// requires numa=1 at create time) and that an explicit false override is
// preserved through ApplyDefaults.
func TestNUMAValue_DefaultTrue(t *testing.T) {
	t.Parallel()
	t.Run("nil pointer returns true", func(t *testing.T) {
		var cfg config.CPIConfig
		if !cfg.NUMAValue() {
			t.Error("NUMAValue() = false on nil pointer, want true")
		}
	})

	t.Run("ApplyDefaults sets pointer to *true when nil", func(t *testing.T) {
		var cfg config.CPIConfig
		cfg.VMStorage = "vm-store"
		cfg.ApplyDefaults()
		if cfg.NUMA == nil {
			t.Fatal("NUMA pointer remained nil after ApplyDefaults")
		}
		if !cfg.NUMAValue() {
			t.Error("NUMAValue() = false after ApplyDefaults, want true")
		}
	})

	t.Run("explicit false survives ApplyDefaults", func(t *testing.T) {
		var cfg config.CPIConfig
		cfg.VMStorage = "vm-store"
		cfg.NUMA = boolPtr(false)
		cfg.ApplyDefaults()
		if cfg.NUMAValue() {
			t.Error("NUMAValue() = true after ApplyDefaults overrode explicit *false")
		}
	})

	t.Run("explicit true survives ApplyDefaults", func(t *testing.T) {
		var cfg config.CPIConfig
		cfg.VMStorage = "vm-store"
		cfg.NUMA = boolPtr(true)
		cfg.ApplyDefaults()
		if !cfg.NUMAValue() {
			t.Error("NUMAValue() = false after ApplyDefaults overrode explicit *true")
		}
	})

	t.Run("JSON explicit false survives Load", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"numa":false
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.NUMAValue() {
			t.Error("NUMAValue() = true after Load decoded explicit false")
		}
	})

	t.Run("JSON omission defaults to true via Load", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br"
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.NUMAValue() {
			t.Error("NUMAValue() = false after Load with numa absent, want true")
		}
	})
}

// --------------------------------------------------------------------------
// TestValidate_StemcellStagingDir
// --------------------------------------------------------------------------

// TestValidate_StemcellStagingDir_EmptyDefault verifies that an omitted
// stemcell_staging_dir leaves StemcellStagingDir empty and passes validation
// unchanged — byte-identical behavior to prior releases when the field is absent.
func TestValidate_StemcellStagingDir_EmptyDefault(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StemcellStagingDir != "" {
		t.Errorf("StemcellStagingDir = %q; want empty string (default)", cfg.StemcellStagingDir)
	}
}

// TestValidate_StemcellStagingDir_ValidDir verifies that an absolute path to
// an existing directory passes validation and is stored correctly.
func TestValidate_StemcellStagingDir_ValidDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"stemcell_staging_dir":"`+dir+`"
	}`)
	if err != nil {
		t.Fatalf("unexpected error for valid stemcell_staging_dir: %v", err)
	}
	if cfg.StemcellStagingDir != dir {
		t.Errorf("StemcellStagingDir = %q; want %q", cfg.StemcellStagingDir, dir)
	}
}

// TestValidate_StemcellStagingDir_RelativePath verifies that a relative path
// is rejected with a clear error message.
func TestValidate_StemcellStagingDir_RelativePath(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"stemcell_staging_dir":"relative/path"
	}`)
	assertCloudError(t, err, "stemcell_staging_dir")
	assertCloudError(t, err, "absolute path")
}

// TestValidate_StemcellStagingDir_NonExistent verifies that a non-existent
// absolute path is rejected with a clear error message.
func TestValidate_StemcellStagingDir_NonExistent(t *testing.T) {
	t.Parallel()
	// Create a temp dir, then remove it so the path is guaranteed absent on this OS.
	absentDir := t.TempDir()
	if err := os.Remove(absentDir); err != nil {
		t.Fatalf("remove temp dir: %v", err)
	}
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"stemcell_staging_dir":"`+absentDir+`"
	}`)
	assertCloudError(t, err, "stemcell_staging_dir")
	assertCloudError(t, err, "does not exist")
}

// TestValidate_StemcellStagingDir_IsFile verifies that a path that exists but
// is a regular file (not a directory) is rejected.
func TestValidate_StemcellStagingDir_IsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmpFile, ferr := os.Create(dir + "/not-a-dir")
	if ferr != nil {
		t.Fatalf("create temp file: %v", ferr)
	}
	_ = tmpFile.Close()

	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"stemcell_staging_dir":"`+tmpFile.Name()+`"
	}`)
	assertCloudError(t, err, "stemcell_staging_dir")
	assertCloudError(t, err, "not a directory")
}

// --------------------------------------------------------------------------
// TestValidate_PVECACertPEM
// --------------------------------------------------------------------------

// selfSignedCAPEMForConfigTest generates a minimal self-signed CA certificate
// and returns it as a PEM string. For use in config validation tests only.
func selfSignedCAPEMForConfigTest(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestValidate_PVECACertPEM_Empty verifies that an absent pve_ca_cert field
// passes validation unchanged — behavior is byte-identical to prior releases.
func TestValidate_PVECACertPEM_Empty(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("empty pve_ca_cert: expected no error, got: %v", err)
	}
}

// TestValidate_PVECACertPEM_ValidPEM verifies that a well-formed PEM CA bundle
// passes validation when verify_ssl is true (the default).
func TestValidate_PVECACertPEM_ValidPEM(t *testing.T) {
	t.Parallel()
	caPEM := selfSignedCAPEMForConfigTest(t)
	// JSON-encode the PEM so it is safely embedded in the JSON literal.
	pemJSON, _ := json.Marshal(caPEM)
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"pve_ca_cert":`+string(pemJSON)+`
	}`)
	if err != nil {
		t.Fatalf("valid pve_ca_cert: expected no error, got: %v", err)
	}
}

// TestValidate_PVECACertPEM_Malformed verifies that a malformed PEM value is
// rejected at validation time with a descriptive error message.
func TestValidate_PVECACertPEM_Malformed(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"pve_ca_cert":"not-a-valid-pem-string"
	}`)
	assertCloudError(t, err, "pve_ca_cert")
	assertCloudError(t, err, "no valid PEM certificates parsed")
}

// TestValidate_PVECACertPEM_IgnoredWhenVerifySSLFalse verifies that a malformed
// PEM is silently ignored when verify_ssl=false (insecure-skip-verify path).
// This confirms the CA cert is not parsed at all when TLS verification is off.
func TestValidate_PVECACertPEM_IgnoredWhenVerifySSLFalse(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"verify_ssl":false,
		"pve_ca_cert":"not-a-valid-pem-string"
	}`)
	if err != nil {
		t.Fatalf("malformed pve_ca_cert with verify_ssl=false: expected no error, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// RegistryAllowPrivateIP field: accessor, JSON decode, validation warning.
// --------------------------------------------------------------------------

// TestRegistryAllowPrivateIPValue_Nil verifies nil pointer → false (guard active).
func TestRegistryAllowPrivateIPValue_Nil(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	if cfg.RegistryAllowPrivateIPValue() {
		t.Error("RegistryAllowPrivateIPValue() = true on nil pointer, want false")
	}
}

// TestRegistryAllowPrivateIPValue_True verifies *true → true (guard disabled).
func TestRegistryAllowPrivateIPValue_True(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{RegistryAllowPrivateIP: boolPtr(true)}
	if !cfg.RegistryAllowPrivateIPValue() {
		t.Error("RegistryAllowPrivateIPValue() = false for *true, want true")
	}
}

// TestRegistryAllowPrivateIPValue_False verifies *false → false (explicit; same as nil).
func TestRegistryAllowPrivateIPValue_False(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{RegistryAllowPrivateIP: boolPtr(false)}
	if cfg.RegistryAllowPrivateIPValue() {
		t.Error("RegistryAllowPrivateIPValue() = true for *false, want false")
	}
}

// TestValidate_RegistryAllowPrivateIP_Unset verifies that omitting
// registry_allow_private_ip from JSON leaves the pointer nil, passes
// validation without error, and RegistryAllowPrivateIPValue() returns false.
func TestValidate_RegistryAllowPrivateIP_Unset(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"agent_mode":"registry",
		"registry_endpoint":"https://registry.example.com:25777",
		"registry_user":"ru","registry_password":"rp"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RegistryAllowPrivateIP != nil {
		t.Errorf("RegistryAllowPrivateIP = %v, want nil (field absent from JSON)", cfg.RegistryAllowPrivateIP)
	}
	if cfg.RegistryAllowPrivateIPValue() {
		t.Error("RegistryAllowPrivateIPValue() = true when field absent, want false")
	}
}

// TestValidate_RegistryAllowPrivateIP_True verifies that registry_allow_private_ip=true
// is accepted, the pointer is non-nil and true, and a warning log entry is emitted.
func TestValidate_RegistryAllowPrivateIP_True(t *testing.T) {
	t.Parallel()
	cfg := registryBaseCfg()
	cfg.RegistryEndpoint = "https://registry.example.com:25777"
	cfg.RegistryAllowPrivateIP = boolPtr(true)

	logger, obs := log.NewObservedLogger(log.LevelWarn)
	if err := cfg.ValidateWithLogger(logger); err != nil {
		t.Fatalf("expected nil error with allow_private_ip=true, got %v", err)
	}

	// Exactly one warn entry must be emitted mentioning the opt-in flag.
	entries := obs.All()
	found := false
	for _, e := range entries {
		if strings.Contains(e.Message, "registry_allow_private_ip=true") {
			found = true
			ep, ok := e.Attrs["endpoint"]
			if !ok {
				t.Errorf("warn entry missing 'endpoint' attribute: %+v", e)
			}
			if epStr, _ := ep.(string); !strings.Contains(epStr, "registry.example.com") {
				t.Errorf("endpoint attr %q lost host", epStr)
			}
		}
	}
	if !found {
		t.Errorf("no warn entry matched registry_allow_private_ip=true; entries=%+v", entries)
	}
}

// TestValidate_RegistryAllowPrivateIP_False verifies that registry_allow_private_ip=false
// is accepted and no warning is emitted (same effective behavior as nil).
func TestValidate_RegistryAllowPrivateIP_False(t *testing.T) {
	t.Parallel()
	cfg := registryBaseCfg()
	cfg.RegistryEndpoint = "https://registry.example.com:25777"
	cfg.RegistryAllowPrivateIP = boolPtr(false)

	logger, obs := log.NewObservedLogger(log.LevelWarn)
	if err := cfg.ValidateWithLogger(logger); err != nil {
		t.Fatalf("expected nil error with allow_private_ip=false, got %v", err)
	}

	// Explicit false must not emit the warning (guard is active, no operator action needed).
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "registry_allow_private_ip") {
			t.Errorf("unexpected warn entry for explicit false: %+v", e)
		}
	}
}

// TestValidate_RegistryAllowPrivateIP_NonRegistryModeIgnored verifies that the
// allow_private_ip field is not evaluated when agent_mode != registry (the entire
// validateRegistryConfig guard fires only in registry mode).
func TestValidate_RegistryAllowPrivateIP_NonRegistryModeIgnored(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"agent_mode":"cloudinit",
		"registry_allow_private_ip":true
	}`)
	if err != nil {
		t.Fatalf("unexpected error in cloudinit mode with registry_allow_private_ip=true: %v", err)
	}
	if !cfg.RegistryAllowPrivateIPValue() {
		t.Error("RegistryAllowPrivateIPValue() = false but JSON set it to true")
	}
}

// TestLoad_RegistryAllowPrivateIP_JSONRoundTrip verifies the field decodes
// correctly from JSON when present.
func TestLoad_RegistryAllowPrivateIP_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"agent_mode":"registry",
		"registry_endpoint":"https://registry.example.com:25777",
		"registry_user":"ru","registry_password":"rp",
		"registry_allow_private_ip":true
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RegistryAllowPrivateIP == nil {
		t.Fatal("RegistryAllowPrivateIP = nil, want *true")
	}
	if !*cfg.RegistryAllowPrivateIP {
		t.Error("*RegistryAllowPrivateIP = false, want true")
	}
}

// --------------------------------------------------------------------------
// Template config fields — ApplyDefaults
// --------------------------------------------------------------------------

// TestApplyDefaults_TemplateVMIDRange verifies that ApplyDefaults fills in
// StemcellTemplateVMIDRangeStart=30000 and StemcellTemplateVMIDRangeEnd=30999
// when both are zero (fields absent from JSON).
func TestApplyDefaults_TemplateVMIDRange(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.StemcellTemplateVMIDRangeStart != 30000 {
		t.Errorf("StemcellTemplateVMIDRangeStart = %d, want 30000 (default)", cfg.StemcellTemplateVMIDRangeStart)
	}
	if cfg.StemcellTemplateVMIDRangeEnd != 30999 {
		t.Errorf("StemcellTemplateVMIDRangeEnd = %d, want 30999 (default)", cfg.StemcellTemplateVMIDRangeEnd)
	}
}

// TestApplyDefaults_TemplateVMIDRangePreservesSet verifies that operator-supplied
// values for both range fields are not overwritten by ApplyDefaults.
func TestApplyDefaults_TemplateVMIDRangePreservesSet(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.StemcellTemplateVMIDRangeStart = 6500
	cfg.StemcellTemplateVMIDRangeEnd = 7500
	cfg.ApplyDefaults()

	if cfg.StemcellTemplateVMIDRangeStart != 6500 {
		t.Errorf("StemcellTemplateVMIDRangeStart = %d, want 6500 (must not overwrite)", cfg.StemcellTemplateVMIDRangeStart)
	}
	if cfg.StemcellTemplateVMIDRangeEnd != 7500 {
		t.Errorf("StemcellTemplateVMIDRangeEnd = %d, want 7500 (must not overwrite)", cfg.StemcellTemplateVMIDRangeEnd)
	}
}

// TestApplyDefaults_CloneMode verifies that ApplyDefaults sets CloneMode to
// "auto" when the field is empty (absent from JSON).
func TestApplyDefaults_CloneMode(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.CloneMode != "auto" {
		t.Errorf("CloneMode = %q, want %q (default)", cfg.CloneMode, "auto")
	}
}

// TestApplyDefaults_CloneModePreservesSet verifies that an explicit CloneMode
// value is not overwritten by ApplyDefaults.
func TestApplyDefaults_CloneModePreservesSet(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"linked", "full", "auto"} {
		t.Run(mode, func(t *testing.T) {
			var cfg config.CPIConfig
			cfg.VMStorage = "vm-store"
			cfg.CloneMode = mode
			cfg.ApplyDefaults()
			if cfg.CloneMode != mode {
				t.Errorf("CloneMode = %q, want %q (explicit value must survive ApplyDefaults)", cfg.CloneMode, mode)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Template config fields — Validate
// --------------------------------------------------------------------------

// TestValidate_CloneMode_Invalid verifies that an unrecognized clone_mode value
// is rejected with a clear error message.
func TestValidate_CloneMode_Invalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"clone_mode":"snapshot"
	}`)
	assertCloudError(t, err, "clone_mode must be one of auto|linked|full")
	assertCloudError(t, err, `"snapshot"`)
}

// TestValidate_CloneMode_ValidValues verifies the three accepted clone_mode values.
func TestValidate_CloneMode_ValidValues(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", "linked", "full"} {
		t.Run(mode, func(t *testing.T) {
			_, err := mustLoad(t, `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"clone_mode":"`+mode+`"
			}`)
			if err != nil {
				t.Errorf("clone_mode=%q: unexpected error: %v", mode, err)
			}
		})
	}
}

// TestValidate_CloneMode_OmittedGetsAuto verifies that omitting clone_mode
// results in "auto" after Load applies defaults (no validation error).
func TestValidate_CloneMode_OmittedGetsAuto(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CloneMode != "auto" {
		t.Errorf("CloneMode = %q, want %q (default when omitted)", cfg.CloneMode, "auto")
	}
}

// TestValidate_TemplateRange_StartGEEnd verifies that start >= end is rejected.
func TestValidate_TemplateRange_StartGEEnd(t *testing.T) {
	t.Parallel()
	// Build a config manually so we can set start == end after defaults apply.
	cfg := &config.CPIConfig{
		Host:                           "h",
		User:                           "u",
		Password:                       "p",
		VMStorage:                      "s",
		DiskStorage:                    "s",
		NetworkBridge:                  "br",
		Port:                           8006,
		VerifySSL:                      boolPtr(true),
		AgentMode:                      "cloudinit",
		VMDiskFormat:                   "qcow2",
		LogLevel:                       "info",
		VMIDRangeStart:                 100,
		VMIDRangeEnd:                   5999,
		RebootMode:                     "soft",
		RebootTimeout:                  60,
		NetworkMode:                    "auto",
		SDNZoneType:                    "simple",
		CloneMode:                      "auto",
		StemcellTemplateVMIDRangeStart: 7000,
		StemcellTemplateVMIDRangeEnd:   7000, // equal to start — invalid
	}
	assertCloudError(t, cfg.Validate(), "stemcell_template_vmid_range_start")
}

// TestValidate_TemplateRange_EndTooHigh verifies that end > 999999999 is rejected.
func TestValidate_TemplateRange_EndTooHigh(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:                           "h",
		User:                           "u",
		Password:                       "p",
		VMStorage:                      "s",
		DiskStorage:                    "s",
		NetworkBridge:                  "br",
		Port:                           8006,
		VerifySSL:                      boolPtr(true),
		AgentMode:                      "cloudinit",
		VMDiskFormat:                   "qcow2",
		LogLevel:                       "info",
		VMIDRangeStart:                 100,
		VMIDRangeEnd:                   5999,
		RebootMode:                     "soft",
		RebootTimeout:                  60,
		NetworkMode:                    "auto",
		SDNZoneType:                    "simple",
		CloneMode:                      "auto",
		StemcellTemplateVMIDRangeStart: 30000,
		StemcellTemplateVMIDRangeEnd:   1000000000, // above PVE max — invalid
	}
	assertCloudError(t, cfg.Validate(), "stemcell_template_vmid_range_end must be ≤999999999")
}

// TestValidate_TemplateRange_StartTooLow verifies that start < 100 is rejected.
func TestValidate_TemplateRange_StartTooLow(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:                           "h",
		User:                           "u",
		Password:                       "p",
		VMStorage:                      "s",
		DiskStorage:                    "s",
		NetworkBridge:                  "br",
		Port:                           8006,
		VerifySSL:                      boolPtr(true),
		AgentMode:                      "cloudinit",
		VMDiskFormat:                   "qcow2",
		LogLevel:                       "info",
		VMIDRangeStart:                 100,
		VMIDRangeEnd:                   5999,
		RebootMode:                     "soft",
		RebootTimeout:                  60,
		NetworkMode:                    "auto",
		SDNZoneType:                    "simple",
		CloneMode:                      "auto",
		StemcellTemplateVMIDRangeStart: 50, // below PVE minimum — invalid
		StemcellTemplateVMIDRangeEnd:   8999,
	}
	assertCloudError(t, cfg.Validate(), "stemcell_template_vmid_range_start must be ≥100")
}

// TestValidate_TemplateRange_OverlapsVMRange verifies that a template range
// overlapping the VM VMID range is rejected.
func TestValidate_TemplateRange_OverlapsVMRange(t *testing.T) {
	t.Parallel()
	// VM range 3000-5999; template range 4000-7000 → overlap at [4000,5999].
	cfg := &config.CPIConfig{
		Host:                           "h",
		User:                           "u",
		Password:                       "p",
		VMStorage:                      "s",
		DiskStorage:                    "s",
		NetworkBridge:                  "br",
		Port:                           8006,
		VerifySSL:                      boolPtr(true),
		AgentMode:                      "cloudinit",
		VMDiskFormat:                   "qcow2",
		LogLevel:                       "info",
		VMIDRangeStart:                 3000,
		VMIDRangeEnd:                   5999,
		RebootMode:                     "soft",
		RebootTimeout:                  60,
		NetworkMode:                    "auto",
		SDNZoneType:                    "simple",
		CloneMode:                      "auto",
		StemcellTemplateVMIDRangeStart: 4000, // overlaps VM range
		StemcellTemplateVMIDRangeEnd:   7000,
	}
	assertCloudError(t, cfg.Validate(), "overlaps VM VMID range")
}

// TestValidate_TemplateRange_OverlapsDiskRange verifies that a template range
// overlapping the persistent disk range 9000-29999 is rejected.
func TestValidate_TemplateRange_OverlapsDiskRange(t *testing.T) {
	t.Parallel()
	// Template range 9500-9800 sits inside disk range [9000,29999] — pure overlap,
	// no end-too-high error (9800 ≤ 999999999).
	cfg := &config.CPIConfig{
		Host:                           "h",
		User:                           "u",
		Password:                       "p",
		VMStorage:                      "s",
		DiskStorage:                    "s",
		NetworkBridge:                  "br",
		Port:                           8006,
		VerifySSL:                      boolPtr(true),
		AgentMode:                      "cloudinit",
		VMDiskFormat:                   "qcow2",
		LogLevel:                       "info",
		VMIDRangeStart:                 100,
		VMIDRangeEnd:                   5999,
		RebootMode:                     "soft",
		RebootTimeout:                  60,
		NetworkMode:                    "auto",
		SDNZoneType:                    "simple",
		CloneMode:                      "auto",
		StemcellTemplateVMIDRangeStart: 9500,
		StemcellTemplateVMIDRangeEnd:   9800, // entirely within disk range
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for template range overlapping disk range, got nil")
	}
	if !strings.Contains(err.Error(), "disk range") {
		t.Errorf("error %q missing disk range overlap message", err.Error())
	}
}

// TestValidate_TemplateRange_ValidDefaults verifies that the default values
// (30000-30999) pass validation alongside the default VM range (100-8999)
// and do not overlap either the VM range or the disk range.
func TestValidate_TemplateRange_ValidDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("default template range: unexpected validation error: %v", err)
	}
	if cfg.StemcellTemplateVMIDRangeStart != 30000 {
		t.Errorf("StemcellTemplateVMIDRangeStart = %d, want 30000", cfg.StemcellTemplateVMIDRangeStart)
	}
	if cfg.StemcellTemplateVMIDRangeEnd != 30999 {
		t.Errorf("StemcellTemplateVMIDRangeEnd = %d, want 30999", cfg.StemcellTemplateVMIDRangeEnd)
	}
}

// TestValidate_TemplateRange_ValidOperatorOverride verifies that a valid
// operator-supplied template range passes all constraints.
func TestValidate_TemplateRange_ValidOperatorOverride(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"stemcell_template_vmid_range_start":40000,
		"stemcell_template_vmid_range_end":41000
	}`)
	if err != nil {
		t.Fatalf("valid operator template range: unexpected error: %v", err)
	}
	if cfg.StemcellTemplateVMIDRangeStart != 40000 {
		t.Errorf("StemcellTemplateVMIDRangeStart = %d, want 40000", cfg.StemcellTemplateVMIDRangeStart)
	}
	if cfg.StemcellTemplateVMIDRangeEnd != 41000 {
		t.Errorf("StemcellTemplateVMIDRangeEnd = %d, want 41000", cfg.StemcellTemplateVMIDRangeEnd)
	}
}

// TestValidate_TemplatePool_AcceptsNonEmpty verifies that a non-empty
// stemcell_template_pool passes validation (validate-only-when-set; any
// non-empty string is accepted — PVE validates pool existence at assignment).
func TestValidate_TemplatePool_AcceptsNonEmpty(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"stemcell_template_pool":"bosh-templates"
	}`)
	if err != nil {
		t.Fatalf("stemcell_template_pool non-empty: unexpected error: %v", err)
	}
	if cfg.StemcellTemplatePool != "bosh-templates" {
		t.Errorf("StemcellTemplatePool = %q, want %q", cfg.StemcellTemplatePool, "bosh-templates")
	}
}

// TestValidate_TemplateNode_AcceptsNonEmpty verifies that a non-empty
// stemcell_template_node passes validation (validate-only-when-set).
func TestValidate_TemplateNode_AcceptsNonEmpty(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"stemcell_template_node":"pve-node-1"
	}`)
	if err != nil {
		t.Fatalf("stemcell_template_node non-empty: unexpected error: %v", err)
	}
	if cfg.StemcellTemplateNode != "pve-node-1" {
		t.Errorf("StemcellTemplateNode = %q, want %q", cfg.StemcellTemplateNode, "pve-node-1")
	}
}

// TestValidate_TemplateFields_AllAbsent verifies that omitting all five new
// fields still produces a valid config (additive-only; zero impact when absent).
func TestValidate_TemplateFields_AllAbsent(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("all template fields absent: unexpected error: %v", err)
	}
	// ApplyDefaults must fill in all three with-default fields.
	if cfg.CloneMode != "auto" {
		t.Errorf("CloneMode = %q, want %q", cfg.CloneMode, "auto")
	}
	if cfg.StemcellTemplateVMIDRangeStart != 30000 {
		t.Errorf("StemcellTemplateVMIDRangeStart = %d, want 30000", cfg.StemcellTemplateVMIDRangeStart)
	}
	if cfg.StemcellTemplateVMIDRangeEnd != 30999 {
		t.Errorf("StemcellTemplateVMIDRangeEnd = %d, want 30999", cfg.StemcellTemplateVMIDRangeEnd)
	}
	// Pool and node default to empty (no default set).
	if cfg.StemcellTemplatePool != "" {
		t.Errorf("StemcellTemplatePool = %q, want empty (default)", cfg.StemcellTemplatePool)
	}
	if cfg.StemcellTemplateNode != "" {
		t.Errorf("StemcellTemplateNode = %q, want empty (default)", cfg.StemcellTemplateNode)
	}
}

// --------------------------------------------------------------------------
// Fixed template VMID band — independence from vmid_range_end
// --------------------------------------------------------------------------

// TestApplyDefaults_TemplateRangeFixedBand verifies that the template default
// band [30000,30999] is fixed and does NOT track vmid_range_end. A config with
// vmid_range_end=7000 and no template props must still yield Start=30000,
// End=30999. The adaptive derivation (VMIDRangeEnd+1) has been removed.
func TestApplyDefaults_TemplateRangeFixedBand(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vmid_range_end":7000
	}`)
	if err != nil {
		t.Fatalf("vmid_range_end=7000 with no template props: unexpected error: %v", err)
	}
	if cfg.VMIDRangeEnd != 7000 {
		t.Errorf("VMIDRangeEnd = %d, want 7000", cfg.VMIDRangeEnd)
	}
	// Template default is the fixed band [30000,30999], independent of vmid_range_end.
	if cfg.StemcellTemplateVMIDRangeStart != 30000 {
		t.Errorf("StemcellTemplateVMIDRangeStart = %d, want 30000 (fixed band, not VMIDRangeEnd+1)", cfg.StemcellTemplateVMIDRangeStart)
	}
	if cfg.StemcellTemplateVMIDRangeEnd != 30999 {
		t.Errorf("StemcellTemplateVMIDRangeEnd = %d, want 30999", cfg.StemcellTemplateVMIDRangeEnd)
	}
}

// TestLoad_HighVMIDRangeEnd_NoTemplateProps verifies that vmid_range_end at the
// maximum (8999, one below the disk range floor) with NO template props loads
// cleanly. Templates default to [30000,30999] regardless of vmid_range_end, so
// even a maxed-out VM range causes no conflict.
func TestLoad_HighVMIDRangeEnd_NoTemplateProps(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vmid_range_end":8999
	}`)
	if err != nil {
		t.Fatalf("vmid_range_end=8999 no template props: unexpected error: %v", err)
	}
	if cfg.VMIDRangeEnd != 8999 {
		t.Errorf("VMIDRangeEnd = %d, want 8999", cfg.VMIDRangeEnd)
	}
	// Fixed band: independent of vmid_range_end.
	if cfg.StemcellTemplateVMIDRangeStart != 30000 {
		t.Errorf("StemcellTemplateVMIDRangeStart = %d, want 30000", cfg.StemcellTemplateVMIDRangeStart)
	}
	if cfg.StemcellTemplateVMIDRangeEnd != 30999 {
		t.Errorf("StemcellTemplateVMIDRangeEnd = %d, want 30999", cfg.StemcellTemplateVMIDRangeEnd)
	}
}

// TestLoad_HighVMIDRangeEnd_ExplicitTemplateRange verifies that an explicit
// non-overlapping template range coexists with a high vmid_range_end.
// vmid_range_end=8000 + explicit stemcell_template_vmid_range_start=8001,end=8999
// must be valid.
func TestLoad_HighVMIDRangeEnd_ExplicitTemplateRange(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vmid_range_end":8000,
		"stemcell_template_vmid_range_start":8001,
		"stemcell_template_vmid_range_end":8999
	}`)
	if err != nil {
		t.Fatalf("vmid_range_end=8000 explicit template [8001,8999]: unexpected error: %v", err)
	}
	if cfg.VMIDRangeEnd != 8000 {
		t.Errorf("VMIDRangeEnd = %d, want 8000", cfg.VMIDRangeEnd)
	}
	if cfg.StemcellTemplateVMIDRangeStart != 8001 {
		t.Errorf("StemcellTemplateVMIDRangeStart = %d, want 8001", cfg.StemcellTemplateVMIDRangeStart)
	}
	if cfg.StemcellTemplateVMIDRangeEnd != 8999 {
		t.Errorf("StemcellTemplateVMIDRangeEnd = %d, want 8999", cfg.StemcellTemplateVMIDRangeEnd)
	}
}

// --------------------------------------------------------------------------
// Persistent-disk VMID range (operator-configurable)
// --------------------------------------------------------------------------

// TestApplyDefaults_DiskVMIDRange verifies the disk range defaults to
// [9000,29999] when both bounds are zero.
func TestApplyDefaults_DiskVMIDRange(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.DiskVMIDRangeStart != 9000 {
		t.Errorf("DiskVMIDRangeStart = %d, want 9000 (default)", cfg.DiskVMIDRangeStart)
	}
	if cfg.DiskVMIDRangeEnd != 29999 {
		t.Errorf("DiskVMIDRangeEnd = %d, want 29999 (default)", cfg.DiskVMIDRangeEnd)
	}
}

// TestApplyDefaults_DiskVMIDRangePreservesSet verifies operator-supplied disk
// range bounds are not overwritten by ApplyDefaults.
func TestApplyDefaults_DiskVMIDRangePreservesSet(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.DiskVMIDRangeStart = 50000
	cfg.DiskVMIDRangeEnd = 69999
	cfg.ApplyDefaults()

	if cfg.DiskVMIDRangeStart != 50000 {
		t.Errorf("DiskVMIDRangeStart = %d, want 50000 (must not overwrite)", cfg.DiskVMIDRangeStart)
	}
	if cfg.DiskVMIDRangeEnd != 69999 {
		t.Errorf("DiskVMIDRangeEnd = %d, want 69999 (must not overwrite)", cfg.DiskVMIDRangeEnd)
	}
}

// TestValidate_DiskRange_ValidDefaults verifies the defaulted disk range passes
// validation alongside the default VM and template ranges.
func TestValidate_DiskRange_ValidDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("default disk range: unexpected validation error: %v", err)
	}
	if cfg.DiskVMIDRangeStart != 9000 || cfg.DiskVMIDRangeEnd != 29999 {
		t.Errorf("disk range = [%d,%d], want [9000,29999]", cfg.DiskVMIDRangeStart, cfg.DiskVMIDRangeEnd)
	}
}

// TestValidate_DiskRange_ValidOperatorOverride verifies a relocated, non-overlapping
// disk range is accepted.
func TestValidate_DiskRange_ValidOperatorOverride(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"disk_vmid_range_start":50000,
		"disk_vmid_range_end":69999
	}`)
	if err != nil {
		t.Fatalf("valid operator disk range: unexpected error: %v", err)
	}
	if cfg.DiskVMIDRangeStart != 50000 || cfg.DiskVMIDRangeEnd != 69999 {
		t.Errorf("disk range = [%d,%d], want [50000,69999]", cfg.DiskVMIDRangeStart, cfg.DiskVMIDRangeEnd)
	}
}

// TestValidate_DiskRange_OverlapsVMRange verifies a disk range overlapping the
// VM range is rejected.
func TestValidate_DiskRange_OverlapsVMRange(t *testing.T) {
	t.Parallel()
	// Default VM range [100,8999]; disk range [5000,6000] sits inside it.
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"disk_vmid_range_start":5000,
		"disk_vmid_range_end":6000
	}`)
	if err == nil {
		t.Fatal("expected error for disk range overlapping VM range, got nil")
	}
	if !strings.Contains(err.Error(), "overlaps VM VMID range") {
		t.Errorf("error %q missing VM-overlap message", err.Error())
	}
}

// TestValidate_DiskRange_OverlapsTemplateRange verifies a disk range overlapping
// the (default) template range is rejected.
func TestValidate_DiskRange_OverlapsTemplateRange(t *testing.T) {
	t.Parallel()
	// Default template range [30000,30999]; disk range [30500,31000] overlaps it.
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"disk_vmid_range_start":30500,
		"disk_vmid_range_end":31000
	}`)
	if err == nil {
		t.Fatal("expected error for disk range overlapping template range, got nil")
	}
	if !strings.Contains(err.Error(), "disk range") {
		t.Errorf("error %q missing disk-range overlap message", err.Error())
	}
}

// TestValidate_DiskRange_EndBelowStart verifies end <= start is rejected.
func TestValidate_DiskRange_EndBelowStart(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"disk_vmid_range_start":9000,
		"disk_vmid_range_end":8000
	}`)
	if err == nil {
		t.Fatal("expected error for disk_vmid_range_end <= start, got nil")
	}
	if !strings.Contains(err.Error(), "disk_vmid_range_end must be > disk_vmid_range_start") {
		t.Errorf("error %q missing disk end>start message", err.Error())
	}
}

// TestValidate_DiskRange_StartTooLow verifies start < 100 is rejected.
func TestValidate_DiskRange_StartTooLow(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"disk_vmid_range_start":50,
		"disk_vmid_range_end":8000
	}`)
	if err == nil {
		t.Fatal("expected error for disk_vmid_range_start < 100, got nil")
	}
	if !strings.Contains(err.Error(), "disk_vmid_range_start must be ≥100") {
		t.Errorf("error %q missing disk start>=100 message", err.Error())
	}
}

// TestValidate_AllThreeRanges_Relocated verifies that with every band moved to a
// custom, non-overlapping location — including a VM range that grows past the
// default disk floor — validation passes. This exercises the relaxed VM ceiling.
func TestValidate_AllThreeRanges_Relocated(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vmid_range_end":40000,
		"disk_vmid_range_start":50000,
		"disk_vmid_range_end":69999,
		"stemcell_template_vmid_range_start":80000,
		"stemcell_template_vmid_range_end":80999
	}`)
	if err != nil {
		t.Fatalf("relocated non-overlapping bands: unexpected error: %v", err)
	}
	if cfg.VMIDRangeEnd != 40000 {
		t.Errorf("VMIDRangeEnd = %d, want 40000 (no hard ceiling)", cfg.VMIDRangeEnd)
	}
}

// TestValidate_DiskFields_Absent verifies omitting the disk range still yields a
// valid, defaulted config.
func TestValidate_DiskFields_Absent(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("disk fields absent: unexpected error: %v", err)
	}
	if cfg.DiskVMIDRangeStart != 9000 || cfg.DiskVMIDRangeEnd != 29999 {
		t.Errorf("disk range = [%d,%d], want default [9000,29999]", cfg.DiskVMIDRangeStart, cfg.DiskVMIDRangeEnd)
	}
}

// --------------------------------------------------------------------------
// Placement config tests
// --------------------------------------------------------------------------

// TestPlacementEnabled_NilPlacement verifies PlacementEnabled returns true
// when the entire Placement block is absent (DEC-2: fully protective default).
func TestPlacementEnabled_NilPlacement(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	if !cfg.PlacementEnabled() {
		t.Error("PlacementEnabled() = false with nil Placement, want true (protective default)")
	}
}

// TestPlacementEnabled_NilEnabled verifies PlacementEnabled returns true when
// Placement block is present but Enabled field is nil.
func TestPlacementEnabled_NilEnabled(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{},
	}
	if !cfg.PlacementEnabled() {
		t.Error("PlacementEnabled() = false with Placement present but Enabled nil, want true")
	}
}

// TestPlacementEnabled_ExplicitFalse verifies PlacementEnabled returns false
// when the operator explicitly disables scoring.
func TestPlacementEnabled_ExplicitFalse(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			Enabled: boolPtr(false),
		},
	}
	if cfg.PlacementEnabled() {
		t.Error("PlacementEnabled() = true with explicit *false, want false")
	}
}

// TestPlacementEnabled_ExplicitTrue verifies PlacementEnabled returns true when
// explicitly set to true.
func TestPlacementEnabled_ExplicitTrue(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			Enabled: boolPtr(true),
		},
	}
	if !cfg.PlacementEnabled() {
		t.Error("PlacementEnabled() = false with explicit *true, want true")
	}
}

// TestPlacementEnabled_JSONRoundTrip verifies placement.enabled=false survives
// JSON decode and produces PlacementEnabled()=false.
func TestPlacementEnabled_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{"enabled":false}
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PlacementEnabled() {
		t.Error("PlacementEnabled() = true after JSON enabled:false, want false")
	}
}

// TestPlacementEnabled_AbsentFromJSON verifies omitting the placement block
// entirely from JSON yields PlacementEnabled()=true.
func TestPlacementEnabled_AbsentFromJSON(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.PlacementEnabled() {
		t.Error("PlacementEnabled() = false with no placement block in JSON, want true")
	}
	if cfg.Placement != nil {
		t.Error("Placement should be nil when block is absent from JSON")
	}
}

// --------------------------------------------------------------------------
// EffectiveWeights tests
// --------------------------------------------------------------------------

// TestEffectiveWeights_NilPlacement verifies all defaults are filled when
// Placement is nil.
func TestEffectiveWeights_NilPlacement(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	w := cfg.EffectiveWeights()
	if w.Mem != 1.0 {
		t.Errorf("Mem = %g, want 1.0", w.Mem)
	}
	if w.Storage != 0.5 {
		t.Errorf("Storage = %g, want 0.5", w.Storage)
	}
	if w.CPU != 0.5 {
		t.Errorf("CPU = %g, want 0.5", w.CPU)
	}
	if w.GuestCount != 0.3 {
		t.Errorf("GuestCount = %g, want 0.3", w.GuestCount)
	}
}

// TestEffectiveWeights_NilWeights verifies defaults fill in when Placement block
// is present but Weights is nil.
func TestEffectiveWeights_NilWeights(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{},
	}
	w := cfg.EffectiveWeights()
	if w.Mem != 1.0 || w.Storage != 0.5 || w.CPU != 0.5 || w.GuestCount != 0.3 {
		t.Errorf("EffectiveWeights defaults wrong: %+v", w)
	}
}

// TestEffectiveWeights_PartialOverride verifies that set weights are preserved
// and zero weights get the default.
func TestEffectiveWeights_PartialOverride(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			Weights: &config.PlacementWeights{
				Mem: 2.0,
				// Storage, CPU, GuestCount left 0 → should get defaults.
			},
		},
	}
	w := cfg.EffectiveWeights()
	if w.Mem != 2.0 {
		t.Errorf("Mem = %g, want 2.0 (explicit override)", w.Mem)
	}
	if w.Storage != 0.5 {
		t.Errorf("Storage = %g, want 0.5 (default)", w.Storage)
	}
	if w.CPU != 0.5 {
		t.Errorf("CPU = %g, want 0.5 (default)", w.CPU)
	}
	if w.GuestCount != 0.3 {
		t.Errorf("GuestCount = %g, want 0.3 (default)", w.GuestCount)
	}
}

// TestEffectiveWeights_AllExplicit verifies fully-explicit weights are all preserved.
func TestEffectiveWeights_AllExplicit(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			Weights: &config.PlacementWeights{
				Mem:        3.0,
				Storage:    1.5,
				CPU:        2.0,
				GuestCount: 0.8,
			},
		},
	}
	w := cfg.EffectiveWeights()
	if w.Mem != 3.0 || w.Storage != 1.5 || w.CPU != 2.0 || w.GuestCount != 0.8 {
		t.Errorf("EffectiveWeights all-explicit wrong: %+v", w)
	}
}

// TestApplyDefaults_FillsWeightBlock verifies ApplyDefaults fills zero axes
// inside an existing Weights block.
func TestApplyDefaults_FillsWeightBlock(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			Weights: &config.PlacementWeights{
				Mem: 2.5, // explicit; others left 0
			},
		},
	}
	cfg.ApplyDefaults()

	if cfg.Placement.Weights.Mem != 2.5 {
		t.Errorf("Mem overwritten by ApplyDefaults, got %g want 2.5", cfg.Placement.Weights.Mem)
	}
	if cfg.Placement.Weights.Storage != 0.5 {
		t.Errorf("Storage = %g, want 0.5 after ApplyDefaults", cfg.Placement.Weights.Storage)
	}
	if cfg.Placement.Weights.CPU != 0.5 {
		t.Errorf("CPU = %g, want 0.5 after ApplyDefaults", cfg.Placement.Weights.CPU)
	}
	if cfg.Placement.Weights.GuestCount != 0.3 {
		t.Errorf("GuestCount = %g, want 0.3 after ApplyDefaults", cfg.Placement.Weights.GuestCount)
	}
}

// TestApplyDefaults_NilPlacementNotMaterialized verifies ApplyDefaults does NOT
// create a Placement block when one was absent. Critical for zero-regression.
func TestApplyDefaults_NilPlacementNotMaterialized(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()
	if cfg.Placement != nil {
		t.Error("ApplyDefaults materialized a nil Placement block, want nil preserved")
	}
}

// --------------------------------------------------------------------------
// AZCandidates tests
// --------------------------------------------------------------------------

// TestAZCandidates_NilPlacement verifies (nil, false) returned when Placement is nil.
func TestAZCandidates_NilPlacement(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	nodes, ok := cfg.AZCandidates("us-east-1a")
	if ok || nodes != nil {
		t.Errorf("AZCandidates with nil Placement = (%v, %v), want (nil, false)", nodes, ok)
	}
}

// TestAZCandidates_EmptyAZ verifies (nil, false) when az is the empty string.
func TestAZCandidates_EmptyAZ(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			AZMap: map[string][]string{"us-east-1a": {"pve01"}},
		},
	}
	nodes, ok := cfg.AZCandidates("")
	if ok || nodes != nil {
		t.Errorf("AZCandidates empty az = (%v, %v), want (nil, false)", nodes, ok)
	}
}

// TestAZCandidates_AZFound verifies the node list is returned for a known AZ.
func TestAZCandidates_AZFound(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			AZMap: map[string][]string{
				"us-east-1a": {"pve01", "pve02"},
				"us-east-1b": {"pve03"},
			},
		},
	}
	nodes, ok := cfg.AZCandidates("us-east-1a")
	if !ok {
		t.Fatal("AZCandidates us-east-1a: ok=false, want true")
	}
	if len(nodes) != 2 || nodes[0] != "pve01" || nodes[1] != "pve02" {
		t.Errorf("AZCandidates us-east-1a nodes = %v, want [pve01 pve02]", nodes)
	}
}

// TestAZCandidates_AZMissing verifies (nil, false) for unknown AZ.
func TestAZCandidates_AZMissing(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			AZMap: map[string][]string{"us-east-1a": {"pve01"}},
		},
	}
	nodes, ok := cfg.AZCandidates("us-west-2a")
	if ok || nodes != nil {
		t.Errorf("AZCandidates unknown az = (%v, %v), want (nil, false)", nodes, ok)
	}
}

// TestAZCandidates_JSONRoundTrip verifies az_map parses from JSON correctly.
func TestAZCandidates_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{
			"az_map":{
				"az1":["pve01","pve02"],
				"az2":["pve03"]
			}
		}
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	nodes, ok := cfg.AZCandidates("az1")
	if !ok {
		t.Fatal("az1 not found in parsed az_map")
	}
	if len(nodes) != 2 {
		t.Errorf("az1 node count = %d, want 2", len(nodes))
	}
	nodes2, ok2 := cfg.AZCandidates("az2")
	if !ok2 || len(nodes2) != 1 || nodes2[0] != "pve03" {
		t.Errorf("az2 nodes = %v ok=%v, want [pve03] true", nodes2, ok2)
	}
}

// --------------------------------------------------------------------------
// Validate placement tests
// --------------------------------------------------------------------------

// TestValidate_Placement_EmptyNodeName verifies an empty node name in AZMap is rejected.
func TestValidate_Placement_EmptyNodeName(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{
			"az_map":{"az1":["pve01","","pve02"]}
		}
	}`)
	if err == nil {
		t.Fatalf("expected error for empty node name, got nil (cfg=%+v)", cfg)
	}
	if !strings.Contains(err.Error(), "placement.az_map") {
		t.Errorf("error %q missing placement.az_map context", err.Error())
	}
}

// TestValidate_Placement_EmptyNodeList verifies an AZ with zero nodes is rejected.
func TestValidate_Placement_EmptyNodeList(t *testing.T) {
	t.Parallel()
	// Must use struct path — JSON cannot encode empty slice without omitempty issues.
	cfg := &config.CPIConfig{
		Host:          "h",
		User:          "u",
		Password:      "p",
		VMStorage:     "s",
		DiskStorage:   "s",
		NetworkBridge: "br",
		Port:          8006,
		VerifySSL:     boolPtr(true),
		AgentMode:     "cloudinit",
		VMDiskFormat:  "qcow2",
		LogLevel:      "info",
		VMIDRangeStart: 100,
		VMIDRangeEnd:  8999,
		RebootMode:    "soft",
		RebootTimeout: 60,
		NetworkMode:   "auto",
		SDNZoneType:   "simple",
		Placement: &config.PlacementConfig{
			AZMap: map[string][]string{"az1": {}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty node list, got nil")
	}
	if !strings.Contains(err.Error(), "at least one node name") {
		t.Errorf("error %q missing 'at least one node name'", err.Error())
	}
}

// TestValidate_Placement_NegativeWeight verifies negative weights are rejected.
func TestValidate_Placement_NegativeWeight(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{"weights":{"mem":-0.1}}
	}`)
	if err == nil {
		t.Fatalf("expected error for negative weight, got nil (cfg=%+v)", cfg)
	}
	if !strings.Contains(err.Error(), "placement.weights.mem") {
		t.Errorf("error %q missing placement.weights.mem", err.Error())
	}
	if !strings.Contains(err.Error(), "≥ 0") {
		t.Errorf("error %q missing ≥ 0 constraint message", err.Error())
	}
}

// TestValidate_Placement_ValidFull verifies a fully-specified valid placement
// block passes Validate without error.
func TestValidate_Placement_ValidFull(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{
			"enabled":true,
			"az_map":{"az1":["pve01","pve02"],"az2":["pve03"]},
			"anti_affinity":false,
			"weights":{"mem":2.0,"storage":1.0,"cpu":1.0,"guest_count":0.5}
		}
	}`)
	if err != nil {
		t.Fatalf("valid full placement block: unexpected error: %v", err)
	}
	if !cfg.PlacementEnabled() {
		t.Error("PlacementEnabled() = false, want true")
	}
	if cfg.AntiAffinityEnabled() {
		t.Error("AntiAffinityEnabled() = true, want false")
	}
	w := cfg.EffectiveWeights()
	if w.Mem != 2.0 || w.Storage != 1.0 || w.CPU != 1.0 || w.GuestCount != 0.5 {
		t.Errorf("weights wrong: %+v", w)
	}
}

// TestValidate_Placement_NilIsSkipped verifies that a nil Placement block
// (absent from JSON) produces no validation errors.
func TestValidate_Placement_NilIsSkipped(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("nil placement: unexpected error: %v", err)
	}
	if cfg.Placement != nil {
		t.Error("Placement should be nil when absent from JSON")
	}
}

// --------------------------------------------------------------------------
// AntiAffinityEnabled tests
// --------------------------------------------------------------------------

// TestAntiAffinityEnabled_NilPlacement verifies false when Placement is nil.
func TestAntiAffinityEnabled_NilPlacement(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	if cfg.AntiAffinityEnabled() {
		t.Error("AntiAffinityEnabled() = true with nil Placement, want false")
	}
}

// TestAntiAffinityEnabled_NilField verifies false when Placement.AntiAffinity is nil.
func TestAntiAffinityEnabled_NilField(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{Placement: &config.PlacementConfig{}}
	if cfg.AntiAffinityEnabled() {
		t.Error("AntiAffinityEnabled() = true with nil AntiAffinity field, want false")
	}
}

// TestAntiAffinityEnabled_ExplicitTrue verifies true when explicitly set.
func TestAntiAffinityEnabled_ExplicitTrue(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			AntiAffinity: boolPtr(true),
		},
	}
	if !cfg.AntiAffinityEnabled() {
		t.Error("AntiAffinityEnabled() = false with explicit *true, want true")
	}
}

// --------------------------------------------------------------------------
// EnsureNoIPConflicts tests
// --------------------------------------------------------------------------

// TestEnsureNoIPConflictsEnabled_Nil verifies true when field is nil (DEC-2 default).
func TestEnsureNoIPConflictsEnabled_Nil(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	if !cfg.EnsureNoIPConflictsEnabled() {
		t.Error("EnsureNoIPConflictsEnabled() = false with nil field, want true (protective default)")
	}
}

// TestEnsureNoIPConflictsEnabled_ExplicitFalse verifies false when explicitly disabled.
func TestEnsureNoIPConflictsEnabled_ExplicitFalse(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{EnsureNoIPConflicts: boolPtr(false)}
	if cfg.EnsureNoIPConflictsEnabled() {
		t.Error("EnsureNoIPConflictsEnabled() = true with explicit *false, want false")
	}
}

// TestEnsureNoIPConflictsEnabled_ExplicitTrue verifies true when explicitly enabled.
func TestEnsureNoIPConflictsEnabled_ExplicitTrue(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{EnsureNoIPConflicts: boolPtr(true)}
	if !cfg.EnsureNoIPConflictsEnabled() {
		t.Error("EnsureNoIPConflictsEnabled() = false with explicit *true, want true")
	}
}

// TestEnsureNoIPConflictsEnabled_AbsentFromJSON verifies nil *bool when key absent
// from JSON, yielding protective default from accessor.
func TestEnsureNoIPConflictsEnabled_AbsentFromJSON(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EnsureNoIPConflicts != nil {
		t.Error("EnsureNoIPConflicts should be nil when absent from JSON")
	}
	if !cfg.EnsureNoIPConflictsEnabled() {
		t.Error("EnsureNoIPConflictsEnabled() = false with nil field, want true")
	}
}

// TestEnsureNoIPConflictsEnabled_ExplicitFalseFromJSON verifies false survives
// JSON decode.
func TestEnsureNoIPConflictsEnabled_ExplicitFalseFromJSON(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"ensure_no_ip_conflicts":false
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EnsureNoIPConflicts == nil {
		t.Fatal("EnsureNoIPConflicts should be non-nil when explicitly false in JSON")
	}
	if cfg.EnsureNoIPConflictsEnabled() {
		t.Error("EnsureNoIPConflictsEnabled() = true with explicit false in JSON, want false")
	}
}
