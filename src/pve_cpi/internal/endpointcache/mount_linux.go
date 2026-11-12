package endpointcache

import (
	"errors"
	"fmt"
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
		return mountKey{}, statxError(err)
	}
	if stx.Mask&unix.STATX_MNT_ID == 0 {
		return mountKey{}, errors.New("the kernel does not report mount IDs")
	}
	return mountKey{
		dev:   unix.Mkdev(stx.Dev_major, stx.Dev_minor),
		mount: strconv.FormatUint(stx.Mnt_id, 10),
	}, nil
}

// statxError names the likely cause when statx itself is refused rather than
// failing on the path. A seccomp profile that does not list statx answers
// EPERM, which BPM's profile does before v1.4.21. A kernel older than 4.11 has
// no statx and answers ENOSYS, as some seccomp profiles do too.
func statxError(err error) error {
	switch {
	case errors.Is(err, unix.EPERM):
		return fmt.Errorf("the statx system call was refused (%w), which is what a seccomp profile that does not "+
			"allow statx answers, such as the one BPM applies before v1.4.21", err)
	case errors.Is(err, unix.ENOSYS):
		return fmt.Errorf("the statx system call is not available (%w), as on a kernel before 4.11 or under a "+
			"seccomp profile that answers ENOSYS for it", err)
	}
	return err
}
