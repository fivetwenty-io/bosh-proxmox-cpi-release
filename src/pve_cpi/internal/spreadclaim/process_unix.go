//go:build unix

package spreadclaim

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process. A process this
// user may not signal still runs, so only ESRCH reads as gone.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
