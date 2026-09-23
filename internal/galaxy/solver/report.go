package solver

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// ConflictError reports that no selection of versions satisfies every
// requirement. It carries a human-readable proof (the derivation graph
// walked into numbered prose) and a set of conditional hints.
type ConflictError struct {
	proofLines []string
	hints      []string
}

// Error renders the proof followed by any hints, one per line.
func (e *ConflictError) Error() string {
	var b strings.Builder
	for i, l := range e.proofLines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	for _, h := range e.hints {
		b.WriteString("\nhint: ")
		b.WriteString(h)
	}
	return b.String()
}

// Is reports that a ConflictError is a version-resolution failure, so it
// matches helpers.ErrNoVersionSatisfiesConstraints under errors.Is.
func (e *ConflictError) Is(target error) bool {
	return target == helpers.ErrNoVersionSatisfiesConstraints
}

// ProofLines returns the rendered derivation-graph proof, one entry per
// line (a blank entry marks a paragraph break).
func (e *ConflictError) ProofLines() []string {
	return e.proofLines
}

// Hints returns the deterministic, package-name-ordered hint texts.
func (e *ConflictError) Hints() []string {
	return e.hints
}

// isDerivedInc reports whether inc is a conflict-resolution-derived
// incompatibility (as opposed to an external one).
func isDerivedInc(inc *incompatibility) bool {
	_, ok := inc.Cause.(causeConflict)
	return ok
}

// causedByTwoExternals reports whether inc is derived and both of its own
// causes are external - the "simple" case the numbered rendering algorithm
// prefers to inline without a line number.
func causedByTwoExternals(inc *incompatibility) bool {
	cc, ok := inc.Cause.(causeConflict)
	if !ok {
		return false
	}
	return !isDerivedInc(cc.Left) && !isDerivedInc(cc.Right)
}

// pickSimple returns, of cause1 and cause2, whichever one (if exactly one)
// is caused by two externals, alongside the other (the "compound" one).
func pickSimple(cause1, cause2 *incompatibility) (*incompatibility, *incompatibility, bool) {
	c1 := causedByTwoExternals(cause1)
	c2 := causedByTwoExternals(cause2)
	switch {
	case c1:
		return cause1, cause2, true
	case c2:
		return cause2, cause1, true
	default:
		return nil, nil, false
	}
}

// splitOneDerived reports whether exactly one of cc's two causes is itself
// derived, returning (derivedCause, externalCause, true) if so.
func splitOneDerived(cc causeConflict) (*incompatibility, *incompatibility, bool) {
	l, r := isDerivedInc(cc.Left), isDerivedInc(cc.Right)
	switch {
	case l && !r:
		return cc.Left, cc.Right, true
	case r && !l:
		return cc.Right, cc.Left, true
	default:
		return nil, nil, false
	}
}

// countOutgoing records, for every node of inc's derivation graph, how many
// parents cause it, so rendering knows up front which nodes need a number.
func countOutgoing(inc *incompatibility, outgoing map[*incompatibility]int, visited map[*incompatibility]bool) {
	if visited[inc] {
		return
	}
	visited[inc] = true
	cc, ok := inc.Cause.(causeConflict)
	if !ok {
		return
	}
	outgoing[cc.Left]++
	outgoing[cc.Right]++
	countOutgoing(cc.Left, outgoing, visited)
	countOutgoing(cc.Right, outgoing, visited)
}

// reportBuilder accumulates the numbered proof. bug holds the first invariant
// violation renderNode recorded; the walk goes on safely because that node is
// a leaf, and outcome reads bug only once the walk has finished.
type reportBuilder struct {
	lineOf   map[*incompatibility]int
	rendered map[*incompatibility]bool
	outgoing map[*incompatibility]int
	bug      error
	lines    []string
	nextLine int
}

// newReportBuilder returns a reportBuilder with its three lookup maps
// initialized, the only fields a zero value leaves unusable.
func newReportBuilder() *reportBuilder {
	return &reportBuilder{
		lineOf:   make(map[*incompatibility]int),
		rendered: make(map[*incompatibility]bool),
		outgoing: make(map[*incompatibility]int),
	}
}

// hasLine reports whether inc has already been assigned a line number.
func (b *reportBuilder) hasLine(inc *incompatibility) bool {
	_, ok := b.lineOf[inc]
	return ok
}

