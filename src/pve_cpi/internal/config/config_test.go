package config_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
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
// TestValidate_DiskPerfInvariantModeInvalid — §7.26 enum validation.
// --------------------------------------------------------------------------

func TestValidate_DiskPerfInvariantModeInvalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"disk_perf_invariant_mode": "strict"
	}`)
	assertCloudError(t, err, "disk_perf_invariant_mode must be one of")
}

func TestValidate_ClusterLockModeInvalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"cluster_lock_mode": "sdn"
	}`)
	assertCloudError(t, err, "cluster_lock_mode must be one of")
}

func TestValidate_ClusterLockTimeoutNegative(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"cluster_lock_timeout_sec": -1
	}`)
	assertCloudError(t, err, "cluster_lock_timeout_sec must be >= 0")
}

func TestReplicaAdoptAccessors(t *testing.T) {
	t.Parallel()
	// Default: disabled, resolves to 0.
	def := &config.CPIConfig{}
	if def.ReplicaAdoptEnabled() {
		t.Error("default replica_adopt_timeout_sec should be disabled")
	}
	if got := def.ReplicaAdoptTimeoutSecValue(); got != 0 {
		t.Errorf("default timeout should resolve to 0; got %d", got)
	}
	// Nil receiver is defensive.
	var nilCfg *config.CPIConfig
	if nilCfg.ReplicaAdoptEnabled() || nilCfg.ReplicaAdoptTimeoutSecValue() != 0 {
		t.Error("nil receiver should resolve to disabled/0")
	}
	// Positive value enables adopt-and-wait.
	on := &config.CPIConfig{ReplicaAdoptTimeoutSec: 300}
	if !on.ReplicaAdoptEnabled() {
		t.Error("positive timeout should enable adopt-and-wait")
	}
	if got := on.ReplicaAdoptTimeoutSecValue(); got != 300 {
		t.Errorf("custom timeout should be 300; got %d", got)
	}
}

func TestValidate_ReplicaAdoptTimeoutNegative(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"replica_adopt_timeout_sec": -1
	}`)
	assertCloudError(t, err, "replica_adopt_timeout_sec must be >= 0")
}

func TestDiskDeleteStateGuardAccessor(t *testing.T) {
	t.Parallel()
	// Default (empty) → disabled, byte-identical behavior.
	if (&config.CPIConfig{}).DiskDeleteStateGuardEnabled() {
		t.Error("empty disk_delete_state_guard should be disabled")
	}
	// Explicit "off" → disabled.
	if (&config.CPIConfig{DiskDeleteStateGuard: "off"}).DiskDeleteStateGuardEnabled() {
		t.Error(`"off" should be disabled`)
	}
	// "on" (any case, padded) → enabled.
	for _, v := range []string{"on", "ON", " On "} {
		if !(&config.CPIConfig{DiskDeleteStateGuard: v}).DiskDeleteStateGuardEnabled() {
			t.Errorf("%q should enable the guard", v)
		}
	}
	// Nil receiver is defensive.
	var nilCfg *config.CPIConfig
	if nilCfg.DiskDeleteStateGuardEnabled() {
		t.Error("nil receiver should be disabled")
	}
}

func TestValidate_DiskDeleteStateGuardInvalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"disk_delete_state_guard": "maybe"
	}`)
	assertCloudError(t, err, "disk_delete_state_guard must be one of off|on")
}

func TestClusterLockAccessors(t *testing.T) {
	t.Parallel()
	// Defaults: off, not enabled, verify off, timeout 60.
	def := &config.CPIConfig{}
	if def.ClusterLockMode() != "off" || def.ClusterLockEnabled() {
		t.Errorf("default lock mode should be off/disabled; got %q enabled=%v", def.ClusterLockMode(), def.ClusterLockEnabled())
	}
	if def.AntiAffinityVerifyEnabled() {
		t.Error("default antiaffinity_verify should be false")
	}
	if got := def.ClusterLockTimeoutSecValue(); got != 60 {
		t.Errorf("default timeout should resolve to 60; got %d", got)
	}
	// Enabled pool mode + verify + custom timeout.
	v := true
	on := &config.CPIConfig{ClusterLock: " POOL ", ClusterLockTimeoutSec: 15, AntiAffinityVerify: &v}
	if on.ClusterLockMode() != "pool" || !on.ClusterLockEnabled() {
		t.Errorf("pool mode should normalize+enable; got %q enabled=%v", on.ClusterLockMode(), on.ClusterLockEnabled())
	}
	if !on.AntiAffinityVerifyEnabled() {
		t.Error("antiaffinity_verify should be true")
	}
	if got := on.ClusterLockTimeoutSecValue(); got != 15 {
		t.Errorf("custom timeout should be 15; got %d", got)
	}
}

func TestClusterLockModeValid(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"off", "pool"} {
		cfg, err := mustLoad(t, `{
			"host": "h", "user": "u", "password": "p",
			"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
			"cluster_lock_mode": "`+mode+`"
		}`)
		if err != nil {
			t.Fatalf("mode %q should be valid, got: %v", mode, err)
		}
		if got := cfg.ClusterLockMode(); got != mode {
			t.Errorf("ClusterLockMode(): got %q, want %q", got, mode)
		}
	}
}

func TestValidate_DiskPerfInvariantModeValid(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"enforce", "warn", "off"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cfg, err := mustLoad(t, `{
				"host": "h", "user": "u", "password": "p",
				"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
				"disk_perf_invariant_mode": "`+mode+`"
			}`)
			if err != nil {
				t.Fatalf("mode %q should be valid, got: %v", mode, err)
			}
			if got := cfg.DiskPerfInvariantModeValue(); got != mode {
				t.Errorf("DiskPerfInvariantModeValue(): got %q, want %q", got, mode)
			}
		})
	}
}

func TestDiskPerfInvariantModeValue_DefaultsToEnforce(t *testing.T) {
	t.Parallel()
	// Empty config field resolves to enforce.
	cfg := &config.CPIConfig{}
	if got := cfg.DiskPerfInvariantModeValue(); got != "enforce" {
		t.Errorf("empty mode: got %q, want enforce", got)
	}
	// Nil receiver resolves to enforce (defensive).
	var nilCfg *config.CPIConfig
	if got := nilCfg.DiskPerfInvariantModeValue(); got != "enforce" {
		t.Errorf("nil receiver: got %q, want enforce", got)
	}
	// Mixed case / whitespace normalized.
	cfg.DiskPerfInvariantMode = "  WARN "
	if got := cfg.DiskPerfInvariantModeValue(); got != "warn" {
		t.Errorf("normalize: got %q, want warn", got)
	}
}

// --------------------------------------------------------------------------
// §7.27 resize-convergence config — accessors + validation.
// --------------------------------------------------------------------------

func TestValidate_ResizeConvergenceTimeoutNegative(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"resize_convergence_timeout_sec": -5
	}`)
	assertCloudError(t, err, "resize_convergence_timeout_sec must be >= 0")
}

func TestResizeConvergenceAccessors(t *testing.T) {
	t.Parallel()

	// Nil receiver: disabled, default budget.
	var nilCfg *config.CPIConfig
	if nilCfg.ResizeWaitForConvergenceEnabled() {
		t.Error("nil receiver: want disabled")
	}
	if got := nilCfg.ResizeConvergenceTimeoutSecValue(); got != 120 {
		t.Errorf("nil receiver budget: got %d, want 120", got)
	}

	// Empty config: disabled, default budget.
	cfg := &config.CPIConfig{}
	if cfg.ResizeWaitForConvergenceEnabled() {
		t.Error("empty config: want disabled")
	}
	if got := cfg.ResizeConvergenceTimeoutSecValue(); got != 120 {
		t.Errorf("empty config budget: got %d, want 120 (0 → default)", got)
	}

	// Enabled with explicit budget.
	tru := true
	cfg.ResizeWaitForConvergence = &tru
	cfg.ResizeConvergenceTimeoutSec = 300
	if !cfg.ResizeWaitForConvergenceEnabled() {
		t.Error("want enabled")
	}
	if got := cfg.ResizeConvergenceTimeoutSecValue(); got != 300 {
		t.Errorf("explicit budget: got %d, want 300", got)
	}

	// Explicit false pointer → disabled.
	fal := false
	cfg.ResizeWaitForConvergence = &fal
	if cfg.ResizeWaitForConvergenceEnabled() {
		t.Error("explicit false: want disabled")
	}
}

// --------------------------------------------------------------------------
// §7.29 health_check.expected_agent_sha256 — validation + accessor.
// --------------------------------------------------------------------------

func TestValidate_ExpectedAgentSHA256Invalid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"health_check": {"enabled": true, "expected_agent_sha256": "not-a-real-digest"}
	}`)
	assertCloudError(t, err, "expected_agent_sha256 must be 64 hex characters")
}

func TestValidate_ExpectedAgentSHA256Valid(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"health_check": {"enabled": true, "expected_agent_sha256": "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855"}
	}`)
	if err != nil {
		t.Fatalf("valid 64-hex digest should pass, got: %v", err)
	}
	// Accessor lower-cases the value.
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := cfg.HealthCheckExpectedAgentSHA256(); got != want {
		t.Errorf("accessor: got %q, want lower-cased %q", got, want)
	}
}

func TestTaskPollAdaptiveEnabled(t *testing.T) {
	t.Parallel()
	var nilCfg *config.CPIConfig
	if nilCfg.TaskPollAdaptiveEnabled() {
		t.Error("nil receiver: want disabled")
	}
	if (&config.CPIConfig{}).TaskPollAdaptiveEnabled() {
		t.Error("unset: want disabled")
	}
	tru := true
	if !(&config.CPIConfig{TaskPollAdaptive: &tru}).TaskPollAdaptiveEnabled() {
		t.Error("explicit true: want enabled")
	}
	fal := false
	if (&config.CPIConfig{TaskPollAdaptive: &fal}).TaskPollAdaptiveEnabled() {
		t.Error("explicit false: want disabled")
	}
}

func TestFastPathDeleteEnabled(t *testing.T) {
	t.Parallel()
	var nilCfg *config.CPIConfig
	// nil receiver: must return false, not panic.
	if nilCfg.FastPathDeleteEnabled() {
		t.Error("nil receiver: want false")
	}
	// absent field (nil *bool): must return false.
	if (&config.CPIConfig{}).FastPathDeleteEnabled() {
		t.Error("nil *bool: want false")
	}
	// explicit *true: must return true.
	tru := true
	if !(&config.CPIConfig{FastPathDelete: &tru}).FastPathDeleteEnabled() {
		t.Error("explicit *true: want true")
	}
	// explicit *false: must return false.
	fal := false
	if (&config.CPIConfig{FastPathDelete: &fal}).FastPathDeleteEnabled() {
		t.Error("explicit *false: want false")
	}
}

