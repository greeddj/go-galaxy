package s3

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// cacheBackendClass names which of the three helpers cache-backend classes,
// or none, an S3 sentinel is expected to carry.
type cacheBackendClass int

const (
	classNone cacheBackendClass = iota
	classUnavailable
	classUnusable
	classBusy
)

// sentinelClassCases pins every S3 sentinel to the cache-backend class it must
// carry, or to none; a reclassified entry would move an exit code.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var sentinelClassCases = []struct {
	err   error
	name  string
	class cacheBackendClass
}{
	{name: "errS3TransportFailed", err: errS3TransportFailed, class: classUnavailable},
	{name: "errS3BucketNotFound", err: errS3BucketNotFound, class: classUnavailable},
	{name: "errS3BucketHeadFailed", err: errS3BucketHeadFailed, class: classUnavailable},
	{name: "errS3CreateBucketFailed", err: errS3CreateBucketFailed, class: classUnavailable},
	{name: "errS3BucketRequestFailed", err: errS3BucketRequestFailed, class: classUnavailable},
	{name: "errS3GetFailed", err: errS3GetFailed, class: classUnavailable},
	{name: "errS3HeadFailed", err: errS3HeadFailed, class: classUnavailable},
	{name: "errS3PutFailed", err: errS3PutFailed, class: classUnavailable},
	{name: "errS3DeleteFailed", err: errS3DeleteFailed, class: classUnavailable},
	{name: "errS3ConditionalConflict", err: errS3ConditionalConflict, class: classUnavailable},
	{name: "errS3InvalidEndpoint", err: errS3InvalidEndpoint, class: classUnusable},
	{name: "errS3ConditionalPutUnsupported", err: errS3ConditionalPutUnsupported, class: classUnusable},
	{name: "errS3CompareAndSwapUnsupported", err: errS3CompareAndSwapUnsupported, class: classUnusable},
	{name: "errS3RedirectRefused", err: errS3RedirectRefused, class: classUnusable},
	{name: "errS3LockWaitTimeout", err: errS3LockWaitTimeout, class: classBusy},
	{name: "errS3LockWaitNoHolderObserved", err: errS3LockWaitNoHolderObserved, class: classUnavailable},
	{name: "errS3LockLost", err: errS3LockLost, class: classNone},
	{name: "errS3TokenGeneration", err: errS3TokenGeneration, class: classNone},
	{name: "errS3NotFound", err: errS3NotFound, class: classNone},
	{name: "errS3BucketEmpty", err: errS3BucketEmpty, class: classNone},
	{name: "errS3PreconditionFailed", err: errS3PreconditionFailed, class: classNone},
	{name: "errS3HTTPClientNil", err: errS3HTTPClientNil, class: classNone},
	{name: "errS3ClientNil", err: errS3ClientNil, class: classNone},
	{name: "errArtifactSHA256Mismatch", err: errArtifactSHA256Mismatch, class: classNone},
}

// TestSentinelClassPartitionIsExhaustiveAndExclusive pins that every sentinel
// has one row and matches its row's class and neither other one, since
// exitcode's FromError would otherwise pick whichever class it checks first.
func TestSentinelClassPartitionIsExhaustiveAndExclusive(t *testing.T) {
	t.Parallel()
	assertEverySentinelHasOneRow(t)
	for _, tt := range sentinelClassCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wantUnavailable := tt.class == classUnavailable
			wantUnusable := tt.class == classUnusable
			wantBusy := tt.class == classBusy

			if got := errors.Is(tt.err, helpers.ErrCacheBackendUnavailable); got != wantUnavailable {
				t.Errorf("errors.Is(err, helpers.ErrCacheBackendUnavailable) = %v, want %v", got, wantUnavailable)
			}
			if got := errors.Is(tt.err, helpers.ErrCacheBackendUnusable); got != wantUnusable {
				t.Errorf("errors.Is(err, helpers.ErrCacheBackendUnusable) = %v, want %v", got, wantUnusable)
			}
			if got := errors.Is(tt.err, helpers.ErrCacheBusy); got != wantBusy {
				t.Errorf("errors.Is(err, helpers.ErrCacheBusy) = %v, want %v", got, wantBusy)
			}
		})
	}
}

// sentinelTableFile holds sentinelClassCases; the exhaustiveness check reads it
// as source, since only the source says which variable a row passes as err.
const sentinelTableFile = "sentinel_class_test.go"