// ensureRendered renders inc unless it already was, so a node referenced by
// one parent is rendered exactly once, at its sole reference point.
func (b *reportBuilder) ensureRendered(s *solveState, inc *incompatibility) {
	if !b.rendered[inc] {
		b.renderNode(s, inc, false)
	}
}

// forceLineNumber assigns inc the next line number if it has none, appending
// it to the last line written; callers invoke it only right after
// ensureRendered(inc) on a first-time node, so that line is inc's own.
func (b *reportBuilder) forceLineNumber(inc *incompatibility) int {
	if ln, ok := b.lineOf[inc]; ok {
		return ln
	}
	b.nextLine++
	b.lineOf[inc] = b.nextLine
	if n := len(b.lines); n > 0 {
		b.lines[n-1] = fmt.Sprintf("%s (%d)", b.lines[n-1], b.nextLine)
	}
	return b.nextLine
}

// recordBug records err as b's first invariant-violation error (first-wins:
// a bug already recorded is never overwritten).
func (b *reportBuilder) recordBug(err error) {
	if b.bug == nil {
		b.bug = err
	}
}

// renderNode appends inc's explanatory line, recursing into its causes, and
// numbers it when two or more parents reference it. final marks the terminal
// incompatibility, whose line finalizeLine rewrites.
func (b *reportBuilder) renderNode(s *solveState, inc *incompatibility, final bool) {
	cc, ok := inc.Cause.(causeConflict)
	if !ok {
		// Unreachable while every call site passes a derived node. The return
		// is required: cc is the zero causeConflict here, so the next statement
		// would dereference a nil *incompatibility.
		b.recordBug(fmt.Errorf("renderNode called on a non-derived incompatibility (cause %T): %w", inc.Cause, errSolverBug))
		return
	}
	ext1, ext2 := !isDerivedInc(cc.Left), !isDerivedInc(cc.Right)

	var line string
	switch {
	case !ext1 && !ext2:
		line = b.renderBothDerived(s, inc, cc.Left, cc.Right)
	case ext1 != ext2:
		derived, external := cc.Right, cc.Left
		if ext2 {
			derived, external = cc.Left, cc.Right
		}
		line = b.renderOneDerived(s, inc, derived, external)
	default:
		line = renderBothExternal(s, cc, inc)
	}

	b.rendered[inc] = true
	if final {
		line = finalizeLine(line, s.describe(inc))
	}
	b.lines = append(b.lines, line)
	if !final && b.outgoing[inc] >= 2 {
		b.nextLine++
		b.lineOf[inc] = b.nextLine
		b.lines[len(b.lines)-1] = fmt.Sprintf("%s (%d)", line, b.nextLine)
	}
}

// renderBothExternal renders inc's line when both causes are external, naming
// a self-resolved cause once. Pointer identity is exact here because
// incompatStore dedups content-identical incompatibilities to one pointer.
func renderBothExternal(s *solveState, cc causeConflict, inc *incompatibility) string {
	if cc.Left == cc.Right {
		return fmt.Sprintf("Because %s, %s.", s.describe(cc.Left), s.describe(inc))
	}
	return fmt.Sprintf("Because %s and %s, %s.", s.describe(cc.Left), s.describe(cc.Right), s.describe(inc))
}

// renderBothDerived implements the numbered algorithm's case 1: inc is
// caused by two other derived incompatibilities.
func (b *reportBuilder) renderBothDerived(s *solveState, inc, cause1, cause2 *incompatibility) string {
	ln1, has1 := b.lineOf[cause1]
	ln2, has2 := b.lineOf[cause2]

	switch {
	case has1 && has2:
		return fmt.Sprintf("Because %s (%d) and %s (%d), %s.", s.describe(cause1), ln1, s.describe(cause2), ln2, s.describe(inc))
	case has1 != has2:
		withLine, without, ln := cause1, cause2, ln1
		if has2 {
			withLine, without, ln = cause2, cause1, ln2
		}
		b.ensureRendered(s, without)
		return fmt.Sprintf("And because %s (%d), %s.", s.describe(withLine), ln, s.describe(inc))
	default:
		if simple, compound, ok := pickSimple(cause1, cause2); ok {
			b.ensureRendered(s, compound)
			b.ensureRendered(s, simple)
			return fmt.Sprintf("Thus, %s.", s.describe(inc))
		}
		b.ensureRendered(s, cause1)
		ln := b.forceLineNumber(cause1)
		b.lines = append(b.lines, "")
		b.ensureRendered(s, cause2)
		return fmt.Sprintf("And because %s (%d), %s.", s.describe(cause1), ln, s.describe(inc))
	}
}

