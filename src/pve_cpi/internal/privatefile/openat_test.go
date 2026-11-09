package privatefile

import "golang.org/x/sys/unix"

func openat(fd int, name string, flags int) (int, error) { return unix.Openat(fd, name, flags, 0o600) }
