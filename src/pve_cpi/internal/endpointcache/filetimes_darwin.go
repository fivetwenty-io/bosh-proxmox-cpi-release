package endpointcache

import (
	"golang.org/x/sys/unix"
)

// setTimesNow sets the access and modification times of the open descriptor fd
// to now. Darwin implements Futimes as a native system call.
func setTimesNow(fd int) error {
	return unix.Futimes(fd, nil)
}