// renderOneDerived implements the numbered algorithm's case 2: inc is
// caused by exactly one derived incompatibility and one external one.
func (b *reportBuilder) renderOneDerived(s *solveState, inc, derived, external *incompatibility) string {
	if ln, ok := b.lineOf[derived]; ok {
		return fmt.Sprintf("Because %s and %s (%d), %s.", s.describe(external), s.describe(derived), ln, s.describe(inc))
	}
	if cc, ok := derived.Cause.(causeConflict); ok {
		if priorDerived, priorExternal, ok2 := splitOneDerived(cc); ok2 && !b.hasLine(priorDerived) {
			b.ensureRendered(s, priorDerived)
			return fmt.Sprintf("And because %s and %s, %s.", s.describe(priorExternal), s.describe(external), s.describe(inc))
		}
	}
	b.ensureRendered(s, derived)
	return fmt.Sprintf("And because %s, %s.", s.describe(external), s.describe(inc))
}

// finalizeLine rewrites the outermost proof line: its trailing ", {desc}."
// clause becomes ", version solving failed.", and its leading connective
// becomes "So,", per the reference algorithm's special-cased final line.
func finalizeLine(line, desc string) string {
	suffix := ", " + desc + "."
	if trimmed, ok := strings.CutSuffix(line, suffix); ok {
		line = trimmed + ", version solving failed."
	}
	switch {
	case strings.HasPrefix(line, "And because "):
		line = "So, because " + strings.TrimPrefix(line, "And because ")
	case strings.HasPrefix(line, "Because "):
		line = "So, because " + strings.TrimPrefix(line, "Because ")
	case strings.HasPrefix(line, "Thus, "):
		line = "So, " + strings.TrimPrefix(line, "Thus, ")
	}
	return line
}

// buildConflictError renders the proof for the terminal incompatibility inc.
// It must run while the solve's fetched universes are live, since hints and
// labels inspect them.
func (s *solveState) buildConflictError(inc *incompatibility) error {
	b := newReportBuilder()
	countOutgoing(inc, b.outgoing, make(map[*incompatibility]bool))

	if isDerivedInc(inc) {
		b.renderNode(s, inc, true)
	} else {
		b.lines = append(b.lines, finalizeLine(s.describe(inc), s.describe(inc)))
	}

	return b.outcome(s, inc)
}

// outcome returns the recorded invariant violation in preference to the
// proof, whose lines are then incomplete, else the ConflictError with hints.
// Both arms are non-nil, so no typed-nil error can escape.
func (b *reportBuilder) outcome(s *solveState, inc *incompatibility) error {
	if b.bug != nil {
		return b.bug
	}
	return &ConflictError{
		proofLines: b.lines,
		hints:      s.collectHints(inc),
	}
}

// collectHints walks inc's derivation graph for causeNoVersions leaves and
// renders the conditional prerelease hint once per distinct package, ordered
// by package name.
func (s *solveState) collectHints(inc *incompatibility) []string {
	type entry struct{ pkg, text string }
	var entries []entry
	pkgsSeen := make(map[string]bool)
	visited := make(map[*incompatibility]bool)

	var walk func(*incompatibility)
	walk = func(n *incompatibility) {
		if visited[n] {
			return
		}
		visited[n] = true
		switch cause := n.Cause.(type) {
		case causeConflict:
			walk(cause.Left)
			walk(cause.Right)
		case causeNoVersions:
			pkg := cause.term.Package
			if pkgsSeen[pkg] {
				return
			}
			if text, ok := s.prereleaseHint(pkg); ok {
				pkgsSeen[pkg] = true
				entries = append(entries, entry{pkg: pkg, text: text})
			}
		}
	}
	walk(inc)

	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.pkg, b.pkg) })
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.text
	}
	return out
}

