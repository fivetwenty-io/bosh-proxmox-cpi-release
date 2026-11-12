package endpointcache

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// setTimesNow sets the access and modification times of the open descriptor fd
// to now. golang.org/x/sys implements Futimes on Linux as a utimes call on
// /proc/self/fd/N, which fails when /proc does not resolve for the process, so
// this calls utimensat with no path, the way glibc implements futimens.
func setTimesNow(fd int) error {
	ts := [2]unix.Timespec{{Nsec: unix.UTIME_NOW}, {Nsec: unix.UTIME_NOW}}
	// #nosec G103 -- the pointer is to a local array the kernel reads during the call
	_, _, errno := unix.Syscall6(unix.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
