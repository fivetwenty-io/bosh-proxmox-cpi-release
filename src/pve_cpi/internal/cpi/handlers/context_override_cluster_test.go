package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const (
	inheritedNamesText = "inherits job-level node, storage, bridge, or profile names"
	d9ResetText        = "destroy_unreferenced_disks is off for this request"
)

// clusterScopeDeps returns override-enabled Deps for base whose log lines
// land in the returned observer.
func clusterScopeDeps(base *config.CPIConfig) (handlers.Deps, *log.Observer, *fakeTransportClientFactory) {
	observedLogger, obs := log.NewObservedLogger(log.LevelDebug)
	tracker := &fakeTransportClientFactory{}
	return handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: observedLogger,
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}, obs, tracker
}

// entriesContaining returns the observed entries whose message contains text.
func entriesContaining(obs *log.Observer, text string) []log.Entry {
	var out []log.Entry
	for _, e := range obs.All() {
		if strings.Contains(e.Message, text) {
			out = append(out, e)
		}
	}
	return out
}

// TestWithRequestOverrides_InheritedClusterNamesWarning asserts the one
// warning an entry on another endpoint logs for the node, storage, and bridge
// names it inherits, and that the job endpoint and an entry naming its own
// values stay quiet.
func TestWithRequestOverrides_InheritedClusterNamesWarning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  []string // nil means the warning must not fire
	}{
		{
			name:  "another host inherits the job names",
			extra: map[string]any{"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken},
			want: []string{
				"pve.node=pve-a-1", "pve.vm_storage=local", "pve.disk_storage=local", "pve.network_bridge=vmbr0",
				"pve.placement.az_map.z1=pve-a-1", "pve.vm_types",
			},
		},
		{
			name: "another host that names its own values lists only the rest",
			extra: map[string]any{
				"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken,
				"pve_node": "pve-b-1", "pve_vm_storage": "vm-b", "pve_disk_storage": "disk-b",
			},
			want: []string{"pve.network_bridge=vmbr0", "pve.placement.az_map.z1=pve-a-1", "pve.vm_types"},
		},
		{
			name: "another host inherits az_map nodes and profiles",
			extra: map[string]any{
				"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken,
				"pve_node": "pve-b-1", "pve_vm_storage": "vm-b", "pve_disk_storage": "disk-b", "pve_network_bridge": "vmbr1",
			},
			want: []string{"pve.placement.az_map.z1=pve-a-1", "pve.vm_types"},
		},
		{
			name:  "the job endpoint is quiet",
			extra: map[string]any{"pve_vm_storage": "vm-b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := overrideTestBaseConfig()
			base.Node = "pve-a-1"
			base.Placement = &config.PlacementConfig{AZMap: map[string][]string{"z1": {"pve-a-1"}}}
			base.VMTypes = map[string]config.TypeProfile{"small": {CloudProperties: map[string]any{"cpu": 1.0}}}
			deps, obs, _ := clusterScopeDeps(base)
			if _, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: tc.extra}); err != nil {
				t.Fatalf("WithRequestOverrides: %v", err)
			}
			lines := entriesContaining(obs, inheritedNamesText)
			if tc.want == nil {
				if len(lines) != 0 {
					t.Errorf("unexpected inherited-names warning: %v", lines[0].Attrs)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("got %d inherited-names lines, want exactly one", len(lines))
			}
			if lines[0].Level != log.LevelWarn {
				t.Errorf("level = %v, want Warn", lines[0].Level)
			}
			if got := lines[0].Attrs["inherited_names"]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("inherited_names = %#v, want %#v", got, tc.want)
			}
			if lines[0].Attrs["effective_pve_host"] != "az2.example" {
				t.Errorf("effective_pve_host = %v, want az2.example", lines[0].Attrs["effective_pve_host"])
			}
			for _, e := range obs.All() {
				rendered := fmt.Sprintf("%s %v", e.Message, e.Attrs)
				if strings.Contains(rendered, base.Password) || strings.Contains(rendered, "FAKE-ENTRY-TOKEN-PLACEHOLDER") {
					t.Errorf("a log line carries a credential fixture value: %q", e.Message)
				}
			}
		})
	}
}