// prereleaseHint returns the hint for pkg's fetched universe when all, or
// only some, of its published versions are prereleases.
func (s *solveState) prereleaseHint(pkg string) (string, bool) {
	u := s.uniFor(pkg)
	if len(u.versions) == 0 {
		return "", false
	}
	allPrerelease := true
	anyPrerelease := false
	for _, v := range u.versions {
		if v.sv().Prerelease() != "" {
			anyPrerelease = true
		} else {
			allPrerelease = false
		}
	}
	switch {
	case allPrerelease:
		return fmt.Sprintf(
			"%s publishes only pre-release versions, which plain constraints exclude; "+
				"if a pre-release is acceptable, pin one exactly or use a >=X.Y.Z-0 floor - "+
				"otherwise no published version can satisfy this requirement", pkg,
		), true
	case anyPrerelease:
		return fmt.Sprintf(
			"pre-release versions of %s exist and are excluded by plain constraints; "+
				"if you intended to allow them, use a >=X.Y.Z-0 floor or an exact pin", pkg,
		), true
	default:
		return "", false
	}
}

// describe renders inc's own conclusion text: a specific phrasing for each
// external cause kind, or a generic term-based phrasing for a derived (or
// root) incompatibility.
func (s *solveState) describe(inc *incompatibility) string {
	switch cause := inc.Cause.(type) {
	case causeDependency:
		return s.describeDependency(cause)
	case causeNoVersions:
		return fmt.Sprintf("no version of %s matches %s", cause.term.Package, cause.term.Set.display)
	case causeUnknownPackage:
		return cause.Package + " has no published versions"
	default:
		return s.describeGeneric(inc)
	}
}

// describeDependency renders "Parent[@Version] depends on Dep Constraint",
// omitting root's version per the reference algorithm's root special case.
func (s *solveState) describeDependency(c causeDependency) string {
	parentLabel := "root"
	if c.Parent != rootPkg {
		parentLabel = fmt.Sprintf("%s %s", c.Parent, c.ParentVersion.Original())
	}
	constraint := c.Constraint
	if constraint == "" {
		constraint = "*"
	}
	return fmt.Sprintf("%s depends on %s %s", parentLabel, c.Dep, constraint)
}

// dependencyPairTermCount is the term count that renders as "{depender}
// requires {dependency}" (one positive term for the depender's own version,
// one negative term for the forbidden dependency range).
const dependencyPairTermCount = 2

// describeGeneric renders a root or derived incompatibility: a negative term
// reads "X is forbidden", a positive/negative pair "A requires B", and any
// other shape joins each term's single-term phrasing.
func (s *solveState) describeGeneric(inc *incompatibility) string {
	switch len(inc.Terms) {
	case 0:
		return "version solving failed"
	case 1:
		return s.describeSingleTerm(inc.Terms[0])
	case dependencyPairTermCount:
		return s.describeTwoTerm(inc.Terms[0], inc.Terms[1])
	default:
		parts := make([]string, len(inc.Terms))
		for i, t := range inc.Terms {
			parts[i] = s.describeSingleTerm(t)
		}
		return strings.Join(parts, " and ")
	}
}

func (s *solveState) describeSingleTerm(t term) string {
	if t.Positive && t.Package == rootPkg {
		return "version solving failed"
	}
	label := s.termLabel(t.Package, t.Set)
	if t.Positive {
		return label + " is required"
	}
	return label + " is forbidden"
}

func (s *solveState) describeTwoTerm(a, b term) string {
	pos, neg := a, b
	if !pos.Positive {
		pos, neg = b, a
	}
	return fmt.Sprintf("%s requires %s", s.termLabel(pos.Package, pos.Set), s.termLabel(neg.Package, neg.Set))
}

// termLabel renders (pkg, set) as "root", "X <version>" for a singleton,
// "every version of X" when set covers the fetched universe, or the set's
// display label. The universe is consulted for display only, never logic.
func (s *solveState) termLabel(pkg string, set verSet) string {
	if pkg == rootPkg {
		return "root"
	}
	if v, ok := set.decidedVersion(); ok {
		return fmt.Sprintf("%s %s", pkg, v.Original())
	}
	if set.isFull() || s.coversFetchedUniverse(pkg, set) {
		return "every version of " + pkg
	}
	if label := set.displayLabel(); label != "" {
		return fmt.Sprintf("%s %s", pkg, label)
	}
	return pkg
}

// coversFetchedUniverse reports whether set admits every version of pkg's
// already-fetched universe, for the "every version of X" phrasing.
func (s *solveState) coversFetchedUniverse(pkg string, set verSet) bool {
	u := s.uniFor(pkg)
	if !u.fetched || len(u.versions) == 0 {
		return false
	}
	for _, v := range u.versions {
		if !set.contains(v) {
			return false
		}
	}
	return true
}