func TestHealthCheckExpectedAgentSHA256_EmptyWhenUnset(t *testing.T) {
	t.Parallel()
	var nilCfg *config.CPIConfig
	if got := nilCfg.HealthCheckExpectedAgentSHA256(); got != "" {
		t.Errorf("nil receiver: got %q, want empty", got)
	}
	if got := (&config.CPIConfig{}).HealthCheckExpectedAgentSHA256(); got != "" {
		t.Errorf("no health_check block: got %q, want empty", got)
	}
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
// when the entire Placement block is absent (fully protective default).
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
		Host:           "h",
		User:           "u",
		Password:       "p",
		VMStorage:      "s",
		DiskStorage:    "s",
		NetworkBridge:  "br",
		Port:           8006,
		VerifySSL:      boolPtr(true),
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
		VMIDRangeEnd:   8999,
		RebootMode:     "soft",
		RebootTimeout:  60,
		NetworkMode:    "auto",
		SDNZoneType:    "simple",
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
			"anti_affinity":{"enabled":true,"use_ha_rules":true},
			"weights":{"mem":2.0,"storage":1.0,"cpu":1.0,"guest_count":0.5}
		}
	}`)
	if err != nil {
		t.Fatalf("valid full placement block: unexpected error: %v", err)
	}
	if !cfg.PlacementEnabled() {
		t.Error("PlacementEnabled() = false, want true")
	}
	if !cfg.AntiAffinityEnabled() {
		t.Error("AntiAffinityEnabled() = false, want true")
	}
	if !cfg.AntiAffinityUseHaRulesEnabled() {
		t.Error("AntiAffinityUseHaRulesEnabled() = false, want true")
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
			AntiAffinity: &config.AntiAffinityConfig{Enabled: boolPtr(true)},
		},
	}
	if !cfg.AntiAffinityEnabled() {
		t.Error("AntiAffinityEnabled() = false with explicit *true, want true")
	}
}

// TestAntiAffinityUseHaRulesEnabled covers the two-flag gate: HA rules only
// activate when both Enabled and UseHaRules are true.
func TestAntiAffinityUseHaRulesEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		aa   *config.AntiAffinityConfig
		want bool
	}{
		{"nil block", nil, false},
		{"enabled only", &config.AntiAffinityConfig{Enabled: boolPtr(true)}, false},
		{"ha without enabled", &config.AntiAffinityConfig{UseHaRules: boolPtr(true)}, false},
		{"both true", &config.AntiAffinityConfig{Enabled: boolPtr(true), UseHaRules: boolPtr(true)}, true},
		{"enabled true ha false", &config.AntiAffinityConfig{Enabled: boolPtr(true), UseHaRules: boolPtr(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.CPIConfig{Placement: &config.PlacementConfig{AntiAffinity: tc.aa}}
			if got := cfg.AntiAffinityUseHaRulesEnabled(); got != tc.want {
				t.Errorf("AntiAffinityUseHaRulesEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// EnsureNoIPConflicts tests
// --------------------------------------------------------------------------

// TestEnsureNoIPConflictsEnabled_Nil verifies true when field is nil.
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

// --------------------------------------------------------------------------
// DLB config accessor tests
// --------------------------------------------------------------------------

// stringPtr returns a pointer to s, for constructing *string fields in test literals.
func stringPtr(s string) *string { return &s }

// TestDLB_NilEverything verifies all DLB accessors return safe defaults when
// both Placement and DLB are nil. This is the zero-regression path: existing
// configs with no placement block must behave identically to before.
func TestDLB_NilEverything(t *testing.T) {
	t.Parallel()
	var cfg config.CPIConfig

	if cfg.DLBExplicitlyEnabled() {
		t.Error("DLBExplicitlyEnabled() = true with nil Placement, want false")
	}
	if got := cfg.DLBAZName(); got != "dlb" {
		t.Errorf("DLBAZName() = %q with nil Placement, want %q", got, "dlb")
	}
	if cfg.DLBManageClusterCRS() {
		t.Error("DLBManageClusterCRS() = true with nil Placement, want false")
	}
	if !cfg.DLBRequireSharedStorage() {
		t.Error("DLBRequireSharedStorage() = false with nil Placement, want true (protective default)")
	}
	if cfg.DLBConfigured() {
		t.Error("DLBConfigured() = true with nil Placement, want false")
	}
	// Sentinel default is "dlb", so DLBEligibleForAZ("dlb") must return true via sentinel.
	if !cfg.DLBEligibleForAZ("dlb") {
		t.Error("DLBEligibleForAZ(\"dlb\") = false with nil Placement, want true (sentinel default matches)")
	}
	// Non-sentinel AZ must not match.
	if cfg.DLBEligibleForAZ("z1") {
		t.Error("DLBEligibleForAZ(\"z1\") = true with nil Placement, want false")
	}
	if cfg.AntiAffinityStrict() {
		t.Error("AntiAffinityStrict() = true with nil Placement, want false")
	}
}

// TestDLB_NilDLBBlock verifies all DLB accessors return safe defaults when
// Placement is present but DLB is nil.
func TestDLB_NilDLBBlock(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{},
	}

	if cfg.DLBExplicitlyEnabled() {
		t.Error("DLBExplicitlyEnabled() = true with nil DLB, want false")
	}
	if got := cfg.DLBAZName(); got != "dlb" {
		t.Errorf("DLBAZName() = %q with nil DLB, want %q (default sentinel)", got, "dlb")
	}
	if cfg.DLBManageClusterCRS() {
		t.Error("DLBManageClusterCRS() = true with nil DLB, want false")
	}
	if !cfg.DLBRequireSharedStorage() {
		t.Error("DLBRequireSharedStorage() = false with nil DLB, want true")
	}
	if cfg.DLBConfigured() {
		t.Error("DLBConfigured() = true with nil DLB, want false")
	}
	if !cfg.DLBEligibleForAZ("dlb") {
		t.Error("DLBEligibleForAZ(\"dlb\") = false with nil DLB and default sentinel, want true")
	}
	if cfg.DLBEligibleForAZ("z1") {
		t.Error("DLBEligibleForAZ(\"z1\") = true with nil DLB, want false")
	}
}

// TestDLB_EnabledTrue verifies the master flag: DLBExplicitlyEnabled true,
// DLBEligibleForAZ true for ANY az, DLBConfigured true.
func TestDLB_EnabledTrue(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			DLB: &config.DLBConfig{
				Enabled: boolPtr(true),
			},
		},
	}

	if !cfg.DLBExplicitlyEnabled() {
		t.Error("DLBExplicitlyEnabled() = false with *true, want true")
	}
	if !cfg.DLBEligibleForAZ("z1") {
		t.Error("DLBEligibleForAZ(\"z1\") = false with master Enabled=true, want true (master overrides)")
	}
	if !cfg.DLBEligibleForAZ("dlb") {
		t.Error("DLBEligibleForAZ(\"dlb\") = false with master Enabled=true, want true")
	}
	if !cfg.DLBEligibleForAZ("") {
		t.Error("DLBEligibleForAZ(\"\") = false with master Enabled=true, want true (master catches all)")
	}
	if !cfg.DLBConfigured() {
		t.Error("DLBConfigured() = false with DLB block present and Enabled=true, want true")
	}
}

// TestDLB_AZNameExplicitEmpty verifies that AZName="" (explicit pointer-to-empty)
// disables the sentinel: DLBAZName returns "", sentinel is disabled.
func TestDLB_AZNameExplicitEmpty(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			DLB: &config.DLBConfig{
				AZName: stringPtr(""),
			},
		},
	}

	if got := cfg.DLBAZName(); got != "" {
		t.Errorf("DLBAZName() = %q with explicit empty string, want %q (sentinel disabled)", got, "")
	}
	// Sentinel disabled: DLBEligibleForAZ("dlb") must be false (not master-enabled either).
	if cfg.DLBEligibleForAZ("dlb") {
		t.Error("DLBEligibleForAZ(\"dlb\") = true with sentinel disabled (AZName=\"\"), want false")
	}
	if cfg.DLBEligibleForAZ("") {
		t.Error("DLBEligibleForAZ(\"\") = true with sentinel disabled, want false")
	}
	// DLBConfigured: DLB block is present but Enabled is nil/false AND AZName is "".
	// No VMs could have been registered — return false.
	if cfg.DLBConfigured() {
		t.Error("DLBConfigured() = true with DLB present but Enabled nil and AZName empty, want false")
	}
}

// TestDLB_AZNameCustom verifies a custom sentinel AZ name matches correctly.
func TestDLB_AZNameCustom(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			DLB: &config.DLBConfig{
				AZName: stringPtr("dlbzone"),
			},
		},
	}

	if got := cfg.DLBAZName(); got != "dlbzone" {
		t.Errorf("DLBAZName() = %q, want %q", got, "dlbzone")
	}
	if !cfg.DLBEligibleForAZ("dlbzone") {
		t.Error("DLBEligibleForAZ(\"dlbzone\") = false with AZName=dlbzone, want true")
	}
	// Default sentinel name "dlb" must NOT match when overridden.
	if cfg.DLBEligibleForAZ("dlb") {
		t.Error("DLBEligibleForAZ(\"dlb\") = true with AZName=dlbzone, want false")
	}
	if cfg.DLBEligibleForAZ("z1") {
		t.Error("DLBEligibleForAZ(\"z1\") = true with AZName=dlbzone, want false")
	}
	if !cfg.DLBConfigured() {
		t.Error("DLBConfigured() = false with DLB present and non-empty AZName, want true")
	}
}

// TestDLB_RequireSharedStorageFalse verifies the explicit false override.
func TestDLB_RequireSharedStorageFalse(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			DLB: &config.DLBConfig{
				RequireSharedStorage: boolPtr(false),
			},
		},
	}

	if cfg.DLBRequireSharedStorage() {
		t.Error("DLBRequireSharedStorage() = true with explicit *false, want false")
	}
}

// TestDLB_ManageClusterCRSTrue verifies the explicit true override.
func TestDLB_ManageClusterCRSTrue(t *testing.T) {
	t.Parallel()
	cfg := config.CPIConfig{
		Placement: &config.PlacementConfig{
			DLB: &config.DLBConfig{
				ManageClusterCRS: boolPtr(true),
			},
		},
	}

	if !cfg.DLBManageClusterCRS() {
		t.Error("DLBManageClusterCRS() = false with explicit *true, want true")
	}
}

// TestDLB_AntiAffinityStrict verifies Strict accessor reads from AntiAffinityConfig.
func TestDLB_AntiAffinityStrict(t *testing.T) {
	t.Parallel()

	t.Run("nil AntiAffinity → false", func(t *testing.T) {
		var cfg config.CPIConfig
		if cfg.AntiAffinityStrict() {
			t.Error("AntiAffinityStrict() = true with nil everything, want false")
		}
	})

	t.Run("Strict nil → false", func(t *testing.T) {
		cfg := config.CPIConfig{
			Placement: &config.PlacementConfig{
				AntiAffinity: &config.AntiAffinityConfig{},
			},
		}
		if cfg.AntiAffinityStrict() {
			t.Error("AntiAffinityStrict() = true with nil Strict field, want false")
		}
	})

	t.Run("Strict *true → true", func(t *testing.T) {
		cfg := config.CPIConfig{
			Placement: &config.PlacementConfig{
				AntiAffinity: &config.AntiAffinityConfig{
					Strict: boolPtr(true),
				},
			},
		}
		if !cfg.AntiAffinityStrict() {
			t.Error("AntiAffinityStrict() = false with explicit *true, want true")
		}
	})

	t.Run("Strict *false → false", func(t *testing.T) {
		cfg := config.CPIConfig{
			Placement: &config.PlacementConfig{
				AntiAffinity: &config.AntiAffinityConfig{
					Strict: boolPtr(false),
				},
			},
		}
		if cfg.AntiAffinityStrict() {
			t.Error("AntiAffinityStrict() = true with explicit *false, want false")
		}
	})
}

// TestDLB_JSONRoundTrip verifies a placement.dlb JSON blob decodes into the
// correct struct fields and all accessors return the decoded values.
func TestDLB_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{
			"dlb":{
				"enabled":true,
				"az_name":"myzone",
				"manage_cluster_crs":true,
				"require_shared_storage":false
			},
			"anti_affinity":{
				"strict":true
			}
		}
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Placement == nil {
		t.Fatal("Placement = nil, want non-nil")
	}
	if cfg.Placement.DLB == nil {
		t.Fatal("Placement.DLB = nil, want non-nil")
	}

	if !cfg.DLBExplicitlyEnabled() {
		t.Error("DLBExplicitlyEnabled() = false, want true")
	}
	if got := cfg.DLBAZName(); got != "myzone" {
		t.Errorf("DLBAZName() = %q, want %q", got, "myzone")
	}
	if !cfg.DLBManageClusterCRS() {
		t.Error("DLBManageClusterCRS() = false, want true")
	}
	if cfg.DLBRequireSharedStorage() {
		t.Error("DLBRequireSharedStorage() = true, want false")
	}
	if !cfg.DLBConfigured() {
		t.Error("DLBConfigured() = false, want true")
	}
	if !cfg.DLBEligibleForAZ("myzone") {
		t.Error("DLBEligibleForAZ(\"myzone\") = false, want true")
	}
	if !cfg.DLBEligibleForAZ("z1") {
		t.Error("DLBEligibleForAZ(\"z1\") = false with master enabled=true, want true")
	}
	if !cfg.AntiAffinityStrict() {
		t.Error("AntiAffinityStrict() = false, want true")
	}
}

// TestDLB_ConfiguredGating verifies DLBConfigured edge cases for delete-time cleanup.
func TestDLB_ConfiguredGating(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dlb     *config.DLBConfig
		wantCfg bool
	}{
		{
			name:    "nil DLB block",
			dlb:     nil,
			wantCfg: false,
		},
		{
			name:    "all-nil DLB fields",
			dlb:     &config.DLBConfig{},
			wantCfg: true, // AZName nil → default "dlb" → non-empty → true
		},
		{
			name:    "Enabled false, AZName nil",
			dlb:     &config.DLBConfig{Enabled: boolPtr(false)},
			wantCfg: true, // AZName nil → "dlb" → non-empty
		},
		{
			name:    "Enabled false, AZName empty string",
			dlb:     &config.DLBConfig{Enabled: boolPtr(false), AZName: stringPtr("")},
			wantCfg: false, // both off: sentinel disabled, master off
		},
		{
			name:    "Enabled true, AZName empty string",
			dlb:     &config.DLBConfig{Enabled: boolPtr(true), AZName: stringPtr("")},
			wantCfg: true, // master on → configured
		},
		{
			name:    "Enabled nil, AZName custom",
			dlb:     &config.DLBConfig{AZName: stringPtr("dlbzone")},
			wantCfg: true, // sentinel non-empty → configured
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.CPIConfig{
				Placement: &config.PlacementConfig{
					DLB: tc.dlb,
				},
			}
			if got := cfg.DLBConfigured(); got != tc.wantCfg {
				t.Errorf("DLBConfigured() = %v, want %v", got, tc.wantCfg)
			}
		})
	}
}

// TestDLB_ValidateDLBBlock verifies that a placement block containing a DLB
// sub-block passes validation without error (validate-only-when-set; no
// enum or range constraints on DLB fields).
func TestDLB_ValidateDLBBlock(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{
			"dlb":{
				"enabled":true,
				"az_name":"",
				"manage_cluster_crs":false,
				"require_shared_storage":false
			}
		}
	}`)
	if err != nil {
		t.Fatalf("DLB block with all fields set: unexpected validation error: %v", err)
	}
}

