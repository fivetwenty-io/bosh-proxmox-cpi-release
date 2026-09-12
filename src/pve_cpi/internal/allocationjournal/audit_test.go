package allocationjournal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRetainAuditDurableIdentityAndBounds(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"complete": true, "namespace": "director"}
	id, err := RetainAudit(directory, report)
	if err != nil {
		t.Fatal(err)
	}
	root, err := openPrivateRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	var decoded json.RawMessage
	if err = readJSON(root, "audit-"+id+".json", &decoded); err != nil {
		t.Fatal(err)
	}
	expected, _, err := VerificationEvidence(decoded)
	if err != nil || expected != id {
		t.Fatal("retained evidence hash changed")
	}
	info, err := os.Stat(filepath.Join(directory, "audit-"+id+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("audit permissions not private")
	}
	if _, err = RetainAudit(directory, map[string]any{"oversized": strings.Repeat("x", MaxEvidenceBytes+1)}); err == nil {
		t.Fatal("unbounded audit retained")
	}
}

func TestRetainAuditNeverOverwritesCorruptOrReidentifiedEvidence(t *testing.T) {
	for _, corrupt := range []bool{true, false} {
		t.Run(strconv.FormatBool(corrupt), func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Chmod(directory, 0700); err != nil {
				t.Fatal(err)
			}
			report := map[string]any{"complete": true}
			id, err := RetainAudit(directory, report)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "audit-"+id+".json")
			if corrupt {
				if err = os.WriteFile(path, []byte("corrupt evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				root, e := openPrivateRoot(directory)
				if e != nil {
					t.Fatal(e)
				}
				if e = atomicJSON(root, "audit-"+id+".json", map[string]any{"complete": false}, defaultFileOps()); e != nil {
					t.Fatal(e)
				}
				if e = root.Close(); e != nil {
					t.Fatal(e)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = RetainAudit(directory, report); err == nil {
				t.Fatal("existing bad evidence overwritten")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("evidence changed on failed retention")
			}
		})
	}
}
func TestRetainAuditRepeatedContentPreservesInode(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"complete": true}
	id, err := RetainAudit(directory, report)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "audit-"+id+".json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := RetainAudit(directory, report)
	if err != nil || again != id {
		t.Fatalf("repeat %s %v", again, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("same evidence replaced")
	}
}
