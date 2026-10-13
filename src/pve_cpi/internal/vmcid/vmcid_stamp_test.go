package vmcid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// cidVectors is the vector file this package and scripts/_vmcid.py both
// read, so the Go parser and its Python twin cannot drift.
type cidVectors struct {
	Valid []struct {
		Raw     string `json:"raw"`
		VMID    int    `json:"vmid"`
		Cluster string `json:"cluster"`
		Token   string `json:"token"`
	} `json:"valid"`
	Invalid []string `json:"invalid"`
}

func loadCIDVectors(t *testing.T) cidVectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cid_vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v cidVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(v.Valid) == 0 || len(v.Invalid) == 0 {
		t.Fatal("the vector file has no valid or no invalid entries")
	}
	return v
}

func TestParse_SharedVectors(t *testing.T) {
	t.Parallel()
	v := loadCIDVectors(t)
	for _, tc := range v.Valid {
		got, err := Parse(tc.Raw)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tc.Raw, err)
			continue
		}
		want := VMCID{VMID: tc.VMID, Cluster: tc.Cluster, Token: tc.Token}
		if got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.Raw, got, want)
		}
		if got.String() != tc.Raw {
			t.Errorf("Parse(%q).String() = %q, want the input back", tc.Raw, got.String())
		}
	}
	for _, raw := range v.Invalid {
		if got, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", raw, got)
		}
	}
}

func TestStamp(t *testing.T) {
	t.Parallel()
	got, err := Stamp(101, "0a1b2c3d", "4e5f6a7b")
	if err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	if got.String() != "101-0a1b2c3d-4e5f6a7b" {
		t.Errorf("Stamp = %q, want 101-0a1b2c3d-4e5f6a7b", got.String())
	}
	if back, err := Parse(got.String()); err != nil || back != got {
		t.Errorf("Parse(Stamp) = %+v, %v; want %+v", back, err, got)
	}

	bad := []struct {
		vmid           int
		cluster, token string
	}{
		{0, "0a1b2c3d", "4e5f6a7b"},
		{-1, "0a1b2c3d", "4e5f6a7b"},
		{1, "", "4e5f6a7b"},
		{1, "0a1b2c3d", ""},
		{1, "0A1B2C3D", "4e5f6a7b"},
		{1, "0a1b2c3d", "4e5f6a7"},
		{1, "0a1b2c3d", "4e5f6a7g"},
	}
	for _, tc := range bad {
		if got, err := Stamp(tc.vmid, tc.cluster, tc.token); err == nil {
			t.Errorf("Stamp(%d, %q, %q) = %+v, want an error", tc.vmid, tc.cluster, tc.token, got)
		}
	}
}

func TestNewToken(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 64 {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if !IsStamp(tok) {
			t.Fatalf("NewToken = %q, want eight lowercase hex characters", tok)
		}
		seen[tok] = true
	}
	// 64 draws from 2^32 values collide with a probability near 5e-7, so a
	// repeat means the source is not random.
	if len(seen) != 64 {
		t.Errorf("NewToken repeated a value in 64 draws: %d distinct", len(seen))
	}
}

func TestKnownCluster(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cid  VMCID
		want bool
	}{
		{VMCID{VMID: 1}, false},
		{VMCID{VMID: 1, Cluster: UnknownCluster, Token: "4e5f6a7b"}, false},
		{VMCID{VMID: 1, Cluster: "0a1b2c3d", Token: "4e5f6a7b"}, true},
	}
	for _, tc := range cases {
		if got := tc.cid.KnownCluster(); got != tc.want {
			t.Errorf("%+v.KnownCluster() = %v, want %v", tc.cid, got, tc.want)
		}
	}
}
