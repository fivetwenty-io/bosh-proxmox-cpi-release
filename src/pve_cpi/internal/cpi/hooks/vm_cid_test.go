package hooks

import (
	"encoding/json"
	"testing"
)

func TestHooksReadStampedCIDs(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal("101-0a1b2c3d-4e5f6a7b")
	if err != nil {
		t.Fatal(err)
	}
	if vmid, ok := vmidFromArg(raw); !ok || vmid != 101 {
		t.Errorf("vmidFromArg stamped = %d, %v; want 101, true", vmid, ok)
	}
	if vmid, ok := vmidFromResult([]any{"101-0a1b2c3d-4e5f6a7b", nil}); !ok || vmid != 101 {
		t.Errorf("vmidFromResult stamped = %d, %v; want 101, true", vmid, ok)
	}
	if _, ok := vmidFromResult([]any{"not-a-cid"}); ok {
		t.Error("vmidFromResult accepted a malformed CID")
	}
}
