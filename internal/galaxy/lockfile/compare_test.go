package lockfile

// Tests for Compare, Diff and Change: Empty agrees with Hash equality, order
// never matters, inputs are never mutated, duplicate deps are significant and
// hostile values pass through verbatim.

import (
	"strings"
	"testing"
)

// Fixed test digests, 64 hex characters like a real sha256 without needing
// to actually be valid hex (Compare never validates digest shape).
const (
	sha1 = "11111111111111111111111111111111111111111111111111111111111111aa"
	sha2 = "22222222222222222222222222222222222222222222222222222222222222bb"
)

// emptyMatchesHashEqualityCase is one row of
// TestCompareEmptyMatchesHashEquality's table.
type emptyMatchesHashEqualityCase struct {
	before    *File
	after     *File
	name      string
	wantEmpty bool
}

// eqWidgets and eqLegacy are the two entries every row of
// TestCompareEmptyMatchesHashEquality starts from; eqFile builds a *File.
func eqWidgets() Entry {
	return Entry{
		Name: "acme.widgets", Version: "1.0.0", Source: "https://galaxy.example",
		SHA256: sha1, Deps: []string{"acme.dep1", "acme.dep2"},
	}
}

func eqLegacy() Entry {
	return Entry{Name: "acme.legacy", Version: "2.0.0", Source: "https://galaxy.example", SHA256: sha2}
}

func eqFile(server string, entries ...Entry) *File {
	return &File{SchemaVersion: SchemaVersion, Server: server, Collections: entries}
}

func withVersion(e Entry, v string) Entry { e.Version = v; return e }
func withSource(e Entry, s string) Entry  { e.Source = s; return e }
func withSHA(e Entry, s string) Entry     { e.SHA256 = s; return e }
func withDeps(e Entry, d []string) Entry  { e.Deps = d; return e }

// samePayloadCases are the table's positive controls, which must report
// Empty: identical files, reordered collections and reordered deps.
func samePayloadCases() []emptyMatchesHashEqualityCase {
	return []emptyMatchesHashEqualityCase{
		{
			name:      "identical",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			wantEmpty: true,
		},
		{
			name:      "reordered_collections",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", eqLegacy(), eqWidgets()),
			wantEmpty: true,
		},
		{
			name:      "reordered_deps",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withDeps(eqWidgets(), []string{"acme.dep2", "acme.dep1"}), eqLegacy()),
			wantEmpty: true,
		},
	}
}

// fieldChangeCases each change one field of acme.widgets (version, source,
// sha256, or deps changed or dropped) and must report a difference.
func fieldChangeCases() []emptyMatchesHashEqualityCase {
	return []emptyMatchesHashEqualityCase{
		{
			name:      "version_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withVersion(eqWidgets(), "1.1.0"), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "source_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withSource(eqWidgets(), "https://other.example"), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "sha_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withSHA(eqWidgets(), sha2), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "deps_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withDeps(eqWidgets(), []string{"acme.dep1", "acme.dep3"}), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "deps_dropped",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", withDeps(eqWidgets(), nil), eqLegacy()),
			wantEmpty: false,
		},
	}
}

// structuralChangeCases covers a change in what the file names, not just in
// one entry's pinned fields: an added or removed collection, or a changed or
// cleared file-level Server. Each must report Empty()==false.
func structuralChangeCases() []emptyMatchesHashEqualityCase {
	return []emptyMatchesHashEqualityCase{
		{
			name:   "entry_added",
			before: eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after: eqFile("https://galaxy.example", eqWidgets(), eqLegacy(),
				Entry{Name: "acme.extra", Version: "1.0.0", Source: "https://galaxy.example"}),
			wantEmpty: false,
		},
		{
			name:      "entry_removed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://galaxy.example", eqWidgets()),
			wantEmpty: false,
		},
		{
			name:      "server_changed",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("https://other-server.example", eqWidgets(), eqLegacy()),
			wantEmpty: false,
		},
		{
			name:      "server_cleared",
			before:    eqFile("https://galaxy.example", eqWidgets(), eqLegacy()),
			after:     eqFile("", eqWidgets(), eqLegacy()),
			wantEmpty: false,
		},
	}
}

