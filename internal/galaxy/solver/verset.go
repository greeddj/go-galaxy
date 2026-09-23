package solver

// verSet is an exact, universe-independent version set over two sublines
// (releases by triple, prereleases by precedence), kept canonical so structural
// equality is set equality. Values are immutable: operations return fresh sets.

import (
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// piece is one half-open run [lo, hi) on a totally ordered subline; hiInf
// marks the run as unbounded above, in which case hi is meaningless. A
// stored piece is always nonempty (lo < hi, or hiInf).
type piece[B any] struct {
	lo    B
	hi    B
	hiInf bool
}

// relBound is a release-subline bound: a bare (major, minor, patch) triple.
// The release order needs no prerelease or metadata parts at all.
type relBound struct {
	major uint64
	minor uint64
	patch uint64
}

// cmpRel is the release-subline order: lexicographic on the triple.
func cmpRel(a, b relBound) int {
	switch {
	case a.major != b.major:
		return cmpUint64(a.major, b.major)
	case a.minor != b.minor:
		return cmpUint64(a.minor, b.minor)
	default:
		return cmpUint64(a.patch, b.patch)
	}
}

func cmpUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// cmpPre is the prerelease-subline order: full semver precedence (bounds
// carry no metadata, so this is exactly the subline's total order).
func cmpPre(a, b *semver.Version) int {
	return a.Compare(b)
}

// preMinBound is the minimum of the prerelease subline: 0.0.0-0, the single
// numeric zero identifier, which no prerelease version sorts below.
//
//nolint:gochecknoglobals // a canonical, immutable bound of the algebra itself, not mutable shared state
var preMinBound = semver.New(0, 0, 0, "0", "")

// ---- generic piece algebra --------------------------------------------------
// Every operation takes and returns canonical lists (sorted, nonempty, disjoint,
// non-abutting); union is derived from the other two via De Morgan.

// intersectPieces returns the canonical intersection of a and b.
func intersectPieces[B any](a, b []piece[B], cmp func(B, B) int) []piece[B] {
	var out []piece[B]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo := a[i].lo
		if cmp(b[j].lo, lo) > 0 {
			lo = b[j].lo
		}
		hi, hiInf := minHi(a[i], b[j], cmp)
		if hiInf || cmp(lo, hi) < 0 {
			out = append(out, piece[B]{lo: lo, hi: hi, hiInf: hiInf})
		}
		if a[i].hiInf && b[j].hiInf {
			break
		}
		if advanceFirst(a[i], b[j], cmp) {
			i++
		} else {
			j++
		}
	}
	return out
}

// minHi returns the smaller of two pieces' upper bounds, honoring hiInf.
func minHi[B any](a, b piece[B], cmp func(B, B) int) (B, bool) {
	switch {
	case a.hiInf && b.hiInf:
		var zero B
		return zero, true
	case a.hiInf:
		return b.hi, false
	case b.hiInf:
		return a.hi, false
	case cmp(a.hi, b.hi) <= 0:
		return a.hi, false
	default:
		return b.hi, false
	}
}

// advanceFirst reports whether a is the piece with the smaller upper bound
// (the one an intersection sweep consumes first); ties advance a.
func advanceFirst[B any](a, b piece[B], cmp func(B, B) int) bool {
	if a.hiInf {
		return false
	}
	if b.hiInf {
		return true
	}
	return cmp(a.hi, b.hi) <= 0
}

// complementPieces returns the canonical complement of a within the subline
// starting at minB.
func complementPieces[B any](a []piece[B], minB B, cmp func(B, B) int) []piece[B] {
	var out []piece[B]
	cur := minB
	for _, p := range a {
		if cmp(cur, p.lo) < 0 {
			out = append(out, piece[B]{lo: cur, hi: p.lo})
		}
		if p.hiInf {
			return out
		}
		cur = p.hi
	}
	return append(out, piece[B]{lo: cur, hiInf: true})
}

// unionPieces returns the canonical union of a and b, via De Morgan over
// the subline starting at minB.
func unionPieces[B any](a, b []piece[B], minB B, cmp func(B, B) int) []piece[B] {
	ca := complementPieces(a, minB, cmp)
	cb := complementPieces(b, minB, cmp)
	return complementPieces(intersectPieces(ca, cb, cmp), minB, cmp)
}

