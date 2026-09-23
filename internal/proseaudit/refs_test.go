package proseaudit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// citationPattern matches a `file.go:NNN` or `file.go:NNN-MMM` reference with
// real numbers. The leading path is matched and later cut to its base name, so
// a citation copied out of a stack frame is still recognized.
var citationPattern = regexp.MustCompile(`([\w./-]+\.go):(\d+)(?:-(\d+))?`)

// citation is one line reference read out of one comment.
type citation struct {
	// target is the cited file's base name, path prefix stripped.
	target string
	// raw is the reference as written, reproduced in the failure message so
	// the reader can find it by searching for what they typed.
	raw string
	// first and last are the cited line span; both hold the same number for
	// the common single-line form.
	first int
	last  int
}

// pkgFiles is one directory's parsed Go files by base name, its `foo` and
// `foo_test` packages held together so helper resolution sees across the split.
type pkgFiles map[string]*ast.File

// TestCommentLineReferencesResolve is the gate: every `file.go:NNN` in every
// comment in the module must name a line `go test` could report a failure on,
// in a test file of the same package.
func TestCommentLineReferencesResolve(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	fset := token.NewFileSet()

	var problems []string
	for _, dir := range goDirs(t, root) {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatalf("relative path of %s: %v", dir, err)
		}
		for _, problem := range auditPackage(fset, parseDir(t, fset, dir)) {
			problems = append(problems, filepath.Join(rel, problem))
		}
	}

	if len(problems) > 0 {
		t.Fatalf("stale or forbidden line citations:\n\t%s", strings.Join(problems, "\n\t"))
	}
}

// TestAuditReportsAStaleLineCitation pins the stale-line check with a positive
// control: one fixture passes citing its assertion's own line and is reported
// citing the line after it.
func TestAuditReportsAStaleLineCitation(t *testing.T) {
	t.Parallel()

	accurate, fatalLine := fixtureSource(0)
	if problems := auditFixture(t, fixtureName, accurate); len(problems) != 0 {
		t.Fatalf("accurate citation of line %d reported as a problem: %v", fatalLine, problems)
	}

	stale, _ := fixtureSource(1)
	problems := auditFixture(t, fixtureName, stale)
	if len(problems) != 1 {
		t.Fatalf("citation one line past the assertion reported %d problems, want 1: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], strconv.Itoa(fatalLine+1)) {
		t.Fatalf("problem does not name the stale line %d: %s", fatalLine+1, problems[0])
	}
}

// TestAuditReportsACitationIntoAProductionFile pins the production-file ban as
// a ban, not a line check: the cited production line exists and is still
// reported, while naming the identifier instead passes.
func TestAuditReportsACitationIntoAProductionFile(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	files := pkgFiles{
		productionName:     parseFixture(t, fset, productionName, productionSource),
		productionTestName: parseFixture(t, fset, productionTestName, productionTestSource(true)),
	}
	problems := auditPackage(fset, files)
	if len(problems) != 1 {
		t.Fatalf("citation into a production file reported %d problems, want 1: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], productionName) {
		t.Fatalf("problem does not name the cited production file: %s", problems[0])
	}

	files[productionTestName] = parseFixture(t, fset, productionTestName, productionTestSource(false))
	if problems := auditPackage(fset, files); len(problems) != 0 {
		t.Fatalf("naming the identifier instead of a line was reported as a problem: %v", problems)
	}
}

// TestAuditAcceptsAHelperCallAndADeclarationLine pins the helper-call and
// declaration-line accepting branches, each against a negative (a plain call,
// a line with no assertion), so neither can rot into accepting everything.
func TestAuditAcceptsAHelperCallAndADeclarationLine(t *testing.T) {
	t.Parallel()

	for _, tc := range helperFixtureCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source, line := helperFixtureSource(tc.marker)
			problems := auditFixture(t, fixtureName, source)
			if tc.wantProblem && len(problems) == 0 {
				t.Fatalf("citation of line %d (%s) was accepted, want reported", line, tc.name)
			}
			if !tc.wantProblem && len(problems) != 0 {
				t.Fatalf("citation of line %d (%s) was reported: %v", line, tc.name, problems)
			}
		})
	}
}

