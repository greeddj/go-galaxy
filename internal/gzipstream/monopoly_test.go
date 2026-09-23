package gzipstream

// This file gates the pgzip monopoly by parsing the module's own source. It
// resolves the import path rather than the identifier, so an alias such as
// `gzip "github.com/klauspost/pgzip"` cannot hide a reader.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pgzipImportPath is the library whose reading side this package holds a
// monopoly on.
const pgzipImportPath = "github.com/klauspost/pgzip"

// pgzipDotImport is what pgzipLocalName reports for a file that dot-imports
// the library: the selector search cannot see through one, so it is treated as
// a use rather than as an absence.
const pgzipDotImport = "."

// pgzipLocalName returns the name file binds klauspost/pgzip to, resolved by
// import path since a call may read gzip.NewReader: "" for no import or a
// blank one, pgzipDotImport for a dot import.
func pgzipLocalName(file *ast.File) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != pgzipImportPath {
			continue
		}
		switch {
		case imp.Name == nil:
			return "pgzip" // the package's own name
		case imp.Name.Name == "_":
			return ""
		default:
			return imp.Name.Name
		}
	}
	return ""
}

// moduleRoot walks up from this package's directory to the one holding go.mod,
// so the walk below covers the module rather than wherever the test binary
// happens to run.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestPgzipBeyondItsWriterAPIIsThisPackagesAlone fails on any non-test file
// outside this package naming a pgzip member beyond pgzipWriterAPI; test
// files, vendor/ and dot-directories are out of scope.
func TestPgzipBeyondItsWriterAPIIsThisPackagesAlone(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return skipUnauditedDir(root, path, d)
		case !auditableFile(root, path):
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		if uses := pgzipUsesOutsideTheWriterAPI(file); len(uses) > 0 {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s reaches %v; only internal/gzipstream may read through pgzip", rel, uses)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// skipUnauditedDir prunes the directories this gate does not read: vendor/ and
// anything dot-prefixed, both of which are outside this module's own source.
func skipUnauditedDir(root, path string, d os.DirEntry) error {
	if path != root && (d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".")) {
		return filepath.SkipDir
	}
	return nil
}

// auditableFile reports whether path is a non-test Go file outside this
// package - the set the monopoly covers.
func auditableFile(root, path string) bool {
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.Dir(rel) != filepath.Join("internal", "gzipstream")
}

// The fixtures the control below parses, spelled out here rather than inside
// it so that the test reads as the table it is. Each one is a construction
// that reaches, or deliberately does not reach, a pgzip reader.
const (
	aliasedPgzipFixture = `package fixture

import (
	"io"

	gzip "github.com/klauspost/pgzip"
)

func open(r io.Reader) { _, _ = gzip.NewReader(r) }
`
	plainPgzipFixture = `package fixture

import (
	"io"

	"github.com/klauspost/pgzip"
)

func open(r io.Reader) { _, _ = pgzip.NewReaderN(r, 1024, 1) }
`
	functionValuePgzipFixture = `package fixture

import (
	"io"

	"github.com/klauspost/pgzip"
)

var ctor = pgzip.NewReader

func open(r io.Reader) { _, _ = ctor(r) }
`
	resetPgzipFixture = `package fixture

import (
	"io"

	"github.com/klauspost/pgzip"
)

func open(r io.Reader) {
	var z pgzip.Reader
	_ = z.Reset(r)
}
`
	dotImportPgzipFixture = `package fixture

import (
	"io"

	. "github.com/klauspost/pgzip"
)

func open(r io.Reader) { _, _ = NewReader(r) }
`
	blankImportPgzipFixture = `package fixture

import (
	_ "github.com/klauspost/pgzip"
)
`
	writerOnlyPgzipFixture = `package fixture

import (
	"io"

	"github.com/klauspost/pgzip"
)

func compress(w io.Writer) { _ = pgzip.NewWriter(w) }
`
	stdlibGzipFixture = `package fixture

import (
	"compress/gzip"
	"io"
)

func open(r io.Reader) { _, _ = gzip.NewReader(r) }
`
)

// TestPgzipUsesSeeEveryConstructionThatReachesAReader is the gate's positive
// and negative control: every construction reaching a pgzip reader is found,
// while a blank import, the writer and stdlib gzip stay silent.
func TestPgzipUsesSeeEveryConstructionThatReachesAReader(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		src  string
		want int
	}{
		{name: "an aliased pgzip import", src: aliasedPgzipFixture, want: 1},
		{name: "an unaliased pgzip import", src: plainPgzipFixture, want: 1},
		{name: "a constructor held as a function value", src: functionValuePgzipFixture, want: 1},
		{name: "a zero value turned into a reader by Reset", src: resetPgzipFixture, want: 1},
		{name: "a dot import", src: dotImportPgzipFixture, want: 1},
		{name: "a blank import", src: blankImportPgzipFixture, want: 0},
		{name: "the writer this seam is allowed", src: writerOnlyPgzipFixture, want: 0},
		{name: "the standard library under the same identifier", src: stdlibGzipFixture, want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing the fixture: %v", err)
			}
			if got := pgzipUsesOutsideTheWriterAPI(file); len(got) != tt.want {
				t.Fatalf("pgzipUsesOutsideTheWriterAPI(%s) = %v, want %d use(s)", tt.name, got, tt.want)
			}
		})
	}
}

// pgzipWriterAPI reports whether name is a pgzip member another package may
// name: an allow-list, since a blocklist of the reader constructors misses a
// constructor held as a function value and a zero Reader made live by Reset.
func pgzipWriterAPI(name string) bool {
	return name == "NewWriter" || name == "NewWriterLevel"
}

// pgzipUsesOutsideTheWriterAPI reports, one string per use, how file reaches
// pgzip outside pgzipWriterAPI. Every selector counts, not only callees, so
// the type naming a zero Reader exposes the Reset route.
func pgzipUsesOutsideTheWriterAPI(file *ast.File) []string {
	local := pgzipLocalName(file)
	if local == "" {
		return nil
	}
	if local == pgzipDotImport {
		return []string{"dot-imports " + pgzipImportPath}
	}

	var uses []string
	ast.Inspect(file, func(n ast.Node) bool {
		if name, found := pgzipSelectorOutsideTheWriterAPI(n, local); found {
			uses = append(uses, local+"."+name)
		}
		return true
	})
	return uses
}

// pgzipSelectorOutsideTheWriterAPI reports the member of the package bound to
// local that n names, if n names one outside pgzipWriterAPI at all.
func pgzipSelectorOutsideTheWriterAPI(n ast.Node, local string) (string, bool) {
	sel, isSel := n.(*ast.SelectorExpr)
	if !isSel {
		return "", false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	if !isIdent || pkg.Name != local {
		return "", false
	}
	if pgzipWriterAPI(sel.Sel.Name) {
		return "", false
	}
	return sel.Sel.Name, true
}