// emptyMatchesHashEqualityCases concatenates the 12-row table
// TestCompareEmptyMatchesHashEquality checks, from its three topic-grouped
// halves.
func emptyMatchesHashEqualityCases() []emptyMatchesHashEqualityCase {
	cases := samePayloadCases()
	cases = append(cases, fieldChangeCases()...)
	cases = append(cases, structuralChangeCases()...)
	return cases
}

// TestCompareEmptyMatchesHashEquality pins Compare's central invariant: for
// two non-nil files, Compare(before, after).Empty() agrees with Hash equality,
// with three rows as positive controls expecting Empty.
func TestCompareEmptyMatchesHashEquality(t *testing.T) {
	t.Parallel()
	for _, tc := range emptyMatchesHashEqualityCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkEmptyMatchesHashEquality(t, tc)
		})
	}
}

// checkEmptyMatchesHashEquality runs one row, checking the Hash invariant
// before the row's wantEmpty so a Compare regression reports as such; a
// wantEmpty mismatch alone means a bad fixture row.
func checkEmptyMatchesHashEquality(t *testing.T, tc emptyMatchesHashEqualityCase) {
	t.Helper()
	diff := Compare(tc.before, tc.after)
	beforeHash, err := tc.before.Hash()
	if err != nil {
		t.Fatalf("before.Hash: %v", err)
	}
	afterHash, err := tc.after.Hash()
	if err != nil {
		t.Fatalf("after.Hash: %v", err)
	}
	if diff.Empty() != (beforeHash == afterHash) {
		t.Fatalf("Compare.Empty()=%v but hash equality=%v; diff=%+v", diff.Empty(), beforeHash == afterHash, diff)
	}
	if diff.Empty() != tc.wantEmpty {
		t.Fatalf("Compare(...).Empty() = %v, want %v; diff=%+v", diff.Empty(), tc.wantEmpty, diff)
	}
}

// TestCompareDepsOnlyDifferenceIsUpdated pins that a deps-only difference is
// Updated: a --frozen install rebuilds its graph from Deps, so they are drift.
func TestCompareDepsOnlyDifferenceIsUpdated(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://galaxy.example", SHA256: sha1, Deps: []string{"acme.dep1"}},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://galaxy.example", SHA256: sha1, Deps: []string{"acme.dep1", "acme.dep2"}},
	}}

	diff := Compare(before, after)
	if len(diff.Updated) != 1 {
		t.Fatalf("deps-only difference reported as no change: diff=%+v", diff)
	}
	fields := diff.Updated[0].Fields()
	if len(fields) != 1 || fields[0].Field != fieldDeps {
		t.Fatalf("Fields() = %+v, want exactly one deps field change", fields)
	}
}

// TestCompareIgnoresOrderAndDoesNotMutate pins that (1) files differing only
// in collection and dep order compare equal and (2) Compare reaches that
// verdict without sorting either input in place.
func TestCompareIgnoresOrderAndDoesNotMutate(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Deps: []string{"acme.dep1", "acme.dep2"}},
		{Name: "acme.legacy", Version: "2.0.0"},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.legacy", Version: "2.0.0"},
		{Name: "acme.widgets", Version: "1.0.0", Deps: []string{"acme.dep2", "acme.dep1"}},
	}}
	beforeNames, beforeDeps := collectionNames(before), collectionDeps(before)
	afterNames, afterDeps := collectionNames(after), collectionDeps(after)

	diff := Compare(before, after)
	if !diff.Empty() { // assertion (1)
		t.Fatalf("reordered but identical files reported as changed: diff=%+v", diff)
	}

	if got := collectionNames(before); !equalStrings(got, beforeNames) { // assertion (2)
		t.Fatalf("Compare mutated before: collections order changed from %v to %v", beforeNames, got)
	}
	if got := collectionNames(after); !equalStrings(got, afterNames) {
		t.Fatalf("Compare mutated after: collections order changed from %v to %v", afterNames, got)
	}
	if got := collectionDeps(before); !equalDeps(got, beforeDeps) {
		t.Fatalf("Compare mutated before: deps order changed from %v to %v", beforeDeps, got)
	}
	if got := collectionDeps(after); !equalDeps(got, afterDeps) {
		t.Fatalf("Compare mutated after: deps order changed from %v to %v", afterDeps, got)
	}
}

