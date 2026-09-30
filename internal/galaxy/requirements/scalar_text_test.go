package requirements

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// scalarTextCase is one unquoted spelling of a collection version and the
// text Parse must carry for it.
type scalarTextCase struct {
	name  string
	input string
	want  string
}

// scalarTextCases are spellings yaml resolves to a number, a bool, a timestamp
// or bytes, each of which fmt.Sprint once rendered as other text.
func scalarTextCases() []scalarTextCase {
	const entry = "- name: ns.name\n  version: "
	return []scalarTextCase{
		{name: "trailing zero", input: entry + "1.10\n", want: "1.10"},
		{name: "partial", input: entry + "1.0\n", want: "1.0"},
		{name: "integer", input: entry + "7\n", want: "7"},
		{name: "leading zero", input: entry + "010\n", want: "010"},
		{name: "hex", input: entry + "0x10\n", want: "0x10"},
		{name: "exponent", input: entry + "1e3\n", want: "1e3"},
		{name: "bool", input: entry + "True\n", want: "True"},
		{name: "date", input: entry + "2024-01-15\n", want: "2024-01-15"},
		{name: "explicit float tag", input: entry + "!!float 1.10\n", want: "1.10"},
		{name: "binary tag", input: entry + "!!binary MS4xMA==\n", want: "MS4xMA=="},
		{name: "alias", input: "- name: ns.other\n  version: &v 1.10\n" + entry + "*v\n", want: "1.10"},
		{name: "merge key", input: "- &b {name: ns.other, version: 1.10}\n- <<: *b\n  name: ns.name\n", want: "1.10"},
	}
}

// TestParseReadsUnquotedScalarsAsWritten pins that every scalar reads as the
// text written, through an alias and a merge key too, and on a git entry.
func TestParseReadsUnquotedScalarsAsWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range scalarTextCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(tc.input), "https://default")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := f.Collections[len(f.Collections)-1].Version; got != tc.want {
				t.Fatalf("Version = %q, want %q", got, tc.want)
			}
		})
	}
	f, err := Parse([]byte("- name: git+https://git.example/acme/app.git\n  version: 1.10\n"), "https://default")
	if err != nil || len(f.Collections) != 1 || f.Collections[0].Ref != "1.10" {
		t.Fatalf("git entry: %+v, %v; want Ref 1.10", f.Collections, err)
	}
}

// TestParseRefusesAnAliasBomb pins that reading scalars as text keeps yaml's
// own alias budget: nine levels of nine aliases each are still refused.
func TestParseRefusesAnAliasBomb(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString(`a: &a ["x","x","x","x","x","x","x","x","x"]` + "\n")
	prev := "a"
	for _, name := range []string{"b", "c", "d", "e", "f", "g", "h", "i"} {
		refs := strings.TrimSuffix(strings.Repeat("*"+prev+",", 9), ",")
		fmt.Fprintf(&b, "%s: &%s [%s]\n", name, name, refs)
		prev = name
	}
	b.WriteString("collections: [ns.name]\n")
	_, err := Parse([]byte(b.String()), "https://default")
	if !errors.Is(err, helpers.ErrInvalidRequirementsYAML) || !strings.Contains(err.Error(), "excessive aliasing") {
		t.Fatalf("Parse error = %v, want ErrInvalidRequirementsYAML for excessive aliasing", err)
	}
}

// TestParseJudgesANumberUnderAnIdentityKey pins that a number under
// namespace: or type:, once dropped as a non-string, is judged as its text.
func TestParseJudgesANumberUnderAnIdentityKey(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		want  error
		input string
	}{
		"namespace beside a dotted name": {input: "- namespace: 123\n  name: ns.coll\n", want: helpers.ErrConflictingNamespaceName},
		"type":                           {input: "- name: ns.coll\n  type: 1\n", want: helpers.ErrUnsupportedCollectionType},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(tc.input), "https://default"); !errors.Is(err, tc.want) {
				t.Fatalf("Parse error = %v, want %v", err, tc.want)
			}
		})
	}
}
