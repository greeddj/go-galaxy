package lockaudit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// holderCase is one row of the closed table this gate audits: the file
// holding a command's lifecycle function, that function's own name, and the
// name of the work function it must run under the holder context.
type holderCase struct {
	file string
	fn   string
	work string
}

// holderCases is the closed table of every function that takes the backend's
// exclusive lock and works under it: withBackend for install, warm and lock,
// and runCleanup. A new such function must be added here; nothing detects it.
func holderCases() []holderCase {
	return []holderCase{
		{file: "internal/galaxy/collections/run.go", fn: "withBackend", work: "work"},
		{file: "internal/galaxy/cleanup/cleanup.go", fn: "runCleanup", work: "cleanupWithState"},
	}
}

// delegateCase is one row of the second closed table: the file holding a
// collection command's entry function, that function's own name, and the work
// function it must hand to withBackend.
type delegateCase struct {
	file string
	fn   string
	work string
}

// delegateCases is the closed table of collection commands that must reach
// withBackend: auditing the funnel alone never proves a command still uses it.
func delegateCases() []delegateCase {
	return []delegateCase{
		{file: "internal/galaxy/collections/install_command.go", fn: "runInstall", work: "installWithState"},
		{file: "internal/galaxy/collections/warm_command.go", fn: "runWarm", work: "warmWithState"},
		{file: "internal/galaxy/collections/lock_command.go", fn: "runLock", work: "lockWithState"},
	}
}

// TestHolderContextIsThreadedAndJudged pins, per holderCases row, that the
// holder context is bound to a name, handed first to the one work call wrapped
// in the lock-loss verdict, and every later return is that verdict or nil.
func TestHolderContextIsThreadedAndJudged(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	for _, tc := range holderCases() {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			path := filepath.Join(root, filepath.FromSlash(tc.file))
			file := parseGoFile(t, fset, path)

			// A named function missing from the file fails rather than skips, so a
			// rename cannot turn the gate into a passing no-op.
			fn := findFunc(file, tc.fn)
			if fn == nil {
				t.Fatalf("%s holds no function %s; this gate names it and cannot audit what it cannot find", tc.file, tc.fn)
			}
			if problems := auditHolder(fset, fn, tc.work); len(problems) > 0 {
				t.Fatalf("%s: %s does not thread and judge its holder context:\n\t%s",
					tc.file, tc.fn, strings.Join(problems, "\n\t"))
			}
		})
	}
}

// TestCollectionCommandsDelegateTheirLifecycle is the second half of the
// gate. For each row: the function exists, it hands its own work function to
// exactly one withBackend call, and it takes no backend lifecycle of its own.
func TestCollectionCommandsDelegateTheirLifecycle(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	for _, tc := range delegateCases() {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			path := filepath.Join(root, filepath.FromSlash(tc.file))
			file := parseGoFile(t, fset, path)

			// Same anti-vacuity check the holder half makes, for the same
			// reason: a renamed command must fail this gate, not vanish
			// from it.
			fn := findFunc(file, tc.fn)
			if fn == nil {
				t.Fatalf("%s holds no function %s; this gate names it and cannot audit what it cannot find", tc.file, tc.fn)
			}
			if problems := auditDelegation(fn, tc.work); len(problems) > 0 {
				t.Fatalf("%s: %s does not delegate its backend lifecycle:\n\t%s",
					tc.file, tc.fn, strings.Join(problems, "\n\t"))
			}
		})
	}
}

// delegateFixtureCase is one row of TestAuditReportsALifecycleTakenOutsideTheFunnel:
// the body under audit and whether the audit must report it.
type delegateFixtureCase struct {
	name        string
	body        string
	wantProblem bool
}

