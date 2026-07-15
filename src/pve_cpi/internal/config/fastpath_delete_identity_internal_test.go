// Package config internal tests for warnFastPathDeleteNonRootIdentity: the
// config-load Warn surfaced when fast_path_delete is enabled under a PVE
// identity that PVE's skiplock parameter is not honored for.
package config

import (
	"bytes"
	"strings"
	"testing"
)

func boolPtrFPD(b bool) *bool { return &b }

func TestWarnFastPathDeleteNonRootIdentity_RootPamPassword_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{
		FastPathDelete: boolPtrFPD(true),
		User:           "root",
		Realm:          "pam",
		Password:       "secret",
	}
	warnFastPathDeleteNonRootIdentity(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("root@pam password auth must not warn, got: %s", buf.String())
	}
}

func TestWarnFastPathDeleteNonRootIdentity_RootPamPasswordComposedForm_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{
		FastPathDelete: boolPtrFPD(true),
		User:           "root@pam",
		Password:       "secret",
	}
	warnFastPathDeleteNonRootIdentity(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("root@pam (composed user) password auth must not warn, got: %s", buf.String())
	}
}

func TestWarnFastPathDeleteNonRootIdentity_OtherUserPassword_WarnsNamingIdentity(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{
		FastPathDelete: boolPtrFPD(true),
		User:           "bosh",
		Realm:          "pve",
		Password:       "secret",
	}
	warnFastPathDeleteNonRootIdentity(cfg, &buf)
	out := buf.String()
	if !strings.Contains(out, "fast_path_delete is enabled") {
		t.Fatalf("expected the fast_path_delete Warn, got: %s", out)
	}
	if !strings.Contains(out, "\"bosh@pve\"") {
		t.Errorf("expected the Warn to name the resolved identity bosh@pve, got: %s", out)
	}
}

func TestWarnFastPathDeleteNonRootIdentity_APIToken_AlwaysWarns(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"non-root token", "bosh@pve!cpi=00000000-0000-0000-0000-000000000000"},
		{"root@pam-owned token", "root@pam!cpi=00000000-0000-0000-0000-000000000000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			cfg := &CPIConfig{
				FastPathDelete: boolPtrFPD(true),
				APIToken:       c.token,
			}
			warnFastPathDeleteNonRootIdentity(cfg, &buf)
			out := buf.String()
			if !strings.Contains(out, "fast_path_delete is enabled") {
				t.Fatalf("expected the fast_path_delete Warn for a token identity (even root@pam-owned), got: %s", out)
			}
			// The logged identity must never include the secret UUID after "=".
			if strings.Contains(out, "00000000-0000-0000-0000-000000000000") {
				t.Errorf("logged identity must not leak the token secret, got: %s", out)
			}
		})
	}
}

func TestWarnFastPathDeleteNonRootIdentity_Disabled_Silent(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{
		FastPathDelete: boolPtrFPD(false),
		User:           "bosh",
		Realm:          "pve",
		Password:       "secret",
	}
	warnFastPathDeleteNonRootIdentity(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("fast_path_delete=false must never warn, got: %s", buf.String())
	}
}

func TestWarnFastPathDeleteNonRootIdentity_Unset_Silent(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{
		User:     "bosh",
		Realm:    "pve",
		Password: "secret",
	}
	warnFastPathDeleteNonRootIdentity(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("fast_path_delete unset (nil) must never warn, got: %s", buf.String())
	}
}

func TestWarnFastPathDeleteNonRootIdentity_NoAuthConfigured_Silent(t *testing.T) {
	// Neither Password nor APIToken set: validateAuth elsewhere already
	// accumulates a hard error for this; this diagnostic has nothing useful
	// to add and must not panic or warn.
	var buf bytes.Buffer
	cfg := &CPIConfig{
		FastPathDelete: boolPtrFPD(true),
		User:           "bosh",
		Realm:          "pve",
	}
	warnFastPathDeleteNonRootIdentity(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("no auth configured must not warn, got: %s", buf.String())
	}
}
