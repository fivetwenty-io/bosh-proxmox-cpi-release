package endpointcache

import (
	"golang.org/x/sys/unix"
)

// mountOf returns the device of path and the directory its filesystem is
// mounted on, which together name the mount.
func mountOf(path string, follow bool) (mountKey, error) {
	var st unix.Stat_t
	stat := unix.Lstat
	if follow {
		stat = unix.Stat
	}
	if err := stat(path, &st); err != nil {
		return mountKey{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return mountKey{}, err
	}
	// #nosec G115 -- the key only compares devices, so a sign change is harmless
	dev := uint64(st.Dev)
	return mountKey{dev: dev, mount: unix.ByteSliceToString(fs.Mntonname[:])}, nil
}
