package dep

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefKey(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	for _, tt := range []struct {
		name      string
		a, b      string
		wantEqual bool
	}{
		{name: "owner/repo casing", a: "Actions/Checkout@v4", b: "actions/checkout@v4", wantEqual: true},
		{name: "full SHA casing", a: "o/a@" + strings.ToUpper(sha), b: "o/a@" + sha, wantEqual: true},
		{name: "branch casing is another ref", a: "o/a@Main", b: "o/a@main"},
		{name: "short hex is a name, not a SHA", a: "o/a@ABCDEF0", b: "o/a@abcdef0"},
		{name: "ref containing @ keeps its case", a: "O/A@Release@2024", b: "o/a@release@2024"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantEqual, RefKeyOf(tt.a) == RefKeyOf(tt.b))
			nwo, ref, _ := strings.Cut(tt.a, "@")
			assert.Equal(t, RefKeyOf(tt.a), RefKey(nwo, ref))
		})
	}
}

func TestNewRef(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	for _, tt := range []struct {
		name string
		a, b string
		same bool
	}{
		{name: "full SHA in any case", a: strings.ToUpper(sha), b: sha, same: true},
		{name: "mixed-case full SHA", a: "AbCdEf" + sha[6:], b: sha, same: true},
		{name: "branch case is another ref", a: "Main", b: "main"},
		{name: "tag case is another ref", a: "V1", b: "v1"},
		{name: "short hex is a name", a: "ABCDEF0", b: "abcdef0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.same, NewRef(tt.a) == NewRef(tt.b))
			assert.Equal(t, tt.same, SameRef(tt.a, tt.b))
		})
	}
	assert.Equal(t, sha, NewRef(strings.ToUpper(sha)).String(), "a full SHA is spelled lowercase")
	assert.Equal(t, "Main", NewRef("Main").String(), "a branch keeps its case")
	assert.Equal(t, NewCommit("abc"), NewCommit("ABC"), "a commit ID folds at any length")
}

func TestKeyCompare(t *testing.T) {
	a, b := RefKey("o/a", "v1"), RefKey("o/b", "v1")
	assert.Negative(t, a.Compare(b))
	assert.Zero(t, a.Compare(RefKey("O/A", "v1")))
	assert.Equal(t, "o/a@v1", RefKey("O/A", "v1").String())
}

// keyFactories are the exported constructors of a non-E2 key, mapped to the
// package directories allowed to call them. Any other caller could build a
// key from one source's spelling and compare it with another source's,
// which is how the full-SHA identity bug kept coming back.
var keyFactories = map[string][]string{
	"github.com/github/actions-lockfile/go/pkg/lockfile.IndexKey":       {"internal/lockfile"},
	"(github.com/github/actions-lockfile/go/pkg/lockfile.Pin).IndexKey": {"internal/lockfile"},
	"(github.com/github/actions-lockfile/go/pkg/lockfile.Pin).String":   {"internal/lockfile"},
	"github.com/github/gh-actions-lock/internal/ghapi.ForNWORef":        {"internal/ghapi", "internal/resolve"},
	"github.com/github/gh-actions-lock/internal/ghapi.ForActionRef":     {"internal/ghapi", "internal/resolve"},
}

// TestReferenceIdentityBoundary fails when a package outside a key's owner
// uses one of keyFactories. Calls through fmt's Stringer are display, not
// keys, and aren't seen.
func TestReferenceIdentityBoundary(t *testing.T) {
	for site, positions := range keyFactoryUses(t) {
		t.Errorf("%s builds a key outside its owner at %s; compare across sources with RefKey or lockfile.RefIndex", site, strings.Join(positions, ", "))
	}
}

// keyFactoryUses type-checks every non-test package in the module and
// returns its disallowed key factory uses, keyed "dir/file.go:function:factory".
func keyFactoryUses(t *testing.T) map[string][]string {
	t.Helper()
	root, module := moduleRoot(t)
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	require.NoError(t, err)

	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "source", nil).(types.ImporterFrom)
	uses := map[string][]string{}
	for _, dir := range dirs {
		bp, err := build.Default.ImportDir(dir, 0)
		var noGo *build.NoGoError
		if errors.As(err, &noGo) {
			continue
		}
		require.NoError(t, err)
		rel, err := filepath.Rel(root, dir)
		require.NoError(t, err)
		rel = filepath.ToSlash(rel)
		var files []*ast.File
		for _, name := range bp.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
			require.NoError(t, err)
			files = append(files, f)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
		conf := types.Config{Importer: imp}
		_, err = conf.Check(path.Join(module, rel), fset, files, info)
		require.NoError(t, err)
		for _, f := range files {
			for _, decl := range f.Decls {
				ast.Inspect(decl, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if !ok {
						return true
					}
					fn, ok := info.Uses[id].(*types.Func)
					if !ok {
						return true
					}
					owners, banned := keyFactories[fn.FullName()]
					if !banned || slices.Contains(owners, rel) {
						return true
					}
					pos := fset.Position(id.Pos())
					site := path.Join(rel, filepath.Base(pos.Filename)) + ":" + declName(decl) + ":" + shortName(fn)
					uses[site] = append(uses[site], fmt.Sprintf("line %d", pos.Line))
					return true
				})
			}
		}
	}
	return uses
}

func declName(decl ast.Decl) string {
	if fd, ok := decl.(*ast.FuncDecl); ok {
		return fd.Name.Name
	}
	return "var"
}

// shortName renders a factory as its package name plus identifier, e.g.
// ghapi.ForNWORef or lockfile.Pin.String.
func shortName(fn *types.Func) string {
	if recv := fn.Signature().Recv(); recv != nil {
		named := recv.Type()
		if p, ok := named.(*types.Pointer); ok {
			named = p.Elem()
		}
		return fn.Pkg().Name() + "." + named.(*types.Named).Obj().Name() + "." + fn.Name()
	}
	return fn.Pkg().Name() + "." + fn.Name()
}

func moduleRoot(t *testing.T) (dir, module string) {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if m, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return dir, strings.TrimSpace(m)
				}
			}
			t.Fatalf("%s/go.mod has no module line", dir)
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.mod above the test")
		dir = parent
	}
}
