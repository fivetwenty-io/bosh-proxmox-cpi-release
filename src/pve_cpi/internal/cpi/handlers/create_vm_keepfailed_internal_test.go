package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// kfNodes records UpdateQemuConfig (the keep-failed tag write) and flags any
// DeleteQemu (a destroy that must NOT happen in keep-failed mode).
type kfNodes struct {
	sdknodes.Service
	updatedTags *string
	updateCalls int
	deleteCalls int
	updateErr   error // when set, UpdateQemuConfig fails (best-effort path)
}

func (n *kfNodes) UpdateQemuConfig(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
	n.updateCalls++
	if params != nil && params.Tags != nil {
		n.updatedTags = params.Tags
	}
	return n.updateErr
}

func (n *kfNodes) DeleteQemu(
	_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams,
) (*sdknodes.DeleteQemuResponse, error) {
	n.deleteCalls++
	return &sdknodes.DeleteQemuResponse{}, nil
}

// kfQEMU records Stop (the first step of cleanupVM, the destroy path) and
// serves a configurable existing-tags config for the preserve-tags path.
type kfQEMU struct {
	qemu.Service
	stopCalls   int
	existingTag string // returned as cfg["tags"] from Config
}

func (q *kfQEMU) Stop(_ context.Context, _ string, _ int) (string, error) {
	q.stopCalls++
	return "", nil
}

func (q *kfQEMU) Config(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	if q.existingTag == "" {
		return map[string]interface{}{}, nil
	}
	return map[string]interface{}{"tags": q.existingTag}, nil
}

type kfClient struct {
	pve.Client
	nodes   sdknodes.Service
	qemu    qemu.Service
	cluster *naClusterStub
}

func (c *kfClient) Nodes() sdknodes.Service { return c.nodes }
func (c *kfClient) QEMU() qemu.Service      { return c.qemu }
func (c *kfClient) Cluster() cluster.Service {
	if c.cluster == nil {
		return newNAStub()
	}
	return c.cluster
}

// Pools returns nil so tagFailedVM's withVMIDLock falls back to the
// best-effort unlocked path — acceptable for keep_failed_vms tests whose
// focus is tag content and VM preservation, not lock ordering.
func (c *kfClient) Pools() pve.PoolService { return nil }

func kfBoolPtr(b bool) *bool { return &b }

func kfEnv() map[string]any {
	return map[string]any{"bosh": map[string]any{
		"group":  "dir-cf-router",
		"groups": []any{"router"},
	}}
}

func kfDeps(keep bool, nodes *kfNodes, q *kfQEMU) Deps {
	cfg := &config.CPIConfig{}
	if keep {
		cfg.Debug = &config.DebugConfig{KeepFailedVMs: kfBoolPtr(true)}
	}
	return Deps{
		Config: cfg,
		PVE:    &kfClient{nodes: nodes, qemu: q},
		Logger: log.NewNopLogger(),
	}
}

func TestRollbackOnExit_KeepFailed_TagsAndPreserves(t *testing.T) {
	nodes := &kfNodes{}
	q := &kfQEMU{}
	deps := kfDeps(true, nodes, q)
	vmCreated := true
	retErr := errors.New("stage 8: agent never came up")

	rollbackOnExit(context.Background(), deps, "pve01", 100, kfEnv(), log.NewNopLogger(), &vmCreated, &retErr)

	if q.stopCalls != 0 || nodes.deleteCalls != 0 {
		t.Fatalf("keep-failed must NOT destroy: stop=%d delete=%d", q.stopCalls, nodes.deleteCalls)
	}
	if nodes.updateCalls != 1 || nodes.updatedTags == nil {
		t.Fatalf("keep-failed must tag the VM once; updateCalls=%d", nodes.updateCalls)
	}
	if !strings.Contains(*nodes.updatedTags, "bosh-create-failed") {
		t.Errorf("tags %q must contain bosh-create-failed", *nodes.updatedTags)
	}
	if !strings.Contains(*nodes.updatedTags, "job--router") {
		t.Errorf("tags %q must carry the instance group", *nodes.updatedTags)
	}
	if retErr == nil || !strings.Contains(retErr.Error(), "100") ||
		!strings.Contains(retErr.Error(), "pve01") || !strings.Contains(retErr.Error(), "preserved") {
		t.Errorf("error must name VMID+node+preserved; got %v", retErr)
	}
	if !strings.Contains(retErr.Error(), "agent never came up") {
		t.Errorf("error must retain the original cause; got %v", retErr)
	}
	if !cpierrors.IsType(retErr, cpierrors.TypeCloud) {
		t.Errorf("preserve error must be a non-retriable CloudError; got %v", retErr)
	}
}

