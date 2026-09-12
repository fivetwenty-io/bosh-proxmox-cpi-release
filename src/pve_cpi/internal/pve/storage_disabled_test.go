package pve

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestParseStorageDisabledStrict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value    string
		disabled bool
	}{{"true", true}, {"1", true}, {`"1"`, true}, {"false", false}, {"0", false}, {`"0"`, false}} {
		t.Run(tc.value, func(t *testing.T) {
			info, err := ParseStorageEntry(json.RawMessage(fmt.Sprintf(`{"storage":"nfs-a","type":"nfs","disable":%s}`, tc.value)))
			if err != nil {
				t.Fatal(err)
			}
			if info.Disabled != tc.disabled {
				t.Fatalf("Disabled=%v", info.Disabled)
			}
		})
	}
	for _, value := range []string{"null", "2", "-1", "0.0", `"true"`, `" 1"`, `[]`, `{}`} {
		t.Run("invalid "+value, func(t *testing.T) {
			if _, err := ParseStorageEntry(json.RawMessage(fmt.Sprintf(`{"storage":"nfs-a","type":"nfs","disable":%s}`, value))); err == nil {
				t.Fatal("accepted malformed disabled state")
			}
		})
	}
	info, err := ParseStorageEntry(json.RawMessage(`{"storage":"nfs-a","type":"nfs"}`))
	if err != nil || info.Disabled {
		t.Fatalf("omitted disable changed legacy default: %+v %v", info, err)
	}
}
