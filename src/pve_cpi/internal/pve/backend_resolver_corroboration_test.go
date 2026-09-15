// Tests for the corroborators a resolver hands its local backends. The sweep
// in NodeForExisting reads a content listing on every candidate node, and an
// export serving the wrong tree lists nothing on all of them, so the sweep is
// the one Backend method that needs a second opinion.
package pve

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// sweepCorroborationSource is the name a refusal must print when the fixture's
// corroborator is what contradicted the listing.
const sweepCorroborationSource = "fixture ledger"

// corroboratedResolverFixture builds the production resolver over a cluster
// where the point probe answers PVE's "no format" 500 and every content
// listing comes back empty, which is what an nfs export mounted from the wrong
// tree looks like from the API. The storage is a dir carrying is_mountpoint, so
// the allow-list would otherwise read that empty listing as an absence.
func corroboratedResolverFixture(
	t *testing.T, opts ...BackendResolverOption,
) (BackendResolver, *fakeContentNodes) {
	t.Helper()
	return corroboratedResolverFixtureOn(t, []string{"pve-01"}, opts...)
}

// corroboratedResolverFixtureOn is the same fixture over a named node set, for
// the cases that care how often a sweep of several nodes reaches its sources.
// The first node is the resolver's default node.
func corroboratedResolverFixtureOn(
	t *testing.T, clusterNodes []string, opts ...BackendResolverOption,
) (BackendResolver, *fakeContentNodes) {
	t.Helper()
	content := &fakeContentNodes{}
	index := newCountingStorageIndex(nil, `{"storage":"dir-images","type":"dir","is_mountpoint":"1"}`)
	rows := make([]map[string]any, 0, len(clusterNodes))
	for _, node := range clusterNodes {
		rows = append(rows, map[string]any{"node": node})
	}
	base := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, _, _, volume string) (bool, error) {
				return false, noFormatProbeErr(volume)
			},
		},
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return clusterResp(rows...), nil
			},
		},
		nodesSvc:          content,
		clusterStorageSvc: index.svc,
	}
	client := &visibleBackendClient{backendTestClient: base}
	cache := NewStorageInfoCache(ClusterStorageAsLister(index.svc), time.Minute)
	return NewBackendResolver(client, cache, clusterNodes[0], opts...), content
}

// contradictingCorroborator answers every question with a contradiction and
// records that it was asked.
func contradictingCorroborator(asked *int) EmptyListingCorroborator {
	return CorroboratorFunc(sweepCorroborationSource,
		func(context.Context, EmptyListingProbe) (Corroboration, error) {
			*asked++
			return Corroboration{
				Contradicted: true,
				Source:       sweepCorroborationSource,
				Detail:       "2 volumes were allocated here and never deleted",
			}, nil
		})
}

// sweepLocalBackend resolves the fixture's storage and fails the test if the
// resolver classified it as anything but local, because only the local backend
// runs the sweep these cases are about.
func sweepLocalBackend(t *testing.T, resolver BackendResolver) Backend {
	t.Helper()
	backend, err := resolver.Resolve(context.Background(), classifyStorageName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if backend.Kind() != BackendLocal {
		t.Fatalf("Kind()=%s, want local, because the sweep only runs on a local backend", backend.Kind())
	}
	return backend
}

// TestBackendResolver_WithoutCorroborators_EmptyListingStillProvesAbsence is
// the control. Nothing contradicts the listing, so the sweep reports the clean
// miss every idempotent delete and has_disk answer depends on.
func TestBackendResolver_WithoutCorroborators_EmptyListingStillProvesAbsence(t *testing.T) {
	t.Parallel()

	resolver, _ := corroboratedResolverFixture(t)
	_, err := sweepLocalBackend(t, resolver).NodeForExisting(context.Background(), classifyVolid)
	if err == nil {
		t.Fatal("an uncontradicted empty listing on an is_mountpoint storage proves the volume gone")
	}
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("want DiskNotFound from a complete clean sweep, got %v", err)
	}
}

// TestBackendResolver_WithEmptyListingCorroborators_SweepFailsClosed is the
// option's whole purpose: the corroborators reach ProveVolumeAbsent inside
// NodeForExisting, so a contradicted listing comes back as an unproven absence
// rather than as the DiskNotFound a caller would act on.
func TestBackendResolver_WithEmptyListingCorroborators_SweepFailsClosed(t *testing.T) {
	t.Parallel()

	asked, supplied := 0, 0
	resolver, _ := corroboratedResolverFixture(t, WithEmptyListingCorroborators(
		func() []EmptyListingCorroborator {
			supplied++
			return []EmptyListingCorroborator{contradictingCorroborator(&asked)}
		}))
	_, err := sweepLocalBackend(t, resolver).NodeForExisting(context.Background(), classifyVolid)
	if err == nil {
		t.Fatal("a contradicted empty listing must not report the volume missing")
	}
	if cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("a contradicted sweep must not conclude DiskNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), sweepCorroborationSource) {
		t.Errorf("the refusal must name the source that contradicted the listing, got %v", err)
	}
	if asked != 1 {
		t.Errorf("the one candidate node owes one corroboration, got %d", asked)
	}
	if supplied != 1 {
		t.Errorf("the supplier is called once for the sweep, got %d", supplied)
	}
}