// helperFixtureCase is one row of TestAuditAcceptsAHelperCallAndADeclarationLine:
// marker names the fixture line to cite, wantProblem states whether the audit
// must reject that citation.
type helperFixtureCase struct {
	name        string
	marker      string
	wantProblem bool
}

// helperFixtureCases returns the four lines of one fixture that separate the
// two accepting branches from their negatives.
func helperFixtureCases() []helperFixtureCase {
	return []helperFixtureCase{
		{name: "call_to_t_Helper_function", marker: helperCallMarker, wantProblem: false},
		{name: "declaration_line", marker: declarationMarker, wantProblem: false},
		{name: "call_to_plain_function", marker: plainCallMarker, wantProblem: true},
		{name: "line_with_no_assertion", marker: inertMarker, wantProblem: true},
	}
}

// auditPackage reports every bad citation in one directory's parsed files, as
// `file:line: message` relative to that directory.
func auditPackage(fset *token.FileSet, files pkgFiles) []string {
	helpers := helperFuncs(files)

	targets := make(map[string]map[int]bool, len(files))
	for name, file := range files {
		targets[name] = failureLines(fset, file, helpers)
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)

	problems := make([]string, 0, len(names))
	for _, name := range names {
		problems = append(problems, auditFile(fset, name, files[name], targets)...)
	}
	return problems
}

// auditFile reports the bad citations carried by one file's comments. Only
// *ast.Comment nodes are read, so string literals, this file's own fixtures
// included, are out of scope by construction.
func auditFile(fset *token.FileSet, name string, file *ast.File, targets map[string]map[int]bool) []string {
	var problems []string
	for _, group := range file.Comments {
		for _, comment := range group.List {
			base := fset.Position(comment.Pos()).Line
			for _, span := range citationPattern.FindAllStringSubmatchIndex(comment.Text, -1) {
				cited, ok := parseCitation(comment.Text, span)
				if !ok {
					continue
				}
				if problem := checkCitation(cited, targets); problem != "" {
					line := base + strings.Count(comment.Text[:span[0]], "\n")
					problems = append(problems, fmt.Sprintf("%s:%d: %s", name, line, problem))
				}
			}
		}
	}
	return problems
}

// parseCitation turns one regexp match into a citation. It reports false for
// a line number too large to be one, which no comment in this repository
// carries but which a malformed citation could produce.
func parseCitation(text string, span []int) (citation, bool) {
	group := func(i int) string {
		if span[2*i] < 0 {
			return ""
		}
		return text[span[2*i]:span[2*i+1]]
	}

	first, err := strconv.Atoi(group(2))
	if err != nil {
		return citation{}, false
	}
	last := first
	if end := group(3); end != "" {
		if last, err = strconv.Atoi(end); err != nil {
			return citation{}, false
		}
	}
	return citation{
		target: filepath.Base(group(1)),
		raw:    group(0),
		first:  first,
		last:   last,
	}, true
}

// checkCitation returns the problem with one citation, or "" when it is sound.
// A production file is refused without reading its lines: the identifier at
// that line survives edits above it, and the number does not.
func checkCitation(cited citation, targets map[string]map[int]bool) string {
	if !strings.HasSuffix(cited.target, "_test.go") {
		return cited.raw + " cites a production file; name the identifier instead of a line"
	}

	lines, ok := targets[cited.target]
	if !ok {
		return cited.raw + " cites a file that is not in this package"
	}

	for line := cited.first; line <= cited.last; line++ {
		if !lines[line] {
			return fmt.Sprintf("%s cites line %d, which is not a line go test could report a failure on", cited.raw, line)
		}
	}
	return ""
}

// failureSites accumulates the failure-attributable lines of one file.
type failureSites struct {
	fset    *token.FileSet
	helpers map[string]bool
	lines   map[int]bool
}

