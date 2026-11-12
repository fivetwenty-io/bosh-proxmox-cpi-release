package endpointcache

import (
	"errors"
	"strconv"

	"golang.org/x/sys/unix"
)

// mountOf returns the device and the mount ID of path. The mount ID tells a
// bind mount from the filesystem it was made from, which the device alone
// cannot. A kernel older than 5.8 reports no mount ID, and then the key cannot
// be built, so the caller declines to create anything.
func mountOf(path string, follow bool) (mountKey, error) {
	flags := unix.AT_STATX_SYNC_AS_STAT
	if !follow {
		flags |= unix.AT_SYMLINK_NOFOLLOW
	}
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, flags, unix.STATX_MNT_ID, &stx); err != nil {
		return mountKey{}, err
	}
	if stx.Mask&unix.STATX_MNT_ID == 0 {
		return mountKey{}, errors.New("the kernel does not report mount IDs")
	}
	return mountKey{
		dev:   unix.Mkdev(stx.Dev_major, stx.Dev_minor),
		mount: strconv.FormatUint(stx.Mnt_id, 10),
	}, nil
}
