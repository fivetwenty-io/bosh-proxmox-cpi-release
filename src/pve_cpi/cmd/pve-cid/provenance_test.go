package main

import (
	"reflect"
	"testing"
)

func TestParseTemplateProvenance_Valid(t *testing.T) {
	desc := `{"name":"ubuntu-jammy","version":"1.719","sha8":"cafebabe","sha256":"cafebabe00000000000000000000000000000000000000000000000000000000",` +
		`"kind":"heavy","cid":":heavy:local:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2",` +
		`"created_by":"dir-uuid-1","created":"2026-08-01T00:00:00Z","director_refs":["dir-uuid-1","dir-uuid-2"]}`

	p, ok := parseTemplateProvenance(desc)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if p.Name != "ubuntu-jammy" || p.Version != "1.719" || p.SHA8 != "cafebabe" {
		t.Errorf("basic fields wrong: %+v", p)
	}
	if p.Kind != "heavy" {
		t.Errorf("Kind = %q, want %q", p.Kind, "heavy")
	}
	want := []string{"dir-uuid-1", "dir-uuid-2"}
	if !reflect.DeepEqual(p.DirectorRefs, want) {
		t.Errorf("DirectorRefs = %v, want %v", p.DirectorRefs, want)
	}
}

func TestParseTemplateProvenance_EmptyAndInvalid(t *testing.T) {
	cases := []string{"", "   ", "not json", "<!--BOSH:{}-->", "{"}
	for _, c := range cases {
		if _, ok := parseTemplateProvenance(c); ok {
			t.Errorf("parseTemplateProvenance(%q) ok=true, want false", c)
		}
	}
}

func TestParseTemplateProvenance_EmptyDirectorRefsOmitted(t *testing.T) {
	desc := `{"name":"x","version":"1","sha8":"aaaaaaaa","created":"2026-01-01T00:00:00Z"}`
	p, ok := parseTemplateProvenance(desc)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(p.DirectorRefs) != 0 {
		t.Errorf("DirectorRefs = %v, want empty", p.DirectorRefs)
	}
}

func TestSplitPVETags(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"bosh-stemcell;bosh-stemcell-name-x", []string{"bosh-stemcell", "bosh-stemcell-name-x"}},
		{"a,b;c", []string{"a", "b", "c"}},
		{" a ; ; b ", []string{"a", "b"}},
	}
	for _, c := range cases {
		got := splitPVETags(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitPVETags(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestHasTagToken(t *testing.T) {
	tokens := []string{"bosh-stemcell", "bosh-stemcell-name-x"}
	if !hasTagToken(tokens, "bosh-stemcell") {
		t.Error("expected exact-token match to succeed")
	}
	if hasTagToken(tokens, "bosh-stemcell-name") {
		t.Error("expected substring (non-exact-token) match to fail")
	}
}

func TestTagValue(t *testing.T) {
	tokens := []string{"bosh-stemcell", "bosh-stemcell-name-ubuntu-jammy", "bosh-stemcell-sha-cafebabe"}
	if v := tagValue(tokens, stemcellNameTagPrefix); v != "ubuntu-jammy" {
		t.Errorf("tagValue(name) = %q, want %q", v, "ubuntu-jammy")
	}
	if v := tagValue(tokens, stemcellSHATagPrefix); v != "cafebabe" {
		t.Errorf("tagValue(sha) = %q, want %q", v, "cafebabe")
	}
	if v := tagValue(tokens, stemcellVersionTagPrefix); v != "" {
		t.Errorf("tagValue(version) = %q, want empty", v)
	}
}