// failureLines returns every line of file `go test` could attribute a failure
// to: each function's declaration line, and every line an assertion or helper
// call spans, since the line reported for a multi-line call depends on layout.
func failureLines(fset *token.FileSet, file *ast.File, helpers map[string]bool) map[int]bool {
	sites := &failureSites{fset: fset, helpers: helpers, lines: make(map[int]bool)}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		sites.lines[fset.Position(fn.Pos()).Line] = true
		sites.scan(fn.Body, fn.Type.Params, nil)
	}
	return sites.lines
}

// scan marks every failure site in one function body; outer carries the
// testing identifiers visible from enclosing functions, for subtest closures.
func (s *failureSites) scan(body *ast.BlockStmt, params *ast.FieldList, outer map[string]bool) {
	if body == nil {
		return
	}
	idents := testingIdents(params, outer)
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.FuncLit:
			s.scan(typed.Body, typed.Type.Params, idents)
			return false
		case *ast.CallExpr:
			if isFailureCall(typed, idents, s.helpers) {
				s.mark(typed.Lparen, typed.Rparen)
			}
		}
		return true
	})
}

// mark records every line the span from..to covers.
func (s *failureSites) mark(from, to token.Pos) {
	for line := s.fset.Position(from).Line; line <= s.fset.Position(to).Line; line++ {
		s.lines[line] = true
	}
}

// isFailureCall reports whether call is one of the two call shapes a failure
// can be attributed to: an assertion on a testing value, or a call to a
// same-package helper.
func isFailureCall(call *ast.CallExpr, idents, helpers map[string]bool) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		receiver, ok := fun.X.(*ast.Ident)
		return ok && idents[receiver.Name] && isFailureSelector(fun.Sel.Name)
	case *ast.Ident:
		return helpers[fun.Name]
	default:
		return false
	}
}

// isFailureSelector reports whether name is a testing method that can carry a
// file:line into the output.
func isFailureSelector(name string) bool {
	switch name {
	case "Fatal", "Fatalf", "Error", "Errorf", "Log", "Logf", "Skip", "Skipf":
		return true
	default:
		return false
	}
}

// helperFuncs returns the names of the package's plain functions whose body
// calls Helper() on a testing value. Methods are excluded: a citation
// resolves a bare identifier, and a method call is never one.
func helperFuncs(files pkgFiles) map[string]bool {
	out := make(map[string]bool)
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !callsHelper(fn) {
				continue
			}
			out[fn.Name.Name] = true
		}
	}
	return out
}

// callsHelper reports whether fn's body calls Helper() on one of its own
// testing parameters.
func callsHelper(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	idents := testingIdents(fn.Type.Params, nil)

	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Helper" {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		found = found || (ok && idents[receiver.Name])
		return !found
	})
	return found
}

// testingIdents returns the identifiers bound to a testing value in a
// function with these parameters, on top of those visible from outer.
func testingIdents(params *ast.FieldList, outer map[string]bool) map[string]bool {
	idents := make(map[string]bool, len(outer))
	for name := range outer {
		idents[name] = true
	}
	if params == nil {
		return idents
	}
	for _, field := range params.List {
		if !isTestingType(field.Type) {
			continue
		}
		for _, name := range field.Names {
			idents[name.Name] = true
		}
	}
	return idents
}

// isTestingType reports whether expr is *testing.T, *testing.B, *testing.F,
// or testing.TB.
func isTestingType(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != "testing" {
		return false
	}
	switch selector.Sel.Name {
	case "T", "B", "F", "TB":
		return true
	default:
		return false
	}
}

// moduleRoot walks up from this package's directory to the one holding
// go.mod.
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

// goDirs returns every directory under root holding a .go file, skipping
// vendor/, testdata/ fixtures and dot-directories of tooling.
func goDirs(t *testing.T, root string) []string {
	t.Helper()

	seen := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDir(path, root, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".go") {
			seen[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	return dirs
}

// skipDir reports whether the walk must not descend into this directory.
func skipDir(path, root, name string) bool {
	if path == root {
		return false
	}
	return name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")
}

// parseDir parses every .go file directly in dir, comments included.
func parseDir(t *testing.T, fset *token.FileSet, dir string) pkgFiles {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	files := make(pkgFiles)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", filepath.Join(dir, entry.Name()), err)
		}
		files[entry.Name()] = file
	}
	return files
}

