package handlers

import (
	"context"
	"errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"slices"
	"testing"
)

type guardTestClient struct {
	pve.Client
	q qemu.Service
}

func (c guardTestClient) QEMU() qemu.Service { return c.q }
func TestManagedAllocationGuardPoisonBlocksRetriesAndCleanup(t *testing.T) {
	for _, failure := range []string{"before", "service", "readback", "panic", "before_panic", "failed_panic"} {
		t.Run(failure, func(t *testing.T) {
			testManagedAllocationGuardFailure(t, failure)
		})
	}
}
func testManagedAllocationGuardFailure(t *testing.T, failure string) {
	t.Helper()
	writes := 0
	failed := 0
	order := []string{}
	q := &guardTestQEMU{createFn: func(context.Context, string, map[string]any) (string, error) {
		writes++
		order = append(order, "write")
		if failure == "service" {
			return "", errors.New("transport")
		}
		if failure == "panic" {
			panic("service panic")
		}
		return "UPID:task", nil
	}}
	g, e := NewManagedAllocationGuard(guardTestClient{q: q}, ManagedAllocationHooks{Before: func(context.Context, ManagedAllocationMutation) (string, error) {
		order = append(order, "intent")
		if failure == "before_panic" {
			panic("admission panic")
		}
		if failure == "before" {
			return "", errors.New("journal unavailable")
		}
		return "step-1", nil
	}, After: func(context.Context, ManagedAllocationMutation, string, any) error {
		order = append(order, "readback")
		return errors.New("unproven")
	}, Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
		failed++
		if failure == "failed_panic" {
			panic("uncertainty hook panic")
		}
		return errors.New("reconciliation")
	}})
	if e != nil {
		t.Fatal(e)
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != nil && failure != "panic" && failure != "before_panic" && failure != "failed_panic" {
				t.Fatal(recovered)
			}
		}()
		_, e = g.Client().QEMU().Create(context.Background(), "n1", map[string]any{"vmid": 101})
		if failure != "panic" && e == nil {
			t.Fatal("failure swallowed")
		}
	}()
	if g.Err() == nil {
		t.Fatal("guard was not poisoned")
	}
	if _, e = g.Client().QEMU().Create(context.Background(), "n1", nil); e == nil {
		t.Fatal("retry accepted")
	}
	if _, e = g.Client().QEMU().Stop(context.Background(), "n1", 101); e == nil {
		t.Fatal("cleanup accepted")
	}
	if order[0] != "intent" {
		t.Fatal("write preceded durable intent")
	}
	if failure == "before" || failure == "before_panic" {
		if writes != 0 || failed != 0 {
			t.Fatal("write after failed intent")
		}
	} else if writes != 1 || failed != 1 {
		t.Fatalf("writes=%d uncertainty=%d", writes, failed)
	}
}

func TestManagedAllocationGuardSuccessfulReadbackBeforeNextWrite(t *testing.T) {
	order := []string{}
	q := &guardTestQEMU{createFn: func(context.Context, string, map[string]any) (string, error) {
		order = append(order, "write")
		return "UPID:task", nil
	}}
	g, e := NewManagedAllocationGuard(guardTestClient{q: q}, ManagedAllocationHooks{Before: func(context.Context, ManagedAllocationMutation) (string, error) {
		order = append(order, "intent")
		return "step", nil
	}, After: func(_ context.Context, m ManagedAllocationMutation, _ string, result any) error {
		if m.Args["node"] != "n1" || result != "UPID:task" {
			t.Fatal("lost operation evidence")
		}
		order = append(order, "readback")
		return nil
	}, Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
		t.Fatal("unexpected uncertainty")
		return nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 2; n++ {
		if _, e = g.Client().QEMU().Create(context.Background(), "n1", nil); e != nil {
			t.Fatal(e)
		}
	}
	want := []string{"intent", "write", "readback", "intent", "write", "readback"}
	for n, v := range want {
		if order[n] != v {
			t.Fatalf("order=%v", order)
		}
	}
}

type guardTestQEMU struct {
	qemu.Service
	createFn func(context.Context, string, map[string]any) (string, error)
}

func (q *guardTestQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	return q.createFn(ctx, node, params)
}

// The synchronous interface must still retain the imgdel task in its evidence hook.
func TestManagedAllocationGuardSynchronousDeleteRetainsTask(t *testing.T) {
	order := []string{}
	guard, err := NewManagedAllocationGuard(guardTestClient{}, ManagedAllocationHooks{
		Before: func(_ context.Context, mutation ManagedAllocationMutation) (string, error) {
			if mutation.Method != "DeleteVolumeAsync" || mutation.Args["volume"] != "vm-101-disk-0" {
				t.Fatalf("wrong deletion evidence: %+v", mutation)
			}
			order = append(order, "intent")
			return "delete-step", nil
		},
		After: func(_ context.Context, _ ManagedAllocationMutation, step string, result any) error {
			if step != "delete-step" || result != "UPID:delete-task" {
				t.Fatalf("deletion task lost: %s %v", step, result)
			}
			order = append(order, "task-readback")
			return nil
		},
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
			t.Fatal("unexpected failed deletion")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := managedStorageService{guard: guard, Service: &guardTestStorage{
		deleteFn: func(context.Context, string, string, string) (string, error) {
			order = append(order, "delete")
			return "UPID:delete-task", nil
		},
	}}
	if err := service.DeleteVolume(context.Background(), "n1", "nfs", "vm-101-disk-0"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"intent", "delete", "task-readback"}) {
		t.Fatalf("deletion order: %v", order)
	}
}

type guardTestStorage struct {
	storage.Service
	deleteFn func(context.Context, string, string, string) (string, error)
}

func (s *guardTestStorage) DeleteVolumeAsync(ctx context.Context, node, store, volume string) (string, error) {
	return s.deleteFn(ctx, node, store, volume)
}

func TestManagedAllocationGuardConditionalDeleteRetainsTask(t *testing.T) {
	observed := false
	guard, err := NewManagedAllocationGuard(guardTestClient{}, ManagedAllocationHooks{
		Before: func(_ context.Context, mutation ManagedAllocationMutation) (string, error) {
			if mutation.Method != "DeleteVolumeIfExistsAsync" {
				t.Fatal("wrong conditional deletion method")
			}
			return "conditional-delete", nil
		},
		After: func(_ context.Context, _ ManagedAllocationMutation, _ string, result any) error {
			values, ok := result.([]any)
			if !ok || len(values) != 2 || values[0] != true || values[1] != "UPID:conditional-delete" {
				t.Fatalf("task missing: %v", result)
			}
			observed = true
			return nil
		},
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
			t.Fatal("unexpected failure")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := managedStorageService{guard: guard, Service: &guardTestStorage{}}
	existed, err := service.DeleteVolumeIfExists(context.Background(), "n1", "nfs", "vm-101-disk-0")
	if err != nil || !existed || !observed {
		t.Fatalf("conditional delete lost task evidence: existed=%v observed=%v err=%v", existed, observed, err)
	}
}
func (s *guardTestStorage) DeleteVolumeIfExistsAsync(context.Context, string, string, string) (bool, string, error) {
	return true, "UPID:conditional-delete", nil
}
