package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The Director job's monit file says how each worker starts, which tells the
// doctor whether the workers run under BPM. These are the two forms the
// director job renders, and a file that says neither leaves it unknown.
func TestWorkersFromReadsHowTheDirectorStartsItsWorkers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cases := map[string]struct {
		monit string
		want  workerMode
	}{
		"bpm": {`check process worker_1
  with pidfile /var/vcap/sys/run/bpm/director/worker_1.pid
  start program "/var/vcap/jobs/bpm/bin/bpm start director -p worker_1"
`, workersUnderBPM},
		"dynamic disks only under bpm": {`check process dynamic_disks_worker_1
  start program "/var/vcap/jobs/bpm/bin/bpm start director -p dynamic_disks_worker_1"
`, workersUnderBPM},
		"host": {`check process director
  start program "/var/vcap/jobs/bpm/bin/bpm start director -p director"
check process worker_1
  with pidfile /var/vcap/sys/run/director/worker_1.pid
  start program "/var/vcap/jobs/director/bin/worker_ctl worker_1 start"
`, workersOnHost},
		"no worker": {`check process director
  start program "/var/vcap/jobs/bpm/bin/bpm start director -p director"
`, workersUnknown},
	}
	for name, tc := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(tc.monit), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := workersFrom(path); got != tc.want {
			t.Errorf("%s: workersFrom = %d, want %d", name, got, tc.want)
		}
	}
	if got := workersFrom(filepath.Join(dir, "absent")); got != workersUnknown {
		t.Errorf("a missing monit file gives %d, want unknown", got)
	}
}

func TestStoreVolumeNamesTheDirectoryJustBelowTheStore(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/var/vcap/store/director/pve-cache": "/var/vcap/store/director",
		"/var/vcap/store/pve_cpi":            "/var/vcap/store/pve_cpi",
		"/var/vcap/store":                    "",
		"/var/vcap/storefront/x":             "",
		"/var/vcap/data/director/tmp":        "",
	}
	for path, want := range cases {
		if got := storeVolume("/var/vcap/store", path); got != want {
			t.Errorf("storeVolume(%s) = %q, want %q", path, got, want)
		}
	}
}

// With no journal the doctor judges with its own temporary directory, so a
// finding that the CPI does not create the override says that the answer holds
// only when the CPI's TMPDIR names the same directory.
func TestCollectNamesItsOwnTemporaryDirectoryWhenTheOverrideIsNotCreated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a doctor that runs as root does not judge the temporary directory")
	}
	if info, err := os.Stat("/tmp"); err != nil || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Skipf("/tmp is not a directory that belongs to root here (%v)", err)
	}
	t.Setenv("TMPDIR", "/tmp")
	override := filepath.Join(t.TempDir(), "cache")
	cfg := haCfg("auto", "none")
	cfg.EndpointCacheDir = override
	want(t, cacheFindingsAs(cfg, os.Geteuid()), haKindCacheDirectory, haSeverityAdvice, override,
		"does not create it because the temporary directory /tmp belongs to root",
		"this check used its own temporary directory, /tmp, which answers for the CPI only when the CPI's TMPDIR "+
			"names the same directory")
}
