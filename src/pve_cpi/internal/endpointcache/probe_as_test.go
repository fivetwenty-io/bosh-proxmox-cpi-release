package endpointcache

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// ProbeAs judges ownership as the user it is given. A directory this process
// owns is refused when judged as another user, with that user named in the
// refusal, and passes when judged as its owner. Probe itself keeps judging as
// the effective user.
func TestProbeAsJudgesTheDirectoryAsTheNamedUser(t *testing.T) {
	temporaryParent(t)
	other := os.Geteuid() + 1
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	makePrivateDirectory(t, sibling)
	cases := map[string]Options{
		"override": {Override: privateDirectory(t)},
		"journal":  {JournalDir: journal},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			refused := ProbeAs(opts, other)
			defer func() { _ = refused.Close() }()
			var unsafe *privatefile.UnsafePathError
			if refused.Usable() || !errors.As(refused.Err, &unsafe) ||
				!errors.Is(refused.Err, privatefile.ErrUnsafeOwnership) || unsafe.EUID != other {
				t.Fatalf("judged as uid %d: %+v, want an ownership refusal that names that uid", other, refused)
			}
			owned := ProbeAs(opts, os.Geteuid())
			defer func() { _ = owned.Close() }()
			if !owned.Usable() {
				t.Fatalf("judged as the owner: %+v, want the directory usable", owned)
			}
			plain := Probe(opts)
			defer func() { _ = plain.Close() }()
			if !plain.Usable() {
				t.Fatalf("Probe must keep judging as the effective user: %+v", plain)
			}
		})
	}
}

// The temporary layer of a judged probe names the directory of the judged
// user, and like Probe it creates nothing.
func TestProbeAsLooksForTheJudgedUsersTemporaryDirectory(t *testing.T) {
	parent := temporaryParent(t)
	other := os.Geteuid() + 1
	choice := ProbeAs(Options{}, other)
	defer func() { _ = choice.Close() }()
	want := filepath.Join(parent, temporaryName+strconv.Itoa(other))
	if choice.Layer != LayerTemporary || choice.Path != want || !errors.Is(choice.Err, os.ErrNotExist) {
		t.Fatalf("choice = %+v, want the absent temporary directory %s", choice, want)
	}
	requireAbsent(t, want, "the judged temporary directory")
	requireAbsent(t, temporaryPath(parent), "the effective user's temporary directory")
}

// DirOptions.Options copies the fields one by one, so a field added to one of
// the two types and not the other must fail here.
func TestDirOptionsAndOptionsCarryTheSameFields(t *testing.T) {
	exported := func(v any) []string {
		var names []string
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			if f := typ.Field(i); f.IsExported() {
				names = append(names, f.Name+" "+f.Type.String())
			}
		}
		return names
	}
	if got, want := exported(DirOptions{}), exported(Options{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("DirOptions fields %v, Options fields %v, want the same", got, want)
	}
	guard := &journalGuard{}
	d := DirOptions{Override: "/o", JournalDir: "/j", DisplayName: "n", Guard: guard}
	if got := d.Options(); got != (Options{Override: "/o", JournalDir: "/j", DisplayName: "n", Guard: guard}) {
		t.Fatalf("Options() = %+v, want every field copied", got)
	}
}

// probeAsCallers are the only non-test sources that may call ProbeAs, because
// a choice judged as another user is never one the CPI may use.
var probeAsCallers = []string{"../../cmd/pve-cid/", "../../internal/endpointcache/"}

func TestProbeAsIsCalledOnlyByTheDiagnostic(t *testing.T) {
	var offenders []string
	seen := false
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		slashed := filepath.ToSlash(path)
		if slashed == "../../cmd/cpi/main.go" {
			seen = true
		}
		for _, allowed := range probeAsCallers {
			if strings.HasPrefix(slashed, allowed) {
				return nil
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ProbeAs" {
					offenders = append(offenders, path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("the walk from ../.. did not reach cmd/cpi/main.go")
	}
	if len(offenders) > 0 {
		t.Errorf("non-test sources outside %v call ProbeAs: %v", probeAsCallers, offenders)
	}
}