// subsetPieces reports whether every point of a lies in b (a is a subset of
// b), without allocating.
func subsetPieces[B any](a, b []piece[B], cmp func(B, B) int) bool {
	j := 0
	for _, pa := range a {
		for j < len(b) && !b[j].hiInf && cmp(b[j].hi, pa.lo) <= 0 {
			j++
		}
		if j == len(b) || cmp(b[j].lo, pa.lo) > 0 {
			return false
		}
		if b[j].hiInf {
			continue
		}
		if pa.hiInf || cmp(pa.hi, b[j].hi) > 0 {
			return false
		}
	}
	return true
}

// disjointPieces reports whether a and b share no point, without allocating.
func disjointPieces[B any](a, b []piece[B], cmp func(B, B) int) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo := a[i].lo
		if cmp(b[j].lo, lo) > 0 {
			lo = b[j].lo
		}
		if hi, hiInf := minHi(a[i], b[j], cmp); hiInf || cmp(lo, hi) < 0 {
			return false
		}
		if a[i].hiInf && b[j].hiInf {
			break
		}
		if advanceFirst(a[i], b[j], cmp) {
			i++
		} else {
			j++
		}
	}
	return true
}

// equalPieces reports structural equality, which on canonical lists is set
// equality.
func equalPieces[B any](a, b []piece[B], cmp func(B, B) int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if cmp(a[i].lo, b[i].lo) != 0 || a[i].hiInf != b[i].hiInf {
			return false
		}
		if !a[i].hiInf && cmp(a[i].hi, b[i].hi) != 0 {
			return false
		}
	}
	return true
}

// containsPoint reports whether x lies in the canonical list a.
func containsPoint[B any](a []piece[B], x B, cmp func(B, B) int) bool {
	for _, p := range a {
		if cmp(x, p.lo) < 0 {
			return false
		}
		if p.hiInf || cmp(x, p.hi) < 0 {
			return true
		}
	}
	return false
}

// ---- the verSet value -------------------------------------------------------

// verSet is an exact set of versions over the rel and pre sublines. single
// keeps a singleton's original registry spelling for decisions and exact pins;
// display is a cosmetic label no logic decision may branch on.
type verSet struct {
	single    Version
	display   string
	rel       []piece[relBound]
	pre       []piece[*semver.Version]
	hasSingle bool
}

// emptyVerSet returns the set containing no versions.
func emptyVerSet() verSet {
	return verSet{}
}

// fullVerSet returns every version, prereleases included, unlike Masterminds'
// "*": unconstrained is vacuous truth, and dropping prereleases would wrongly
// contradict a prerelease decision.
func fullVerSet() verSet {
	return verSet{
		rel:     []piece[relBound]{{lo: relBound{}, hiInf: true}},
		pre:     []piece[*semver.Version]{{lo: preMinBound, hiInf: true}},
		display: "any",
	}
}

// contains reports whether v is a member of s, by dispatching v to its
// subline. Metadata never participates: cmpPre delegates to semver's
// Compare, and the release triple carries no metadata at all.
func (s verSet) contains(v Version) bool {
	if v.sv().Prerelease() == "" {
		return containsPoint(s.rel, relOf(v.sv()), cmpRel)
	}
	return containsPoint(s.pre, v.sv(), cmpPre)
}

func (s verSet) intersect(o verSet) verSet {
	return verSet{
		rel: intersectPieces(s.rel, o.rel, cmpRel),
		pre: intersectPieces(s.pre, o.pre, cmpPre),
	}
}

func (s verSet) union(o verSet) verSet {
	return verSet{
		rel: unionPieces(s.rel, o.rel, relBound{}, cmpRel),
		pre: unionPieces(s.pre, o.pre, preMinBound, cmpPre),
	}
}

func (s verSet) complement() verSet {
	return verSet{
		rel: complementPieces(s.rel, relBound{}, cmpRel),
		pre: complementPieces(s.pre, preMinBound, cmpPre),
	}
}

// difference returns s minus o.
func (s verSet) difference(o verSet) verSet {
	return s.intersect(o.complement())
}

func (s verSet) isEmpty() bool {
	return len(s.rel) == 0 && len(s.pre) == 0
}

// isFull reports whether s is the whole version space (both sublines run
// from their minimum to infinity).
func (s verSet) isFull() bool {
	return len(s.rel) == 1 && s.rel[0].hiInf && s.rel[0].lo == relBound{} &&
		len(s.pre) == 1 && s.pre[0].hiInf && cmpPre(s.pre[0].lo, preMinBound) == 0
}

