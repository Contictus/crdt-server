package crdt_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The CRDT engine is written from scratch: no third-party code decides how a
// byte on the wire is interpreted. Test files are exempt (DECISIONS.md §D9) so
// that property tests may use a generator library; production code is not.
func TestNoThirdPartyImports(t *testing.T) {
	const selfPrefix = "github.com/mesutokul/ycollab/internal/crdt"

	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("%s: parse: %v", path, err)
			return nil
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Errorf("%s: bad import %s", path, imp.Path.Value)
				continue
			}
			if strings.HasPrefix(p, selfPrefix) {
				continue
			}
			// A standard library path has no dot in its first element.
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q; internal/crdt must depend on the standard library only", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files == 0 {
		t.Fatal("no non-test Go files found; the test is not checking anything")
	}
	t.Logf("checked %d files", files)
}
