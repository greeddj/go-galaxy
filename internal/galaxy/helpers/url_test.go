package helpers

import "testing"

// TestWithoutQuery pins the cut and that a URL carrying no query is left alone,
// which matters as much because the function runs over values this tool did
// not author: a server's download URL, a signature source.
func TestWithoutQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"no query", "https://h/x/artifact.tar.gz", "https://h/x/artifact.tar.gz"},
		{"presigned capability", "https://h/x?X-Amz-Signature=deadbeef", "https://h/x"},
		{"multi-parameter query", "https://h/x?a=1&b=2", "https://h/x"},
		{"empty query", "https://h/x?", "https://h/x"},
		{"question mark in a later parameter", "https://h/x?a=b?c", "https://h/x"},
		{"bare question mark", "?", ""},
		{"query on a file url", "file:///tmp/sig.asc?a=b", "file:///tmp/sig.asc"},
		{"fragment is left alone", "https://h/x#frag", "https://h/x#frag"},
		// The cut is textual, so a value url.Parse would refuse is stripped
		// exactly like one it accepts. That is the whole reason the cut is
		// textual, and this row is what says so.
		{"unparseable url", "http://%zz/x?token=t", "http://%zz/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WithoutQuery(tc.raw); got != tc.want {
				t.Errorf("WithoutQuery(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestWithoutFragment pins the cut and that a URL carrying no fragment is left
// alone. The file row is why the cut exists: url.Parse drops the fragment
// before a file:// Path is opened, so a kept fragment names a file nothing read.
func TestWithoutFragment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"no fragment", "https://h/x/sig.asc", "https://h/x/sig.asc"},
		{"fragment", "https://h/x#frag", "https://h/x"},
		{"empty fragment", "https://h/x#", "https://h/x"},
		{"second hash inside the fragment", "https://h/x#a#b", "https://h/x"},
		{"bare hash", "#", ""},
		{"fragment on a file url", "file:///tmp/a#b.asc", "file:///tmp/a"},
		{"query is left for WithoutQuery", "https://h/x?a=b", "https://h/x?a=b"},
		{"hash after a query", "https://h/x?a=b#c", "https://h/x?a=b"},
		// The cut is textual, so a value url.Parse would refuse is stripped
		// exactly like one it accepts - the same property WithoutQuery's own
		// table records, for the same reason.
		{"unparseable url", "http://%zz/x#f", "http://%zz/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WithoutFragment(tc.raw); got != tc.want {
				t.Errorf("WithoutFragment(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// urlCase is one raw value and what WithoutUserinfo must make of it.
type urlCase struct {
	name string
	raw  string
	want string
}

// withoutUserinfoCases is the table TestWithoutUserinfo and
// TestTheThreeCutsComposeInAnyOrder share, returned fresh by a function so no
// test can mutate the table another reads.
func withoutUserinfoCases() []urlCase {
	return []urlCase{
		// The values that are cut. The opaque and two-at-sign rows need the
		// hand-written scan: url.Parse sees no authority in "http:u:p@h/x", and
		// only two "@" tell the last from the first.
		{"credentialed https url", "https://u:p@h/x", "https://h/x"},
		{"scheme-relative url", "//u:p@h/x", "//h/x"},
		{"opaque url", "http:u:p@h/x", "http:h/x"},
		{"two at signs in the authority", "https://u@p@h/x", "https://h/x"},
		{"query is left for WithoutQuery", "https://u:p@h/x?a=b", "https://h/x?a=b"},
		// The values that are not: each resembles the cut (an "@" in a path, a
		// ":" that is not a scheme, no authority at all) but holds no credential.
		{"no userinfo", "https://h/x", "https://h/x"},
		{"at sign in the path", "https://h/x@y", "https://h/x@y"},
		{"authority ends at the query", "https://h?a=b", "https://h?a=b"},
		{"at sign in a file path", "file:///tmp/a@b.asc", "file:///tmp/a@b.asc"},
		{"file url with a single slash", "file:/abs/p", "file:/abs/p"},
		{"bare absolute path", "/bare/abs/path", "/bare/abs/path"},
		{"relative path", "relative/path", "relative/path"},
		{"data url", "data:text/plain;base64,aGk=", "data:text/plain;base64,aGk="},
		{"unparseable url", "http://%zz/x", "http://%zz/x"},
		{"empty", "", ""},
		// The residual: a "#" or "/" inside the intended userinfo ends the
		// authority scan before any "@", so the value comes back whole; what a
		// message shows is TestDisplayCutsOnADelimiterInsideUserinfo's to pin.
		{"hash inside the userinfo", "https://user:pa#55w0rd@h/x", "https://user:pa#55w0rd@h/x"},
		{"slash inside the userinfo", "https://user:pa/55w0rd@h/x", "https://user:pa/55w0rd@h/x"},
		// The one legitimate value the cut rewrites: no caller fetches a mailto,
		// and erring toward removing too much is the deliberate direction.
		{"mailto", "mailto:u@e.com", "mailto:e.com"},
	}
}

// TestWithoutUserinfo pins the cut and that every shape carrying no credential
// survives it untouched, since it runs over values this tool did not author.
func TestWithoutUserinfo(t *testing.T) {
	t.Parallel()

	for _, tc := range withoutUserinfoCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The two-at-signs row alone tells LastIndex from Index, and the
			// scheme-relative row alone checks that only a ":" ends a scheme.
			if got := WithoutUserinfo(tc.raw); got != tc.want {
				t.Errorf("WithoutUserinfo(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestTheThreeCutsComposeInAnyOrder pins that no cut reaches across another's
// boundary, which URLForMessage relies on: all six orders of the three cuts
// agree on every value of the shared table.
func TestTheThreeCutsComposeInAnyOrder(t *testing.T) {
	t.Parallel()

	// Bound to one-letter names so each composition below fits on one line and
	// reads as the order its own name spells out.
	q, f, u := WithoutQuery, WithoutFragment, WithoutUserinfo
	orders := []struct {
		apply func(string) string
		name  string
	}{
		{name: "query, fragment, userinfo", apply: func(raw string) string { return u(f(q(raw))) }},
		{name: "query, userinfo, fragment", apply: func(raw string) string { return f(u(q(raw))) }},
		{name: "fragment, query, userinfo", apply: func(raw string) string { return u(q(f(raw))) }},
		{name: "fragment, userinfo, query", apply: func(raw string) string { return q(u(f(raw))) }},
		{name: "userinfo, query, fragment", apply: func(raw string) string { return f(q(u(raw))) }},
		{name: "userinfo, fragment, query", apply: func(raw string) string { return q(f(u(raw))) }},
	}

	// A literal "?" inside the userinfo is the residual's third spelling, and
	// every order truncates at it rather than disagreeing.
	cases := withoutUserinfoCases()
	raws := make([]string, 0, len(cases)+1)
	raws = append(raws, "https://user:pa?55w0rd@h/x")
	for _, tc := range cases {
		raws = append(raws, tc.raw)
	}

	for _, raw := range raws {
		want := orders[0].apply(raw)
		for _, order := range orders[1:] {
			if got := order.apply(raw); got != want {
				t.Errorf("composing on %q: %s = %q, %s = %q", raw, order.name, got, orders[0].name, want)
			}
		}
	}
}

// TestDisplayCutsOnADelimiterInsideUserinfo pins what URLForMessage shows when
// the userinfo holds "?", "#" or "/": the first two truncate the password, and
// "/" renders it whole, since cutting there would cut "https://h/x@y" too.
func TestDisplayCutsOnADelimiterInsideUserinfo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"positive control: no delimiter inside the userinfo", "https://user:pa55w0rd@h/x", "https://h/x"},
		{"question mark", "https://user:pa?55w0rd@h/x", "https://user:pa"},
		{"hash", "https://user:pa#55w0rd@h/x", "https://user:pa"},
		{"slash", "https://user:pa/55w0rd@h/x", "https://user:pa/55w0rd@h/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := URLForMessage(tc.raw)
			if got != tc.want {
				t.Errorf("display cuts on %q = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// WithoutCredentials has no table of its own: it only composes the cuts pinned
// above, and a body replacing one cut rather than composing both is caught by
// collections.TestBuildGalaxyYAMLStripsUserinfoAndQueryTogether.