// TestCompareNilBaselineIsAllAdded pins that a nil side reports every entry
// Added or Removed, sorted by name (f is built in reverse order to prove the
// sort), and that Compare(nil, nil) is empty.
func TestCompareNilBaselineIsAllAdded(t *testing.T) {
	t.Parallel()
	f := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.zebra", Version: "1.0.0"},
		{Name: "acme.mid", Version: "1.0.0"},
		{Name: "acme.alpha", Version: "1.0.0"},
	}}
	wantNames := []string{"acme.alpha", "acme.mid", "acme.zebra"}

	added := Compare(nil, f)
	assertSortedEntryNames(t, added.Added, wantNames, "Compare(nil, f).Added")
	if len(added.Updated) != 0 || len(added.Removed) != 0 || added.Server != nil {
		t.Fatalf("Compare(nil, f) reported more than Added: %+v", added)
	}

	if empty := Compare(nil, nil); !empty.Empty() {
		t.Fatalf("Compare(nil, nil) = %+v, want empty", empty)
	}

	removed := Compare(f, nil)
	assertSortedEntryNames(t, removed.Removed, wantNames, "Compare(f, nil).Removed")
}

// assertSortedEntryNames fails the test unless got's entries are named want,
// in order.
func assertSortedEntryNames(t *testing.T, got []Entry, want []string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %+v, want %d entries", label, got, len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("%s[%d].Name = %q, want %q (sorted by name)", label, i, got[i].Name, name)
		}
	}
}

// TestCompareFieldsReportsEveryChangedField changes four fields at once and
// pins Fields' fixed order and raw values, a cleared Deps rendering as ""
// (the collections package substitutes "(none)" for display).
func TestCompareFieldsReportsEveryChangedField(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{
			Name: "acme.widgets", Version: "1.0.0", Source: "https://old.example",
			SHA256: sha1, Deps: []string{"acme.dep1", "acme.dep2"},
		},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.1.0", Source: "https://new.example", SHA256: sha2, Deps: nil},
	}}

	diff := Compare(before, after)
	if len(diff.Updated) != 1 {
		t.Fatalf("expected exactly one updated entry, got %+v", diff.Updated)
	}
	got := diff.Updated[0].Fields()
	want := []FieldChange{
		{Field: fieldVersion, From: "1.0.0", To: "1.1.0"},
		{Field: fieldSource, From: "https://old.example", To: "https://new.example"},
		{Field: fieldSHA256, From: sha1, To: sha2},
		{Field: fieldDeps, From: "acme.dep1,acme.dep2", To: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("Fields() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Fields()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestCompareDuplicateDepsAreSignificant pins that deps compare as a
// multiset: [x.x, x.x] and [x.x, y.y] differ under Compare, and Hash agrees.
func TestCompareDuplicateDepsAreSignificant(t *testing.T) {
	t.Parallel()
	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Deps: []string{"x.x", "x.x"}},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Deps: []string{"x.x", "y.y"}},
	}}

	diff := Compare(before, after)
	if len(diff.Updated) != 1 {
		t.Fatalf("duplicate-vs-distinct deps must differ: diff=%+v", diff)
	}

	beforeHash, err := before.Hash()
	if err != nil {
		t.Fatalf("before.Hash: %v", err)
	}
	afterHash, err := after.Hash()
	if err != nil {
		t.Fatalf("after.Hash: %v", err)
	}
	if beforeHash == afterHash {
		t.Fatalf("Hash agrees that [x.x, x.x] and [x.x, y.y] are the same file, hash=%q", beforeHash)
	}
}

// hostileEntryName and hostileEntrySource are a path-traversal name and a
// source carrying a NUL byte, an ANSI escape and a CRLF.
const (
	hostileEntryName   = "../../../../etc/passwd"
	hostileEntrySource = "https://x.example\x00\x1b[31m\r\nInstalled: totally.fine"
)

