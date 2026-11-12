package endpointcache

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// mountEntry is one line of /proc/self/mountinfo.
type mountEntry struct {
	id    string
	dev   string
	point string
}

// readMountInfo returns the mounts this process sees, in the kernel's order.
func readMountInfo(t *testing.T) []mountEntry {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("the mount table could not be read: %v", err)
	}
	var entries []mountEntry
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		point, err := unescapeMountPoint(fields[4])
		if err != nil {
			t.Fatalf("mount point %q: %v", fields[4], err)
		}
		entries = append(entries, mountEntry{id: fields[0], dev: fields[2], point: point})
	}
	return entries
}

// unescapeMountPoint undoes the octal escapes mountinfo writes for a space, a
// tab, a newline, and a backslash.
func unescapeMountPoint(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", err
		}
		b.WriteByte(byte(n))
		i += 3
	}
	return b.String(), nil
}

// Every mount this process can reach answers with its own mount ID, so a bind
// mount of a directory, which keeps the device of its source, still gets a key
// of its own. That is what lets the CPI tell a volume BPM mounted into the
// worker from the worker's private filesystem on the same disk. A mount point
// that a later mount covers, or that the process cannot reach, is skipped.
func TestMountOfReportsEachMountsOwnID(t *testing.T) {
	entries := readMountInfo(t)
	top := map[string]int{}
	for i, e := range entries {
		top[e.point] = i
	}
	checked := map[string][]string{}
	for i, e := range entries {
		if top[e.point] != i {
			continue
		}
		info, err := os.Stat(e.point)
		if err != nil || !info.IsDir() {
			continue
		}
		key, err := mountOf(e.point, true)
		if err != nil {
			t.Fatalf("mountOf(%s): %v", e.point, err)
		}
		if key.mount != e.id {
			// A mount below a covered one can resolve elsewhere, so only a
			// mismatch on a mount nothing hides is a failure.
			if covered(entries, i) {
				continue
			}
			t.Errorf("mountOf(%s) has mount ID %s, want %s", e.point, key.mount, e.id)
		}
		checked[e.dev] = append(checked[e.dev], e.point)
	}
	if len(checked) == 0 {
		t.Fatal("no mount point could be checked")
	}
	for dev, points := range checked {
		if len(points) > 1 {
			t.Logf("device %s holds %d mounts with distinct keys, such as %s and %s", dev, len(points), points[0],
				points[1])
			return
		}
	}
	t.Log("no two reachable mounts share a device here, so only the mount IDs themselves were checked")
}

// covered reports whether a later mount sits on a directory above entry i's
// mount point, which can hide that mount point from path lookups.
func covered(entries []mountEntry, i int) bool {
	point := entries[i].point
	for _, later := range entries[i+1:] {
		if later.point == "/" || strings.HasPrefix(point, later.point+"/") {
			return true
		}
	}
	return false
}
