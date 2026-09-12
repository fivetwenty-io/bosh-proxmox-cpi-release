package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

func TestStoragePlanRequestParsing(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"disk", `{"method":"create_disk","arguments":[1024,{}]}`, true},
		{"vm", `{"method":"create_vm","arguments":[]}`, true},
		{"context forbidden", `{"method":"create_disk","arguments":[],"context":{"password":"SECRET"}}`, false},
		{"unknown method", `{"method":"delete_vm","arguments":[]}`, false},
		{"trailing", `{"method":"create_disk","arguments":[]} {}`, false},
		{"duplicate", `{"method":"create_disk","method":"create_vm","arguments":[]}`, false},
		{"missing", `{"method":"create_disk"}`, false},
		{"oversized", strings.Repeat("x", storagePlanRequestLimit+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readStoragePlanRequest(strings.NewReader(tc.input))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatal("secret leaked")
			}
		})
	}
}
func TestStoragePlanCommandRendering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{"method":"create_disk","arguments":[1024,{}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		var out, errout bytes.Buffer
		calls := 0
		args := []string{"--request", path, "--config", "config.json"}
		if asJSON {
			args = append(args, "--json")
		}
		code := runStoragePlanWithObserver(args, &out, &errout, func(p string) (handlers.Deps, error) {
			if p != "config.json" {
				t.Fatal(p)
			}
			return handlers.Deps{}, nil
		}, func(_ context.Context, _ handlers.Deps, r handlers.StoragePlanDiagnosticRequest) (*handlers.StoragePlanDiagnostic, error) {
			calls++
			if r.Method != "create_disk" {
				t.Fatal(r)
			}
			return &handlers.StoragePlanDiagnostic{Method: r.Method, ObservationOnly: true, Node: "n1", FrozenMembership: map[string][]string{"P": {"a", "b"}}, Findings: []string{"private journal unavailable"}}, nil
		})
		if code != exitOK || calls != 1 || !strings.Contains(out.String(), "n1") || errout.Len() != 0 {
			t.Fatalf("code=%d out=%s err=%s", code, &out, &errout)
		}
		if asJSON && !strings.Contains(out.String(), `"observation_only": true`) {
			t.Fatal(out.String())
		}
		if !asJSON && !strings.Contains(out.String(), "no capacity is reserved") {
			t.Fatal(out.String())
		}
	}
}
func TestStoragePlanCommandScrubsLoaderError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{"method":"create_disk","arguments":[1024,{}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := runStoragePlanWithLoader([]string{"--request", path}, &out, &errout, func(string) (handlers.Deps, error) { return handlers.Deps{}, errors.New("SECRET-token") })
	if code != exitError || strings.Contains(errout.String(), "SECRET") || out.Len() != 0 {
		t.Fatalf("code=%d output=%s %s", code, &out, &errout)
	}
}
func TestStoragePlanInvalidFlagsNeverLoadClient(t *testing.T) {
	var out, errout bytes.Buffer
	code := runStoragePlanWithLoader([]string{"--password=SECRET"}, &out, &errout, func(string) (handlers.Deps, error) { t.Fatal("loaded client"); return handlers.Deps{}, nil })
	if code != exitUsage || strings.Contains(errout.String(), "SECRET") {
		t.Fatalf("%d %s", code, &errout)
	}
}

func TestStoragePlanHelpSucceedsWithoutLoadingConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runStoragePlanWithLoader([]string{"--help"}, &stdout, &stderr, func(string) (handlers.Deps, error) { t.Fatal("help loaded configuration"); return handlers.Deps{}, nil })
	if code != exitOK || !strings.Contains(stderr.String(), "sanitized CPI request JSON file") {
		t.Fatalf("help exit=%d output=%s", code, &stderr)
	}
}