// assertEverySentinelHasOneRow fails when a sentinel this package's non-test
// files declare has no row, or when a row repeats one or names no such sentinel.
func assertEverySentinelHasOneRow(t *testing.T) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	declared := declaredSentinels(t, dir)
	listed := listedSentinels(t, filepath.Join(dir, sentinelTableFile))

	missing, surplus := sentinelRowDrift(declared, listed)
	if len(missing) > 0 {
		t.Errorf("sentinels with no row in sentinelClassCases: %v", missing)
	}
	if len(surplus) > 0 {
		t.Errorf("sentinelClassCases rows repeating a sentinel or naming none this package declares: %v", surplus)
	}
}

// declaredSentinels returns, sorted, every package-level err* or Err* variable
// in dir's non-test files, and fails when there is none rather than pass empty.
func declaredSentinels(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		names = append(names, fileSentinels(file)...)
	}
	if len(names) == 0 {
		t.Fatalf("no package-level err* variable in %s; a gate that finds no sentinel checks nothing", dir)
	}
	slices.Sort(names)
	return names
}

// listedSentinels returns the variable each sentinelClassCases row passes as err,
// read from path, and fails a row whose name is not that variable's own name.
func listedSentinels(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	table := varCompositeLiteral(file, "sentinelClassCases")
	if table == nil {
		t.Fatalf("%s assigns no composite literal to sentinelClassCases", path)
	}
	listed := make([]string, 0, len(table.Elts))
	for i, elt := range table.Elts {
		name, errIdent := rowFields(elt)
		if errIdent == "" || name != errIdent {
			t.Errorf("sentinelClassCases row %d is named %q but passes %q as err; a row names the variable it checks", i, name, errIdent)
		}
		if errIdent != "" {
			listed = append(listed, errIdent)
		}
	}
	return listed
}

// sentinelRowDrift returns the declared sentinels no row lists, then every
// listed name that repeats an earlier row or is not a declared sentinel.
func sentinelRowDrift(declared, listed []string) ([]string, []string) {
	seen := make(map[string]bool, len(listed))
	var surplus []string
	for _, name := range listed {
		if seen[name] || !slices.Contains(declared, name) {
			surplus = append(surplus, name)
		}
		seen[name] = true
	}
	var missing []string
	for _, name := range declared {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	return missing, surplus
}

// fileSentinels names file's package-level variables spelled like a sentinel.
func fileSentinels(file *ast.File) []string {
	var names []string
	for _, spec := range packageVarSpecs(file) {
		for _, ident := range spec.Names {
			if isSentinelName(ident.Name) {
				names = append(names, ident.Name)
			}
		}
	}
	return names
}

// packageVarSpecs returns every value spec of file's package-level var
// declarations, grouped or not.
func packageVarSpecs(file *ast.File) []*ast.ValueSpec {
	var specs []*ast.ValueSpec
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			if value, ok := spec.(*ast.ValueSpec); ok {
				specs = append(specs, value)
			}
		}
	}
	return specs
}

// isSentinelName reports whether name is spelled err or Err followed by an
// upper-case letter, the shape every sentinel in this package takes.
func isSentinelName(name string) bool {
	rest, ok := strings.CutPrefix(name, "err")
	if !ok {
		rest, ok = strings.CutPrefix(name, "Err")
	}
	return ok && rest != "" && unicode.IsUpper(rune(rest[0]))
}

// varCompositeLiteral returns the composite literal file assigns to the
// package-level variable named name, or nil when it assigns none.
func varCompositeLiteral(file *ast.File, name string) *ast.CompositeLit {
	for _, spec := range packageVarSpecs(file) {
		if len(spec.Names) != 1 || spec.Names[0].Name != name || len(spec.Values) != 1 {
			continue
		}
		if lit, ok := spec.Values[0].(*ast.CompositeLit); ok {
			return lit
		}
	}
	return nil
}

// rowFields returns the name literal and the err identifier one table row
// spells, each empty when the row does not spell it as a literal or identifier.
func rowFields(elt ast.Expr) (string, string) {
	row, ok := elt.(*ast.CompositeLit)
	if !ok {
		return "", ""
	}
	var name, errIdent string
	for _, field := range row.Elts {
		kv, ok := field.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch value := kv.Value.(type) {
		case *ast.BasicLit:
			if key.Name == "name" && value.Kind == token.STRING {
				name, _ = strconv.Unquote(value.Value)
			}
		case *ast.Ident:
			if key.Name == "err" {
				errIdent = value.Name
			}
		}
	}
	return name, errIdent
}