// Fixture names, and the placeholder the builders replace with a computed line
// number, so the built source stays line-for-line the one that was measured.
const (
	fixtureName        = "fixture_test.go"
	productionName     = "thing.go"
	productionTestName = "thing_test.go"
	citedPlaceholder   = "CITED"
)

// Markers naming the fixture lines TestAuditAcceptsAHelperCallAndADeclarationLine
// cites, one per row of its table.
const (
	helperCallMarker  = "\tcheckThing(t)"
	declarationMarker = "func TestFixture(t *testing.T) {"
	plainCallMarker   = "\tsetUp()"
	inertMarker       = "\tvalue := 1"
)

// fixtureLines is the source TestAuditReportsAStaleLineCitation audits, held
// as lines so the assertion's number is computed rather than counted by hand.
func fixtureLines() []string {
	return []string{
		"package fixture",
		"",
		`import "testing"`,
		"",
		"// TestFixture fails with:",
		"//",
		"//\tfixture_test.go:CITED: boom",
		"func TestFixture(t *testing.T) {",
		"\tt.Fatalf(\"boom\")",
		"}",
	}
}

// fixtureSource returns the fixture citing its assertion's line plus offset,
// together with the line that assertion really sits on.
func fixtureSource(offset int) (string, int) {
	lines := fixtureLines()
	fatal := slices.Index(lines, "\tt.Fatalf(\"boom\")") + 1
	source := strings.ReplaceAll(strings.Join(lines, "\n"), citedPlaceholder, strconv.Itoa(fatal+offset))
	return source, fatal
}

// helperFixtureLines is the source TestAuditAcceptsAHelperCallAndADeclarationLine
// audits: one file holding both accepting branches and both of their
// negatives.
func helperFixtureLines() []string {
	return []string{
		"package fixture",
		"",
		`import "testing"`,
		"",
		"// TestFixture fails with:",
		"//",
		"//\tfixture_test.go:CITED: boom",
		declarationMarker,
		plainCallMarker,
		inertMarker,
		"\t_ = value",
		helperCallMarker,
		"}",
		"",
		"func checkThing(t *testing.T) {",
		"\tt.Helper()",
		"\tt.Fatalf(\"boom\")",
		"}",
		"",
		"func setUp() {}",
	}
}

// helperFixtureSource returns that fixture citing the line marker sits on,
// together with that line.
func helperFixtureSource(marker string) (string, int) {
	lines := helperFixtureLines()
	cited := slices.Index(lines, marker) + 1
	source := strings.ReplaceAll(strings.Join(lines, "\n"), citedPlaceholder, strconv.Itoa(cited))
	return source, cited
}

// productionSource is the fixture's production file: the cited line exists and
// holds real code, so rejecting a citation of it can only be the ban firing.
const productionSource = `package fixture

func Thing() int {
	return 1
}
`

// productionTestSource returns the fixture's test file, citing the production
// file by line when byLine is set and by identifier otherwise.
func productionTestSource(byLine bool) string {
	reference := "Thing"
	if byLine {
		reference = productionName + ":4"
	}
	return fmt.Sprintf("package fixture\n\n// TestThing pins %s.\nfunc TestThing() {}\n", reference)
}

// auditFixture parses one in-memory file and audits it as its own package.
func auditFixture(t *testing.T, name, source string) []string {
	t.Helper()

	fset := token.NewFileSet()
	return auditPackage(fset, pkgFiles{name: parseFixture(t, fset, name, source)})
}

// parseFixture parses one in-memory source file, comments included.
func parseFixture(t *testing.T, fset *token.FileSet, name, source string) *ast.File {
	t.Helper()

	file, err := parser.ParseFile(fset, name, source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	return file
}
