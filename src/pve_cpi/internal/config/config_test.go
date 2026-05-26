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
// TestApplyDefaults_RebootFields
// --------------------------------------------------------------------------

// TestApplyDefaults_RebootFields verifies that ApplyDefaults fills in the
// reboot_mode and reboot_timeout defaults when both fields are absent.
func TestApplyDefaults_RebootFields(t *testing.T) {
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

// --------------------------------------------------------------------------
// TestApplyDefaults_VMIDRangeEnd
// --------------------------------------------------------------------------

// TestApplyDefaults_VMIDRangeEnd verifies VMIDRangeEnd defaults to 5999 when absent.
func TestApplyDefaults_VMIDRangeEnd(t *testing.T) {
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

// TestLoad_NetworkMode_Omitted_GetsAuto confirms that omitting network_mode
// from JSON results in NetworkMode="auto" after Load applies defaults.
func TestLoad_NetworkMode_Omitted_GetsAuto(t *testing.T) {
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
