package projectfile

// This file gates the BurntSushi/toml monopoly by parsing the module's own
// source. It resolves the import path rather than the identifier, so an alias,
// a dot import or a blank import cannot hide a second importer.

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

// tomlImportPath is the library this package alone may import.
const tomlImportPath = "github.com/BurntSushi/toml"

// importsTOML reports whether file imports tomlImportPath under any name.
func importsTOML(file *ast.File) bool {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err == nil && path == tomlImportPath {
			return true
		}
	}
	return false
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

// TestTOMLIsThisPackagesAlone fails on any non-test file outside this package
// importing BurntSushi/toml; test files, vendor/ and dot-directories are out
// of scope. Its positive control is this package's own decoder.
func TestTOMLIsThisPackagesAlone(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	fset := token.NewFileSet()
	own, err := parser.ParseFile(fset, filepath.Join(root, "internal", "galaxy", "projectfile", "projectfile.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing projectfile.go: %v", err)
	}
	if !importsTOML(own) {
		t.Fatalf("the gate does not see projectfile.go importing %s", tomlImportPath)
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return skipUnauditedDir(root, path, d)
		case !auditableFile(root, path):
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		if importsTOML(file) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s imports %s; only internal/galaxy/projectfile may", rel, tomlImportPath)
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
	return err == nil && filepath.Dir(rel) != filepath.Join("internal", "galaxy", "projectfile")
}

// The fixtures the control below parses: every spelling of an import of the
// library, and the one that is not an import of it.
const (
	plainTOMLFixture   = "package fixture\n\nimport \"github.com/BurntSushi/toml\"\n\nvar _ = toml.Unmarshal\n"
	aliasedTOMLFixture = "package fixture\n\nimport t \"github.com/BurntSushi/toml\"\n\nvar _ = t.Unmarshal\n"
	dotTOMLFixture     = "package fixture\n\nimport . \"github.com/BurntSushi/toml\"\n\nvar _ = Unmarshal\n"
	blankTOMLFixture   = "package fixture\n\nimport _ \"github.com/BurntSushi/toml\"\n"
	otherTOMLFixture   = "package fixture\n\nimport toml \"github.com/pelletier/go-toml/v2\"\n\nvar _ = toml.Unmarshal\n"
)

// TestImportsTOMLSeesEverySpelling is the gate's control: an import under
// any name is found, while another library bound to the same identifier is
// not.
func TestImportsTOMLSeesEverySpelling(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		src  string
		want bool
	}{
		{name: "a plain import", src: plainTOMLFixture, want: true},
		{name: "an aliased import", src: aliasedTOMLFixture, want: true},
		{name: "a dot import", src: dotTOMLFixture, want: true},
		{name: "a blank import", src: blankTOMLFixture, want: true},
		{name: "another library under the same identifier", src: otherTOMLFixture, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", tt.src, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing the fixture: %v", err)
			}
			if got := importsTOML(file); got != tt.want {
				t.Fatalf("importsTOML(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
