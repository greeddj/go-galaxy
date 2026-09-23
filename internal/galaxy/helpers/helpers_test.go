package helpers

import "testing"

// TestSplitFQDNDoesNotValidatePathSafety pins that SplitFQDN splits a
// namespace holding a path separator, so a caller building a path from its
// halves must apply IsPathElement itself.
func TestSplitFQDNDoesNotValidatePathSafety(t *testing.T) {
	t.Parallel()
	ns, name, ok := SplitFQDN("foo/bar.baz")
	if !ok {
		t.Fatalf(`SplitFQDN("foo/bar.baz") ok = false, want true (SplitFQDN performs no path-safety validation)`)
	}
	if ns != "foo/bar" || name != "baz" {
		t.Fatalf(`SplitFQDN("foo/bar.baz") = (%q, %q), want ("foo/bar", "baz")`, ns, name)
	}
	if IsPathElement(ns) {
		t.Fatalf("test setup bug: %q must itself be unsafe (contain a path separator) for this pinning to matter", ns)
	}
}

// TestNormalizeConstraint pins the match-all mapping, passthrough without
// "==", the per-clause "==" to "=" rewrite, and that "===" and ">==" are left
// alone because they are not ansible's exact-match operator.
func TestNormalizeConstraint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"empty string", "", ""},
		{"whitespace only", "   ", ""},
		{"match-all sentinel", "*", ""},
		{"match-all sentinel with surrounding whitespace", " * ", ""},
		{"passthrough without ==", ">=1.0.0,<2.0.0", ">=1.0.0,<2.0.0"},
		{"passthrough trims surrounding whitespace", "  >=1.0.0  ", ">=1.0.0"},
		{"== rewritten to =", "==1.2.3", "=1.2.3"},
		{"== in a later clause is rewritten", "!=1.0.5,==1.2.3", "!=1.0.5,=1.2.3"},
		{"== in the first clause, other clause preserved", "==1.0.0,!=1.0.5", "=1.0.0,!=1.0.5"},
		{"=== guard is left untouched", "===1.2.3", "===1.2.3"},
		{"does not start with == is left untouched", ">==1.2.3", ">==1.2.3"},
		{"per-clause whitespace is normalized around a == rewrite", " ==1.2.3 , !=1.0.5 ", "=1.2.3,!=1.0.5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeConstraint(tc.value); got != tc.want {
				t.Errorf("NormalizeConstraint(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestIsCollectionNamePart pins the collection name half alphabet as an
// allow-list, with rows for what is accepted as well as what is refused.
func TestIsCollectionNamePart(t *testing.T) {
	t.Parallel()
	for _, tc := range collectionNamePartCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsCollectionNamePart(tc.value); got != tc.want {
				t.Errorf("IsCollectionNamePart(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// collectionNamePartCase is one row of TestIsCollectionNamePart.
type collectionNamePartCase struct {
	name  string
	value string
	want  bool
}

// collectionNamePartCases enumerates the alphabet's boundaries: real Galaxy
// names, the shapes Galaxy servers reject, and hostile shapes such as a
// newline that would forge a report line.
func collectionNamePartCases() []collectionNamePartCase {
	return []collectionNamePartCase{
		{name: "plain lowercase", value: "acme", want: true},
		{name: "digits after the first rune", value: "acme2", want: true},
		{name: "underscore after the first rune", value: "my_collection", want: true},
		{name: "empty", value: "", want: false},
		{name: "uppercase", value: "Acme", want: false},
		{name: "hyphen", value: "my-collection", want: false},
		{name: "leading digit", value: "1acme", want: false},
		{name: "leading underscore", value: "_acme", want: false},
		{name: "embedded dot", value: "acme.widgets", want: false},
		{name: "path separator", value: "foo/bar", want: false},
		{name: "newline, the forged-line shape", value: "acme\nUp to date: nothing", want: false},
		{name: "non-ASCII letter", value: "acmé", want: false},
	}
}

// TestIsCollectionName pins the whole-identifier form: exactly two halves,
// each satisfying the part predicate. The dotted-halves rows are what
// separate it from a naive check on the joined string.
func TestIsCollectionName(t *testing.T) {
	t.Parallel()
	for _, tc := range collectionNameCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsCollectionName(tc.value); got != tc.want {
				t.Errorf("IsCollectionName(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// collectionNameCase is one row of TestIsCollectionName.
type collectionNameCase struct {
	name  string
	value string
	want  bool
}

// collectionNameCases covers the shapes SplitFQDN accepts but the alphabet
// must not, alongside the ones it rejects on shape alone.
func collectionNameCases() []collectionNameCase {
	return []collectionNameCase{
		{name: "well formed", value: "acme.widgets", want: true},
		{name: "underscores in both halves", value: "my_ns.my_coll", want: true},
		{name: "no dot", value: "acme", want: false},
		{name: "three parts", value: "acme.sub.widgets", want: false},
		{name: "empty half", value: "acme.", want: false},
		{name: "path separator in the namespace", value: "foo/bar.baz", want: false},
		{name: "newline in the name half", value: "acme.widgets\nUp to date: nothing", want: false},
		{name: "uppercase half", value: "Acme.widgets", want: false},
	}
}