// TestCompareRendersHostileEntryVerbatim pins that hostile names, sources and
// an oversized dep pass through Compare and Fields unescaped and without a
// panic; sanitizing is the printer's job. acme.widgets is the control.
func TestCompareRendersHostileEntryVerbatim(t *testing.T) {
	t.Parallel()
	oversizedDep := strings.Repeat("d", 10000)

	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: hostileEntryName, Version: "1.0.0", Source: "https://safe.example", SHA256: sha1, Deps: []string{"a.a"}},
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://safe.example", SHA256: sha1},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: hostileEntryName, Version: "1.0.0", Source: hostileEntrySource, SHA256: sha2, Deps: []string{oversizedDep}},
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://safe.example", SHA256: sha1},
	}}

	diff := Compare(before, after)

	if len(diff.Added) != 0 || len(diff.Removed) != 0 {
		t.Fatalf("expected no Added/Removed, got diff=%+v", diff)
	}
	if len(diff.Updated) != 1 {
		t.Fatalf("expected exactly one updated entry (the hostile one; acme.widgets is unchanged), got %+v", diff.Updated)
	}
	assertHostileFieldsRendered(t, diff.Updated[0], oversizedDep)
}

// assertHostileFieldsRendered checks that change keeps the hostile name and
// carries source, sha256 and deps exactly as given, in Fields' fixed order.
func assertHostileFieldsRendered(t *testing.T, change Change, oversizedDep string) {
	t.Helper()
	if change.To.Name != hostileEntryName {
		t.Fatalf("Updated[0].To.Name = %q, want the hostile name unmangled", change.To.Name)
	}
	fields := change.Fields()
	want := []FieldChange{
		{Field: fieldSource, From: "https://safe.example", To: hostileEntrySource},
		{Field: fieldSHA256, From: sha1, To: sha2},
		{Field: fieldDeps, From: "a.a", To: oversizedDep},
	}
	if len(fields) != len(want) {
		t.Fatalf("Fields() = %+v, want %+v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("Fields()[%d] = %+v, want %+v", i, fields[i], want[i])
		}
	}
}

// TestCompareRendersHostileEntryVerbatimDoesNotMutate pins that Compare leaves
// both files' collection and deps order untouched; acme.widgets is listed
// ahead of the hostile name, which sorts first, so an in-place sort would show.
func TestCompareRendersHostileEntryVerbatimDoesNotMutate(t *testing.T) {
	t.Parallel()
	oversizedDep := strings.Repeat("d", 10000)
	unsortedDeps := []string{"z.z", "a.a"}

	before := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://safe.example", SHA256: sha1, Deps: append([]string(nil), unsortedDeps...)},
		{Name: hostileEntryName, Version: "1.0.0", Source: "https://safe.example", SHA256: sha1},
	}}
	after := &File{SchemaVersion: SchemaVersion, Collections: []Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://safe.example", SHA256: sha1, Deps: append([]string(nil), unsortedDeps...)},
		{Name: hostileEntryName, Version: "1.0.0", Source: hostileEntrySource, SHA256: sha2, Deps: []string{oversizedDep}},
	}}
	beforeNames, beforeDeps := collectionNames(before), collectionDeps(before)
	afterNames, afterDeps := collectionNames(after), collectionDeps(after)

	_ = Compare(before, after)

	if got := collectionNames(before); !equalStrings(got, beforeNames) {
		t.Fatalf("Compare mutated before: names changed from %v to %v", beforeNames, got)
	}
	if got := collectionNames(after); !equalStrings(got, afterNames) {
		t.Fatalf("Compare mutated after: names changed from %v to %v", afterNames, got)
	}
	if got := collectionDeps(before); !equalDeps(got, beforeDeps) {
		t.Fatalf("Compare mutated before: deps changed from %v to %v", beforeDeps, got)
	}
	if got := collectionDeps(after); !equalDeps(got, afterDeps) {
		t.Fatalf("Compare mutated after: deps changed from %v to %v", afterDeps, got)
	}
}
