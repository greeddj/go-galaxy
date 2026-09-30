package projectfile

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

// TestEncodeWritesDocumentedLayout pins the exact bytes: [project], the name,
// then each list one entry per line, two spaces in, a comma after each.
func TestEncodeWritesDocumentedLayout(t *testing.T) {
	t.Parallel()
	draft := Draft{
		Name: "infra",
		Collections: []Entry{
			{Text: "ansible.utils"},
			{Fields: []Field{
				{Key: "name", Value: "acme.signed"},
				{Key: "signatures", List: []string{"https://keys.example.com/a.asc"}, IsList: true},
			}},
		},
		Roles: []Entry{
			{Text: "geerlingguy.docker,7.4.1"},
			{Fields: []Field{{Key: "name", Value: "postgres"}, {Key: "src", Value: "geerlingguy.postgresql"}}},
		},
		HasCollections: true,
		HasRoles:       true,
	}
	want := `[project]
name = "infra"
collections = [
  "ansible.utils",
  { name = "acme.signed", signatures = ["https://keys.example.com/a.asc"] },
]
roles = [
  "geerlingguy.docker,7.4.1",
  { name = "postgres", src = "geerlingguy.postgresql" },
]
`
	got := Encode(draft)
	if string(got) != want {
		t.Fatalf("Encode =\n%s\nwant\n%s", got, want)
	}
	if _, err := Decode(got); err != nil {
		t.Fatalf("Decode(Encode): %v", err)
	}
}

// TestEncodeListPresence pins which keys a draft writes: an empty list that
// is present is "= []", an absent one and an empty name write nothing, and
// every output decodes.
func TestEncodeListPresence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		want  string
		draft Draft
	}{
		{name: "empty collections", draft: Draft{Name: "p", HasCollections: true}, want: "[project]\nname = \"p\"\ncollections = []\n"},
		{
			name:  "no roles line",
			draft: Draft{Name: "p", HasCollections: true, Collections: []Entry{{Text: "a.b"}}},
			want:  "[project]\nname = \"p\"\ncollections = [\n  \"a.b\",\n]\n",
		},
		{name: "no name", draft: Draft{HasRoles: true}, want: "[project]\nroles = []\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Encode(tc.draft)
			if string(got) != tc.want {
				t.Fatalf("Encode = %q, want %q", got, tc.want)
			}
			doc, err := Decode(got)
			if err != nil {
				t.Fatalf("Decode(Encode): %v", err)
			}
			if doc.Project.Name != tc.draft.Name {
				t.Fatalf("name = %q, want %q", doc.Project.Name, tc.draft.Name)
			}
		})
	}
}

// decodedName encodes s as the project name and decodes it back.
func decodedName(t *testing.T, s string) string {
	t.Helper()
	doc, err := Decode(Encode(Draft{Name: s, HasCollections: true}))
	if err != nil {
		t.Fatalf("Decode(Encode(%q)): %v", s, err)
	}
	return doc.Project.Name
}

// TestQuoteBasicEscapes pins each escape: the short ones TOML has, \uXXXX in
// upper-case hex for every other rune safeout will not print, and any other
// text as written; each value decodes back to itself.
func TestQuoteBasicEscapes(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{`"`, `"\""`},
		{`\`, `"\\"`},
		{"\b\t\n\f\r", `"\b\t\n\f\r"`},
		{"\u0000", `"\u0000"`},
		{"\u001f", `"\u001F"`},
		{"\u007f", `"\u007F"`},
		{"\u0080", `"\u0080"`},
		{"\u0085", `"\u0085"`},
		{"\u009f", `"\u009F"`},
		{"\u2028", `"\u2028"`},
		{"\u2029", `"\u2029"`},
		{"п", `"п"`},
		{"日本", `"日本"`}, //nolint:gosmopolitan // CJK text must pass unescaped.
		{"\u200e", "\"\u200e\""},
	}
	for _, tc := range cases {
		got := quoteBasic(tc.in)
		if got != tc.want {
			t.Errorf("quoteBasic(%q) = %q, want %q", tc.in, got, tc.want)
		}
		for _, banned := range []string{`\x`, `\a`, `\v`} {
			if strings.Contains(got, banned) {
				t.Errorf("quoteBasic(%q) = %q carries %s", tc.in, got, banned)
			}
		}
		if back := decodedName(t, tc.in); back != tc.in {
			t.Errorf("Decode(quoteBasic(%q)) = %q", tc.in, back)
		}
	}
}

// FuzzQuoteBasicRoundTrip pins that any UTF-8 name decodes back to itself and
// that the bytes carry no rune a terminal acts on beyond the line breaks.
func FuzzQuoteBasicRoundTrip(f *testing.F) {
	//nolint:gosmopolitan // a CJK seed pins text that passes unescaped.
	for _, seed := range []string{"infra", "a\"b\\c", "\x00\x1f\x7f", "\u0085\u2028\u2029", "п日本\u200e", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return
		}
		out := Encode(Draft{Name: s, HasCollections: true})
		doc, err := Decode(out)
		if err != nil {
			t.Fatalf("Decode(Encode(%q)): %v", s, err)
		}
		if doc.Project.Name != s {
			t.Fatalf("name = %q, want %q", doc.Project.Name, s)
		}
		for _, r := range string(out) {
			if r != '\n' && safeout.IsUnsafeRune(r) {
				t.Fatalf("Encode(%q) = %q carries %U", s, out, r)
			}
		}
	})
}