func (s verSet) subsetOf(o verSet) bool {
	return subsetPieces(s.rel, o.rel, cmpRel) && subsetPieces(s.pre, o.pre, cmpPre)
}

func (s verSet) disjointFrom(o verSet) bool {
	return disjointPieces(s.rel, o.rel, cmpRel) && disjointPieces(s.pre, o.pre, cmpPre)
}

// equalSet reports set equality (canonical lists make this structural).
// single and display are cosmetic carriers and deliberately not compared.
func (s verSet) equalSet(o verSet) bool {
	return equalPieces(s.rel, o.rel, cmpRel) && equalPieces(s.pre, o.pre, cmpPre)
}

// decidedVersion returns the concrete version a singleton-built set carries.
func (s verSet) decidedVersion() (Version, bool) {
	return s.single, s.hasSingle
}

// writeCanonical writes s's injective encoding: equal sets, and only those,
// produce identical bytes, which incompatibility hashing relies on.
func (s verSet) writeCanonical(w io.Writer) {
	for _, p := range s.rel {
		_, _ = fmt.Fprintf(w, "R%d.%d.%d,", p.lo.major, p.lo.minor, p.lo.patch)
		if p.hiInf {
			_, _ = io.WriteString(w, "inf;")
		} else {
			_, _ = fmt.Fprintf(w, "%d.%d.%d;", p.hi.major, p.hi.minor, p.hi.patch)
		}
	}
	for _, p := range s.pre {
		_, _ = fmt.Fprintf(w, "P%s,", p.lo.String())
		if p.hiInf {
			_, _ = io.WriteString(w, "inf;")
		} else {
			_, _ = fmt.Fprintf(w, "%s;", p.hi.String())
		}
	}
}

// ---- bound arithmetic -------------------------------------------------------
// An overflowed successor is exact, not an approximation: as a lower bound it
// makes the run empty, as an upper bound it makes the run unbounded.

// relOf returns v's release-subline bound (its bare triple).
func relOf(v *semver.Version) relBound {
	return relBound{major: v.Major(), minor: v.Minor(), patch: v.Patch()}
}

// succRel returns the release immediately after b; ok is false when the
// patch segment overflows (no release lies above b).
func succRel(b relBound) (relBound, bool) {
	if b.patch == math.MaxUint64 {
		return relBound{}, false
	}
	return relBound{major: b.major, minor: b.minor, patch: b.patch + 1}, true
}

// nextMinor returns the first release of the minor row after b's.
func nextMinor(b relBound) (relBound, bool) {
	if b.minor == math.MaxUint64 {
		return relBound{}, false
	}
	return relBound{major: b.major, minor: b.minor + 1}, true
}

// nextMajor returns the first release of the major row after b's.
func nextMajor(b relBound) (relBound, bool) {
	if b.major == math.MaxUint64 {
		return relBound{}, false
	}
	return relBound{major: b.major + 1}, true
}

// preFloor returns the minimal prerelease of triple b: b's triple with the
// single numeric zero identifier, which every other prerelease of that
// triple sorts above and every prerelease of a smaller triple sorts below.
func preFloor(b relBound) *semver.Version {
	return semver.New(b.major, b.minor, b.patch, "0", "")
}

// succPre returns the prerelease immediately after w: w with ".0" appended.
// Nothing sorts strictly between, since "0" is the minimal identifier and a
// shorter prerelease precedes every extension of itself.
func succPre(w *semver.Version) *semver.Version {
	return semver.New(w.Major(), w.Minor(), w.Patch(), w.Prerelease()+".0", "")
}

// stripMeta returns w without build metadata, the canonical bound form.
func stripMeta(w *semver.Version) *semver.Version {
	return semver.New(w.Major(), w.Minor(), w.Patch(), w.Prerelease(), "")
}

// relCeilStrict returns the smallest release strictly above con: con's own
// triple when con is a prerelease (the release of a triple sorts above all
// its prereleases), the next triple otherwise.
func relCeilStrict(con *semver.Version) (relBound, bool) {
	if con.Prerelease() != "" {
		return relOf(con), true
	}
	return succRel(relOf(con))
}

// preCeil returns the smallest prerelease at or above con: con itself when
// con is a prerelease, the floor of the next triple otherwise (a release's
// own prereleases all sort below it).
func preCeil(con *semver.Version) (*semver.Version, bool) {
	if con.Prerelease() != "" {
		return stripMeta(con), true
	}
	next, ok := succRel(relOf(con))
	if !ok {
		return nil, false
	}
	return preFloor(next), true
}

