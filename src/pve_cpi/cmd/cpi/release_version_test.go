package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/version"
)

func writeVersionConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunWithArgs_VersionNamesTheRelease(t *testing.T) {
	t.Parallel()
	withRelease := writeVersionConfig(t, `{"host":"pve.test","release_version":"0.9.5"}`)
	cases := []struct {
		name       string
		args       []string
		want       string
		wantStderr string
	}{
		{"no config", []string{"--version"}, version.String(), ""},
		{"config with a release", []string{"--version", "--config", withRelease},
			"bosh-proxmox-cpi 0.9.5 (package " + version.Version + ", built " + version.BuildDate + ")", ""},
		{"config without a release", []string{"--version", "--config", writeVersionConfig(t, `{"host":"pve.test"}`)},
			version.String(), ""},
		{"implausible release", []string{"--version", "--config",
			writeVersionConfig(t, `{"release_version":"0.9.5; rm -rf /"}`)}, version.String(), ""},
		{"config that won't parse", []string{"--version", "--config", writeVersionConfig(t, `{not json`)},
			version.String(), "cpi: release version unavailable: parse"},
		{"missing config", []string{"--version", "--config", filepath.Join(t.TempDir(), "absent.json")},
			version.String(), "cpi: release version unavailable: open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := runWithArgs(tc.args, strings.NewReader(""), &stdout, &stderr, runOptions{})
			if code != 0 {
				t.Fatalf("exit %d stderr %q", code, stderr.String())
			}
			if got := strings.TrimSpace(stdout.String()); got != tc.want {
				t.Errorf("stdout = %q, want %q", got, tc.want)
			}
			if tc.wantStderr == "" && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing", stderr.String())
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

func TestReleaseVersionFromConfigRejectsOversizedFile(t *testing.T) {
	t.Parallel()
	path := writeVersionConfig(t, `{"release_version":"0.9.5","pad":"`+strings.Repeat("x", maxVersionConfigBytes)+`"}`)
	if rel, err := releaseVersionFromConfig(path); err == nil || rel != "" {
		t.Fatalf("release %q err %v; want an error for an oversized file", rel, err)
	}
}

func TestRunWithArgs_LogsReleaseVersionAtStartup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, release, want string
	}{
		{"with a release", `,"release_version":"0.9.5"`, `"release_version":"0.9.5"`},
		{"without a release", "", `"release_version":"unknown"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfgFile := writeVersionConfig(t, `{"host":"127.0.0.1","user":"root","password":"test-password",`+
				`"vm_storage":"local-lvm","disk_storage":"local-lvm","stemcell_storage":"local",`+
				`"network_bridge":"vmbr0","node":"pve1","log_level":"info","verify_ssl":false`+tc.release+`}`)
			failFactory := func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
				return nil, errors.New("test: stop after startup")
			}
			var stdout, stderr bytes.Buffer
			code := runWithArgs([]string{"--config", cfgFile}, strings.NewReader(""), &stdout, &stderr,
				runOptions{ClientFactory: failFactory})
			if code != 1 {
				t.Fatalf("exit %d, want 1 from the failing client factory", code)
			}
			logs := stderr.String()
			if strings.Count(logs, "cpi: starting") != 1 || !strings.Contains(logs, tc.want) ||
				!strings.Contains(logs, `"package_version":"`+version.Version+`"`) {
				t.Errorf("startup log missing or wrong:\n%s", logs)
			}
		})
	}
}
