package main

import (
	"errors"
	"testing"

	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// stubEntryClient replaces newEntryClient for one test and records the
// arguments the production factory passes it.
type stubEntryClient struct {
	calls  int
	eff    *config.CPIConfig
	job    *config.CPIConfig
	logger *log.Logger
	tracer trace.Tracer
	err    error
}

func (s *stubEntryClient) install(t *testing.T) {
	t.Helper()
	prev := newEntryClient
	t.Cleanup(func() { newEntryClient = prev })
	newEntryClient = func(eff, job *config.CPIConfig, logger *log.Logger, tracer trace.Tracer) (pve.Client, error) {
		s.calls++
		s.eff, s.job, s.logger, s.tracer = eff, job, logger, tracer
		return nil, s.err
	}
}

func TestEntryClientFactoryPassesTheEntryAndTheJobAsTwoConfigs(t *testing.T) {
	var stub stubEntryClient
	stub.install(t)
	tracer := tracenoop.NewTracerProvider().Tracer("test")
	logger := log.NewNopLogger()
	entry := &config.CPIConfig{Host: "entry.example.com"}
	job := &config.CPIConfig{Host: "job.example.com"}

	_, err := entryClientFactory(nil, tracer)(entry, job, logger)

	if err != nil {
		t.Fatalf("factory = %v, want the stub's nil error", err)
	}
	if stub.calls != 1 {
		t.Fatalf("newEntryClient ran %d times, want once", stub.calls)
	}
	if stub.eff != entry {
		t.Errorf("the client was built from %p, want the entry's effective config %p", stub.eff, entry)
	}
	if stub.job != job {
		t.Errorf("the client's job was %p, want the job-level config %p", stub.job, job)
	}
	if stub.job == stub.eff {
		t.Error("the entry was passed as its own job, which would make it own an inherited az_map")
	}
	if stub.logger != logger || stub.tracer != tracer {
		t.Errorf("the client got logger %p and tracer %v, want the request's logger and the process tracer",
			stub.logger, stub.tracer)
	}
}

func TestEntryClientFactoryReturnsTheConstructorsError(t *testing.T) {
	boom := errors.New("construction failed")
	stub := stubEntryClient{err: boom}
	stub.install(t)

	_, err := entryClientFactory(nil, nil)(&config.CPIConfig{}, &config.CPIConfig{}, log.NewNopLogger())

	if !errors.Is(err, boom) {
		t.Fatalf("factory = %v, want the constructor's error", err)
	}
}

func TestEntryClientFactoryUsesAnInjectedFactoryWithTheEntryAlone(t *testing.T) {
	var stub stubEntryClient
	stub.install(t)
	tracer := tracenoop.NewTracerProvider().Tracer("test")
	entry := &config.CPIConfig{Host: "entry.example.com"}
	var gotCfg *config.CPIConfig
	var gotTracer trace.Tracer
	injected := func(cfg *config.CPIConfig, _ *log.Logger, tr trace.Tracer) (pve.Client, error) {
		gotCfg, gotTracer = cfg, tr
		return nil, nil
	}

	_, err := entryClientFactory(injected, tracer)(entry, &config.CPIConfig{}, log.NewNopLogger())

	if err != nil {
		t.Fatalf("factory = %v, want nil", err)
	}
	if gotCfg != entry || gotTracer != tracer {
		t.Errorf("injected factory got cfg %p and tracer %v, want the entry and the process tracer", gotCfg, gotTracer)
	}
	if stub.calls != 0 {
		t.Error("the production constructor ran though a factory was injected")
	}
}
