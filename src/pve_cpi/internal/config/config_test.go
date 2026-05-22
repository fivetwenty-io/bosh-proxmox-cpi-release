package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
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
func boolPtr(b bool) *bool { return &b }

// assertCloudError asserts err is a *cpierrors.Error with TypeCloud and that
// its message contains the given substring.
func assertCloudError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", contains)
	}
	var ce *cpierrors.Error
	var ok bool
	// Use type assertion since cpierrors.Error is a concrete type.
	ce, ok = err.(*cpierrors.Error)
	if !ok {
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
	_, err := config.LoadFile("testdata/invalid_missing_host.json")
	assertCloudError(t, err, "host is required")
}

// --------------------------------------------------------------------------
// TestValidate_AgentModeInvalid
// --------------------------------------------------------------------------

func TestValidate_AgentModeInvalid(t *testing.T) {
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
	_, err := mustLoad(t, `{
		"host": "h", "user": "u",
		"password": "pw", "api_token": "tok",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	assertCloudError(t, err, "mutually exclusive")
}

// --------------------------------------------------------------------------
// TestValidate_RegistryRequiresFields
// --------------------------------------------------------------------------

func TestValidate_RegistryRequiresFields(t *testing.T) {
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
	_, err := config.LoadFile("/nonexistent/path/config.json")
	assertCloudError(t, err, "config: open")
}

// --------------------------------------------------------------------------
// TestLoadFile_MalformedJSON
// --------------------------------------------------------------------------

func TestLoadFile_MalformedJSON(t *testing.T) {
	_, err := config.Load(strings.NewReader(`{this is not json}`))
	assertCloudError(t, err, "config: decode failed")
}

// --------------------------------------------------------------------------
// TestValidate_VMDiskFormatInvalid
// --------------------------------------------------------------------------

func TestValidate_VMDiskFormatInvalid(t *testing.T) {
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
// TestLoad_RegistryMode_Valid
// --------------------------------------------------------------------------

func TestLoad_RegistryMode_Valid(t *testing.T) {
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"agent_mode":"registry",
		"registry_endpoint":"http://registry:25777",
		"registry_user":"admin",
		"registry_password":"secret"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RegistryEndpoint != "http://registry:25777" {
		t.Errorf("RegistryEndpoint = %q", cfg.RegistryEndpoint)
	}
}
