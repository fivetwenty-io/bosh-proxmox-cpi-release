package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// namespaceRefused ends a test whose child process could not get a private
// mount namespace. When PVE_CPI_REQUIRE_NAMESPACE_TESTS is set it fails,
// because the caller set up a container that allows the namespace and a skip
// would hide that the test never ran. Anywhere else it skips.
func namespaceRefused(t *testing.T, what string, err error) {
	t.Helper()
	if os.Getenv("PVE_CPI_REQUIRE_NAMESPACE_TESTS") != "" {
		t.Fatalf("this system refuses a private mount namespace, so %s: %v", what, err)
	}
	t.Skipf("this system refuses a private mount namespace, so %s: %v", what, err)
}

// workerDirEnv names the directory the stand-in worker of
// TestBPMWorkerViewSeesTheWorkersMounts works in.
const workerDirEnv = "PVE_CID_TEST_WORKER_DIR"

// workerReady is what the stand-in worker prints once its mount is in place.
const workerReady = "worker ready"

// A stand-in for a BPM worker runs in a mount namespace of its own, with a
// bind mount of a volume that the host does not have, as BPM gives a worker.
// The view through its pidfile sees that mount, which the host's mounts do
// not show, and sees a parent the worker lacks as missing. A pidfile whose
// process shares the doctor's mounts is stale and gives no view. The test
// skips only when this system refuses the stand-in its namespaces.
func TestBPMWorkerViewSeesTheWorkersMounts(t *testing.T) {
	if dir := os.Getenv(workerDirEnv); dir != "" {
		standInWorker(t, dir)
		return
	}
	dir := t.TempDir()
	volume := filepath.Join(dir, "disk", "volume")
	inside := filepath.Join(dir, "worker", "volume")
	journal := filepath.Join(inside, "journal")
	for _, path := range []string{filepath.Join(volume, "journal"), journal} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := filepath.Join(dir, "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	writePid := func(pid int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(run, "worker_1.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePid(os.Getpid())
	if bpmWorkerView(run) != nil {
		t.Fatal("a pidfile whose process shares this process's mounts gave a view")
	}

	// #nosec G204 -- the test binary runs itself with a fixed test name
	cmd := exec.Command(os.Args[0], "-test.run=^TestBPMWorkerViewSeesTheWorkersMounts$", "-test.count=1")
	cmd.Env = append(os.Environ(), workerDirEnv+"="+dir)
	attr := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if os.Geteuid() != 0 {
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}
	}
	cmd.SysProcAttr = attr
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		namespaceRefused(t, "no stand-in worker can run here", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("the stand-in worker failed: %v", err)
		}
	})
	ready := false
	for lines := bufio.NewScanner(stdout); lines.Scan(); {
		if lines.Text() == workerReady {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("the stand-in worker never put its mount in place")
	}
	writePid(cmd.Process.Pid)
	view := bpmWorkerView(run)
	if view == nil {
		t.Fatal("no view into the stand-in worker")
	}
	cases := []struct {
		parent string
		want   workerMount
	}{
		{inside, workerSameMount},
		{filepath.Join(dir, "worker"), workerOtherMount},
		{filepath.Join(dir, "absent"), workerNoParent},
	}
	for _, tc := range cases {
		got, err := view(tc.parent, journal)
		if err != nil || got != tc.want {
			t.Errorf("view(%s, %s) = %d, %v, want %d", tc.parent, journal, got, err, tc.want)
		}
	}
	if _, err := view(inside, filepath.Join(dir, "no-journal")); err == nil {
		t.Error("an anchor the worker cannot see gave no error")
	}
}

// standInWorker runs in the stand-in worker's own mount namespace. It
// bind-mounts the volume into place, says so, and waits for the test to close
// its standard input.
func standInWorker(t *testing.T, dir string) {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatalf("making the stand-in worker's mounts private: %v", err)
	}
	volume := filepath.Join(dir, "disk", "volume")
	inside := filepath.Join(dir, "worker", "volume")
	if err := unix.Mount(volume, inside, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind-mounting %s onto %s: %v", volume, inside, err)
	}
	if _, err := os.Stdout.WriteString(workerReady + "\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return
		}
	}
}
