package handlers

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCreatedVolumeRecordComesOnlyFromTheCreateSteps keeps the record that
// lets a park skip the holder scan's offline refusal where it belongs. Only
// create_disk's two create steps may make one, from the volume their own
// create call returned, and only its constructor may fill one in. A record
// made anywhere else would let a park attach a disk that a guest on an
// offline node may still hold.
func TestCreatedVolumeRecordComesOnlyFromTheCreateSteps(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	allowed := []string{
		filepath.Join("cpi", "handlers", "create_disk.go"),
		filepath.Join("cpi", "handlers", "managed_disk.go"),
		filepath.Join("pve", "disk_holder_offline.go"),
	}
	var makers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path) // #nosec G304 -- walks the module's own source tree
		if err != nil {
			return err
		}
		text := string(body)
		if !strings.Contains(text, "VolumeCreatedByThisRequest(") && !strings.Contains(text, "CreatedVolume{volid") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		makers = append(makers, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the source tree: %v", err)
	}
	slices.Sort(makers)
	want := slices.Clone(allowed)
	slices.Sort(want)
	if !slices.Equal(makers, want) {
		t.Fatalf("files that make a created-volume record = %v; want exactly %v", makers, want)
	}
}