// TestWithRequestOverrides_DestroyUnreferencedDisksReset asserts that the
// request served for an entry on another endpoint runs with the D9 flag off
// and says so, while an entry that names the flag keeps its own value.
func TestWithRequestOverrides_DestroyUnreferencedDisksReset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		extra     map[string]any
		wantFlag  bool
		wantNotes int
	}{
		{"another host without the key", map[string]any{"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken}, false, 1},
		{"another host that names it", map[string]any{
			"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken, "pve_destroy_unreferenced_disks": true,
		}, true, 0},
		{"the job endpoint", map[string]any{"pve_vm_storage": "vm-b"}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := overrideTestBaseConfig()
			base.DestroyUnreferencedDisks = true
			deps, obs, _ := clusterScopeDeps(base)
			got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: tc.extra})
			if err != nil {
				t.Fatalf("WithRequestOverrides: %v", err)
			}
			if got.Config.DestroyUnreferencedDisks != tc.wantFlag {
				t.Errorf("request DestroyUnreferencedDisks = %t, want %t", got.Config.DestroyUnreferencedDisks, tc.wantFlag)
			}
			if !deps.Config.DestroyUnreferencedDisks {
				t.Error("the job config's flag changed")
			}
			lines := entriesContaining(obs, d9ResetText)
			if len(lines) != tc.wantNotes {
				t.Fatalf("got %d reset lines, want %d", len(lines), tc.wantNotes)
			}
			if len(lines) == 1 && lines[0].Level != log.LevelInfo {
				t.Errorf("reset line level = %v, want Info", lines[0].Level)
			}
		})
	}
}

// TestWithRequestOverrides_InheritedAutoManagedSDN_NonRetriable asserts that
// an entry on another endpoint that would carry the job's SDN fabric onto
// that cluster is refused as a non-retriable CloudError before any client is
// built for it.
func TestWithRequestOverrides_InheritedAutoManagedSDN_NonRetriable(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	base.NetworkMode = config.NetworkModeSDN
	base.SDNZoneType = "vxlan"
	base.SDNVxlanPeers = []string{"192.0.2.11"}
	deps, _, tracker := clusterScopeDeps(base)

	_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: map[string]any{
		"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken,
	}})
	if err == nil {
		t.Fatal("expected the request to be refused")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
		t.Errorf("error %v must be a non-retriable CloudError", err)
	}
	if !strings.Contains(err.Error(), "pve.sdn_vxlan_peers") {
		t.Errorf("error = %q, want it to name pve.sdn_vxlan_peers", err.Error())
	}
	if len(tracker.calls) != 0 {
		t.Errorf("ClientFactory must not be invoked for a refused entry, got %d call(s)", len(tracker.calls))
	}

	got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: map[string]any{
		"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken,
		"pve_sdn_vxlan_peers": []any{"198.51.100.11"},
	}})
	if err != nil {
		t.Fatalf("an entry that names its own peers must pass: %v", err)
	}
	if !reflect.DeepEqual(got.Config.SDNVxlanPeers, []string{"198.51.100.11"}) {
		t.Errorf("request peers = %v, want the entry's own", got.Config.SDNVxlanPeers)
	}
}

// TestWithRequestOverrides_IgnoredProfileFieldsWarning asserts that profile
// fields other than cloud_properties in an entry's vm_types or disk_types do
// not fail the request, and that one warning names each of them with its
// profile.
func TestWithRequestOverrides_IgnoredProfileFieldsWarning(t *testing.T) {
	t.Parallel()
	const ignoredText = "profile fields other than cloud_properties"
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  []string
	}{
		{"sibling fields", map[string]any{
			"pve_vm_types": map[string]any{"small": map[string]any{
				"description": "two cores", "cloud_properties": map[string]any{"cpu": 2.0},
			}},
			"pve_disk_types": map[string]any{"fast": map[string]any{"name": "fast"}},
		}, []string{"pve.vm_types.small.description", "pve.disk_types.fast.name"}},
		{"only cloud_properties", map[string]any{
			"pve_vm_types": map[string]any{"small": map[string]any{"cloud_properties": map[string]any{"cpu": 2.0}}},
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, obs, _ := clusterScopeDeps(overrideTestBaseConfig())
			got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: tc.extra})
			if err != nil {
				t.Fatalf("WithRequestOverrides: %v", err)
			}
			if got.Config.VMTypes["small"].CloudProperties["cpu"] != 2.0 {
				t.Errorf("the small profile lost its cloud_properties: %#v", got.Config.VMTypes)
			}
			lines := entriesContaining(obs, ignoredText)
			if tc.want == nil {
				if len(lines) != 0 {
					t.Errorf("unexpected ignored-fields warning: %v", lines[0].Attrs)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("got %d ignored-fields lines, want exactly one", len(lines))
			}
			if lines[0].Level != log.LevelWarn {
				t.Errorf("level = %v, want Warn", lines[0].Level)
			}
			if got := lines[0].Attrs["ignored_profile_fields"]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ignored_profile_fields = %#v, want %#v", got, tc.want)
			}
		})
	}
}
