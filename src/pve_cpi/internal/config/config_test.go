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

// TestApplyDefaults_VMIDRangeEnd verifies VMIDRangeEnd defaults to 5999 when absent.
func TestApplyDefaults_VMIDRangeEnd(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.VMIDRangeEnd != 5999 {
		t.Errorf("VMIDRangeEnd = %d, want 5999 (default)", cfg.VMIDRangeEnd)
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

// TestValidate_VMIDRangeEndTooHigh confirms end > 9999 is rejected.
func TestValidate_VMIDRangeEndTooHigh(t *testing.T) {
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
		VMIDRangeEnd:   10000, // above max — invalid
		RebootMode:     "soft",
		RebootTimeout:  60,
	}
	assertCloudError(t, cfg.Validate(), "vmid_range_end must be ≤9999")
}

// TestValidate_VMIDRangeEndAt9999 confirms end == 9999 is valid.
func TestValidate_VMIDRangeEndAt9999(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"vmid_range_end":9999
	}`)
	if err != nil {
		t.Fatalf("unexpected error for vmid_range_end=9999: %v", err)
	}
	if cfg.VMIDRangeEnd != 9999 {
		t.Errorf("VMIDRangeEnd = %d, want 9999", cfg.VMIDRangeEnd)
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
