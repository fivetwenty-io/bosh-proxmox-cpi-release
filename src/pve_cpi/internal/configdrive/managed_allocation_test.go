package configdrive_test

import (
	"bytes"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/configdrive"
	"os"
	"testing"
)

func TestBuildAllocationBound(t *testing.T) {
	t.Parallel()
	for _, size := range []int{1, 1024, 1024 * 1024} {
		path, cleanup, err := configdrive.Build(bytes.Repeat([]byte("x"), size))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() <= 0 || uint64(info.Size()) != configdrive.AllocationBytes() {
			t.Fatalf("payload=%d image=%d exceeds bound=%d", size, info.Size(), configdrive.AllocationBytes())
		}
	}
}