// TestDLB_AbsentDLBBlock_NoRegression verifies that a placement block without
// a dlb key produces no error and leaves Placement.DLB nil.
func TestDLB_AbsentDLBBlock_NoRegression(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement":{
			"enabled":true,
			"az_map":{"z1":["pve1"]}
		}
	}`)
	if err != nil {
		t.Fatalf("placement without dlb: unexpected error: %v", err)
	}
	if cfg.Placement == nil {
		t.Fatal("Placement = nil, want non-nil")
	}
	if cfg.Placement.DLB != nil {
		t.Error("Placement.DLB = non-nil when key absent from JSON, want nil")
	}
	// Accessor defaults must still work.
	if cfg.DLBExplicitlyEnabled() {
		t.Error("DLBExplicitlyEnabled() = true with nil DLB block, want false")
	}
	if got := cfg.DLBAZName(); got != "dlb" {
		t.Errorf("DLBAZName() = %q with nil DLB block, want %q", got, "dlb")
	}
}

// ---------------------------------------------------------------------------
// ExcludeMaintenanceNodesEnabled
// ---------------------------------------------------------------------------

func TestExcludeMaintenanceNodesEnabled_NilPlacement(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	// nil Placement → default true (protective).
	if !cfg.ExcludeMaintenanceNodesEnabled() {
		t.Error("ExcludeMaintenanceNodesEnabled() = false with nil Placement; want true")
	}
}

func TestExcludeMaintenanceNodesEnabled_NilField(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Placement: &config.PlacementConfig{}}
	// Placement present, ExcludeMaintenanceNodes nil → default true.
	if !cfg.ExcludeMaintenanceNodesEnabled() {
		t.Error("ExcludeMaintenanceNodesEnabled() = false with nil field; want true")
	}
}

func TestExcludeMaintenanceNodesEnabled_ExplicitFalse(t *testing.T) {
	t.Parallel()
	f := false
	cfg := &config.CPIConfig{Placement: &config.PlacementConfig{ExcludeMaintenanceNodes: &f}}
	if cfg.ExcludeMaintenanceNodesEnabled() {
		t.Error("ExcludeMaintenanceNodesEnabled() = true with *false; want false")
	}
}

func TestExcludeMaintenanceNodesEnabled_ExplicitTrue(t *testing.T) {
	t.Parallel()
	tr := true
	cfg := &config.CPIConfig{Placement: &config.PlacementConfig{ExcludeMaintenanceNodes: &tr}}
	if !cfg.ExcludeMaintenanceNodesEnabled() {
		t.Error("ExcludeMaintenanceNodesEnabled() = false with *true; want true")
	}
}

// ---------------------------------------------------------------------------
// MaintenanceNodeTagsValue
// ---------------------------------------------------------------------------

func TestMaintenanceNodeTagsValue_Default(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	got := cfg.MaintenanceNodeTagsValue()
	if len(got) != 1 || got[0] != "maintenance" {
		t.Errorf("MaintenanceNodeTagsValue() = %v; want [maintenance]", got)
	}
}

func TestMaintenanceNodeTagsValue_Custom(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Placement: &config.PlacementConfig{MaintenanceNodeTags: []string{"maint", "drain"}}}
	got := cfg.MaintenanceNodeTagsValue()
	if len(got) != 2 || got[0] != "maint" || got[1] != "drain" {
		t.Errorf("MaintenanceNodeTagsValue() = %v; want [maint drain]", got)
	}
}

// ---------------------------------------------------------------------------
// AZFallbackOrderValue / AZShuffleEnabled
// ---------------------------------------------------------------------------

func TestAZFallbackOrderValue_NilPlacement(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	if got := cfg.AZFallbackOrderValue(); got != nil {
		t.Errorf("AZFallbackOrderValue() = %v; want nil", got)
	}
}

func TestAZFallbackOrderValue_Set(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Placement: &config.PlacementConfig{AZFallbackOrder: []string{"az-a", "az-b"}}}
	got := cfg.AZFallbackOrderValue()
	if len(got) != 2 || got[0] != "az-a" || got[1] != "az-b" {
		t.Errorf("AZFallbackOrderValue() = %v; want [az-a az-b]", got)
	}
}

func TestAZShuffleEnabled_Default(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	if cfg.AZShuffleEnabled() {
		t.Error("AZShuffleEnabled() = true with nil Placement; want false")
	}
}

func TestAZShuffleEnabled_ExplicitTrue(t *testing.T) {
	t.Parallel()
	tr := true
	cfg := &config.CPIConfig{Placement: &config.PlacementConfig{AZShuffle: &tr}}
	if !cfg.AZShuffleEnabled() {
		t.Error("AZShuffleEnabled() = false with *true; want true")
	}
}

// ---------------------------------------------------------------------------
// IPConflictProbeMode / ActiveIPProbeEnabled
// ---------------------------------------------------------------------------

// TestIPConflictProbeMode_Empty verifies empty field normalizes to "off".
func TestIPConflictProbeMode_Empty(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	if got := cfg.IPConflictProbeMode(); got != "off" {
		t.Errorf("IPConflictProbeMode() = %q with empty field; want %q", got, "off")
	}
}

// TestIPConflictProbeMode_Off verifies "off" is returned verbatim.
func TestIPConflictProbeMode_Off(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{IPConflictProbe: "off"}
	if got := cfg.IPConflictProbeMode(); got != "off" {
		t.Errorf("IPConflictProbeMode() = %q; want %q", got, "off")
	}
}

// TestIPConflictProbeMode_OffUppercase verifies "OFF" normalizes to "off".
func TestIPConflictProbeMode_OffUppercase(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{IPConflictProbe: "OFF"}
	if got := cfg.IPConflictProbeMode(); got != "off" {
		t.Errorf("IPConflictProbeMode() = %q for %q; want %q", got, "OFF", "off")
	}
}

// TestIPConflictProbeMode_Agent verifies "agent" is returned normalized.
func TestIPConflictProbeMode_Agent(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{IPConflictProbe: "agent"}
	if got := cfg.IPConflictProbeMode(); got != "agent" {
		t.Errorf("IPConflictProbeMode() = %q; want %q", got, "agent")
	}
}

// TestActiveIPProbeEnabled_EmptyField verifies empty → not enabled.
func TestActiveIPProbeEnabled_EmptyField(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	if cfg.ActiveIPProbeEnabled() {
		t.Error("ActiveIPProbeEnabled() = true with empty IPConflictProbe; want false")
	}
}

// TestActiveIPProbeEnabled_Off verifies "off" → not enabled.
func TestActiveIPProbeEnabled_Off(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{IPConflictProbe: "off"}
	if cfg.ActiveIPProbeEnabled() {
		t.Error("ActiveIPProbeEnabled() = true with IPConflictProbe=off; want false")
	}
}

// TestActiveIPProbeEnabled_Agent verifies "agent" → enabled.
func TestActiveIPProbeEnabled_Agent(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{IPConflictProbe: "agent"}
	if !cfg.ActiveIPProbeEnabled() {
		t.Error("ActiveIPProbeEnabled() = false with IPConflictProbe=agent; want true")
	}
}

// TestIPConflictProbeValidation_Valid_Agent verifies "agent" passes validation.
func TestIPConflictProbeValidation_Valid_Agent(t *testing.T) {
	t.Parallel()
	json := baseConfigJSON(`"ip_conflict_probe": "agent"`)
	_, err := mustLoad(t, json)
	if err != nil {
		t.Errorf("unexpected validation error for ip_conflict_probe=agent: %v", err)
	}
}

// TestIPConflictProbeValidation_Valid_Off verifies "off" passes validation.
func TestIPConflictProbeValidation_Valid_Off(t *testing.T) {
	t.Parallel()
	json := baseConfigJSON(`"ip_conflict_probe": "off"`)
	_, err := mustLoad(t, json)
	if err != nil {
		t.Errorf("unexpected validation error for ip_conflict_probe=off: %v", err)
	}
}

// TestIPConflictProbeValidation_Valid_Empty verifies absent field passes validation.
func TestIPConflictProbeValidation_Valid_Empty(t *testing.T) {
	t.Parallel()
	json := baseConfigJSON(``)
	_, err := mustLoad(t, json)
	if err != nil {
		t.Errorf("unexpected validation error for absent ip_conflict_probe: %v", err)
	}
}

// TestIPConflictProbeValidation_Invalid_ARP verifies "arp" fails validation.
func TestIPConflictProbeValidation_Invalid_ARP(t *testing.T) {
	t.Parallel()
	json := baseConfigJSON(`"ip_conflict_probe": "arp"`)
	_, err := mustLoad(t, json)
	assertCloudError(t, err, "ip_conflict_probe")
}

// TestIPConflictProbeValidation_Invalid_Garbage verifies unknown value fails validation.
func TestIPConflictProbeValidation_Invalid_Garbage(t *testing.T) {
	t.Parallel()
	json := baseConfigJSON(`"ip_conflict_probe": "garbage"`)
	_, err := mustLoad(t, json)
	assertCloudError(t, err, "ip_conflict_probe")
}

// TestIPConflictProbeMode_NilReceiver verifies nil *CPIConfig returns "off" (LOW-1 guard).
func TestIPConflictProbeMode_NilReceiver(t *testing.T) {
	t.Parallel()
	var c *config.CPIConfig
	if got := c.IPConflictProbeMode(); got != "off" {
		t.Errorf("IPConflictProbeMode() on nil receiver = %q; want %q", got, "off")
	}
}

// TestActiveIPProbeEnabled_NilReceiver verifies nil *CPIConfig returns false.
func TestActiveIPProbeEnabled_NilReceiver(t *testing.T) {
	t.Parallel()
	var c *config.CPIConfig
	if c.ActiveIPProbeEnabled() {
		t.Error("ActiveIPProbeEnabled() on nil receiver = true; want false")
	}
}

// baseConfigJSON builds a minimal valid config JSON with an optional extra
// field (comma-prefixed) injected after network_bridge.
func baseConfigJSON(extra string) string {
	comma := ""
	if extra != "" {
		comma = ", "
	}
	return `{
		"host": "pve.test.local",
		"user": "root@pam",
		"api_token": "test-token",
		"vm_storage": "local-lvm",
		"disk_storage": "local-lvm",
		"network_bridge": "vmbr0"` + comma + extra + `
	}`
}

// --------------------------------------------------------------------------
// TestLoad_VMTypes_RoundTrip
// --------------------------------------------------------------------------

// TestLoad_VMTypes_RoundTrip verifies that vm_types loaded from JSON are
// preserved through Load and that the omit-when-empty contract holds: an
// absent vm_types key leaves the field nil (Go zero-value).
func TestLoad_VMTypes_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("present", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"vm_types": {
				"large": {"cloud_properties": {"cpu": 8, "ram": 16384}},
				"small": {"cloud_properties": {"cpu": 2, "ram": 2048}}
			}
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.VMTypes) != 2 {
			t.Errorf("VMTypes len = %d, want 2", len(cfg.VMTypes))
		}
		large, ok := cfg.VMTypes["large"]
		if !ok {
			t.Fatal("VMTypes missing 'large'")
		}
		if large.CloudProperties == nil {
			t.Error("large.CloudProperties is nil, want non-nil")
		}
		if cpu, _ := large.CloudProperties["cpu"].(float64); int(cpu) != 8 {
			t.Errorf("large.CloudProperties[cpu] = %v, want 8", large.CloudProperties["cpu"])
		}
	})

	t.Run("absent — nil map", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br"
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.VMTypes != nil {
			t.Errorf("VMTypes = %v, want nil when absent", cfg.VMTypes)
		}
	})
}

// --------------------------------------------------------------------------
// TestLoad_DiskTypes_RoundTrip
// --------------------------------------------------------------------------

// TestLoad_DiskTypes_RoundTrip verifies that disk_types loaded from JSON are
// preserved through Load and that the omit-when-empty contract holds.
func TestLoad_DiskTypes_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("present", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"disk_types": {
				"ssd": {"cloud_properties": {"storage": "local-ssd"}},
				"archive": {"cloud_properties": {"storage": "bulk-pool"}}
			}
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.DiskTypes) != 2 {
			t.Errorf("DiskTypes len = %d, want 2", len(cfg.DiskTypes))
		}
		ssd, ok := cfg.DiskTypes["ssd"]
		if !ok {
			t.Fatal("DiskTypes missing 'ssd'")
		}
		if ssd.CloudProperties["storage"] != "local-ssd" {
			t.Errorf("ssd.CloudProperties[storage] = %v, want %q", ssd.CloudProperties["storage"], "local-ssd")
		}
	})

	t.Run("absent — nil map", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br"
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.DiskTypes != nil {
			t.Errorf("DiskTypes = %v, want nil when absent", cfg.DiskTypes)
		}
	})
}

// --------------------------------------------------------------------------
// TestLoad_StorageTiers_RoundTrip
// --------------------------------------------------------------------------

// TestLoad_StorageTiers_RoundTrip verifies that storage_tiers loaded from JSON
// are preserved through Load and that the omit-when-empty contract holds.
func TestLoad_StorageTiers_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("present with types and shared", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"storage_tiers": {
				"fast": {"types": ["lvmthin","rbd"], "shared": true},
				"local": {"types": ["dir","lvm"], "shared": false}
			}
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.StorageTiers) != 2 {
			t.Errorf("StorageTiers len = %d, want 2", len(cfg.StorageTiers))
		}
		fast, ok := cfg.StorageTiers["fast"]
		if !ok {
			t.Fatal("StorageTiers missing 'fast'")
		}
		if len(fast.Types) != 2 || fast.Types[0] != "lvmthin" {
			t.Errorf("fast.Types = %v, want [lvmthin rbd]", fast.Types)
		}
		if fast.Shared == nil || !*fast.Shared {
			t.Error("fast.Shared = nil or false, want *true")
		}
		local, ok := cfg.StorageTiers["local"]
		if !ok {
			t.Fatal("StorageTiers missing 'local'")
		}
		if local.Shared == nil || *local.Shared {
			t.Error("local.Shared = nil or true, want *false")
		}
	})

	t.Run("invalid type rejected", func(t *testing.T) {
		_, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"storage_tiers": {"bad": {"types": ["xfs"]}}
		}`)
		if err == nil {
			t.Fatal("expected validation error for unknown storage type")
		}
		if !strings.Contains(err.Error(), "storage_tiers") {
			t.Errorf("error %q does not mention storage_tiers", err.Error())
		}
	})

	t.Run("absent — nil map", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br"
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.StorageTiers != nil {
			t.Errorf("StorageTiers = %v, want nil when absent", cfg.StorageTiers)
		}
	})
}

