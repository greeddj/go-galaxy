package solver

// Constraint-to-verSet construction. semver.NewConstraint alone decides what
// parses; the mirror parser then replicates the vendored v3.5.0 grammar, held
// to agreement with Check by TestVerSetDifferentialAgainstCheck.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errNonIntervalConstraint marks the one shape Masterminds accepts with no
// interval form: "!=" with a patch x-range and a prerelease operand excludes an
// infinite comb, and admitting one would break closure under complement.
var errNonIntervalConstraint = errors.New("solver: constraint has no exact interval representation")

// vbOps and vbCVRegex mirror the vendored Masterminds v3.5.0 operator and
// constraint-version grammars verbatim (constraints.go); the differential
// test suite is the drift enforcement.
const vbOps = `=||!=|>|<|>=|=>|<=|=<|~|~>|\^`

const vbCVRegex = `v?([0-9|x|X|\*]+)(\.[0-9|x|X|\*]+)?(\.[0-9|x|X|\*]+)?` +
	`(-([0-9A-Za-z\-]+(\.[0-9A-Za-z\-]+)*))?` +
	`(\+([0-9A-Za-z\-]+(\.[0-9A-Za-z\-]+)*))?`

var vbConstraintRegex = regexp.MustCompile(`^\s*(` + vbOps + `)\s*(` + vbCVRegex + `)\s*$`)

var vbFindRegex = regexp.MustCompile(`(` + vbOps + `)\s*(` + vbCVRegex + `)`)

var vbRangeRegex = regexp.MustCompile(`\s*(` + vbCVRegex + `)\s+-\s+(` + vbCVRegex + `)\s*`)

// comparator is one parsed token mirroring the vendored constraint struct: con
// is the zero-filled comparison version (metadata kept), orig the text after
// the operator, and the dirty flags mark x-ranged or omitted segments.
type comparator struct {
	con        *semver.Version
	op         string
	orig       string
	dirty      bool
	minorDirty bool
	patchDirty bool
}

// vbRewriteRange mirrors the vendored rewriteRange, turning every "A - B" into
// ">= A, <= B " before splitting. Groups 1 and 11 hold the two versions, since
// each cv contributes nine inner groups.
func vbRewriteRange(s string) string {
	m := vbRangeRegex.FindAllStringSubmatch(s, -1)
	if m == nil {
		return s
	}
	out := s
	for _, v := range m {
		t := fmt.Sprintf(">= %s, <= %s ", v[1], v[11])
		out = strings.Replace(out, v[0], t, 1)
	}
	return out
}

// vbIsX mirrors the vendored isX: an x-range segment marker.
func vbIsX(x string) bool {
	switch x {
	case "x", "*", "X":
		return true
	default:
		return false
	}
}

// parseComparator mirrors the vendored parseConstraint's submatch handling
// and dirty-flag substitutions for one operator+version token.
func parseComparator(token string) (comparator, error) {
	m := vbConstraintRegex.FindStringSubmatch(token)
	if m == nil {
		return comparator{}, fmt.Errorf("mirror parser rejected %q that semver.NewConstraint accepted: %w", token, errSolverBug)
	}
	c := comparator{op: m[1], orig: m[2]}
	ver := m[2]
	switch {
	case vbIsX(m[3]) || m[3] == "":
		ver = "0.0.0" + m[6]
		c.dirty = true
	case vbIsX(strings.TrimPrefix(m[4], ".")) || m[4] == "":
		c.minorDirty = true
		c.dirty = true
		ver = m[3] + ".0.0" + m[6]
	case vbIsX(strings.TrimPrefix(m[5], ".")) || m[5] == "":
		c.patchDirty = true
		c.dirty = true
		ver = m[3] + m[4] + ".0" + m[6]
	}
	con, err := semver.NewVersion(ver)
	if err != nil {
		return comparator{}, fmt.Errorf("mirror parser failed on comparison version %q from %q: %w", ver, token, errSolverBug)
	}
	c.con = con
	return c, nil
}