// preCeilStrict returns the smallest prerelease strictly above con.
func preCeilStrict(con *semver.Version) (*semver.Version, bool) {
	if con.Prerelease() != "" {
		return succPre(stripMeta(con)), true
	}
	return preCeil(con)
}

// ---- piece constructors -----------------------------------------------------
// Each returns a canonical, possibly empty list; a false ok flag on a lower
// bound means an empty run, on an upper bound an unbounded one.

func relFrom(lo relBound, ok bool) []piece[relBound] {
	if !ok {
		return nil
	}
	return []piece[relBound]{{lo: lo, hiInf: true}}
}

func relUpto(hi relBound, ok bool) []piece[relBound] {
	if !ok {
		return relFrom(relBound{}, true)
	}
	if cmpRel(relBound{}, hi) >= 0 {
		return nil
	}
	return []piece[relBound]{{lo: relBound{}, hi: hi}}
}

func relRange(lo relBound, hi relBound, hiOK bool) []piece[relBound] {
	if !hiOK {
		return relFrom(lo, true)
	}
	if cmpRel(lo, hi) >= 0 {
		return nil
	}
	return []piece[relBound]{{lo: lo, hi: hi}}
}

// relPoint returns the single-triple run [t, succ(t)).
func relPoint(t relBound) []piece[relBound] {
	next, ok := succRel(t)
	return relRange(t, next, ok)
}

func preFrom(lo *semver.Version, ok bool) []piece[*semver.Version] {
	if !ok {
		return nil
	}
	return []piece[*semver.Version]{{lo: lo, hiInf: true}}
}

func preUpto(hi *semver.Version, ok bool) []piece[*semver.Version] {
	if !ok {
		return preFrom(preMinBound, true)
	}
	if cmpPre(preMinBound, hi) >= 0 {
		return nil
	}
	return []piece[*semver.Version]{{lo: preMinBound, hi: hi}}
}

func preRange(lo *semver.Version, loOK bool, hi *semver.Version, hiOK bool) []piece[*semver.Version] {
	if !loOK {
		return nil
	}
	if !hiOK {
		return preFrom(lo, true)
	}
	if cmpPre(lo, hi) >= 0 {
		return nil
	}
	return []piece[*semver.Version]{{lo: lo, hi: hi}}
}

// ---- display rendering ------------------------------------------------------

// displayLabel returns s's human-readable label: the stored display when
// one was attached at construction, otherwise a rendering of the pieces.
// Cosmetic only; never branched on for any logic decision.
func (s verSet) displayLabel() string {
	if s.display != "" {
		return s.display
	}
	switch {
	case s.isEmpty():
		return "no versions"
	case s.isFull():
		return "any"
	}
	parts := make([]string, 0, len(s.rel))
	for _, p := range s.rel {
		parts = append(parts, renderRelPiece(p))
	}
	label := strings.Join(parts, " || ")
	if pre := renderPrePieces(s.pre); pre != "" {
		if label == "" {
			return pre
		}
		label += " " + pre
	}
	return label
}

// renderRelPiece renders one release run in constraint-like notation.
func renderRelPiece(p piece[relBound]) string {
	lo := fmt.Sprintf("%d.%d.%d", p.lo.major, p.lo.minor, p.lo.patch)
	if next, ok := succRel(p.lo); ok && !p.hiInf && cmpRel(next, p.hi) == 0 {
		return "=" + lo
	}
	if p.hiInf {
		return ">=" + lo
	}
	hi := fmt.Sprintf("%d.%d.%d", p.hi.major, p.hi.minor, p.hi.patch)
	if (p.lo == relBound{}) {
		return "<" + hi
	}
	return ">=" + lo + ",<" + hi
}

// renderPrePieces renders the prerelease runs, or "" when there are none.
// A full prerelease subline is summarized rather than spelled out.
func renderPrePieces(pre []piece[*semver.Version]) string {
	if len(pre) == 0 {
		return ""
	}
	if len(pre) == 1 && pre[0].hiInf && cmpPre(pre[0].lo, preMinBound) == 0 {
		return "(all prereleases)"
	}
	parts := make([]string, 0, len(pre))
	for _, p := range pre {
		if p.hiInf {
			parts = append(parts, ">="+p.lo.String())
			continue
		}
		parts = append(parts, ">="+p.lo.String()+",<"+p.hi.String())
	}
	return "(prereleases " + strings.Join(parts, " || ") + ")"
}