// --------------------------------------------------------------------------
// TestLoad_SecurityGroups_RoundTrip
// --------------------------------------------------------------------------

// TestLoad_SecurityGroups_RoundTrip verifies that security_groups loaded from
// JSON are preserved through Load and that the omit-when-empty contract holds.
func TestLoad_SecurityGroups_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("present", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br",
			"security_groups": ["web-dmz", "bosh-vms"]
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.SecurityGroups) != 2 {
			t.Errorf("SecurityGroups len = %d, want 2", len(cfg.SecurityGroups))
		}
		if cfg.SecurityGroups[0] != "web-dmz" || cfg.SecurityGroups[1] != "bosh-vms" {
			t.Errorf("SecurityGroups = %v, want [web-dmz bosh-vms]", cfg.SecurityGroups)
		}
	})

	t.Run("absent — nil slice", func(t *testing.T) {
		cfg, err := mustLoad(t, `{
			"host":"h","user":"u","password":"p",
			"vm_storage":"s","disk_storage":"s","network_bridge":"br"
		}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.SecurityGroups != nil {
			t.Errorf("SecurityGroups = %v, want nil when absent", cfg.SecurityGroups)
		}
	})
}

// --------------------------------------------------------------------------
// TestValidate_DiskPerformance
// --------------------------------------------------------------------------

// floatPtr returns a pointer to f for constructing *float64 fields in literals.
func floatPtr(f float64) *float64 { return &f }

// intPtr returns a pointer to i for constructing *int fields in literals.
func intPtr(i int) *int { return &i }

// TestValidate_DiskPerformance_NilBlock confirms that a nil DiskPerformance block
// passes validation without error (only-when-set contract).
func TestValidate_DiskPerformance_NilBlock(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("nil disk_performance block: unexpected error: %v", err)
	}
}

// TestValidate_DiskPerformance_ValidCache confirms accepted cache mode values.
func TestValidate_DiskPerformance_ValidCache(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"none", "writethrough", "writeback", "unsafe", "directsync"} {
		t.Run(mode, func(t *testing.T) {
			_, err := mustLoad(t, `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"disk_performance":{"cache":"`+mode+`"}
			}`)
			if err != nil {
				t.Errorf("cache=%q: unexpected error: %v", mode, err)
			}
		})
	}
}

// TestValidate_DiskPerformance_InvalidCache confirms an unknown cache string is rejected.
func TestValidate_DiskPerformance_InvalidCache(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"disk_performance":{"cache":"bogus"}
	}`)
	assertCloudError(t, err, "disk_performance.cache")
}

