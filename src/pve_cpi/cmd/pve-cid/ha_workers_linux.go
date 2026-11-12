package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// bpmWorkerView returns a function that compares two directories inside a
// running BPM worker of the director job, as the CPI that worker runs sees
// them, or nil when no worker can be looked into. run is BPM's pidfile
// directory for the job. A pidfile names the worker's process on this host,
// and that process's root directory leads into the worker's mounts. A pidfile
// whose process shares this process's mount namespace is stale, so it is
// skipped. Looking into another process's root needs root.
func bpmWorkerView(run string) func(parent, anchor string) (workerMount, error) {
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return nil
	}
	pidfiles, err := filepath.Glob(filepath.Join(run, "*worker_*.pid"))
	if err != nil {
		return nil
	}
	for _, pidfile := range pidfiles {
		data, err := os.ReadFile(filepath.Clean(pidfile))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			continue
		}
		proc := "/proc/" + strconv.Itoa(pid)
		if ns, err := os.Readlink(filepath.Join(proc, "ns", "mnt")); err != nil || ns == self {
			continue
		}
		root := filepath.Join(proc, "root")
		if _, err := os.Stat(root); err != nil {
			continue
		}
		return func(parent, anchor string) (workerMount, error) { return compareInRoot(root, parent, anchor) }
	}
	return nil
}

// compareInRoot compares parent with anchor below root, which is a worker's
// root directory. Every lookup stays inside root, so an absolute symbolic link
// resolves inside the worker as it does for the CPI.
func compareInRoot(root, parent, anchor string) (workerMount, error) {
	dir, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("opening the worker's root directory %s: %w", root, err)
	}
	defer func() { _ = unix.Close(dir) }()
	want, err := mountIDInRoot(dir, anchor, true)
	if err != nil {
		return 0, err
	}
	got, err := mountIDInRoot(dir, parent, false)
	switch {
	case errors.Is(err, unix.ENOENT):
		return workerNoParent, nil
	case err != nil:
		return 0, err
	case got != want:
		return workerOtherMount, nil
	}
	return workerSameMount, nil
}

// mountIDInRoot returns the mount ID of path as a lookup that starts at root
// and stays inside it finds it. follow says whether a symbolic link in the
// last component is followed.
func mountIDInRoot(root int, path string, follow bool) (uint64, error) {
	flags := uint64(unix.O_PATH | unix.O_CLOEXEC)
	if !follow {
		flags |= unix.O_NOFOLLOW
	}
	how := unix.OpenHow{Flags: flags, Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS}
	fd, err := unix.Openat2(root, strings.TrimLeft(path, "/"), &how)
	if err != nil {
		return 0, fmt.Errorf("looking up %s inside the worker: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT, unix.STATX_MNT_ID, &stx); err != nil {
		return 0, fmt.Errorf("reading the mount of %s inside the worker: %w", path, err)
	}
	if stx.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("the kernel does not report mount IDs")
	}
	return stx.Mnt_id, nil
}