func TestRollbackOnExit_DefaultDestroys(t *testing.T) {
	nodes := &kfNodes{}
	q := &kfQEMU{}
	deps := kfDeps(false, nodes, q)
	vmCreated := true
	retErr := errors.New("stage 8 failed")

	rollbackOnExit(context.Background(), deps, "pve01", 100, kfEnv(), log.NewNopLogger(), &vmCreated, &retErr)

	if q.stopCalls != 1 {
		t.Errorf("default mode must call cleanupVM (Stop); stopCalls=%d", q.stopCalls)
	}
	if nodes.updateCalls != 0 {
		t.Errorf("default mode must NOT tag-preserve; updateCalls=%d", nodes.updateCalls)
	}
	if retErr.Error() != "stage 8 failed" {
		t.Errorf("default mode must leave the error unchanged; got %v", retErr)
	}
}

func TestRollbackOnExit_SuccessNoAction(t *testing.T) {
	nodes := &kfNodes{}
	q := &kfQEMU{}
	deps := kfDeps(true, nodes, q)
	vmCreated := true
	var retErr error // nil = success

	rollbackOnExit(context.Background(), deps, "pve01", 100, kfEnv(), log.NewNopLogger(), &vmCreated, &retErr)

	if q.stopCalls != 0 || nodes.updateCalls != 0 {
		t.Errorf("success path must take no action; stop=%d tag=%d", q.stopCalls, nodes.updateCalls)
	}
}

func TestRollbackOnExit_PanicKeepFailed_TagsAndRepanics(t *testing.T) {
	nodes := &kfNodes{}
	q := &kfQEMU{}
	deps := kfDeps(true, nodes, q)

	repanicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				repanicked = true
			}
		}()
		vmCreated := true
		var retErr error
		defer rollbackOnExit(context.Background(), deps, "pve01", 100, kfEnv(), log.NewNopLogger(), &vmCreated, &retErr)
		panic("boom in stage 9")
	}()

	if !repanicked {
		t.Fatal("panic must propagate after rollback (re-panic)")
	}
	if nodes.updateCalls != 1 {
		t.Errorf("keep-failed panic path must tag the VM; updateCalls=%d", nodes.updateCalls)
	}
	if q.stopCalls != 0 {
		t.Errorf("keep-failed panic path must NOT destroy; stopCalls=%d", q.stopCalls)
	}
}

func TestTagFailedVM_PreservesExistingTags(t *testing.T) {
	nodes := &kfNodes{}
	q := &kfQEMU{existingTag: "env--prod;owner--cpi-test"}
	deps := kfDeps(true, nodes, q)

	tagFailedVM(context.Background(), deps, "pve01", 100, kfEnv(), log.NewNopLogger())

	if nodes.updatedTags == nil {
		t.Fatal("expected a tag write")
	}
	got := *nodes.updatedTags
	for _, want := range []string{"env--prod", "owner--cpi-test", "bosh-create-failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged tags %q must preserve %q", got, want)
		}
	}
}