// TestValidate_DiskPerformance_NegativeMBpsRd confirms mbps_rd < 0 is rejected.
func TestValidate_DiskPerformance_NegativeMBpsRd(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
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
		DiskPerformance: &config.DiskPerformanceDefaults{
			MBpsRd: floatPtr(-1),
		},
	}
	assertCloudError(t, cfg.Validate(), "disk_performance.mbps_rd")
}

// TestValidate_DiskPerformance_NegativeIOPSWr confirms iops_wr < 0 is rejected.
func TestValidate_DiskPerformance_NegativeIOPSWr(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
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
		DiskPerformance: &config.DiskPerformanceDefaults{
			IOPSWr: intPtr(-5),
		},
	}
	assertCloudError(t, cfg.Validate(), "disk_performance.iops_wr")
}

// TestValidate_DiskPerformance_FullValidBlock confirms a fully-populated valid
// block passes validation without error.
func TestValidate_DiskPerformance_FullValidBlock(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
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
		DiskPerformance: &config.DiskPerformanceDefaults{
			Iothread:         boolPtr(true),
			Cache:            "writeback",
			Discard:          boolPtr(true),
			SSD:              boolPtr(false),
			MBpsRd:           floatPtr(500),
			MBpsWr:           floatPtr(250),
			IOPSRd:           intPtr(1000),
			IOPSWr:           intPtr(500),
			VirtioSCSISingle: boolPtr(true),
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("full valid disk_performance block: unexpected error: %v", err)
	}
}

// TestValidate_DiskPerformance_NegativeMBpsWr confirms mbps_wr < 0 is rejected.
func TestValidate_DiskPerformance_NegativeMBpsWr(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
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
		DiskPerformance: &config.DiskPerformanceDefaults{
			MBpsWr: floatPtr(-0.5),
		},
	}
	assertCloudError(t, cfg.Validate(), "disk_performance.mbps_wr")
}

// TestValidate_DiskPerformance_NegativeIOPSRd confirms iops_rd < 0 is rejected.
func TestValidate_DiskPerformance_NegativeIOPSRd(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Host:           "h",
		User:           "u",
		Password:       "p",
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
		DiskPerformance: &config.DiskPerformanceDefaults{
			IOPSRd: intPtr(-1),
		},
	}
	assertCloudError(t, cfg.Validate(), "disk_performance.iops_rd")
}

// TestLoad_DiskPerformance_RoundTrip confirms all DiskPerformance fields parse
// and survive the Load → JSON round-trip.
func TestLoad_DiskPerformance_RoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"disk_performance":{
			"iothread":true,
			"cache":"writeback",
			"discard":true,
			"ssd":false,
			"mbps_rd":500.0,
			"mbps_wr":250.0,
			"iops_rd":1000,
			"iops_wr":500,
			"virtio_scsi_single":true
		}
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dp := cfg.DiskPerformance
	if dp == nil {
		t.Fatal("DiskPerformance is nil, want non-nil")
	}
	if dp.Iothread == nil || !*dp.Iothread {
		t.Errorf("Iothread = %v, want *true", dp.Iothread)
	}
	if dp.Cache != "writeback" {
		t.Errorf("Cache = %q, want %q", dp.Cache, "writeback")
	}
	if dp.Discard == nil || !*dp.Discard {
		t.Errorf("Discard = %v, want *true", dp.Discard)
	}
	if dp.SSD == nil || *dp.SSD {
		t.Errorf("SSD = %v, want *false", dp.SSD)
	}
	if dp.MBpsRd == nil || *dp.MBpsRd != 500.0 {
		t.Errorf("MBpsRd = %v, want 500.0", dp.MBpsRd)
	}
	if dp.MBpsWr == nil || *dp.MBpsWr != 250.0 {
		t.Errorf("MBpsWr = %v, want 250.0", dp.MBpsWr)
	}
	if dp.IOPSRd == nil || *dp.IOPSRd != 1000 {
		t.Errorf("IOPSRd = %v, want 1000", dp.IOPSRd)
	}
	if dp.IOPSWr == nil || *dp.IOPSWr != 500 {
		t.Errorf("IOPSWr = %v, want 500", dp.IOPSWr)
	}
	if dp.VirtioSCSISingle == nil || !*dp.VirtioSCSISingle {
		t.Errorf("VirtioSCSISingle = %v, want *true", dp.VirtioSCSISingle)
	}
}

// TestStemcellProvenance
// --------------------------------------------------------------------------

// TestStemcellProvenanceEnabled_NilConfig confirms a nil *CPIConfig returns false.
func TestStemcellProvenanceEnabled_NilConfig(t *testing.T) {
	t.Parallel()
	var c *config.CPIConfig
	if c.StemcellProvenanceEnabled() {
		t.Error("nil *CPIConfig: StemcellProvenanceEnabled() = true, want false")
	}
}

// TestStemcellOrphanPruneEnabled_NilConfig confirms a nil *CPIConfig returns false.
func TestStemcellOrphanPruneEnabled_NilConfig(t *testing.T) {
	t.Parallel()
	var c *config.CPIConfig
	if c.StemcellOrphanPruneEnabled() {
		t.Error("nil *CPIConfig: StemcellOrphanPruneEnabled() = true, want false")
	}
}

// TestStemcellOrphanPruneDryRun_NilConfig confirms a nil *CPIConfig returns false.
func TestStemcellOrphanPruneDryRun_NilConfig(t *testing.T) {
	t.Parallel()
	var c *config.CPIConfig
	if c.StemcellOrphanPruneDryRun() {
		t.Error("nil *CPIConfig: StemcellOrphanPruneDryRun() = true, want false")
	}
}

// TestStemcellDirectorID_NilConfig confirms a nil *CPIConfig returns "".
func TestStemcellDirectorID_NilConfig(t *testing.T) {
	t.Parallel()
	var c *config.CPIConfig
	if got := c.StemcellDirectorID(); got != "" {
		t.Errorf("nil *CPIConfig: StemcellDirectorID() = %q, want \"\"", got)
	}
}

// TestStemcellAccessors_NilBlock confirms all accessors return zero values when
// Stemcell block is nil on an otherwise valid CPIConfig.
func TestStemcellAccessors_NilBlock(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{}
	if c.StemcellProvenanceEnabled() {
		t.Error("nil Stemcell block: StemcellProvenanceEnabled() = true, want false")
	}
	if c.StemcellOrphanPruneEnabled() {
		t.Error("nil Stemcell block: StemcellOrphanPruneEnabled() = true, want false")
	}
	if c.StemcellOrphanPruneDryRun() {
		t.Error("nil Stemcell block: StemcellOrphanPruneDryRun() = true, want false")
	}
	if got := c.StemcellDirectorID(); got != "" {
		t.Errorf("nil Stemcell block: StemcellDirectorID() = %q, want \"\"", got)
	}
}

// TestStemcellAccessors_PtrFalse confirms *false pointers return false.
func TestStemcellAccessors_PtrFalse(t *testing.T) {
	t.Parallel()
	f := false
	c := &config.CPIConfig{
		Stemcell: &config.StemcellProvenanceConfig{
			Provenance:   &f,
			PruneOrphans: &f,
			PruneDryRun:  &f,
		},
	}
	if c.StemcellProvenanceEnabled() {
		t.Error("*false Provenance: StemcellProvenanceEnabled() = true, want false")
	}
	if c.StemcellOrphanPruneEnabled() {
		t.Error("*false PruneOrphans: StemcellOrphanPruneEnabled() = true, want false")
	}
	if c.StemcellOrphanPruneDryRun() {
		t.Error("*false PruneDryRun: StemcellOrphanPruneDryRun() = true, want false")
	}
}

// TestStemcellAccessors_PtrTrue confirms *true pointers return true.
func TestStemcellAccessors_PtrTrue(t *testing.T) {
	t.Parallel()
	tr := true
	c := &config.CPIConfig{
		Stemcell: &config.StemcellProvenanceConfig{
			Provenance:   &tr,
			PruneOrphans: &tr,
			PruneDryRun:  &tr,
			DirectorID:   "bosh-director-1",
		},
	}
	if !c.StemcellProvenanceEnabled() {
		t.Error("*true Provenance: StemcellProvenanceEnabled() = false, want true")
	}
	if !c.StemcellOrphanPruneEnabled() {
		t.Error("*true PruneOrphans: StemcellOrphanPruneEnabled() = false, want true")
	}
	if !c.StemcellOrphanPruneDryRun() {
		t.Error("*true PruneDryRun: StemcellOrphanPruneDryRun() = false, want true")
	}
	if got := c.StemcellDirectorID(); got != "bosh-director-1" {
		t.Errorf("StemcellDirectorID() = %q, want \"bosh-director-1\"", got)
	}
}

// TestStemcellDirectorID_EmptyString confirms empty DirectorID returns "".
func TestStemcellDirectorID_EmptyString(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{
		Stemcell: &config.StemcellProvenanceConfig{
			DirectorID: "",
		},
	}
	if got := c.StemcellDirectorID(); got != "" {
		t.Errorf("empty DirectorID: StemcellDirectorID() = %q, want \"\"", got)
	}
}

