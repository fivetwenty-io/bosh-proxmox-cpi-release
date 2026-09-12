package handlers

import (
	"context"
	"encoding/json"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"testing"
)

func TestStoragePlacementMetricsBoundedPolicyLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	metrics, err := NewStoragePlacementMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.CPIConfig{StorageSets: map[string]config.StorageSet{"persistent": {Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	selection := &StoragePlacementSelection{Policy: cfg, Persistent: &StorageRoleSelection{SetName: "persistent"}}
	deps := Deps{StorageMetrics: metrics}
	for _, event := range []string{"allocation", "fallback", "rejection", "uuid-unrecognized"} {
		deps.recordStoragePlacement(context.Background(), selection, event)
	}
	selection.Persistent.SetName = "uuid-unconfigured"
	deps.recordStoragePlacement(context.Background(), selection, "allocation")
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 1 || len(data.ScopeMetrics[0].Metrics) != 3 {
		t.Fatalf("unexpected instruments: %+v", data)
	}
	for _, m := range data.ScopeMetrics[0].Metrics {
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 {
			t.Fatalf("bad count: %+v", m)
		}
		attrs := sum.DataPoints[0].Attributes
		if attrs.Len() != 4 {
			t.Fatalf("unexpected dimensions: %+v", attrs)
		}
		for _, a := range attrs.ToSlice() {
			switch string(a.Key) {
			case "storage.set":
				if a.Value.AsString() != "persistent" {
					t.Fatal("unconfigured set leaked")
				}
			case "storage.role", "storage.strategy", "storage.strategy.version":
			default:
				t.Fatal("unbounded metric label")
			}
		}
	}
}

func TestStorageReconciliationMetricsRejectArbitraryOutcomes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	metrics, err := NewStoragePlacementMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{StorageMetrics: metrics}
	for _, outcome := range []string{"adopted", "cleaned", "required", "rejected", "secret-arbitrary-id"} {
		deps.recordStorageReconciliation(context.Background(), outcome)
	}
	var data metricdata.ResourceMetrics
	if err = reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 1 || len(data.ScopeMetrics[0].Metrics) != 1 {
		t.Fatalf("unexpected metrics: %+v", data)
	}
	m := data.ScopeMetrics[0].Metrics[0]
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok || m.Name != "cpi.storage.reconciliations" || len(sum.DataPoints) != 4 {
		t.Fatalf("unexpected outcomes: %+v", m)
	}
	for _, point := range sum.DataPoints {
		if point.Value != 1 || point.Attributes.Len() != 1 {
			t.Fatalf("unexpected point: %+v", point)
		}
		for _, a := range point.Attributes.ToSlice() {
			if string(a.Key) != "storage.reconciliation.outcome" || a.Value.AsString() == "secret-arbitrary-id" {
				t.Fatal("unbounded outcome label")
			}
		}
	}
}

func TestStorageCandidateRejectionsFollowProductionIterator(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() {
		if err := provider.Shutdown(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	metrics, err := NewStoragePlacementMetrics(provider.Meter("candidate-test"))
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := planFixture(t, func(source *planFixtureSource, _ *config.CPIConfig) {
		for node, entries := range source.statuses {
			for index, raw := range entries {
				var value map[string]any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				if value["storage"] == "a" {
					value["avail"] = uint64(1 << 20)
					source.statuses[node][index] = planJSON(t, value)
				}
			}
		}
	})
	deps := Deps{StorageMetrics: metrics}
	request.OnCandidateRejected = deps.storageCandidateRejectionObserver(t.Context(), request.Selection)
	iterator, err := NewStoragePlanIterator(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = iterator.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(iterator.rejections) == 0 {
		t.Fatal("fixture rejected no candidates")
	}
	request.OnCandidateRejected("untrusted-storage-id")
	var data metricdata.ResourceMetrics
	if err = reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 1 || len(data.ScopeMetrics[0].Metrics) != 1 {
		t.Fatalf("unexpected instruments: %+v", data)
	}
	observed := data.ScopeMetrics[0].Metrics[0]
	sum, ok := observed.Data.(metricdata.Sum[int64])
	if !ok || observed.Name != "cpi.storage.candidate_rejections" || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != int64(len(iterator.rejections)) {
		t.Fatalf("candidate count: %+v", observed)
	}
	attrs := sum.DataPoints[0].Attributes
	if attrs.Len() != 4 {
		t.Fatalf("unexpected cardinality: %+v", attrs)
	}
	for _, a := range attrs.ToSlice() {
		switch string(a.Key) {
		case "storage.set", "storage.role", "storage.strategy", "storage.strategy.version":
		default:
			t.Fatal("unbounded metric dimension")
		}
		if a.Value.AsString() == "a" || a.Value.AsString() == "n1" || a.Value.AsString() == "untrusted-storage-id" {
			t.Fatal("physical identity leaked")
		}
	}
}
