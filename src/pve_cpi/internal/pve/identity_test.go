package pve_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

func TestIsRootPamIdentity_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsRootPamIdentity(nil) {
		t.Error("nil cfg should not be root@pam")
	}
}

func TestIsRootPamIdentity_NoCredentialsConfigured(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{User: "root", Realm: "pam"}
	if pve.IsRootPamIdentity(cfg) {
		t.Error("neither api_token nor password set should not be root@pam")
	}
}

func TestIsRootPamIdentity_PasswordAuth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		user     string
		realm    string
		password string
		want     bool
	}{
		{"user with explicit @pam realm", "root@pam", "", "secret", true},
		{"user root, realm pam", "root", "pam", "secret", true},
		{"user root, realm empty defaults pam", "root", "", "secret", true},
		{"user root, realm pve (not pam)", "root", "pve", "secret", false},
		{"non-root user, realm pam", "bosh", "pam", "secret", false},
		{"user with explicit non-pam realm", "root@pve", "", "secret", false},
		{"non-root user with explicit @pam", "admin@pam", "", "secret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{User: tc.user, Realm: tc.realm, Password: tc.password}
			if got := pve.IsRootPamIdentity(cfg); got != tc.want {
				t.Errorf("IsRootPamIdentity(User=%q, Realm=%q) = %v, want %v", tc.user, tc.realm, got, tc.want)
			}
		})
	}
}

func TestIsRootPamIdentity_TokenAuth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		apiToken string
		want     bool
	}{
		{"root@pam token", "root@pam!bosh-token=1234-5678-uuid", true},
		{"non-root user token", "bosh@pve!bosh-token=1234-5678-uuid", false},
		{"admin@pam token (not root)", "admin@pam!tok=uuid", false},
		{"malformed token, no bang separator", "root@pam-no-bang-here", false},
		{"empty token", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{APIToken: tc.apiToken}
			if got := pve.IsRootPamIdentity(cfg); got != tc.want {
				t.Errorf("IsRootPamIdentity(APIToken=%q) = %v, want %v", tc.apiToken, got, tc.want)
			}
		})
	}
}

func TestIsRootPamIdentity_TokenTakesPrecedenceOverPassword(t *testing.T) {
	t.Parallel()
	// Config validation rejects both api_token and password being set
	// simultaneously in production, but IsRootPamIdentity itself must still
	// resolve deterministically (token-first) rather than panic or pick
	// inconsistently, mirroring newClient's own hasToken-first precedence.
	cfg := &config.CPIConfig{
		APIToken: "root@pam!tok=uuid",
		User:     "someoneelse",
		Realm:    "pve",
		Password: "secret",
	}
	if !pve.IsRootPamIdentity(cfg) {
		t.Error("token identity should take precedence and resolve to root@pam")
	}
}