// TestValidate_Stemcell_NilBlock confirms nil Stemcell block passes validation
// (only-when-set contract).
func TestValidate_Stemcell_NilBlock(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br"
	}`)
	if err != nil {
		t.Fatalf("nil stemcell block: unexpected error: %v", err)
	}
}

// TestValidate_Stemcell_ValidDirectorID confirms well-formed director IDs pass.
func TestValidate_Stemcell_ValidDirectorID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"bosh-1", "director", "a", "my-bosh-director-2", "123"} {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			_, err := mustLoad(t, `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"stemcell":{"director_id":"`+id+`"}
			}`)
			if err != nil {
				t.Errorf("director_id=%q: unexpected error: %v", id, err)
			}
		})
	}
}

// TestValidate_Stemcell_InvalidDirectorID confirms all-symbol director IDs are
// rejected (must contain at least one alphanumeric or hyphen character).
func TestValidate_Stemcell_InvalidDirectorID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"@@@", "!!!", "...", "***"} {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			_, err := mustLoad(t, `{
				"host":"h","user":"u","password":"p",
				"vm_storage":"s","disk_storage":"s","network_bridge":"br",
				"stemcell":{"director_id":"`+id+`"}
			}`)
			assertCloudError(t, err, "stemcell.director_id")
		})
	}
}

// TestValidate_Stemcell_EmptyDirectorIDValid confirms an absent director_id
// (empty string) passes validation even when the block is present.
func TestValidate_Stemcell_EmptyDirectorIDValid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"stemcell":{"provenance":true}
	}`)
	if err != nil {
		t.Fatalf("stemcell block with no director_id: unexpected error: %v", err)
	}
}

// TestStemcell_JSONMarshal_NilOmitted confirms a CPIConfig with nil Stemcell
// does not emit a "stemcell" key (byte-identical / omitempty contract).
func TestStemcell_JSONMarshal_NilOmitted(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{
		Host:          "h",
		User:          "u",
		Password:      "p",
		VMStorage:     "s",
		DiskStorage:   "s",
		NetworkBridge: "br",
		// Stemcell deliberately absent
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(b, []byte(`"stemcell"`)) {
		t.Errorf("nil Stemcell: JSON contains \"stemcell\" key, want omitted; got %s", b)
	}
}

// TestStemcell_JSONMarshal_SetBlockIncluded confirms a CPIConfig with a
// non-nil Stemcell block emits the "stemcell" key with correct fields.
func TestStemcell_JSONMarshal_SetBlockIncluded(t *testing.T) {
	t.Parallel()
	tr := true
	c := &config.CPIConfig{
		Host:          "h",
		User:          "u",
		Password:      "p",
		VMStorage:     "s",
		DiskStorage:   "s",
		NetworkBridge: "br",
		Stemcell: &config.StemcellProvenanceConfig{
			Provenance: &tr,
			DirectorID: "my-director",
		},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"stemcell"`)) {
		t.Errorf("non-nil Stemcell: JSON missing \"stemcell\" key; got %s", b)
	}
	if !bytes.Contains(b, []byte(`"director_id":"my-director"`)) {
		t.Errorf("non-nil Stemcell: JSON missing director_id; got %s", b)
	}
	if !bytes.Contains(b, []byte(`"provenance":true`)) {
		t.Errorf("non-nil Stemcell: JSON missing provenance:true; got %s", b)
	}
}

// ---------------------------------------------------------------------------
// RetryConfig.Pushback — accessor defaults, JSON round-trip, validation
// ---------------------------------------------------------------------------

func TestRetryPushback_Defaults_WhenUnset(t *testing.T) {
	t.Parallel()
	// A nil config returns the class defaults (5000ms base, 60000ms cap).
	var c *config.CPIConfig
	p := c.RetryPushback()
	if p.BaseMs != 5000 {
		t.Errorf("default BaseMs = %d, want 5000", p.BaseMs)
	}
	if p.CapMs != 60000 {
		t.Errorf("default CapMs = %d, want 60000", p.CapMs)
	}
	if p.MaxAttempts != 0 {
		t.Errorf("default MaxAttempts = %d, want 0 (caller chooses)", p.MaxAttempts)
	}
}

func TestRetryPushback_Defaults_WhenRetryBlockNil(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{}
	p := c.RetryPushback()
	if p.BaseMs != 5000 {
		t.Errorf("default BaseMs = %d, want 5000", p.BaseMs)
	}
	if p.CapMs != 60000 {
		t.Errorf("default CapMs = %d, want 60000", p.CapMs)
	}
}

func TestRetryPushback_Defaults_WhenPushbackSubfieldNil(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{
		Retry: &config.RetryConfig{},
	}
	p := c.RetryPushback()
	if p.BaseMs != 5000 {
		t.Errorf("default BaseMs = %d, want 5000", p.BaseMs)
	}
	if p.CapMs != 60000 {
		t.Errorf("default CapMs = %d, want 60000", p.CapMs)
	}
}

func TestRetryPushback_SetValues_Override(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{
		Retry: &config.RetryConfig{
			Pushback: &config.RetryPolicy{
				BaseMs:      8000,
				CapMs:       90000,
				MaxAttempts: 7,
			},
		},
	}
	p := c.RetryPushback()
	if p.BaseMs != 8000 {
		t.Errorf("BaseMs = %d, want 8000", p.BaseMs)
	}
	if p.CapMs != 90000 {
		t.Errorf("CapMs = %d, want 90000", p.CapMs)
	}
	if p.MaxAttempts != 7 {
		t.Errorf("MaxAttempts = %d, want 7", p.MaxAttempts)
	}
}

func TestRetryPushback_JSONMarshal_NilOmitted(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{
		Retry: &config.RetryConfig{},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(b, []byte(`"pushback"`)) {
		t.Errorf("nil Pushback block must not appear in JSON; got %s", b)
	}
}

func TestRetryPushback_JSONMarshal_SetIncluded(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{
		Retry: &config.RetryConfig{
			Pushback: &config.RetryPolicy{
				BaseMs: 6000,
				CapMs:  70000,
			},
		},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"pushback"`)) {
		t.Errorf("non-nil Pushback must appear in JSON; got %s", b)
	}
	if !bytes.Contains(b, []byte(`"base_ms":6000`)) {
		t.Errorf("base_ms:6000 missing; got %s", b)
	}
}

func TestValidate_RetryPushback_NegativeBaseMs(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.Retry = &config.RetryConfig{
		Pushback: &config.RetryPolicy{BaseMs: -1},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for negative base_ms, got nil")
	}
	if !strings.Contains(err.Error(), "retry.pushback.base_ms") {
		t.Errorf("error must mention retry.pushback.base_ms; got %v", err)
	}
}

func TestValidate_RetryPushback_NegativeCapMs(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.Retry = &config.RetryConfig{
		Pushback: &config.RetryPolicy{CapMs: -1},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for negative cap_ms, got nil")
	}
	if !strings.Contains(err.Error(), "retry.pushback.cap_ms") {
		t.Errorf("error must mention retry.pushback.cap_ms; got %v", err)
	}
}

func TestValidate_RetryPushback_CapLtBase(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.Retry = &config.RetryConfig{
		Pushback: &config.RetryPolicy{BaseMs: 10000, CapMs: 5000},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for cap_ms < base_ms, got nil")
	}
	if !strings.Contains(err.Error(), "retry.pushback") {
		t.Errorf("error must mention retry.pushback; got %v", err)
	}
}

func TestValidate_RetryPushback_CapEqualsBase_Valid(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.RegistryEndpoint = "https://registry.example.com:25777"
	c.Retry = &config.RetryConfig{
		Pushback: &config.RetryPolicy{BaseMs: 5000, CapMs: 5000},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("cap == base should be valid; got %v", err)
	}
}

func TestValidate_RetryPushback_NegativeMaxAttempts(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.Retry = &config.RetryConfig{
		Pushback: &config.RetryPolicy{MaxAttempts: -1},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_attempts, got nil")
	}
	if !strings.Contains(err.Error(), "retry.pushback.max_attempts") {
		t.Errorf("error must mention retry.pushback.max_attempts; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// max_inflight_per_node
// ---------------------------------------------------------------------------

// TestMaxInflightPerNode_DefaultZero confirms that an unset max_inflight_per_node
// produces 0 via the accessor (unlimited, byte-identical current behavior).
func TestMaxInflightPerNode_DefaultZero(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{}
	if got := c.MaxInflightPerNodeLimit(); got != 0 {
		t.Errorf("expected 0 for unset field, got %d", got)
	}
}

// TestMaxInflightPerNode_SetValue confirms accessor returns the configured value.
func TestMaxInflightPerNode_SetValue(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{MaxInflightPerNode: 4}
	if got := c.MaxInflightPerNodeLimit(); got != 4 {
		t.Errorf("expected 4, got %d", got)
	}
}

// TestMaxInflightPerNode_JSONRoundTrip confirms the field marshals and unmarshals.
func TestMaxInflightPerNode_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{MaxInflightPerNode: 8}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"max_inflight_per_node":8`) {
		t.Errorf("marshaled JSON missing field: %s", b)
	}
	var c2 config.CPIConfig
	if err := json.Unmarshal(b, &c2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c2.MaxInflightPerNode != 8 {
		t.Errorf("after round-trip, expected 8, got %d", c2.MaxInflightPerNode)
	}
}

// TestMaxInflightPerNode_OmitWhenZero confirms omitempty drops the field at zero.
func TestMaxInflightPerNode_OmitWhenZero(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "max_inflight_per_node") {
		t.Errorf("zero value should be omitted from JSON; got %s", b)
	}
}

// TestValidate_MaxInflightPerNode_NegativeRejected confirms negative values fail validation.
func TestValidate_MaxInflightPerNode_NegativeRejected(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.MaxInflightPerNode = -1
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_inflight_per_node, got nil")
	}
	if !strings.Contains(err.Error(), "max_inflight_per_node") {
		t.Errorf("error must mention max_inflight_per_node; got %v", err)
	}
}

// TestValidate_MaxInflightPerNode_ZeroValid confirms zero (unlimited) passes validation.
func TestValidate_MaxInflightPerNode_ZeroValid(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.RegistryEndpoint = "https://registry.example.com"
	c.MaxInflightPerNode = 0
	if err := c.Validate(); err != nil {
		t.Errorf("expected no error for zero max_inflight_per_node, got: %v", err)
	}
}

// TestValidate_MaxInflightPerNode_PositiveValid confirms a positive value passes.
func TestValidate_MaxInflightPerNode_PositiveValid(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.RegistryEndpoint = "https://registry.example.com"
	c.MaxInflightPerNode = 4
	if err := c.Validate(); err != nil {
		t.Errorf("expected no error for max_inflight_per_node=4, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// StrictConfigValidation — accessor tests
// --------------------------------------------------------------------------

// TestStrictConfigValidationEnabled_NilField confirms nil *bool → false.
func TestStrictConfigValidationEnabled_NilField(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{}
	if c.StrictConfigValidationEnabled() {
		t.Error("nil StrictConfigValidation should return false")
	}
}

// TestStrictConfigValidationEnabled_ExplicitFalse confirms *false → false.
func TestStrictConfigValidationEnabled_ExplicitFalse(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{StrictConfigValidation: boolPtr(false)}
	if c.StrictConfigValidationEnabled() {
		t.Error("explicit false should return false")
	}
}

// TestStrictConfigValidationEnabled_ExplicitTrue confirms *true → true.
func TestStrictConfigValidationEnabled_ExplicitTrue(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{StrictConfigValidation: boolPtr(true)}
	if !c.StrictConfigValidationEnabled() {
		t.Error("explicit true should return true")
	}
}

// TestStrictConfigValidationEnabled_NilReceiver confirms nil *CPIConfig → false.
func TestStrictConfigValidationEnabled_NilReceiver(t *testing.T) {
	t.Parallel()
	var c *config.CPIConfig
	if c.StrictConfigValidationEnabled() {
		t.Error("nil receiver should return false")
	}
}

// --------------------------------------------------------------------------
// StrictConfigValidation — flag OFF: byte-identical guarantee
// --------------------------------------------------------------------------

// TestStrictOff_UseHaRulesWithoutAntiAffinityStillLoads confirms that with the
// flag off, a config with use_ha_rules=true but anti_affinity.enabled=false
// loads without error (current tolerated behavior preserved).
func TestStrictOff_UseHaRulesWithoutAntiAffinityStillLoads(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement": {
			"anti_affinity": {
				"enabled": false,
				"use_ha_rules": true
			}
		}
	}`)
	if err != nil {
		t.Errorf("flag off: use_ha_rules without anti_affinity.enabled must not error; got: %v", err)
	}
}

// TestStrictOff_UnknownKeyOnlyWarns confirms an unknown top-level key loads
// without error when strict is off.
func TestStrictOff_UnknownKeyOnlyWarns(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"totally_unknown_key_xyz": 42
	}`)
	if err != nil {
		t.Errorf("flag off: unknown key must not produce an error; got: %v", err)
	}
}

// --------------------------------------------------------------------------
// StrictConfigValidation — flag ON: each rule fires
// --------------------------------------------------------------------------

// TestStrictOn_UnknownKey confirms an unknown top-level key becomes a hard
// error when strict_config_validation=true.
func TestStrictOn_UnknownKey(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"strict_config_validation": true,
		"totally_unknown_key_xyz": 42
	}`)
	if err == nil {
		t.Fatal("expected error for unknown key under strict mode, got nil")
	}
	if !strings.Contains(err.Error(), "totally_unknown_key_xyz") {
		t.Errorf("error must name the unknown key; got: %v", err)
	}
}