func TestTagFailedVM_WriteFailureIsBestEffort(t *testing.T) {
	nodes := &kfNodes{updateErr: errors.New("pve 500")}
	q := &kfQEMU{}
	deps := kfDeps(true, nodes, q)

	// Must not panic; the failure is logged and swallowed.
	tagFailedVM(context.Background(), deps, "pve01", 100, kfEnv(), log.NewNopLogger())

	if nodes.updateCalls != 1 {
		t.Errorf("tag write attempted once even when it fails; got %d", nodes.updateCalls)
	}
}

func TestTagFailedVM_CreateEnvNoGroup_FallsBackToInstanceName(t *testing.T) {
	nodes := &kfNodes{}
	q := &kfQEMU{}
	deps := kfDeps(true, nodes, q)
	// create-env env: no bosh.group/groups; instance name only.
	env := map[string]any{"bosh": map[string]any{
		"instance": map[string]any{"name": "bosh/0"},
	}}

	tagFailedVM(context.Background(), deps, "pve01", 100, env, log.NewNopLogger())

	if nodes.updatedTags == nil || !strings.Contains(*nodes.updatedTags, "bosh-create-failed") {
		t.Fatalf("create-env path must still apply bosh-create-failed; got %v", nodes.updatedTags)
	}
}

func TestCleanupVM_RemovesNodeAffinityPinWhenEnabled(t *testing.T) {
	cl := newNAStub()
	cl.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100"}
	cl.resources["vm:100"] = true
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01"}}),
		PVE:    &kfClient{nodes: &kfNodes{}, qemu: &kfQEMU{}, cluster: cl},
		Logger: log.NewNopLogger(),
	}

	cleanupVM(context.Background(), deps, "pve01", 100, nil, log.NewNopLogger())

	if _, ok := cl.rules["bosh-na-100"]; ok {
		t.Error("cleanupVM must remove the node-affinity pin rule on rollback")
	}
	if cl.resources["vm:100"] {
		t.Error("cleanupVM must deregister the HA resource on rollback")
	}
}

func TestCleanupVM_RemovesPinEvenWhenAZPinDisabled(t *testing.T) {
	// The bosh-na-<vmid> rule has two writers: the AZ pin (gated by
	// placement.pin_az_via_ha_rules) and the PCI strict pin (written whenever
	// pci_passthroughs is set, regardless of that flag). Rollback removal is
	// therefore unconditional — a flag-gated removal would orphan a PCI pin
	// forever on a default (flag-off) deployment.
	cl := newNAStub()
	cl.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100"}
	deps := Deps{
		Config: icMinConfig(), // no Placement → AZ pin disabled
		PVE:    &kfClient{nodes: &kfNodes{}, qemu: &kfQEMU{}, cluster: cl},
		Logger: log.NewNopLogger(),
	}

	cleanupVM(context.Background(), deps, "pve01", 100, nil, log.NewNopLogger())

	if _, ok := cl.rules["bosh-na-100"]; ok {
		t.Error("cleanupVM must remove the node-affinity pin rule even when the AZ-pin flag is off")
	}
}

func TestDisposeFailedVM_KeepTagsDefaultDestroys(t *testing.T) {
	// keep mode: tag, no destroy.
	kn, kq := &kfNodes{}, &kfQEMU{}
	disposeFailedVM(context.Background(), kfDeps(true, kn, kq), "pve01", 100, kfEnv(), log.NewNopLogger())
	if kq.stopCalls != 0 || kn.updateCalls != 1 {
		t.Errorf("keep: want tag-no-destroy; stop=%d tag=%d", kq.stopCalls, kn.updateCalls)
	}
	// default mode: destroy, no tag.
	dn, dq := &kfNodes{}, &kfQEMU{}
	disposeFailedVM(context.Background(), kfDeps(false, dn, dq), "pve01", 100, kfEnv(), log.NewNopLogger())
	if dq.stopCalls != 1 || dn.updateCalls != 0 {
		t.Errorf("default: want destroy-no-tag; stop=%d tag=%d", dq.stopCalls, dn.updateCalls)
	}
}
