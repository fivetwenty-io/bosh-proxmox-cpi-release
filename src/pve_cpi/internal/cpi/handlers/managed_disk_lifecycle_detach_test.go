package handlers

import (
	"context"
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"maps"
	"reflect"
	"strconv"
	"testing"
)

type lifecycleDetachFake struct {
	pve.Client
	config map[string]any
	puts   int
	events []string
}

func (c *lifecycleDetachFake) QEMU() qemu.Service   { return &lifecycleDetachQEMU{c: c} }
func (c *lifecycleDetachFake) Nodes() nodes.Service { return &lifecycleDetachNodes{c: c} }

type lifecycleDetachQEMU struct {
	qemu.Service
	c *lifecycleDetachFake
}

func (q *lifecycleDetachQEMU) Config(context.Context, string, int) (map[string]any, error) {
	return maps.Clone(q.c.config), nil
}
func (q *lifecycleDetachQEMU) DetachDisk(context.Context, string, int, string) error {
	panic("compound SDK detach must never run")
}

type lifecycleDetachNodes struct {
	nodes.Service
	c *lifecycleDetachFake
}

func (n *lifecycleDetachNodes) UpdateQemuConfig(_ context.Context, _ string, _ string, p *nodes.UpdateQemuConfigParams) error {
	if p.Digest == nil || *p.Digest != n.c.config["digest"] {
		return fmt.Errorf("generation conflict")
	}
	n.c.puts++
	n.c.events = append(n.c.events, "put")
	slot := *p.Delete
	volume := n.c.config[slot]
	delete(n.c.config, slot)
	if slot == "scsi1" {
		n.c.config["unused0"] = volume
	}
	n.c.config["digest"] = strconv.Itoa(n.c.puts + 1)
	return nil
}
func TestManagedDetachJournalsEachPUTAndRejectsGenerationRace(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			c := &lifecycleDetachFake{config: map[string]any{"digest": "1", "scsi1": "pool:100/birth.raw"}}
			intents := 0
			guard, err := NewManagedAllocationGuard(c, ManagedAllocationHooks{
				Before: func(context.Context, ManagedAllocationMutation) (string, error) {
					intents++
					c.events = append(c.events, "intent")
					if race && intents == 2 {
						c.config["unused0"] = "pool:999/foreign.raw"
						c.config["digest"] = "external"
					}
					return fmt.Sprint(intents), nil
				},
				After: func(context.Context, ManagedAllocationMutation, string, any) error {
					c.events = append(c.events, "observed")
					return nil
				},
				Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
					c.events = append(c.events, "uncertain")
					return fmt.Errorf("reconciliation")
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			err = managedDetachDisk(context.Background(), guard.Client(), "n1", 100, "scsi1", "pool:100/birth.raw")
			if race {
				if err == nil || c.puts != 1 || c.config["unused0"] != "pool:999/foreign.raw" {
					t.Fatalf("race erased foreign resource: err=%v puts=%d config=%v", err, c.puts, c.config)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if c.puts != 2 || !reflect.DeepEqual(c.events, []string{"intent", "put", "observed", "intent", "put", "observed"}) {
					t.Fatalf("mutation boundaries: %v", c.events)
				}
			}
		})
	}
}
func TestManagedDetachRequiresKnownVolumeAndDigest(t *testing.T) {
	for _, cfg := range []map[string]any{{"scsi1": "pool:100/birth.raw"}, {"digest": "1", "scsi1": "pool:999/foreign.raw"}} {
		c := &lifecycleDetachFake{config: cfg}
		if err := managedDetachDisk(context.Background(), c, "n1", 100, "scsi1", "pool:100/birth.raw"); err == nil || c.puts != 0 {
			t.Fatalf("unsafe detach accepted: err=%v puts=%d", err, c.puts)
		}
	}
}
