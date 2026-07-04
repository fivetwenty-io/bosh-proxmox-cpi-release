package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// --------------------------------------------------------------------------
// OTelConfig: zero value is inert (byte-identical behavior when unset)
// --------------------------------------------------------------------------

func TestOTel_ZeroValue_NoValidationErrors_NoDefaultsApplied(t *testing.T) {
	c := baseValidCfg() // Host/User/Password/VMStorage/DiskStorage/NetworkBridge set; OTel left zero
	if err := c.Validate(); err != nil {
		t.Fatalf("zero-value OTel block produced a validation error: %v", err)
	}
	if c.OTel != (config.OTelConfig{}) {
		t.Errorf("ApplyDefaults mutated a disabled OTel block: got %+v, want zero value", c.OTel)
	}
	if c.OTelEnabled() {
		t.Errorf("OTelEnabled() = true for zero-value block, want false")
	}
}

func TestOTelEnabled_NilReceiver_DoesNotPanic(t *testing.T) {
	var c *config.CPIConfig
	if c.OTelEnabled() {
		t.Errorf("OTelEnabled() on nil receiver = true, want false")
	}
}

// --------------------------------------------------------------------------
// Validation: only runs when Enabled is true
// --------------------------------------------------------------------------

func TestOTel_Enabled_EmptyEndpoint_Errors(t *testing.T) {
	c := baseValidCfg()
	c.OTel = config.OTelConfig{Enabled: true, ServiceName: "svc", SampleRatio: 1.0, ExportTimeoutMs: 5000}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected a validation error for empty exporter_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "otel.exporter_endpoint is required") {
		t.Errorf("error %q does not mention otel.exporter_endpoint", err.Error())
	}
}

func TestOTel_Enabled_SampleRatioAboveOne_Errors(t *testing.T) {
	c := baseValidCfg()
	c.OTel = config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:      "svc",
		SampleRatio:      1.5,
		ExportTimeoutMs:  5000,
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected a validation error for sample_ratio 1.5, got nil")
	}
	if !strings.Contains(err.Error(), "otel.sample_ratio must be 0.0-1.0, got 1.5") {
		t.Errorf("error %q does not mention the sample_ratio violation", err.Error())
	}
}

func TestOTel_Enabled_SampleRatioNegative_Errors(t *testing.T) {
	c := baseValidCfg()
	c.OTel = config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:      "svc",
		SampleRatio:      -0.1,
		ExportTimeoutMs:  5000,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "otel.sample_ratio must be 0.0-1.0") {
		t.Fatalf("expected sample_ratio range error, got %v", err)
	}
}

func TestOTel_Enabled_NonPositiveExportTimeout_Errors(t *testing.T) {
	c := baseValidCfg()
	c.OTel = config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:      "svc",
		SampleRatio:      1.0,
		ExportTimeoutMs:  0,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "otel.export_timeout_ms must be > 0") {
		t.Fatalf("expected export_timeout_ms error, got %v", err)
	}
}

func TestOTel_Enabled_BlankServiceNameOverride_Errors(t *testing.T) {
	// Simulates a caller that invokes Validate directly without running
	// ApplyDefaults first (ApplyDefaults would otherwise fill ServiceName).
	c := baseValidCfg()
	c.OTel = config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:      "",
		SampleRatio:      1.0,
		ExportTimeoutMs:  5000,
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "otel.service_name must not be empty") {
		t.Fatalf("expected service_name error, got %v", err)
	}
}

func TestOTel_Enabled_AllFieldsValid_NoError(t *testing.T) {
	c := baseValidCfg()
	c.OTel = config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		Insecure:         true,
		ServiceName:      "bosh-pve-cpi",
		SampleRatio:      0.5,
		ExportTimeoutMs:  3000,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("expected valid OTel config, got %v", err)
	}
}

// --------------------------------------------------------------------------
// ApplyDefaults: fills zero fields only when Enabled is true
// --------------------------------------------------------------------------

func TestOTel_ApplyDefaults_FillsZeroFieldsWhenEnabled(t *testing.T) {
	c := &config.CPIConfig{
		Host:          "pve.example.com",
		User:          "root@pam",
		Password:      "secret",
		VMStorage:     "local-lvm",
		DiskStorage:   "local-lvm",
		NetworkBridge: "vmbr0",
		OTel: config.OTelConfig{
			Enabled:          true,
			ExporterEndpoint: "otel-collector.example.internal:4318",
			// ServiceName, SampleRatio, ExportTimeoutMs left zero.
		},
	}
	c.ApplyDefaults()

	if c.OTel.ServiceName != "bosh-pve-cpi" {
		t.Errorf("ServiceName = %q, want default %q", c.OTel.ServiceName, "bosh-pve-cpi")
	}
	if c.OTel.SampleRatio != 1.0 {
		t.Errorf("SampleRatio = %v, want default 1.0", c.OTel.SampleRatio)
	}
	if c.OTel.ExportTimeoutMs != 5000 {
		t.Errorf("ExportTimeoutMs = %d, want default 5000", c.OTel.ExportTimeoutMs)
	}
	// Explicit fields are preserved, not overwritten.
	if c.OTel.ExporterEndpoint != "otel-collector.example.internal:4318" {
		t.Errorf("ExporterEndpoint mutated by ApplyDefaults: got %q", c.OTel.ExporterEndpoint)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("defaulted+enabled OTel config should validate clean, got %v", err)
	}
}

func TestOTel_ApplyDefaults_PreservesExplicitOverrides(t *testing.T) {
	c := baseValidCfg()
	c.OTel = config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:      "custom-service",
		SampleRatio:      0.25,
		ExportTimeoutMs:  1234,
	}
	c.ApplyDefaults()
	if c.OTel.ServiceName != "custom-service" {
		t.Errorf("ServiceName override overwritten: got %q", c.OTel.ServiceName)
	}
	if c.OTel.SampleRatio != 0.25 {
		t.Errorf("SampleRatio override overwritten: got %v", c.OTel.SampleRatio)
	}
	if c.OTel.ExportTimeoutMs != 1234 {
		t.Errorf("ExportTimeoutMs override overwritten: got %d", c.OTel.ExportTimeoutMs)
	}
}

// TestOTel_SampleRatioZeroSemantics documents and asserts the chosen
// zero-value convention for SampleRatio: zero means "unset/use default", the
// same convention used elsewhere in this package for float64 config fields
// (e.g. EphemeralDiskMinRatio). An explicit sample ratio of exactly 0.0
// ("sample nothing") is therefore not an expressible configuration — it is
// indistinguishable from an absent value and gets defaulted to 1.0 by
// ApplyDefaults when Enabled is true.
func TestOTel_SampleRatioZeroSemantics(t *testing.T) {
	c := baseValidCfg()
	c.OTel = config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		SampleRatio:      0, // explicit zero, indistinguishable from unset
	}
	c.ApplyDefaults()
	if c.OTel.SampleRatio != 1.0 {
		t.Errorf("SampleRatio zero was not treated as unset: got %v, want defaulted 1.0", c.OTel.SampleRatio)
	}

	// Disabled block: zero SampleRatio is left untouched (no default fill),
	// confirming ApplyDefaults never mutates a disabled block.
	d := baseValidCfg()
	d.OTel = config.OTelConfig{SampleRatio: 0}
	d.ApplyDefaults()
	if d.OTel.SampleRatio != 0 {
		t.Errorf("ApplyDefaults filled SampleRatio on a disabled block: got %v", d.OTel.SampleRatio)
	}
}
