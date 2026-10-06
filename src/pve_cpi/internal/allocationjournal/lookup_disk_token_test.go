package allocationjournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordFile returns the path of allocation id's record file under dir.
func recordFile(t *testing.T, dir, id string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*", recordName(id)))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one record file for %s, got %v (%v)", id, matches, err)
	}
	return matches[0]
}

// tokenOf returns the disk token of allocation id.
func tokenOf(t *testing.T, id string) string {
	t.Helper()
	token, err := DiskCorrelationToken(id)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestInspectDiskTokenFindsTheRecord checks that InspectDiskToken returns
// the disk record whose token it is given, and finds nothing for a token no
// record names or for the token of a VM record's ID, which names no disk.
func TestInspectDiskTokenFindsTheRecord(t *testing.T) {
	j, _ := fixture(t)
	id, h := createdDisk(t, j)
	closeHandle(t, h)
	other, h := createdDisk(t, j)
	closeHandle(t, h)
	vm := acquireVM(t, j)
	vmID := vm.Record().ID
	closeHandle(t, vm)

	r, found, err := j.InspectDiskToken(tokenOf(t, id))
	if err != nil || !found || r.ID != id || r.DiskToken != tokenOf(t, id) {
		t.Fatalf("InspectDiskToken(%s) = %s, %v, %v, want record %s", tokenOf(t, id), r.ID, found, err, id)
	}
	r, found, err = j.InspectDiskToken(tokenOf(t, other))
	if err != nil || !found || r.ID != other {
		t.Fatalf("InspectDiskToken for the second disk = %s, %v, %v, want record %s", r.ID, found, err, other)
	}
	unnamed, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"a token no record names": tokenOf(t, unnamed), "a VM record's token": tokenOf(t, vmID), "a token of another form": "bpd-00000000000000aa"} {
		if r, found, err := j.InspectDiskToken(token); err != nil || found {
			t.Errorf("InspectDiskToken for %s = %s, %v, %v, want nothing found", name, r.ID, found, err)
		}
	}
	if _, _, err := j.InspectDiskToken(" "); err == nil || !strings.Contains(err.Error(), "disk token is required") {
		t.Errorf("InspectDiskToken with a blank token: %v, want the refusal", err)
	}
}

// TestInspectDiskTokenReadsOnlyItsRecord checks that a corrupt record of
// another allocation, and a file whose name holds no allocation UUID, don't
// stop InspectDiskToken, although they fail List, and that a corrupt record
// whose name gives the token is an error rather than nothing found.
func TestInspectDiskTokenReadsOnlyItsRecord(t *testing.T) {
	j, dir := fixture(t)
	id, h := createdDisk(t, j)
	closeHandle(t, h)
	namespace := filepath.Dir(recordFile(t, dir, id))
	corrupt, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{recordName(corrupt), "allocation-not-a-uuid.json"} {
		if err := os.WriteFile(filepath.Join(namespace, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.List(); err == nil {
		t.Fatal("List with a corrupt record succeeded, want the row to start from a journal List can't read")
	}
	r, found, err := j.InspectDiskToken(tokenOf(t, id))
	if err != nil || !found || r.ID != id {
		t.Fatalf("InspectDiskToken beside a corrupt record = %s, %v, %v, want record %s", r.ID, found, err, id)
	}
	unnamed, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	if r, found, err := j.InspectDiskToken(tokenOf(t, unnamed)); err != nil || found {
		t.Fatalf("InspectDiskToken for an unnamed token beside a corrupt record = %s, %v, %v, want nothing found", r.ID, found, err)
	}
	if _, found, err := j.InspectDiskToken(tokenOf(t, corrupt)); err == nil || found || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("InspectDiskToken for the corrupt record's own token = %v, %v, want a corrupt-record error", found, err)
	}
}
