package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// syncBuffer is a bytes.Buffer that a command and its background workers can
// write to together, as they do with the process's standard error.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// exitDrainLab is a cluster of two configured endpoints, the first of which
// refuses connections, and a config that turns the endpoint cache on for it.
type exitDrainLab struct {
	dead, live string
	cacheDir   string
	configPath string
	requestDir string
}

func newExitDrainLab(t *testing.T) *exitDrainLab {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	lab := &exitDrainLab{dead: deadAddr, live: srv.Listener.Addr().String(), requestDir: t.TempDir()}
	lab.cacheDir = filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(lab.cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(lab.dead)
	port, _ := strconv.Atoi(portText)
	body := fmt.Sprintf(`{
  "host": %q,
  "port": %d,
  "user": "root",
  "api_token": "root@pam!b=secret",
  "endpoints": [%q, %q],
  "api_endpoint_discovery": "on",
  "endpoint_cache_dir": %q,
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "info",
  "verify_ssl": false
}`, host, port, lab.dead, lab.live, lab.cacheDir)
	lab.configPath = filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(lab.configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return lab
}

// request writes a storage-plan request file and returns its path.
func (l *exitDrainLab) request(t *testing.T) string {
	t.Helper()
	path := filepath.Join(l.requestDir, "request.json")
	if err := os.WriteFile(path, []byte(`{"method":"create_disk","arguments":[1024,{}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// marks reads the down marks the cache file of the lab's config holds.
func (l *exitDrainLab) marks(t *testing.T) map[string]endpointcache.DownMark {
	t.Helper()
	cfg, err := config.LoadFile(l.configPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	choice := endpointcache.OptionsFor(cfg).Resolve()
	if !choice.Usable() {
		t.Fatalf("the cache directory is not usable: %v", choice.Err)
	}
	key := endpointcache.KeyConfig{Endpoints: []string{l.dead, l.live}, VerifySSL: false, User: "root@pam!b"}
	store, err := endpointcache.OpenStore(context.Background(), choice.Root, key, "secret",
		endpointcache.StoreOptions{Logger: log.NewNopLogger(), ConfiguredMode: cfg.DiscoveryModeValue()})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	payload, _, err := store.ReadPayload(time.Now(), nil)
	if err != nil {
		t.Fatalf("ReadPayload: %v", err)
	}
	return payload.Marks
}

// Every subcommand that builds a client which can use the endpoint cache drains
// the process registry before run returns, so the down mark a failover wrote
// just before the command ended is in the cache file for the next process.
func TestRunDrainsTheEndpointCacheBeforeItReturns(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(l *exitDrainLab, t *testing.T) []string
	}{
		{"locate", func(l *exitDrainLab, _ *testing.T) []string {
			return []string{"locate", "local-lvm:vm-100-disk-0", "--config", l.configPath}
		}},
		{"stemcells", func(l *exitDrainLab, _ *testing.T) []string {
			return []string{"stemcells", "--config", l.configPath}
		}},
		{"ha", func(l *exitDrainLab, _ *testing.T) []string {
			return []string{"ha", "--config", l.configPath}
		}},
		{"storage-plan", func(l *exitDrainLab, t *testing.T) []string {
			return []string{"storage-plan", "--config", l.configPath, "--request", l.request(t)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lab := newExitDrainLab(t)
			// An earlier command of the same process drained the registry, so run
			// has to open it again before it builds a client.
			endpointcache.Drain(context.Background(), time.Second)
			t.Cleanup(endpointcache.Open)

			var stdout, stderr syncBuffer
			code := run(tc.args(lab, t), &stdout, &stderr)
			t.Logf("%s exited %d: %s", tc.name, code, stderr.String())

			if !endpointcache.Draining() {
				t.Error("run returned with the endpoint cache's worker registry still open")
			}
			mark, ok := lab.marks(t)[lab.dead]
			if !ok {
				t.Fatalf("marks = %v, want a down mark for %s that run left behind", lab.marks(t), lab.dead)
			}
			if mark.Class != endpointcache.MarkDial || time.Since(mark.DownAt) > time.Minute {
				t.Errorf("mark = %+v, want a fresh dial mark", mark)
			}
			if _, ok := lab.marks(t)[lab.live]; ok {
				t.Errorf("the endpoint that answered carries a mark")
			}
		})
	}
}

// finishedIfNotCancelled starts a worker that records a write 300 milliseconds
// after it starts, unless its context ends first, and returns a flag that says
// whether the write happened. The 300 milliseconds are well short of the
// drain's default bound and long enough that a drain with no wait cancels the
// worker first.
func finishedIfNotCancelled(t *testing.T) *atomic.Bool {
	t.Helper()
	var wrote atomic.Bool
	if err := endpointcache.Go("slow write", config.DiscoveryModeAuto, func(ctx context.Context) {
		select {
		case <-time.After(300 * time.Millisecond):
			wrote.Store(true)
		case <-ctx.Done():
		}
	}); err != nil {
		t.Fatal(err)
	}
	return &wrote
}

// pve-cid passes the drain no wait of its own, so the drain applies its default
// bound. A worker that needs a fraction of that bound finishes before run
// returns, whichever command ran.
func TestRunWaitsForABackgroundWriteWithTheDefaultBound(t *testing.T) {
	endpointcache.Open()
	t.Cleanup(endpointcache.Open)
	wrote := finishedIfNotCancelled(t)

	var stdout, stderr syncBuffer
	if code := run([]string{"version"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d, want %d: %s", code, exitOK, stderr.String())
	}
	if !wrote.Load() {
		t.Error("run returned before the background write finished, or cancelled it")
	}
}

// A worker that never returns is let go once the default bound runs out. The
// command returns after about three seconds, well short of anything a stuck
// write could make an operator wait for, and the worker's context is ended.
func TestRunLetsGoOfAWorkerThatNeverReturnsAfterTheDefaultBound(t *testing.T) {
	endpointcache.Open()
	t.Cleanup(endpointcache.Open)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	ended := make(chan struct{})
	if err := endpointcache.Go("stuck write", config.DiscoveryModeAuto, func(ctx context.Context) {
		<-ctx.Done()
		close(ended)
		<-release
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr syncBuffer
	start := time.Now()
	codeCh := make(chan int, 1)
	go func() { codeCh <- run([]string{"version"}, &stdout, &stderr) }()
	var took time.Duration
	select {
	case code := <-codeCh:
		took = time.Since(start)
		if code != exitOK {
			t.Errorf("exit %d, want %d: %s", code, exitOK, stderr.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("run was still waiting for a stuck worker after 6s, twice the default bound")
	}
	if took < 2*time.Second {
		t.Errorf("run returned after %s, want it to wait for the default bound of %s first", took,
			endpointcache.DrainWait)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("the stuck worker's context was not ended when the drain gave up")
	}
}

// A SIGTERM that arrives while the exit drain waits ends the wait at once, and
// the command still returns the exit code it computed. The signal arrives with
// most of the default bound left, so a drain that ignores it returns about
// three seconds later and fails the deadline.
func TestRunEndsItsDrainOnSIGTERMAndKeepsTheExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"version", []string{"version"}, exitOK},
		{"unknown subcommand", []string{"no-such-subcommand"}, exitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpointcache.Open()
			t.Cleanup(endpointcache.Open)
			held := make(chan os.Signal, 1)
			signal.Notify(held, syscall.SIGTERM)
			t.Cleanup(func() { signal.Stop(held) })
			cancelled := make(chan struct{})
			if err := endpointcache.Go("stuck write", config.DiscoveryModeAuto, func(ctx context.Context) {
				<-ctx.Done()
				close(cancelled)
			}); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr syncBuffer
			codeCh := make(chan int, 1)
			go func() { codeCh <- run(tc.args, &stdout, &stderr) }()
			deadline := time.Now().Add(10 * time.Second)
			for !endpointcache.Draining() {
				if time.Now().After(deadline) {
					t.Fatal("run never started its drain")
				}
				time.Sleep(time.Millisecond)
			}
			sent := time.Now()
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-codeCh:
				if code != tc.want {
					t.Errorf("exit %d after a signal during the drain, want %d: %s", code, tc.want, stderr.String())
				}
				if took := time.Since(sent); took > 1500*time.Millisecond {
					t.Errorf("run returned %s after the signal, want it at once and well under the %s bound", took,
						endpointcache.DrainWait)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("SIGTERM did not end the exit drain")
			}
			select {
			case <-cancelled:
			case <-time.After(5 * time.Second):
				t.Error("the signal did not cancel the slow worker")
			}
		})
	}
}
