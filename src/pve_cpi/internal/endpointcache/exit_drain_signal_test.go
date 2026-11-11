package endpointcache

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// exitDrainChildEnv makes TestExitDrainChild act as the child of
// TestDrainAtExitGivesTheSignalsBackWhenItReturns.
const exitDrainChildEnv = "ENDPOINTCACHE_EXIT_DRAIN_CHILD"

// TestExitDrainChild is the child process of the test below. It runs the last
// drain of a command, says so, and then waits as a command that has finished
// its drain and not yet exited would.
func TestExitDrainChild(t *testing.T) {
	if os.Getenv(exitDrainChildEnv) == "" {
		t.Skip("runs only as the child of TestDrainAtExitGivesTheSignalsBackWhenItReturns")
	}
	Open()
	DrainAtExit("child", io.Discard, 0)
	if _, err := os.Stdout.WriteString("drained\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

// DrainAtExit catches SIGINT and SIGTERM only while it waits. Once it returns,
// the signals have their default meaning again, so a process that has finished
// its drain and not yet exited still ends on a SIGTERM instead of ignoring it.
func TestDrainAtExitGivesTheSignalsBackWhenItReturns(t *testing.T) {
	// #nosec G204 -- reruns this test binary
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestExitDrainChild$")
	cmd.Env = append(os.Environ(), exitDrainChildEnv+"=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "drained") {
		t.Fatalf("the child said %q (err %v), want it to report its drain", line, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() {
			t.Errorf("the child ended with %v, want it killed by the SIGTERM", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the child ignored a SIGTERM that arrived after its drain returned")
	}
}
