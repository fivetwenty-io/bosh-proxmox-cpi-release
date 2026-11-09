package endpointpool

import (
	"context"
	"testing"
)

func TestConfiguredOnlyMarker(t *testing.T) {
	base := context.Background()
	if IsConfiguredOnly(base) {
		t.Error("a bare context is not configured only")
	}
	marked := WithConfiguredOnly(base)
	if !IsConfiguredOnly(marked) {
		t.Error("the marked context should be configured only")
	}
	if IsConfiguredOnly(base) {
		t.Error("marking must not change the parent")
	}
	type k struct{}
	child := context.WithValue(marked, k{}, 1)
	if !IsConfiguredOnly(child) {
		t.Error("a derived context should keep the marker")
	}
	cancelCtx, cancel := context.WithCancel(marked)
	defer cancel()
	if !IsConfiguredOnly(cancelCtx) {
		t.Error("a cancellable child should keep the marker")
	}
}