// TestStrictOn_UseHaRulesWithoutAntiAffinity confirms that use_ha_rules=true
// without anti_affinity.enabled=true is a hard error under strict mode.
func TestStrictOn_UseHaRulesWithoutAntiAffinity(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"strict_config_validation": true,
		"placement": {
			"anti_affinity": {
				"enabled": false,
				"use_ha_rules": true
			}
		}
	}`)
	if err == nil {
		t.Fatal("expected error for use_ha_rules without anti_affinity.enabled, got nil")
	}
	if !strings.Contains(err.Error(), "use_ha_rules") {
		t.Errorf("error must mention use_ha_rules; got: %v", err)
	}
	if !strings.Contains(err.Error(), "anti_affinity") {
		t.Errorf("error must mention anti_affinity; got: %v", err)
	}
}

// TestStrictOn_NetworkModeSdnNeedsZone confirms that network_mode=sdn with no
// sdn_zone and sdn_auto_manage_zone=false is a hard error under strict mode.
func TestStrictOn_NetworkModeSdnNeedsZone(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"strict_config_validation": true,
		"network_mode": "sdn",
		"sdn_zone": "",
		"sdn_auto_manage_zone": false
	}`)
	if err == nil {
		t.Fatal("expected error for sdn mode without zone, got nil")
	}
	if !strings.Contains(err.Error(), "sdn_zone") {
		t.Errorf("error must mention sdn_zone; got: %v", err)
	}
}

// TestStrictOn_NetworkModeAutoZoneEmptyNoError confirms that network_mode=auto
// with an empty sdn_zone does NOT error under strict mode (auto exempted).
func TestStrictOn_NetworkModeAutoZoneEmptyNoError(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"strict_config_validation": true,
		"network_mode": "auto"
	}`)
	if err != nil {
		t.Errorf("network_mode=auto with empty sdn_zone must not error under strict; got: %v", err)
	}
}

// TestStrictOn_DlbRequireSharedStorageWithDlbDisabled confirms that setting
// require_shared_storage when DLB is not enabled is a hard error under strict.
func TestStrictOn_DlbRequireSharedStorageWithDlbDisabled(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"strict_config_validation": true,
		"placement": {
			"dlb": {
				"enabled": false,
				"require_shared_storage": false
			}
		}
	}`)
	if err == nil {
		t.Fatal("expected error for require_shared_storage with dlb disabled, got nil")
	}
	if !strings.Contains(err.Error(), "require_shared_storage") {
		t.Errorf("error must mention require_shared_storage; got: %v", err)
	}
}

// TestStrictOn_ValidStrictConfig confirms a fully consistent config loads
// cleanly under strict mode.
func TestStrictOn_ValidStrictConfig(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"strict_config_validation": true,
		"network_mode": "sdn",
		"sdn_zone": "myzone",
		"placement": {
			"anti_affinity": {
				"enabled": true,
				"use_ha_rules": true
			},
			"dlb": {
				"enabled": true,
				"require_shared_storage": false
			}
		}
	}`)
	if err != nil {
		t.Errorf("valid strict config must load cleanly; got: %v", err)
	}
}

// TestStrictOn_NetworkModeSdnAutoManageZoneNoError confirms that
// network_mode=sdn with sdn_auto_manage_zone=true does not require sdn_zone.
func TestStrictOn_NetworkModeSdnAutoManageZoneNoError(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"strict_config_validation": true,
		"network_mode": "sdn",
		"sdn_auto_manage_zone": true
	}`)
	if err != nil {
		t.Errorf("network_mode=sdn with sdn_auto_manage_zone=true must not error; got: %v", err)
	}
}

// TestStrictOff_DlbRequireSharedStorageWithDlbDisabled_NoError confirms that
// with the flag off, require_shared_storage with DLB disabled still loads
// without error (byte-identical behavior preserved).
func TestStrictOff_DlbRequireSharedStorageWithDlbDisabled_NoError(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host":"h","user":"u","password":"p",
		"vm_storage":"s","disk_storage":"s","network_bridge":"br",
		"placement": {
			"dlb": {
				"enabled": false,
				"require_shared_storage": false
			}
		}
	}`)
	if err != nil {
		t.Errorf("flag off: require_shared_storage with dlb disabled must not error; got: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestValidate_AgentModeAuto_Valid
// --------------------------------------------------------------------------

func TestValidate_AgentModeAuto_Valid(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"agent_mode": "auto"
	}`)
	if err != nil {
		t.Errorf("agent_mode=auto must be valid; got: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestValidate_AgentModeUnknown_StillFails
// --------------------------------------------------------------------------

func TestValidate_AgentModeUnknown_StillFails(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"agent_mode": "bogus"
	}`)
	assertCloudError(t, err, "agent_mode must be one of")
}

// --------------------------------------------------------------------------
// TestApplyDefaults_AutoNotDefault
// --------------------------------------------------------------------------

func TestApplyDefaults_AutoNotDefault(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	cfg.ApplyDefaults()
	if cfg.AgentMode != "cloudinit" {
		t.Errorf("blank AgentMode: ApplyDefaults must set cloudinit, got %q", cfg.AgentMode)
	}
}

// --------------------------------------------------------------------------
// §7.30 PVE API transport tuning — round-trip and validation tests.
// --------------------------------------------------------------------------

// TestLoad_PVEAPITransportTuning_RoundTrip verifies all 5 transport fields
// decode correctly from JSON and land in the struct with the expected values.
func TestLoad_PVEAPITransportTuning_RoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"pve_api_dial_timeout_sec": 15,
		"pve_api_tls_handshake_timeout_sec": 20,
		"pve_api_max_idle_conns_per_host": 50,
		"pve_api_idle_conn_timeout_sec": 120,
		"pve_api_tcp_keepalive_sec": 30
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PVEDialTimeoutSec != 15 {
		t.Errorf("PVEDialTimeoutSec: got %d, want 15", cfg.PVEDialTimeoutSec)
	}
	if cfg.PVETLSHandshakeTimeoutSec != 20 {
		t.Errorf("PVETLSHandshakeTimeoutSec: got %d, want 20", cfg.PVETLSHandshakeTimeoutSec)
	}
	if cfg.PVEMaxIdleConnsPerHost != 50 {
		t.Errorf("PVEMaxIdleConnsPerHost: got %d, want 50", cfg.PVEMaxIdleConnsPerHost)
	}
	if cfg.PVEIdleConnTimeoutSec != 120 {
		t.Errorf("PVEIdleConnTimeoutSec: got %d, want 120", cfg.PVEIdleConnTimeoutSec)
	}
	if cfg.PVETCPKeepAliveSec != 30 {
		t.Errorf("PVETCPKeepAliveSec: got %d, want 30", cfg.PVETCPKeepAliveSec)
	}
}

// TestLoad_PVEAPITransportTuning_ZeroByteIdentical verifies that absent fields
// produce zero values — byte-identical behavior to prior releases (SDK no-op).
func TestLoad_PVEAPITransportTuning_ZeroByteIdentical(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br"
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PVEDialTimeoutSec != 0 {
		t.Errorf("PVEDialTimeoutSec: got %d, want 0 (byte-identical)", cfg.PVEDialTimeoutSec)
	}
	if cfg.PVETLSHandshakeTimeoutSec != 0 {
		t.Errorf("PVETLSHandshakeTimeoutSec: got %d, want 0 (byte-identical)", cfg.PVETLSHandshakeTimeoutSec)
	}
	if cfg.PVEMaxIdleConnsPerHost != 0 {
		t.Errorf("PVEMaxIdleConnsPerHost: got %d, want 0 (byte-identical)", cfg.PVEMaxIdleConnsPerHost)
	}
	if cfg.PVEIdleConnTimeoutSec != 0 {
		t.Errorf("PVEIdleConnTimeoutSec: got %d, want 0 (byte-identical)", cfg.PVEIdleConnTimeoutSec)
	}
	if cfg.PVETCPKeepAliveSec != 0 {
		t.Errorf("PVETCPKeepAliveSec: got %d, want 0 (byte-identical)", cfg.PVETCPKeepAliveSec)
	}
}

// TestValidate_PVEAPITransportTuning_NegativeRejected verifies that negative
// values for any of the 5 transport fields are rejected at Validate time.
func TestValidate_PVEAPITransportTuning_NegativeRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		json    string
		wantMsg string
	}{
		{
			name:    "pve_api_dial_timeout_sec negative",
			json:    `{"host":"h","user":"u","password":"p","vm_storage":"s","disk_storage":"s","network_bridge":"br","pve_api_dial_timeout_sec":-1}`,
			wantMsg: "pve_api_dial_timeout_sec must be >= 0",
		},
		{
			name:    "pve_api_tls_handshake_timeout_sec negative",
			json:    `{"host":"h","user":"u","password":"p","vm_storage":"s","disk_storage":"s","network_bridge":"br","pve_api_tls_handshake_timeout_sec":-1}`,
			wantMsg: "pve_api_tls_handshake_timeout_sec must be >= 0",
		},
		{
			name:    "pve_api_max_idle_conns_per_host negative",
			json:    `{"host":"h","user":"u","password":"p","vm_storage":"s","disk_storage":"s","network_bridge":"br","pve_api_max_idle_conns_per_host":-1}`,
			wantMsg: "pve_api_max_idle_conns_per_host must be >= 0",
		},
		{
			name:    "pve_api_idle_conn_timeout_sec negative",
			json:    `{"host":"h","user":"u","password":"p","vm_storage":"s","disk_storage":"s","network_bridge":"br","pve_api_idle_conn_timeout_sec":-1}`,
			wantMsg: "pve_api_idle_conn_timeout_sec must be >= 0",
		},
		{
			name:    "pve_api_tcp_keepalive_sec negative",
			json:    `{"host":"h","user":"u","password":"p","vm_storage":"s","disk_storage":"s","network_bridge":"br","pve_api_tcp_keepalive_sec":-1}`,
			wantMsg: "pve_api_tcp_keepalive_sec must be >= 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := mustLoad(t, tc.json)
			assertCloudError(t, err, tc.wantMsg)
		})
	}
}

// TestValidate_PVEAPITransportTuning_PositiveValid verifies that positive
// values for all 5 fields pass validation.
func TestValidate_PVEAPITransportTuning_PositiveValid(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"pve_api_dial_timeout_sec": 5,
		"pve_api_tls_handshake_timeout_sec": 10,
		"pve_api_max_idle_conns_per_host": 100,
		"pve_api_idle_conn_timeout_sec": 60,
		"pve_api_tcp_keepalive_sec": 45
	}`)
	if err != nil {
		t.Fatalf("positive values should pass validation, got: %v", err)
	}
	if cfg.PVEDialTimeoutSec != 5 {
		t.Errorf("PVEDialTimeoutSec: got %d, want 5", cfg.PVEDialTimeoutSec)
	}
}

