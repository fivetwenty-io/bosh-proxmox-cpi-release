package pvetest_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const importPath = "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"

// moduleRoot walks up from the test's directory to the directory with go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// TestOnlyTestFilesImportPackage fails when a non-test Go file outside vendor
// imports pvetest, because the package imports testing and must not reach the
// production binary.
func TestOnlyTestFilesImportPackage(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	var scanned int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == "pvetest" && filepath.Base(filepath.Dir(path)) == "pve" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == importPath {
				t.Errorf("%s imports %s, which only _test.go files may import", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d production files under %s; the walk is not reaching the module", scanned, root)
	}
}
