package handlers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const ensureTestPool = "bosh-director"

type ensureGuardPool struct {
	pve.PoolService
	found        bool
	comment      string
	createErr    error
	readErr      error
	readAfterErr error
	createAbsent bool
	events       *[]string
	creates      int
}

func (p *ensureGuardPool) GetPoolComment(context.Context, string) (string, bool, error) {
	*p.events = append(*p.events, "read")
	if p.creates > 0 && p.readAfterErr != nil {
		return "", false, p.readAfterErr
	}
	return p.comment, p.found, p.readErr
}
func (p *ensureGuardPool) CreatePool(_ context.Context, _, comment string) error {
	*p.events = append(*p.events, "create")
	p.creates++
	p.found = !p.createAbsent
	if p.createErr == nil {
		p.comment = comment
	}
	return p.createErr
}

type ensureGuardClient struct {
	pve.Client
	pools *ensureGuardPool
}

func (c ensureGuardClient) Pools() pve.PoolService { return c.pools }

func newEnsureGuard(t *testing.T, pools *ensureGuardPool) *ManagedAllocationGuard {
	t.Helper()
	raw := ensureGuardClient{pools: pools}
	vm := &managedVMAllocation{deps: Deps{PVE: raw}}
	guard, err := NewManagedAllocationGuard(raw, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) {
			*pools.events = append(*pools.events, "intent")
			return "step", nil
		},
		After: func(ctx context.Context, call ManagedAllocationMutation, step string, result any) error {
			*pools.events = append(*pools.events, "observe")
			return vm.observePolicyMutation(ctx, call, step, result)
		},
		Failed: func(_ context.Context, _ ManagedAllocationMutation, _ string, err error) error {
			*pools.events = append(*pools.events, "failed")
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

func TestManagedEnsurePoolExistsMutationBoundary(t *testing.T) {
	cases := []struct {
		name      string
		found     bool
		createErr error
		want      []string
		failed    bool
	}{
		{name: "existing operator pool", found: true, want: []string{"read"}},
		{name: "missing pool", want: []string{"read", "intent", "create", "observe", "read"}},
		{name: "concurrent existing pool", createErr: sdkerrors.ParseAPIError(500, []byte(`{"message":"pool 'bosh-director' already exists\n"}`)), want: []string{"read", "intent", "create", "read", "observe", "read"}},
		{name: "uncertain create despite pool appearing", createErr: errors.New("connection lost"), failed: true, want: []string{"read", "intent", "create", "failed"}},
		{name: "untyped duplicate is uncertain", createErr: errors.New("pool 'bosh-director' already exists"), failed: true, want: []string{"read", "intent", "create", "failed"}},
		{name: "unrelated duplicate", createErr: sdkerrors.ParseAPIError(500, []byte(`{"message":"pool 'other' already exists"}`)), failed: true, want: []string{"read", "intent", "create", "failed"}},
	}
	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			pools := &ensureGuardPool{found: tc.found, comment: "operator pool", createErr: tc.createErr, events: &events}
			guard := newEnsureGuard(t, pools)
			err := pve.EnsurePoolExists(context.Background(), guard.Client(), ensureTestPool, pve.PoolProvenanceComment, nil)
			if (err != nil) != tc.failed || (guard.Err() != nil) != tc.failed {
				t.Fatalf("error=%v poison=%v", err, guard.Err())
			}
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("events=%v want=%v", events, tc.want)
			}
			if tc.failed {
				before := len(events)
				if err := pve.EnsurePoolExists(context.Background(), guard.Client(), ensureTestPool, pve.PoolProvenanceComment, nil); err == nil {
					t.Fatal("uncertain create retry accepted")
				}
				if len(events) != before {
					t.Fatal("poisoned guard performed another observation or mutation")
				}
			}
		})
	}
}

func TestManagedEnsurePoolExistsRequiresReadback(t *testing.T) {
	for _, mode := range []string{"initial error", "missing after success", "missing after duplicate", "read error after duplicate"} {
		t.Run(mode, func(t *testing.T) {
			events := []string{}
			pools := &ensureGuardPool{events: &events}
			switch mode {
			case "initial error":
				pools.readErr = errors.New("permission denied")
			case "missing after success":
				pools.createAbsent = true
			default:
				pools.createErr = sdkerrors.ParseAPIError(500, []byte(`{"message":"pool 'bosh-director' already exists"}`))
				pools.createAbsent = true
				if mode == "read error after duplicate" {
					pools.readAfterErr = errors.New("read failed")
				}
			}
			guard := newEnsureGuard(t, pools)
			if err := pve.EnsurePoolExists(context.Background(), guard.Client(), ensureTestPool, pve.PoolProvenanceComment, nil); err == nil {
				t.Fatal("unverified pool accepted")
			}
			if mode == "initial error" && pools.creates != 0 {
				t.Fatal("created after unreadable pool")
			}
			if mode != "initial error" && guard.Err() == nil {
				t.Fatal("failed post-write readback did not poison guard")
			}
		})
	}
}

func TestManagedPoolDirectCreateRetainsExclusiveSemantics(t *testing.T) {
	events := []string{}
	pools := &ensureGuardPool{events: &events, found: true, createErr: sdkerrors.ParseAPIError(500, []byte(`{"message":"pool 'bosh-director' already exists"}`))}
	guard := newEnsureGuard(t, pools)
	if err := guard.Client().Pools().CreatePool(context.Background(), ensureTestPool, "lock owner"); err == nil || guard.Err() == nil {
		t.Fatal("exclusive pool create was treated as idempotent")
	}
	if !reflect.DeepEqual(events, []string{"intent", "create", "failed"}) {
		t.Fatal(events)
	}
}

func TestExactPoolAlreadyExistsRejectsAmbiguity(t *testing.T) {
	for _, body := range []string{
		`{"message":"pool 'bosh-director' already exists","errors":{"other":"bad"}}`,
		`{"message":"pool 'bosh-director' already exists after unknown failure"}`,
		`{"message":"pool 'bosh-director' already exists\n\n"}`,
	} {
		if exactPoolAlreadyExists(sdkerrors.ParseAPIError(500, []byte(body)), ensureTestPool) {
			t.Fatalf("accepted %s", body)
		}
	}
	err := sdkerrors.ParseAPIError(409, []byte(`{"message":"pool 'bosh-director' already exists"}`))
	if exactPoolAlreadyExists(err, ensureTestPool) {
		t.Fatal("generic conflict accepted")
	}
	exact := sdkerrors.ParseAPIError(500, []byte(`{"message":"pool 'bosh-director' already exists"}`))
	if !exactPoolAlreadyExists(fmt.Errorf("create pool: %w", exact), ensureTestPool) {
		t.Fatal("wrapped exact API error rejected")
	}
}