// delegateFixtureCases returns the two departures the delegation half catches,
// a command inlining its own lifecycle and one handing the funnel another
// work function, plus the control that must be accepted.
func delegateFixtureCases() []delegateFixtureCase {
	return []delegateFixtureCase{
		{
			name:        "the command took a backend lifecycle of its own",
			body:        "\tholder, state, err := initInstall(ctx, cfg)\n\t_ = holder\n\t_ = state\n\treturn err",
			wantProblem: true,
		},
		{
			name:        "the command hands the funnel a different work function",
			body:        `	return withBackend(ctx, cfg, runtime, "banner", otherWithState)`,
			wantProblem: true,
		},
		{
			name:        "the correct shape",
			body:        `	return withBackend(ctx, cfg, runtime, "banner", workWithState)`,
			wantProblem: false,
		},
	}
}

// TestAuditReportsALifecycleTakenOutsideTheFunnel runs the delegation
// predicate against synthetic sources, so each departure is produced in
// isolation without mutating the tree under test.
func TestAuditReportsALifecycleTakenOutsideTheFunnel(t *testing.T) {
	t.Parallel()

	for _, tc := range delegateFixtureCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			problems := auditDelegationFixture(t, tc.body)
			if tc.wantProblem && len(problems) == 0 {
				t.Fatalf("the audit accepted a fixture it must report")
			}
			if !tc.wantProblem && len(problems) != 0 {
				t.Fatalf("the audit reported the correct shape: %v", problems)
			}
		})
	}
}

// auditDelegationFixture parses one in-memory command function and audits its
// delegation, so a control states only the departure it is about.
func auditDelegationFixture(t *testing.T, body string) []string {
	t.Helper()

	source := fmt.Sprintf("package fixture\n\nfunc runWork(ctx context.Context, cfg *config.Config) error {\n%s\n}\n", body)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	fn := findFunc(file, "runWork")
	if fn == nil {
		t.Fatalf("fixture holds no runWork")
	}
	return auditDelegation(fn, "workWithState")
}

// auditDelegation reports every way fn departs from delegating its lifecycle:
// exactly one withBackend call, handed work as its last argument, and no
// initInstall of its own. A wrong withBackend count bails out at once.
func auditDelegation(fn *ast.FuncDecl, work string) []string {
	calls := collectCalls(fn, "withBackend")
	if len(calls) != 1 {
		return []string{fmt.Sprintf("holds %d calls to withBackend, want exactly 1", len(calls))}
	}

	var problems []string
	if !lastArgIs(calls[0], work) {
		problems = append(problems, fmt.Sprintf(
			"withBackend is not handed %s as its work half, so this command's lifecycle would run somebody else's work", work))
	}
	if len(collectInitAssignments(fn)) > 0 {
		problems = append(problems,
			"this command takes a backend lifecycle of its own instead of going through withBackend, so nothing audits its holder context")
	}
	return problems
}

// lastArgIs reports whether call's final argument is the identifier name;
// counting from the end survives withBackend's other arguments moving.
func lastArgIs(call *ast.CallExpr, name string) bool {
	return len(call.Args) > 0 && isIdent(call.Args[len(call.Args)-1], name)
}

// holderFixtureCase is one row of TestAuditReportsAMisthreadedLifecycle: the
// identifier the fixture's work call is handed, whether that call is wrapped
// in the verdict, and whether the audit must report the result.
type holderFixtureCase struct {
	name        string
	workArg     string
	wrapped     bool
	wantProblem bool
}

// holderFixtureCases returns the two single-edit departures no other test
// catches, the work call handed the caller's context and its outcome returned
// unjudged (a stolen lock exits 0), plus the control that must be accepted.
func holderFixtureCases() []holderFixtureCase {
	return []holderFixtureCase{
		{
			name:        "the work call is handed the caller's context instead of the holder's",
			workArg:     "ctx",
			wrapped:     true,
			wantProblem: true,
		},
		{
			name:        "the work call's outcome is returned without the verdict",
			workArg:     "holder",
			wrapped:     false,
			wantProblem: true,
		},
		{
			name:        "the correct shape",
			workArg:     "holder",
			wrapped:     true,
			wantProblem: false,
		},
	}
}

