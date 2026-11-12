package endpointcache

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// noProcEnv marks the child process of TestTouchLockWorksWithoutProc.
const noProcEnv = "ENDPOINTCACHE_NO_PROC"

// noProcChecked is what the child logs once the touch has run.
const noProcChecked = "the lock was touched without /proc"

// The lock's times are set through the open descriptor, so they must not depend
// on /proc/self resolving. The child gets a mount namespace of its own, and a
// user namespace too when it does not run as root, and hides /proc under an
// empty tmpfs before it touches a lock.
func TestTouchLockWorksWithoutProc(t *testing.T) {
	if os.Getenv(noProcEnv) != "" {
		checkTouchWithoutProc(t)
		return
	}
	// #nosec G204 -- the test binary runs itself with a fixed test name
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0],
		"-test.run=^TestTouchLockWorksWithoutProc$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), noProcEnv+"=1")
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
		namespaceRefused(t, "/proc cannot be hidden here", err)
	case err != nil:
		t.Fatalf("the child that touches a lock without /proc failed: %v\n%s", err, out)
	case !strings.Contains(string(out), noProcChecked):
		t.Fatalf("the child that touches a lock without /proc did not run the check:\n%s", out)
	}
}

// checkTouchWithoutProc runs in the child's own mount namespace.
func checkTouchWithoutProc(t *testing.T) {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatalf("making the child's mounts private: %v", err)
	}
	if err := unix.Mount("tmpfs", "/proc", "tmpfs", 0, ""); err != nil {
		t.Fatalf("hiding /proc: %v", err)
	}
	if _, err := os.Stat("/proc/self"); err == nil {
		t.Fatal("/proc/self still resolves")
	}
	touchAdvancesLockTime(t)
	t.Log(noProcChecked)
}
