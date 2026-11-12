package endpointcache

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
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

// A refused statx names the seccomp profile that refuses it, so an operator on
// an old BPM can match the decline to its cause, and still matches the error.
func TestStatxErrorNamesTheLikelyCause(t *testing.T) {
	t.Parallel()
	cases := map[error]string{
		unix.EPERM:  "such as the one BPM applies before v1.4.21",
		unix.ENOSYS: "a kernel before 4.11",
	}
	for errno, want := range cases {
		err := statxError(errno)
		if !errors.Is(err, errno) || !strings.Contains(err.Error(), want) {
			t.Errorf("statxError(%v) = %v, want it to match the error and name %q", errno, err, want)
		}
	}
	if err := statxError(unix.ENOENT); err != unix.ENOENT {
		t.Errorf("statxError(ENOENT) = %v, want the error unchanged", err)
	}
}

// bindMountDirEnv names the directory the child process of
// TestMountOfTellsABindMountFromItsSource works in.
const bindMountDirEnv = "ENDPOINTCACHE_BIND_MOUNT_DIR"

// bindMountChecked is what the child logs once every check has run, so the
// parent knows the child did not pass by doing nothing.
const bindMountChecked = "the bind mount was checked"

// A bind mount of a subdirectory keeps the device of the filesystem it comes
// from, as each volume BPM mounts into a worker does, so only the mount ID
// tells it from its source. The test makes one in a child process with a mount
// namespace of its own, and a user namespace too when it does not run as root,
// so nothing outside the test sees the mount. It skips only when this system
// refuses the child its namespaces.
func TestMountOfTellsABindMountFromItsSource(t *testing.T) {
	if dir := os.Getenv(bindMountDirEnv); dir != "" {
		checkBindMount(t, dir)
		return
	}
	dir := t.TempDir()
	// #nosec G204 -- the test binary runs itself with a fixed test name
	cmd := exec.Command(os.Args[0], "-test.run=^TestMountOfTellsABindMountFromItsSource$", "-test.count=1",
		"-test.v")
	cmd.Env = append(os.Environ(), bindMountDirEnv+"="+dir)
	attr := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if os.Geteuid() != 0 {
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}
	}
	cmd.SysProcAttr = attr
	out, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	switch {
	case err != nil && !errors.As(err, &exited):
		t.Skipf("this system refuses a private mount namespace, so no bind mount can be made here: %v", err)
	case err != nil:
		t.Fatalf("the child that checks the bind mount failed: %v\n%s", err, out)
	case !strings.Contains(string(out), bindMountChecked):
		t.Fatalf("the child that checks the bind mount did not run the check:\n%s", out)
	}
}

// checkBindMount runs in the child's own mount namespace. It bind-mounts a
// subdirectory of dir onto another directory, as BPM mounts a volume into a
// worker, and checks that the mount keys and the same-mount rule tell the two
// apart although the device is the same.
func checkBindMount(t *testing.T, dir string) {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatalf("making the child's mounts private: %v", err)
	}
	disk := filepath.Join(dir, "disk")
	volume := filepath.Join(disk, "volume")
	inside := filepath.Join(dir, "worker", "volume")
	for _, path := range []string{filepath.Join(volume, "journal"), inside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mount(volume, inside, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind-mounting %s onto %s: %v", volume, inside, err)
	}
	key := func(path string) mountKey {
		t.Helper()
		k, err := mountOf(path, true)
		if err != nil {
			t.Fatalf("mountOf(%s): %v", path, err)
		}
		return k
	}
	source, bound := key(volume), key(inside)
	if source != key(disk) {
		t.Errorf("mountOf(%s) = %+v, want the key of %s, %+v", volume, source, disk, key(disk))
	}
	if bound == source {
		t.Fatalf("the bind mount %s and its source %s have the same key %+v", inside, volume, bound)
	}
	if bound.dev != source.dev {
		t.Logf("the bind mount reports device %d and its source %d, so the mount ID was not the only difference",
			bound.dev, source.dev)
	}
	if key(filepath.Join(inside, "journal")) != bound {
		t.Errorf("a directory inside the bind mount has a key other than the mount's")
	}
	journal := anchor{path: filepath.Join(inside, "journal"), what: "the allocation journal directory"}
	if err := sameMount(inside, journal, systemHooks); err != nil {
		t.Errorf("a parent inside the bind mount was refused: %v", err)
	}
	if err := sameMount(dir, journal, systemHooks); err == nil {
		t.Errorf("a parent outside the bind mount, on the same device, was accepted")
	}
	t.Log(bindMountChecked)
}