// TestAuditReportsAMisthreadedLifecycle runs the predicate against synthetic
// sources rather than against the real files, so each departure can be
// produced in isolation without mutating the tree under test.
func TestAuditReportsAMisthreadedLifecycle(t *testing.T) {
	t.Parallel()

	for _, tc := range holderFixtureCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			problems := auditFixture(t, holderFixtureSource(tc.workArg, tc.wrapped))
			if tc.wantProblem && len(problems) == 0 {
				t.Fatalf("the audit accepted a fixture it must report")
			}
			if !tc.wantProblem && len(problems) != 0 {
				t.Fatalf("the audit reported the correct shape: %v", problems)
			}
		})
	}
}

// TestAuditReportsADiscardedHolderContext pins that a holder context bound to
// the blank identifier is reported, while the same fixture with a name is not.
func TestAuditReportsADiscardedHolderContext(t *testing.T) {
	t.Parallel()

	discarded := strings.Replace(holderFixtureSource("holder", true), "holder, state, err :=", "_, state, err :=", 1)
	if problems := auditFixture(t, discarded); len(problems) == 0 {
		t.Fatalf("the audit accepted a holder context bound to the blank identifier")
	}
	if problems := auditFixture(t, holderFixtureSource("holder", true)); len(problems) != 0 {
		t.Fatalf("the audit reported the same fixture with the holder context bound to a name: %v", problems)
	}
}

// holderFixtureSource builds a parsed-only lifecycle function whose work call
// is handed workArg first and, when wrapped, returned through the verdict.
func holderFixtureSource(workArg string, wrapped bool) string {
	work := fmt.Sprintf("workWithState(%s, cfg)", workArg)
	final := "return " + work
	if wrapped {
		final = fmt.Sprintf("return cacheManager.LockLostError(ctx, holder, %s)", work)
	}
	return fmt.Sprintf(`package fixture

func runWork(ctx context.Context, cfg *config.Config) error {
	holder, state, err := initInstall(ctx, cfg)
	if err != nil {
		return cacheManager.LockLostError(ctx, holder, err)
	}
	defer func() { _ = state.release() }()
	%s
}
`, final)
}

// auditFixture parses one in-memory lifecycle function and audits it, so a
// control states only the departure it is about.
func auditFixture(t *testing.T, source string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	fn := findFunc(file, "runWork")
	if fn == nil {
		t.Fatalf("fixture holds no runWork")
	}
	return auditHolder(fset, fn, "workWithState")
}

// auditHolder reports every way fn departs from the required shape, one line
// each, empty when fn threads and judges its holder context. The first three
// checks bail out, since later ones are phrased in terms of what they resolve.
func auditHolder(fset *token.FileSet, fn *ast.FuncDecl, work string) []string {
	inits := collectInitAssignments(fn)
	if len(inits) != 1 {
		return []string{fmt.Sprintf("holds %d assignments from initInstall/initCleanup, want exactly 1", len(inits))}
	}
	holder, problem := holderIdent(inits[0])
	if problem != "" {
		return []string{problem}
	}

	calls := collectCalls(fn, work)
	if len(calls) != 1 {
		return []string{fmt.Sprintf("holds %d calls to %s, want exactly 1", len(calls), work)}
	}

	var problems []string
	if !firstArgIs(calls[0], holder) {
		problems = append(problems, fmt.Sprintf(
			"%s is not handed %s as its first argument, so the work would run under a context that cannot end when the lock does",
			work, holder))
	}
	if !wrappedInVerdict(fn, calls[0], holder) {
		problems = append(problems, fmt.Sprintf(
			"the %s call is not the third argument of a cacheManager.LockLostError(_, %s, _) call, so its outcome is never judged",
			work, holder))
	}
	return append(problems, unjudgedReturns(fset, fn, inits[0], holder)...)
}

// holderIdent returns the name the init assignment binds its first result to,
// or the problem with that binding.
func holderIdent(assign *ast.AssignStmt) (string, string) {
	if len(assign.Lhs) != 3 {
		return "", fmt.Sprintf("the initInstall/initCleanup assignment binds %d values, want 3", len(assign.Lhs))
	}
	ident, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return "", "the initInstall/initCleanup assignment's first bound value is not an identifier"
	}
	if ident.Name == "_" {
		return "", "the holder context is discarded into _, so nothing downstream can judge this run against it"
	}
	return ident.Name, ""
}