// parseSegment extracts every comparator of one AND segment, in order.
func parseSegment(segment string) ([]comparator, error) {
	tokens := vbFindRegex.FindAllString(segment, -1)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("mirror parser found no comparators in %q that semver.NewConstraint accepted: %w", segment, errSolverBug)
	}
	out := make([]comparator, 0, len(tokens))
	for _, tok := range tokens {
		c, err := parseComparator(tok)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// newVerSet builds the exact set a constraint denotes. The unconstrained form
// ("" or "*") is fullVerSet, prereleases included, deliberately unlike
// Masterminds' closed-gate reading of "*".
func newVerSet(raw string) (verSet, error) {
	normalized := helpers.NormalizeConstraint(raw)
	if normalized == "" {
		return fullVerSet(), nil
	}
	if _, err := semver.NewConstraint(normalized); err != nil {
		return verSet{}, fmt.Errorf("invalid version constraint %q: %w", raw, err)
	}

	segments := strings.Split(vbRewriteRange(normalized), "||")
	out := emptyVerSet()
	var soleGroup []comparator
	for _, seg := range segments {
		comps, err := parseSegment(seg)
		if err != nil {
			return verSet{}, err
		}
		if len(segments) == 1 {
			soleGroup = comps
		}
		group, err := buildGroup(comps)
		if err != nil {
			return verSet{}, fmt.Errorf("invalid version constraint %q: %w", raw, err)
		}
		out = out.union(group)
	}

	out.display = normalized
	attachSingleton(&out, soleGroup)
	return out, nil
}

// attachSingleton records the pinned version on a set built from one exact
// comparator ("=X" or bare), so exact-pin decisions recover the original
// spelling; comps is nil when the expression had several OR groups.
func attachSingleton(s *verSet, comps []comparator) {
	if len(comps) != 1 || comps[0].dirty {
		return
	}
	if op := comps[0].op; op != "" && op != "=" {
		return
	}
	s.single = Version{parsed: comps[0].con, original: comps[0].orig}
	s.hasSingle = true
}

// singletonVerSet returns the set denoting exactly v's precedence point,
// carrying v itself for decision extraction.
func singletonVerSet(v Version) verSet {
	s := comparatorPoint(stripMeta(v.sv()))
	s.single = v
	s.hasSingle = true
	s.display = v.Original()
	return s
}

// buildGroup intersects one AND group's comparators, then applies the vendored
// prerelease gate: a group with no prerelease operand admits no prerelease.
func buildGroup(comps []comparator) (verSet, error) {
	hasPre := false
	acc := fullVerSet()
	for _, c := range comps {
		if c.con.Prerelease() != "" {
			hasPre = true
		}
		vs, err := buildComparator(c)
		if err != nil {
			return verSet{}, err
		}
		acc = acc.intersect(vs)
	}
	if !hasPre {
		acc.pre = nil
	}
	return acc, nil
}

// buildComparator maps one comparator to its gate-open exact set (the
// group gate is applied by buildGroup afterward). Operator aliases collapse
// exactly as the vendored operator table does.
func buildComparator(c comparator) (verSet, error) {
	switch c.op {
	case ">":
		return buildGreater(c), nil
	case ">=", "=>":
		return buildGreaterEqual(c), nil
	case "<":
		return buildLess(c), nil
	case "<=", "=<":
		return buildLessEqual(c), nil
	case "~", "~>":
		return buildTilde(c), nil
	case "", "=":
		return buildEqual(c), nil
	case "^":
		return buildCaret(c), nil
	case "!=":
		return buildNotEqual(c)
	default:
		return verSet{}, fmt.Errorf("mirror parser saw unknown operator %q: %w", c.op, errSolverBug)
	}
}

// fullDirty reports the fully wildcarded form ("*", "x": major itself is an
// x-range), which the vendored comparators mostly treat as its zero-filled
// comparison version.
func (c comparator) fullDirty() bool {
	return c.dirty && !c.minorDirty && !c.patchDirty
}

// buildGreaterEqual: every dirty form is ignored by the vendored ">=" (it
// compares against the zero-filled version directly), so a single rule
// serves all shapes: everything at or above con on both sublines.
func buildGreaterEqual(c comparator) verSet {
	preLo, preOK := preCeil(c.con)
	return verSet{
		rel: relFrom(relOf(c.con), true),
		pre: preFrom(preLo, preOK),
	}
}

// buildGreater: plain and fully wildcarded forms compare strictly against con;
// a minor x-range needs the next major row, a patch x-range the next minor row,
// and a prerelease operand plays no part in either x-range branch.
func buildGreater(c comparator) verSet {
	switch {
	case c.minorDirty:
		return rowFrom(nextMajor(relOf(c.con)))
	case c.patchDirty:
		return rowFrom(nextMinor(relOf(c.con)))
	default:
		relLo, relOK := relCeilStrict(c.con)
		preLo, preOK := preCeilStrict(c.con)
		return verSet{rel: relFrom(relLo, relOK), pre: preFrom(preLo, preOK)}
	}
}

// rowFrom returns everything from the given release row's start upward, on
// both sublines.
func rowFrom(lo relBound, ok bool) verSet {
	if !ok {
		return emptyVerSet()
	}
	return verSet{rel: relFrom(lo, true), pre: preFrom(preFloor(lo), true)}
}

// rowUpto returns everything strictly below the given release row's start,
// on both sublines.
func rowUpto(hi relBound, ok bool) verSet {
	return verSet{rel: relUpto(hi, ok), pre: preUptoFloor(hi, ok)}
}

// preUptoFloor is preUpto against the floor prerelease of hi's row.
func preUptoFloor(hi relBound, ok bool) []piece[*semver.Version] {
	if !ok {
		return preFrom(preMinBound, true)
	}
	return preUpto(preFloor(hi), true)
}

// buildLess ignores every dirty flag, as the vendored "<" does: releases below
// con's triple and prereleases below the smallest prerelease at or above con,
// which admits all of a release operand's own prereleases.
func buildLess(c comparator) verSet {
	preHi, preOK := preCeil(c.con)
	return verSet{
		rel: relUpto(relOf(c.con), true),
		pre: preUpto(preHi, preOK),
	}
}

// buildLessEqual: the non-dirty form is a precedence comparison; x-range
// forms admit everything up to the end of the named row, and the fully
// wildcarded form is the vendored quirk that admits only the 0.0 row.
func buildLessEqual(c comparator) verSet {
	t := relOf(c.con)
	switch {
	case c.minorDirty:
		return rowUpto(nextMajor(t))
	case c.patchDirty:
		return rowUpto(nextMinor(t))
	case c.fullDirty():
		return rowUpto(relBound{minor: 1}, true)
	case c.con.Prerelease() != "":
		return verSet{
			rel: relUpto(t, true),
			pre: preUpto(succPre(stripMeta(c.con)), true),
		}
	default:
		next, ok := succRel(t)
		return verSet{rel: relUpto(next, ok), pre: preUptoFloor(next, ok)}
	}
}

// buildTilde: a 0.0.0 comparison version with neither minor nor patch x-ranged
// ("~0.0.0", "~*") degenerates to ">= con", as vendored; a minor x-range spans
// con's major row, everything else con's minor row.
func buildTilde(c comparator) verSet {
	t := relOf(c.con)
	if t == (relBound{}) && !c.minorDirty && !c.patchDirty {
		return buildGreaterEqual(c)
	}
	hi, hiOK := nextMinor(t)
	if c.minorDirty {
		hi, hiOK = nextMajor(t)
	}
	preLo, preOK := preCeil(c.con)
	return verSet{
		rel: relRange(t, hi, hiOK),
		pre: preRange(preLo, preOK, preFloor(hi), hiOK),
	}
}

// buildEqual: an x-ranged "=" (or bare version) opts into tilde, exactly as
// the vendored constraintTildeOrEqual does; otherwise it is the single
// precedence point of con.
func buildEqual(c comparator) verSet {
	if c.dirty {
		return buildTilde(c)
	}
	return comparatorPoint(stripMeta(c.con))
}

// comparatorPoint returns the set holding exactly con's precedence point:
// a release triple's point cell, or a prerelease's [con, succ(con)) run.
func comparatorPoint(con *semver.Version) verSet {
	if con.Prerelease() == "" {
		return verSet{rel: relPoint(relOf(con))}
	}
	return verSet{pre: preRange(con, true, succPre(con), true)}
}

// buildCaret follows the vendored branch order: a positive major or a minor
// x-range spans the major row, else a positive minor or a patch x-range the
// minor row, else (as for "^*") only an exact match of con's triple from con up.
func buildCaret(c comparator) verSet {
	t := relOf(c.con)
	preLo, preOK := preCeil(c.con)
	switch {
	case t.major > 0 || c.minorDirty:
		hi, hiOK := nextMajor(t)
		return verSet{rel: relRange(t, hi, hiOK), pre: preRange(preLo, preOK, preFloor(hi), hiOK)}
	case t.minor > 0 || c.patchDirty:
		hi, hiOK := nextMinor(t)
		return verSet{rel: relRange(t, hi, hiOK), pre: preRange(preLo, preOK, preFloor(hi), hiOK)}
	case c.con.Prerelease() != "":
		next, ok := succRel(t)
		return verSet{
			rel: relPoint(t),
			pre: preRange(stripMeta(c.con), true, preFloor(next), ok),
		}
	default:
		return verSet{rel: relPoint(t)}
	}
}

// buildNotEqual: plain and fully wildcarded forms exclude con's point, a minor
// x-range con's whole major row, a patch x-range only the releases of con's
// minor row; a patch x-range with a prerelease operand is refused as a comb.
func buildNotEqual(c comparator) (verSet, error) {
	t := relOf(c.con)
	switch {
	case c.minorDirty:
		row := relBound{major: t.major}
		return rowUpto(row, true).union(rowFrom(nextMajor(t))), nil
	case c.patchDirty:
		if c.con.Prerelease() != "" {
			return verSet{}, fmt.Errorf("%q excludes an infinite prerelease comb: %w", "!="+c.orig, errNonIntervalConstraint)
		}
		row := relBound{major: t.major, minor: t.minor}
		hi, hiOK := nextMinor(t)
		excluded := verSet{rel: relRange(row, hi, hiOK)}
		return fullVerSet().difference(excluded), nil
	default:
		return fullVerSet().difference(comparatorPoint(stripMeta(c.con))), nil
	}
}