// TestBackendResolver_Corroborators_UnusedWhenTheListingCarriesTheVolume keeps
// the cost claim honest. A listing that carries the volume answers the question
// on its own, so no corroborator is consulted and the sweep returns the node.
func TestBackendResolver_Corroborators_UnusedWhenTheListingCarriesTheVolume(t *testing.T) {
	t.Parallel()

	asked := 0
	resolver, content := corroboratedResolverFixture(t, WithEmptyListingCorroborators(
		func() []EmptyListingCorroborator {
			return []EmptyListingCorroborator{contradictingCorroborator(&asked)}
		}))
	content.volids = []string{classifyVolid}
	node, err := sweepLocalBackend(t, resolver).NodeForExisting(context.Background(), classifyVolid)
	if err != nil {
		t.Fatalf("a listing carrying the volume locates it: %v", err)
	}
	if node != "pve-01" {
		t.Errorf("node=%q, want pve-01", node)
	}
	if asked != 0 {
		t.Errorf("a listing that found the volume owes no corroboration, got %d", asked)
	}
}

// TestBackendResolver_Corroborators_BuiltOncePerSweep pins the cost of a
// multi-node sweep. Every candidate asks the same sources the same question,
// and a source such as the allocation journal reads a file on local disk to
// answer, so building them per node would read that file once per node in the
// cluster while the sources themselves are still asked per node.
func TestBackendResolver_Corroborators_BuiltOncePerSweep(t *testing.T) {
	t.Parallel()

	supplied := 0
	var askedOn []string
	resolver, _ := corroboratedResolverFixtureOn(t, []string{"pve-01", "pve-02", "pve-03"},
		WithEmptyListingCorroborators(func() []EmptyListingCorroborator {
			supplied++
			return []EmptyListingCorroborator{
				CorroboratorFunc(sweepCorroborationSource,
					func(_ context.Context, probe EmptyListingProbe) (Corroboration, error) {
						askedOn = append(askedOn, probe.Node)
						return Corroboration{}, nil
					}),
			}
		}))
	_, err := sweepLocalBackend(t, resolver).NodeForExisting(context.Background(), classifyVolid)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("three silent corroborations leave the sweep a complete clean miss, got %v", err)
	}
	if supplied != 1 {
		t.Errorf("the sources are built once for the whole sweep, got %d builds", supplied)
	}
	want := []string{"pve-01", "pve-02", "pve-03"}
	if !slices.Equal(askedOn, want) {
		t.Errorf("every candidate node owes its own corroboration, want %v, got %v", want, askedOn)
	}
}

// TestNodeForExistingCorroborated_CallerExtrasComeFirst covers the per-call
// half: the handler paths hold config-reference counts the resolver cannot
// supply, and those go ahead of the resolver's own sources so the cheapest
// evidence settles the question.
func TestNodeForExistingCorroborated_CallerExtrasComeFirst(t *testing.T) {
	t.Parallel()

	var order []string
	supplied := CorroboratorFunc("resolver source", func(context.Context, EmptyListingProbe) (Corroboration, error) {
		order = append(order, "resolver")
		return Corroboration{}, nil
	})
	resolver, _ := corroboratedResolverFixture(t, WithEmptyListingCorroborators(
		func() []EmptyListingCorroborator { return []EmptyListingCorroborator{supplied} }))
	extra := CorroboratorFunc(sweepCorroborationSource,
		func(context.Context, EmptyListingProbe) (Corroboration, error) {
			order = append(order, "caller")
			return Corroboration{}, nil
		})
	_, err := NodeForExistingCorroborated(
		context.Background(), sweepLocalBackend(t, resolver), classifyVolid, extra)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("two silent corroborators leave the absence proven, got %v", err)
	}
	if len(order) != 2 || order[0] != "caller" || order[1] != "resolver" {
		t.Errorf("the caller's own evidence is weighed first, got %v", order)
	}
}

// TestNodeForExistingCorroborated_FallsBackOnAPlainBackend pins the capability
// check: a backend that cannot take extras still answers the ordinary
// question rather than losing the call.
func TestNodeForExistingCorroborated_FallsBackOnAPlainBackend(t *testing.T) {
	t.Parallel()

	extra := CorroboratorFunc(sweepCorroborationSource,
		func(context.Context, EmptyListingProbe) (Corroboration, error) {
			return Corroboration{Contradicted: true}, nil
		})
	node, err := NodeForExistingCorroborated(
		context.Background(), &staticBackend{defaultNode: "pve-07"}, classifyVolid, extra)
	if err != nil {
		t.Fatalf("the static backend answers without corroboration: %v", err)
	}
	if node != "pve-07" {
		t.Errorf("node=%q, want pve-07", node)
	}
}

// TestNodeForExistingCorroborated_NilBackend refuses rather than panicking, on
// the same fail-closed principle the proof itself follows.
func TestNodeForExistingCorroborated_NilBackend(t *testing.T) {
	t.Parallel()

	if _, err := NodeForExistingCorroborated(context.Background(), nil, classifyVolid); err == nil {
		t.Fatal("a missing backend is a wiring fault, not an absence")
	}
}