// --------------------------------------------------------------------------
// StemcellReplicationConcurrency — accessor + validation tests
// --------------------------------------------------------------------------

// TestStemcellReplicationConcurrencyValue_ZeroResolves confirms 0 → 1 (serial default).
func TestStemcellReplicationConcurrencyValue_ZeroResolves(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{StemcellReplicationConcurrency: 0}
	if got := c.StemcellReplicationConcurrencyValue(); got != 1 {
		t.Errorf("StemcellReplicationConcurrencyValue() = %d; want 1", got)
	}
}

// TestStemcellReplicationConcurrencyValue_NilConfigResolves confirms nil pointer → 1.
func TestStemcellReplicationConcurrencyValue_NilConfigResolves(t *testing.T) {
	t.Parallel()
	var c *config.CPIConfig
	if got := c.StemcellReplicationConcurrencyValue(); got != 1 {
		t.Errorf("nil.StemcellReplicationConcurrencyValue() = %d; want 1", got)
	}
}

// TestStemcellReplicationConcurrencyValue_PositivePassThrough confirms 4 → 4.
func TestStemcellReplicationConcurrencyValue_PositivePassThrough(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{StemcellReplicationConcurrency: 4}
	if got := c.StemcellReplicationConcurrencyValue(); got != 4 {
		t.Errorf("StemcellReplicationConcurrencyValue() = %d; want 4", got)
	}
}

// TestValidate_StemcellReplicationConcurrency_NegativeRejected confirms negative fails.
func TestValidate_StemcellReplicationConcurrency_NegativeRejected(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.RegistryEndpoint = "https://registry.example.com"
	c.StemcellReplicationConcurrency = -1
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for negative stemcell_replication_concurrency, got nil")
	}
	if !strings.Contains(err.Error(), "stemcell_replication_concurrency") {
		t.Errorf("error must mention stemcell_replication_concurrency; got %v", err)
	}
}

// TestValidate_StemcellReplicationConcurrency_TooHighRejected confirms > 64 fails.
func TestValidate_StemcellReplicationConcurrency_TooHighRejected(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.RegistryEndpoint = "https://registry.example.com"
	c.StemcellReplicationConcurrency = 65
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for stemcell_replication_concurrency > 64, got nil")
	}
	if !strings.Contains(err.Error(), "stemcell_replication_concurrency") {
		t.Errorf("error must mention stemcell_replication_concurrency; got %v", err)
	}
}

// TestValidate_StemcellReplicationConcurrency_ZeroValid confirms 0 passes (serial default).
func TestValidate_StemcellReplicationConcurrency_ZeroValid(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.RegistryEndpoint = "https://registry.example.com"
	c.StemcellReplicationConcurrency = 0
	if err := c.Validate(); err != nil {
		t.Errorf("expected no error for stemcell_replication_concurrency=0, got: %v", err)
	}
}

// TestValidate_StemcellReplicationConcurrency_64Valid confirms 64 passes (max allowed).
func TestValidate_StemcellReplicationConcurrency_64Valid(t *testing.T) {
	t.Parallel()
	c := registryBaseCfg()
	c.RegistryEndpoint = "https://registry.example.com"
	c.StemcellReplicationConcurrency = 64
	if err := c.Validate(); err != nil {
		t.Errorf("expected no error for stemcell_replication_concurrency=64, got: %v", err)
	}
}

// TestStemcellReplicationConcurrency_JSONRoundTrip confirms JSON marshal/unmarshal.
func TestStemcellReplicationConcurrency_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{StemcellReplicationConcurrency: 8}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"stemcell_replication_concurrency":8`) {
		t.Errorf("marshaled JSON does not contain field: %s", raw)
	}
	var c2 config.CPIConfig
	if err := json.Unmarshal(raw, &c2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c2.StemcellReplicationConcurrency != 8 {
		t.Errorf("after round-trip, expected 8, got %d", c2.StemcellReplicationConcurrency)
	}
}

// TestStemcellReplicationConcurrency_OmitWhenZero confirms omitempty drops the field at 0.
func TestStemcellReplicationConcurrency_OmitWhenZero(t *testing.T) {
	t.Parallel()
	c := &config.CPIConfig{StemcellReplicationConcurrency: 0}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "stemcell_replication_concurrency") {
		t.Errorf("zero value should be omitted in JSON, but got: %s", raw)
	}
}

// --------------------------------------------------------------------------
// PlacementFallbackMax (§7.31)
// --------------------------------------------------------------------------

// TestPlacementFallbackMax_Negative rejects negative values.
func TestPlacementFallbackMax_Negative(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"placement": {"fallback_max": -1}
	}`)
	assertCloudError(t, err, "placement.fallback_max must be >= 0")
}

// TestPlacementFallbackMax_OverCap rejects values > 5.
func TestPlacementFallbackMax_OverCap(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"placement": {"fallback_max": 6}
	}`)
	assertCloudError(t, err, "placement.fallback_max must be <= 5")
}

// TestPlacementFallbackMax_ZeroIsValid confirms 0 (disabled) passes validation.
func TestPlacementFallbackMax_ZeroIsValid(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "h", "user": "u", "password": "p",
		"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
		"placement": {"fallback_max": 0}
	}`)
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if got := cfg.PlacementFallbackMaxValue(); got != 0 {
		t.Errorf("PlacementFallbackMaxValue(): want 0, got %d", got)
	}
}

// TestPlacementFallbackMax_ValidRange tests accepted values 1-5.
func TestPlacementFallbackMax_ValidRange(t *testing.T) {
	t.Parallel()
	for _, v := range []int{1, 2, 3, 4, 5} {
		v := v
		t.Run(fmt.Sprintf("max=%d", v), func(t *testing.T) {
			t.Parallel()
			cfg, err := mustLoad(t, fmt.Sprintf(`{
				"host": "h", "user": "u", "password": "p",
				"vm_storage": "s", "disk_storage": "s", "network_bridge": "br",
				"placement": {"fallback_max": %d}
			}`, v))
			if err != nil {
				t.Fatalf("fallback_max=%d should be valid, got: %v", v, err)
			}
			if got := cfg.PlacementFallbackMaxValue(); got != v {
				t.Errorf("PlacementFallbackMaxValue(): want %d, got %d", v, got)
			}
		})
	}
}

// TestPlacementFallbackMax_NilReturnsZero confirms nil Placement returns 0.
func TestPlacementFallbackMax_NilReturnsZero(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{} // nil Placement
	if got := cfg.PlacementFallbackMaxValue(); got != 0 {
		t.Errorf("nil Placement: PlacementFallbackMaxValue() = %d, want 0", got)
	}
}

// TestPlacementFallbackMax_NilFallbackMaxReturnsZero confirms nil FallbackMax returns 0.
func TestPlacementFallbackMax_NilFallbackMaxReturnsZero(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Placement: &config.PlacementConfig{
			// FallbackMax nil (absent from JSON)
		},
	}
	if got := cfg.PlacementFallbackMaxValue(); got != 0 {
		t.Errorf("nil FallbackMax: PlacementFallbackMaxValue() = %d, want 0", got)
	}
}

// TestPlacementFallbackMax_RoundTrip confirms JSON marshal/unmarshal preserves the value.
func TestPlacementFallbackMax_RoundTrip(t *testing.T) {
	t.Parallel()
	original := &config.CPIConfig{
		Placement: &config.PlacementConfig{
			FallbackMax: intPtr(2),
		},
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded config.CPIConfig
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := decoded.PlacementFallbackMaxValue(); got != 2 {
		t.Errorf("round-trip: want 2, got %d", got)
	}
}

// TestPlacementFallbackMax_OmitWhenNil confirms nil FallbackMax is omitted from JSON.
func TestPlacementFallbackMax_OmitWhenNil(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Placement: &config.PlacementConfig{},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "fallback_max") {
		t.Errorf("nil FallbackMax should be omitted in JSON, but got: %s", raw)
	}
}
