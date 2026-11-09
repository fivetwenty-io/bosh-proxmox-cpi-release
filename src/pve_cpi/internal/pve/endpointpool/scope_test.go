package endpointpool

import (
	"context"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestScopeOnContext(t *testing.T) {
	if ScopeFromContext(context.Background()) != nil {
		t.Error("a bare context has no scope")
	}
	ctx, release := WithAffinityScope(context.Background())
	defer release()
	s := ScopeFromContext(ctx)
	if s == nil {
		t.Fatal("the context should carry a scope")
	}
	type k struct{}
	if ScopeFromContext(context.WithValue(ctx, k{}, 1)) != s {
		t.Error("a derived context should carry the same scope")
	}
	other, releaseOther := WithAffinityScope(context.Background())
	defer releaseOther()
	if ScopeFromContext(other) == s {
		t.Error("two scopes must differ")
	}
}

func TestNestedScopeReplacesWithoutReleasingOuter(t *testing.T) {
	outerCtx, releaseOuter := WithAffinityScope(context.Background())
	outer := ScopeFromContext(outerCtx)
	var outerRan, innerRan atomic.Int32
	outer.OnRelease(func() { outerRan.Add(1) })

	innerCtx, releaseInner := WithAffinityScope(outerCtx)
	inner := ScopeFromContext(innerCtx)
	if inner == outer {
		t.Fatal("the nested scope should be new")
	}
	inner.OnRelease(func() { innerRan.Add(1) })
	releaseInner()
	if innerRan.Load() != 1 || outerRan.Load() != 0 {
		t.Errorf("inner=%d outer=%d after releasing the inner scope", innerRan.Load(), outerRan.Load())
	}
	releaseOuter()
	if outerRan.Load() != 1 {
		t.Errorf("outer ran %d times", outerRan.Load())
	}
}

func TestScopeReleaseRunsRegisteredFunctionsOnceInOrder(t *testing.T) {
	ctx, release := WithAffinityScope(context.Background())
	s := ScopeFromContext(ctx)
	var order []int
	for i := range 3 {
		if !s.OnRelease(func() { order = append(order, i) }) {
			t.Fatalf("registration %d refused before release", i)
		}
	}
	if s.Released() {
		t.Error("scope should not be released yet")
	}
	release()
	release()
	release()
	if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
		t.Errorf("functions ran %v, want [0 1 2] exactly once", order)
	}
	if !s.Released() {
		t.Error("scope should be released")
	}
}

func TestScopeReleaseWithNothingRegistered(t *testing.T) {
	ctx, release := WithAffinityScope(context.Background())
	release()
	if !ScopeFromContext(ctx).Released() {
		t.Error("an empty scope still releases")
	}
}

func TestScopeOnReleaseAfterReleaseIsRefused(t *testing.T) {
	ctx, release := WithAffinityScope(context.Background())
	s := ScopeFromContext(ctx)
	release()
	ran := false
	if s.OnRelease(func() { ran = true }) {
		t.Error("registration after release should report false")
	}
	release()
	if ran {
		t.Error("a refused function must never run")
	}
}

func TestScopeNilAndNilFunction(t *testing.T) {
	var nilScope *Scope
	if nilScope.OnRelease(func() {}) {
		t.Error("a nil scope registers nothing")
	}
	if nilScope.Released() {
		t.Error("a nil scope is not released")
	}
	ctx, release := WithAffinityScope(context.Background())
	defer release()
	if ScopeFromContext(ctx).OnRelease(nil) {
		t.Error("a nil function registers nothing")
	}
}

func TestScopeConcurrentReleaseRunsEachFunctionOnce(t *testing.T) {
	ctx, release := WithAffinityScope(context.Background())
	s := ScopeFromContext(ctx)
	var count atomic.Int32
	for range 20 {
		s.OnRelease(func() { count.Add(1) })
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release()
		}()
	}
	wg.Wait()
	if count.Load() != 20 {
		t.Errorf("ran %d functions, want 20", count.Load())
	}
}

func TestScopeConcurrentRegistrationAndRelease(t *testing.T) {
	ctx, release := WithAffinityScope(context.Background())
	s := ScopeFromContext(ctx)
	var ran, accepted atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.OnRelease(func() { ran.Add(1) }) {
				accepted.Add(1)
			}
		}()
	}
	release()
	wg.Wait()
	// Every accepted function ran exactly once, and a refused one never ran.
	if ran.Load() != accepted.Load() {
		t.Errorf("ran %d, accepted %d", ran.Load(), accepted.Load())
	}
}

func TestScopeFunctionMayUseTheScope(t *testing.T) {
	ctx, release := WithAffinityScope(context.Background())
	s := ScopeFromContext(ctx)
	var lateAccepted atomic.Bool
	s.OnRelease(func() {
		// The functions run without the scope's lock, so these must not deadlock.
		_ = s.Released()
		lateAccepted.Store(s.OnRelease(func() {}))
	})
	release()
	if lateAccepted.Load() {
		t.Error("registration from inside a release function should be refused")
	}
}

func TestScopeReleaseDropsPanicsAndRunsTheRest(t *testing.T) {
	ctx, release := WithAffinityScope(context.Background())
	s := ScopeFromContext(ctx)
	var ran atomic.Int32
	s.OnRelease(func() { ran.Add(1) })
	s.OnRelease(func() { panic("first") })
	s.OnRelease(func() { ran.Add(1) })
	s.OnRelease(func() { panic("second") })

	// The dispatcher calls release from a defer after the call's work is done,
	// so a faulty callback must not raise a panic out of it.
	release()
	if ran.Load() != 2 {
		t.Errorf("%d healthy functions ran, want 2", ran.Load())
	}
	if !s.Released() {
		t.Error("the scope should read as released after a panicking callback")
	}
	release() // already released, so nothing runs and nothing panics
}

// The package sits below everything that uses it, so its production files
// import only the standard library and the CPI's logger, and never
// internal/pve, internal/endpointcache, or the handlers. Its tests may also
// import the shared TLS test servers.
func TestPackageImportsStayBelowThePVEPackage(t *testing.T) {
	const (
		logPkg  = "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
		testPkg = "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
	)
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v, %d files", err, len(files))
	}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		isTest := strings.HasSuffix(name, "_test.go")
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s", name, imp.Path.Value)
			}
			first, _, _ := strings.Cut(path, "/")
			if !strings.Contains(first, ".") || path == logPkg || (isTest && path == testPkg) {
				continue
			}
			t.Errorf("%s imports %s, which is outside the standard library and the allowed CPI packages", name, path)
		}
	}
}
