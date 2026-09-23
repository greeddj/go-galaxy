package store

// This file audits snapshot.go's AST rather than running code. It gates one
// type, one file and one field: a *Store mutator added in another file is
// covered only by the hand-kept table in TestEveryMutatorMarksDirty.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// dirtyAuditExemptMethod is the one method excused from setting s.dirty: an
// unmarshal is a load, and marking it would make every S3-backed run dirty on
// arrival. It is a literal name, so it excuses nothing that merely resembles it.
const dirtyAuditExemptMethod = "UnmarshalJSON"

// TestEveryWriteLockedStoreMethodMarksDirty pins that every *Store method in
// snapshot.go taking s.mu.Lock() also sets s.dirty = true, bar one exemption;
// an exemption it cannot find fails, so a rename cannot turn the gate green.
func TestEveryWriteLockedStoreMethodMarksDirty(t *testing.T) {
	t.Parallel()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	path := filepath.Join(dir, "snapshot.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	exemptFound, problems := auditDirtyMutators(file)
	if !exemptFound {
		t.Fatalf(
			"the exemption names %s, which %s does not contain any *Store method for; "+
				"this gate cannot audit an exemption it cannot find, so a rename must not pass silently",
			dirtyAuditExemptMethod, path,
		)
	}
	if len(problems) > 0 {
		t.Fatalf("the following *Store methods in %s take the write lock but never set dirty = true: %v", path, problems)
	}
}

// auditDirtyMutators reports whether dirtyAuditExemptMethod is among file's
// *Store methods, and names every other write-locked *Store method that never
// sets dirty = true.
func auditDirtyMutators(file *ast.File) (bool, []string) {
	exemptFound := false
	var problems []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		recv, ok := storeReceiverIdent(fn)
		if !ok {
			continue
		}
		if fn.Name.Name == dirtyAuditExemptMethod {
			exemptFound = true
			continue
		}
		if !callsWriteLock(fn, recv) {
			continue
		}
		if !setsDirtyTrue(fn, recv) {
			problems = append(problems, fn.Name.Name)
		}
	}
	return exemptFound, problems
}

// storeReceiverIdent returns the receiver identifier name of fn when fn is a
// method on *Store, or ("", false) otherwise (a plain function, a method on
// some other type, or a *Store method with no named receiver).
func storeReceiverIdent(fn *ast.FuncDecl) (string, bool) {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "", false
	}
	field := fn.Recv.List[0]
	star, ok := field.Type.(*ast.StarExpr)
	if !ok {
		return "", false
	}
	ident, ok := star.X.(*ast.Ident)
	if !ok || ident.Name != "Store" {
		return "", false
	}
	if len(field.Names) != 1 {
		return "", false
	}
	return field.Names[0].Name, true
}

// callsWriteLock reports whether fn's body calls recv.mu.Lock() anywhere.
// RLock is a different selector name and never matches, so a read-only
// method is correctly left out of this gate's scope.
func callsWriteLock(fn *ast.FuncDecl, recv string) bool {
	if fn.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		lockSel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || lockSel.Sel.Name != "Lock" {
			return true
		}
		muSel, ok := lockSel.X.(*ast.SelectorExpr)
		if !ok || muSel.Sel.Name != "mu" {
			return true
		}
		if ident, ok := muSel.X.(*ast.Ident); ok && ident.Name == recv {
			found = true
		}
		return true
	})
	return found
}

// setsDirtyTrue reports whether fn's body contains an assignment statement
// of the exact shape recv.dirty = true anywhere.
func setsDirtyTrue(fn *ast.FuncDecl, recv string) bool {
	if fn.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if isDirtyTrueAssignment(node, recv) {
			found = true
		}
		return true
	})
	return found
}

// isDirtyTrueAssignment reports whether node is exactly the assignment
// statement recv.dirty = true. Split out of setsDirtyTrue purely to stay
// under this repository's cyclomatic-complexity budget.
func isDirtyTrueAssignment(node ast.Node, recv string) bool {
	assign, ok := node.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "dirty" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != recv {
		return false
	}
	rhs, ok := assign.Rhs[0].(*ast.Ident)
	return ok && rhs.Name == "true"
}