// unjudgedReturns reports every return positioned after the lock was acquired
// that is neither a bare nil nor a verdict carrying the holder; a return before
// the init assignment has no holder to judge against and stays legal.
func unjudgedReturns(fset *token.FileSet, fn *ast.FuncDecl, init *ast.AssignStmt, holder string) []string {
	var problems []string
	inspectBody(fn, func(node ast.Node) {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok || ret.Pos() < init.Pos() {
			return
		}
		if isBareNilReturn(ret) || isVerdictReturn(ret, holder) {
			return
		}
		problems = append(problems, fmt.Sprintf(
			"the return at %s is neither `return nil` nor a cacheManager.LockLostError(_, %s, _) verdict",
			fset.Position(ret.Pos()), holder))
	})
	return problems
}

// isBareNilReturn reports whether ret returns exactly nil.
func isBareNilReturn(ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 1 {
		return false
	}
	ident, ok := ret.Results[0].(*ast.Ident)
	return ok && ident.Name == "nil"
}

// isVerdictReturn reports whether ret returns exactly one lock-loss verdict
// judged against holder.
func isVerdictReturn(ret *ast.ReturnStmt, holder string) bool {
	if len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	return ok && isVerdictCall(call, holder)
}

// isVerdictCall reports whether call is cacheManager.LockLostError(_, holder,
// _). The package qualifier is required: this gate audits callers of that
// function, never the package declaring it.
func isVerdictCall(call *ast.CallExpr, holder string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "LockLostError" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != "cacheManager" || len(call.Args) != 3 {
		return false
	}
	return isIdent(call.Args[1], holder)
}

// wrappedInVerdict reports whether work appears as the judged expression of
// some verdict call in fn. Identity, not shape, is what is compared: the
// verdict must wrap THAT call, not a second call that happens to look alike.
func wrappedInVerdict(fn *ast.FuncDecl, work *ast.CallExpr, holder string) bool {
	found := false
	inspectBody(fn, func(node ast.Node) {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isVerdictCall(call, holder) {
			return
		}
		found = found || call.Args[2] == ast.Expr(work)
	})
	return found
}

// collectInitAssignments returns every assignment in fn whose sole right-hand
// side is a call to initInstall or initCleanup.
func collectInitAssignments(fn *ast.FuncDecl) []*ast.AssignStmt {
	var out []*ast.AssignStmt
	inspectBody(fn, func(node ast.Node) {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return
		}
		if isIdent(call.Fun, "initInstall") || isIdent(call.Fun, "initCleanup") {
			out = append(out, assign)
		}
	})
	return out
}

// collectCalls returns every call to the plain function named name in fn.
func collectCalls(fn *ast.FuncDecl, name string) []*ast.CallExpr {
	var out []*ast.CallExpr
	inspectBody(fn, func(node ast.Node) {
		if call, ok := node.(*ast.CallExpr); ok && isIdent(call.Fun, name) {
			out = append(out, call)
		}
	})
	return out
}

// firstArgIs reports whether call's first argument is the identifier name.
func firstArgIs(call *ast.CallExpr, name string) bool {
	return len(call.Args) > 0 && isIdent(call.Args[0], name)
}

// isIdent reports whether expr is exactly the identifier name.
func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

// inspectBody walks fn's own body, never descending into a function literal
// inside it: a return in a deferred closure is that closure's return, and a
// call made there is not the lifecycle function's own work call.
func inspectBody(fn *ast.FuncDecl, visit func(ast.Node)) {
	if fn.Body == nil {
		return
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		visit(node)
		return true
	})
}

// findFunc returns the plain function named name in file, or nil.
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// parseGoFile parses one file of the module under test. Comments are not
// requested: this gate reads structure only.
func parseGoFile(t *testing.T, fset *token.FileSet, path string) *ast.File {
	t.Helper()

	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return file
}

// moduleRoot walks up from this package's directory to the one holding
// go.mod, so the table's paths can be module-relative rather than relative to
// wherever the test binary happens to run.
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
