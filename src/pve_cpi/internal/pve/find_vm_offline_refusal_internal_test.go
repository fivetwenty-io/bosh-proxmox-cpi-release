package pve

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// offlineRefusalClient is a three-member cluster whose quorate status reports
// the members in offline as offline, and whose config probe answers through
// configFn.
func offlineRefusalClient(offline map[string]bool, configFn func(node string) (map[string]any, error)) *lgClient {
	online := map[string]bool{}
	for _, n := range []string{"pve1", "pve2", "pve3"} {
		online[n] = !offline[n]
	}
	return &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2", "pve3"}, statusFn: lgStatus(online)},
		nodes:   &lgNodes{},
		qemu: &mnQEMU{configFn: func(node string, _ int) (map[string]any, error) {
			if offline[node] {
				return nil, errors.New("an offline member must not be probed")
			}
			return configFn(node)
		}},
	}
}

func offlineRefusalNotFound(string) (map[string]any, error) {
	return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
}

// TestConfigProbeOfflineMembers_NamesTheOfflineMembersOfAProbeWithNoHit runs
// FindVMByConfigProbe on a cluster where every member that answers says the
// VM's config is missing and two members are offline. The refusal is the
// offline-only one, and the classifier names both members.
func TestConfigProbeOfflineMembers_NamesTheOfflineMembersOfAProbeWithNoHit(t *testing.T) {
	t.Parallel()
	c := offlineRefusalClient(map[string]bool{"pve2": true, "pve3": true}, offlineRefusalNotFound)
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4120)
	if err == nil || loc.Found {
		t.Fatalf("loc=%+v err=%v; want the offline refusal", loc, err)
	}
	offline, ok := ConfigProbeOfflineMembers(err, 4120)
	if !ok || !slices.Equal(offline, []string{"pve2", "pve3"}) {
		t.Fatalf("ConfigProbeOfflineMembers(%v) = %v, %t; want [pve2 pve3], true", err, offline, ok)
	}
	if _, ok := ConfigProbeOfflineMembers(err, 4121); ok {
		t.Fatalf("ConfigProbeOfflineMembers matched the refusal for VM 4120 against VM 4121")
	}
}

// TestConfigProbeOfflineMembers_RefusesEveryOtherProbeError pins that only
// the offline-only refusal matches. A member whose probe fails, an
// enumeration that fails, a wrapped refusal, and errors that only look like it
// don't.
func TestConfigProbeOfflineMembers_RefusesEveryOtherProbeError(t *testing.T) {
	t.Parallel()
	probeFails := offlineRefusalClient(map[string]bool{"pve3": true}, func(node string) (map[string]any, error) {
		if node == "pve2" {
			return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}
		}
		return offlineRefusalNotFound(node)
	})
	enumerationFails := offlineRefusalClient(nil, offlineRefusalNotFound)
	enumerationFails.cluster.configNodesFn = func() (*sdkcluster.ListConfigNodesResponse, error) {
		return nil, &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "connection refused"}
	}

	for _, tc := range []struct {
		name string
		err  func(t *testing.T) error
	}{
		{"a probe error beside an offline member", func(t *testing.T) error {
			_, err := FindVMByConfigProbe(lgCtx(), probeFails, 4122)
			return err
		}},
		{"an enumeration failure", func(t *testing.T) error {
			_, err := FindVMByConfigProbe(lgCtx(), enumerationFails, 4122)
			return err
		}},
		{"a wrapped refusal", func(t *testing.T) error {
			c := offlineRefusalClient(map[string]bool{"pve3": true}, offlineRefusalNotFound)
			_, err := FindVMByConfigProbe(lgCtx(), c, 4122)
			if err == nil {
				t.Fatal("the probe proved absence with pve3 offline")
			}
			return fmt.Errorf("locate: %w", err)
		}},
		{"the text as a plain error", func(*testing.T) error {
			return errors.New("FindVMByConfigProbe: VM 4122 contradicted by its node, and the cluster reports " +
				"node(s) pve3 offline, so their configs cannot be probed; cannot prove absence")
		}},
		{"the text as a permanent error", func(*testing.T) error {
			return cpierrors.Cloud("FindVMByConfigProbe: VM 4122 contradicted by its node, and the cluster " +
				"reports node(s) pve3 offline, so their configs cannot be probed; cannot prove absence")
		}},
		{"no error", func(*testing.T) error { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.err(t)
			if offline, ok := ConfigProbeOfflineMembers(err, 4122); ok {
				t.Fatalf("ConfigProbeOfflineMembers(%v) = %v, true; want no match", err, offline)
			}
		})
	}
}
