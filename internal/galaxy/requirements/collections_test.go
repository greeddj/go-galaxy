package requirements

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestParseCollectionsAcceptedCases pins the requirements shapes Parse
// accepts; each row carries its own check, since what a shape must produce
// differs per shape.
func TestParseCollectionsAcceptedCases(t *testing.T) {
	t.Parallel()
	for _, tc := range parseCollectionsAcceptedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(tc.input), tc.source)
			collections, rolesFound := f.Collections, len(f.Roles) > 0
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			tc.check(t, collections, rolesFound)
		})
	}
}

// parseCollectionsAcceptedCase is one row of TestParseCollectionsAcceptedCases.
type parseCollectionsAcceptedCase struct {
	check  func(t *testing.T, collections Collections, rolesFound bool)
	name   string
	input  string
	source string
}

// parseCollectionsAcceptedCases enumerates the shapes Parse accepts: a string
// list, roles-only, null and empty collections, both ways to name a
// collection, and the two source: forms the userinfo check leaves alone.
func parseCollectionsAcceptedCases() []parseCollectionsAcceptedCase {
	return append(signatureShapeAcceptedCases(), []parseCollectionsAcceptedCase{
		{
			name:   "string list",
			input:  "- community.general\n- ansible.posix\n",
			source: "https://default",
			check:  checkAcceptedStringList,
		},
		{
			name:   "roles only",
			input:  "roles:\n  - geerlingguy.foo\n",
			source: "https://default",
			check:  checkAcceptedRolesOnly,
		},
		{
			// checks that a null collections value alongside a roles key still
			// reports rolesFound, matching the roles-only case, with no error
			// and no collections.
			name:   "null collections value with roles",
			input:  "collections:\nroles:\n  - geerlingguy.foo\n",
			source: "https://default",
			check:  checkAcceptedNullValueWithRoles,
		},
		{
			// a regression guard: an explicit empty list ("collections: []")
			// is accepted and yields zero collections and no roles.
			name:   "explicit empty list",
			input:  "collections: []\n",
			source: "https://default",
			check:  checkAcceptedEmptyList,
		},
		{
			// a regression guard: an explicit namespace with a plain
			// (non-dotted) name is unaffected and resolves normally.
			name:   "explicit namespace with plain name",
			input:  "- namespace: foo\n  name: bar\n",
			source: "https://default",
			check:  checkAcceptedNamespaceWithPlainName,
		},
		{
			// a regression guard: a dotted name with no explicit namespace is
			// unaffected and still splits normally, since there is nothing for
			// the split to conflict with.
			name:   "dotted name without namespace",
			input:  "- name: bar.baz\n",
			source: "https://default",
			check:  checkAcceptedDottedNameWithoutNamespace,
		},
		{
			// A bare server_list id has no scheme or host, so the userinfo
			// check leaves it alone.
			name:   "source bare server_list id",
			input:  "- name: ns.name\n  source: internal\n",
			source: "",
			check:  checkAcceptedBareSourceID,
		},
		{
			// a regression guard: a URL-shaped source: carrying no userinfo
			// passes the userinfo check and is carried through verbatim.
			name:   "source plain URL",
			input:  "- name: ns.name\n  source: https://hub.example/api/\n",
			source: "",
			check:  checkAcceptedPlainSourceURL,
		},
	}...)
}

// signatureShapeAcceptedCases is the positive control for the signatures:
// refusals: an empty, absent or blank value, a single string, and exactly
// helpers.MaxSignaturesPerCollection sources must all be accepted.
func signatureShapeAcceptedCases() []parseCollectionsAcceptedCase {
	return []parseCollectionsAcceptedCase{
		{
			name:   "signatures empty list",
			input:  "- name: ns.name\n  signatures: []\n",
			source: "https://default",
			check:  checkAcceptedNoSignatures,
		},
		{
			name:   "signatures absent",
			input:  "- name: ns.name\n  signatures:\n",
			source: "https://default",
			check:  checkAcceptedNoSignatures,
		},
		{
			name:   "signatures list of blanks",
			input:  "- name: ns.name\n  signatures:\n    - \"\"\n    - \"  \"\n",
			source: "https://default",
			check:  checkAcceptedNoSignatures,
		},
		{
			name:   "signatures single string",
			input:  "- name: ns.name\n  signatures: file:///keys/ns-name.asc\n",
			source: "https://default",
			check:  checkAcceptedOneSignature,
		},
		{
			name:   "signatures exactly at the cap",
			input:  signatureSourcesAtCapInput(),
			source: "https://default",
			check:  checkAcceptedSignaturesAtCap,
		},
	}
}

// signatureSourcesAtCapInput builds an entry declaring exactly
// helpers.MaxSignaturesPerCollection sources, the last value the gate accepts.
func signatureSourcesAtCapInput() string {
	var b strings.Builder
	b.WriteString("- name: ns.name\n  signatures:\n")
	for i := range helpers.MaxSignaturesPerCollection {
		fmt.Fprintf(&b, "    - https://sigs.example/%d.asc\n", i)
	}

	return b.String()
}

// checkAcceptedNoSignatures asserts a row whose signatures: value contributes
// nothing at all.
func checkAcceptedNoSignatures(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if len(collections[0].Signatures) != 0 {
		t.Fatalf("expected no signatures, got %v", collections[0].Signatures)
	}
}

// checkAcceptedOneSignature asserts the single-string row: the value survives
// as one source rather than being refused for not being a list.
func checkAcceptedOneSignature(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	want := []string{"file:///keys/ns-name.asc"}
	if got := collections[0].Signatures; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("expected signatures %v, got %v", want, got)
	}
}

// checkAcceptedSignaturesAtCap asserts the boundary row: exactly
// helpers.MaxSignaturesPerCollection sources are kept, none dropped.
func checkAcceptedSignaturesAtCap(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if got := len(collections[0].Signatures); got != helpers.MaxSignaturesPerCollection {
		t.Fatalf("expected %d signatures, got %d", helpers.MaxSignaturesPerCollection, got)
	}
}

// checkAcceptedStringList asserts the "string list" row: both entries parse,
// and the first one carries the defaults a bare name gets.
func checkAcceptedStringList(t *testing.T, collections Collections, rolesFound bool) {
	t.Helper()
	if rolesFound {
		t.Fatalf("unexpected rolesFound")
	}
	if len(collections) != 2 {
		t.Fatalf("expected 2 collections, got %d", len(collections))
	}
	if collections[0].Namespace != "community" || collections[0].Name != "general" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
	if collections[0].Version != "*" {
		t.Fatalf("expected default version '*', got %q", collections[0].Version)
	}
	if collections[0].Source != "https://default" {
		t.Fatalf("expected default source, got %q", collections[0].Source)
	}
}

// checkAcceptedRolesOnly asserts the "roles only" row: the collections list
// must be nil, not merely empty.
func checkAcceptedRolesOnly(t *testing.T, collections Collections, rolesFound bool) {
	t.Helper()
	if !rolesFound {
		t.Fatalf("expected rolesFound")
	}
	if collections != nil {
		t.Fatalf("expected nil collections, got %#v", collections)
	}
}

// checkAcceptedNullValueWithRoles asserts the "null collections value with
// roles" row.
func checkAcceptedNullValueWithRoles(t *testing.T, collections Collections, rolesFound bool) {
	t.Helper()
	if !rolesFound {
		t.Fatalf("expected rolesFound")
	}
	if len(collections) != 0 {
		t.Fatalf("expected 0 collections, got %d", len(collections))
	}
}

// checkAcceptedEmptyList asserts the "explicit empty list" row.
func checkAcceptedEmptyList(t *testing.T, collections Collections, rolesFound bool) {
	t.Helper()
	if rolesFound {
		t.Fatalf("unexpected rolesFound")
	}
	if len(collections) != 0 {
		t.Fatalf("expected 0 collections, got %d", len(collections))
	}
}

// checkAcceptedNamespaceWithPlainName asserts the "explicit namespace with
// plain name" row.
func checkAcceptedNamespaceWithPlainName(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if collections[0].Namespace != "foo" || collections[0].Name != "bar" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
}

// checkAcceptedDottedNameWithoutNamespace asserts the "dotted name without
// namespace" row.
func checkAcceptedDottedNameWithoutNamespace(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if collections[0].Namespace != "bar" || collections[0].Name != "baz" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
}

// checkAcceptedBareSourceID asserts the "source bare server_list id" row.
func checkAcceptedBareSourceID(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 || collections[0].Source != "internal" {
		t.Fatalf("unexpected collections: %#v", collections)
	}
}

// checkAcceptedPlainSourceURL asserts the "source plain URL" row.
func checkAcceptedPlainSourceURL(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 || collections[0].Source != "https://hub.example/api/" {
		t.Fatalf("unexpected collections: %#v", collections)
	}
}

// TestParseCollectionsRejectedCases pins the shapes Parse refuses: each row
// names the sentinel the refusal must carry and, for a credential-bearing
// input, the substring its error must never echo.
func TestParseCollectionsRejectedCases(t *testing.T) {
	t.Parallel()
	for _, tc := range parseCollectionsRejectedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.input), tc.source)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
			if tc.mustNotContain != "" && strings.Contains(err.Error(), tc.mustNotContain) {
				t.Fatalf("error must not echo the rejected source's credential, got %v", err)
			}
		})
	}
}

// parseCollectionsRejectedCase is one row of TestParseCollectionsRejectedCases.
// An empty mustNotContain skips the substring check, which only the
// credential-bearing rows need.
type parseCollectionsRejectedCase struct {
	wantErr        error
	name           string
	input          string
	source         string
	mustNotContain string
}

// parseCollectionsRejectedCases enumerates the shapes Parse refuses: bad
// formats, a scalar collections value, namespace/name conflicts, and a
// credential-bearing source: in both entry shapes.
func parseCollectionsRejectedCases() []parseCollectionsRejectedCase {
	return append([]parseCollectionsRejectedCase{
		{
			name:    "unsupported format",
			input:   "foo: bar\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedRequirementsFormat,
		},
		{
			name:    "unsupported source",
			input:   "- ftp://example.com/collections\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedCollectionSource,
		},
		{
			// a regression guard: a scalar collections value (neither null nor
			// a list) must still be rejected; only nil is accepted.
			name:    "scalar collections value",
			input:   "collections: foo\n",
			source:  "https://default",
			wantErr: helpers.ErrInvalidCollectionsList,
		},
		{
			// namespace: plus a dotted name is refused: normalizeCollectionName
			// would otherwise keep the namespace and take the name's last segment.
			name:    "namespace plus dotted name conflict",
			input:   "- namespace: foo\n  name: bar.baz\n",
			source:  "https://default",
			wantErr: helpers.ErrConflictingNamespaceName,
		},
		{
			// Refused even when the two agree: the rule is about the ambiguous
			// shape, not whether this combination happens to resolve harmlessly.
			name:    "namespace plus dotted name conflict even when consistent",
			input:   "- namespace: community\n  name: community.general\n",
			source:  "https://default",
			wantErr: helpers.ErrConflictingNamespaceName,
		},
		{
			// A source: embedding userinfo is refused at parse time with the
			// sentinel config.Server uses, and the error must not echo it.
			name: "source userinfo rejected",
			// #nosec G101 -- test fixture literal, not a real credential
			input:          "- name: ns.name\n  source: https://user:tok3n-must-not-leak@hub.example/api/\n",
			source:         "",
			wantErr:        helpers.ErrGalaxyServerURLUserinfo,
			mustNotContain: "tok3n-must-not-leak",
		},
		{
			// A missing name echoes the raw entry in its error, so the userinfo
			// check must run first or the entry would leak its credential.
			name: "invalid entry does not leak source credential",
			// #nosec G101 -- test fixture literal, not a real credential
			input:          "- source: https://user:tok3n-must-not-leak@hub.example/api/\n  version: \"*\"\n",
			source:         "",
			wantErr:        helpers.ErrGalaxyServerURLUserinfo,
			mustNotContain: "tok3n-must-not-leak",
		},
	}, signatureSourceRejectedCases()...)
}

// signatureSourceRejectedCases is the signatures: half of the table above:
// every row is one way checkSignatureSources refuses a declared value.
func signatureSourceRejectedCases() []parseCollectionsRejectedCase {
	return append([]parseCollectionsRejectedCase{
		{
			// A source this tool cannot fetch is a configuration error at load,
			// not one collection's install failure.
			name:    "unfetchable signature source rejected",
			input:   "- name: ns.name\n  signatures:\n    - ftp://sigs.example/ns-name.asc\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			// A userinfo-bearing signature source is refused, like source:,
			// without the refusal printing the credential.
			name: "signature source userinfo rejected without echoing it",
			// #nosec G101 -- test fixture literal, not a real credential
			input:          "- name: ns.name\n  signatures:\n    - https://bot:tok3n-must-not-leak@sig.example/a.asc\n",
			source:         "https://default",
			wantErr:        helpers.ErrSignatureSourceUserinfo,
			mustNotContain: "tok3n-must-not-leak",
		},
		{
			// Without the shape check, parseStringList's fmt.Sprint arm turns a
			// mapping into the plausible-looking source "map[]", which nothing
			// downstream can tell from one an author wrote.
			name:    "signatures mapping rejected",
			input:   "- name: ns.name\n  signatures:\n    key: value\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			// The same arm one level in: a list carrying a non-string element,
			// which fmt.Sprint would render as "false" or "0".
			name:    "signatures list element that is not a string rejected",
			input:   "- name: ns.name\n  signatures:\n    - false\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
	}, fileSourceRejectedCases()...)
}

// fileSourceRejectedCases is the file-scheme half of the signatures: table,
// split out for the length budget. Every row is a file URL that named no local
// path this tool can read.
func fileSourceRejectedCases() []parseCollectionsRejectedCase {
	return []parseCollectionsRejectedCase{
		{
			// This and the next four file URLs name no readable local path;
			// refused at load they exit 2, not 5 as an install failure.
			name:    "file source naming another host",
			input:   "- name: ns.name\n  signatures:\n    - file://otherhost/abs/sig.asc\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			name:    "file source naming an evil host",
			input:   "- name: ns.name\n  signatures:\n    - file://evil.example/abs/sig.asc\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			name:    "bare file scheme",
			input:   "- name: ns.name\n  signatures:\n    - \"file:\"\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			name:    "file scheme with an empty authority and no path",
			input:   "- name: ns.name\n  signatures:\n    - file://\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			name:    "file scheme naming localhost and no path",
			input:   "- name: ns.name\n  signatures:\n    - file://localhost\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			// Pins only the cap on what one entry may declare; gatherLimit caps
			// the combined declared-plus-server candidate set separately.
			name:    "more signature sources than the cap allows",
			input:   tooManySignatureSourcesInput(),
			source:  "https://default",
			wantErr: helpers.ErrTooManySignatureSources,
		},
	}
}

// tooManySignatureSourcesInput builds an entry declaring one more signature
// source than helpers.MaxSignaturesPerCollection, derived from the constant
// since the boundary, not a count, is the point.
func tooManySignatureSourcesInput() string {
	var b strings.Builder
	b.WriteString("- name: ns.name\n  signatures:\n")
	for i := range helpers.MaxSignaturesPerCollection + 1 {
		fmt.Fprintf(&b, "    - https://sigs.example/%d.asc\n", i)
	}

	return b.String()
}

// TestParseCollectionsNullValue pins that ansible's "collections:" and
// "collections: ~" idioms parse as an empty list, since both decode to nil.
func TestParseCollectionsNullValue(t *testing.T) {
	t.Parallel()
	inputs := map[string]string{
		"bare key":   "collections:\n",
		"tilde null": "collections: ~\n",
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(input), "https://default")
			collections, rolesFound := f.Collections, len(f.Roles) > 0
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if rolesFound {
				t.Fatalf("unexpected rolesFound")
			}
			if len(collections) != 0 {
				t.Fatalf("expected 0 collections, got %d", len(collections))
			}
		})
	}
}

// TestParseCollectionsNamespaceWithThreePartNameIsRejectedAsAName pins that
// namespace: plus a three-part name is refused as an invalid name, not as a
// conflict: helpers.SplitFQDN does not split three parts.
func TestParseCollectionsNamespaceWithThreePartNameIsRejectedAsAName(t *testing.T) {
	t.Parallel()
	input := "- namespace: foo\n  name: a.b.c\n"
	_, err := Parse([]byte(input), "https://default")
	if !errors.Is(err, helpers.ErrInvalidCollectionName) {
		t.Fatalf("Parse error = %v, want errors.Is helpers.ErrInvalidCollectionName", err)
	}
	if errors.Is(err, helpers.ErrConflictingNamespaceName) {
		t.Fatalf("a three-part name must not be reported as a namespace conflict: %v", err)
	}

	// Positive control on the same shape: an explicit namespace with a
	// dot-free name is accepted, so the rejection above is the dots and not
	// the explicit-namespace form itself.
	f, err := Parse([]byte("- namespace: acme\n  name: widgets\n"), "https://default")
	collections := f.Collections
	if err != nil {
		t.Fatalf("Parse with an explicit namespace and a plain name: %v", err)
	}
	if len(collections) != 1 || collections[0].Namespace != "acme" || collections[0].Name != "widgets" {
		t.Fatalf("unexpected collections: %#v", collections)
	}
}

// TestParseCollectionsRejectsNamesOutsideTheAlphabet pins that every declared
// identity must match the Galaxy name alphabet at load, including an explicit
// namespace:, which never passes through helpers.SplitFQDN.
func TestParseCollectionsRejectsNamesOutsideTheAlphabet(t *testing.T) {
	t.Parallel()
	for _, tc := range rejectedRequirementNameCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.input), "https://default")
			if !errors.Is(err, helpers.ErrInvalidCollectionName) {
				t.Fatalf("Parse error = %v, want errors.Is helpers.ErrInvalidCollectionName", err)
			}
		})
	}

	// Control on the same two shapes: the dotted form and the explicit form
	// both parse when their identities are inside the alphabet, so neither
	// rejection above is the shape itself being refused.
	controls := []struct {
		name  string
		input string
	}{
		{name: "dotted name", input: "- name: acme.widgets\n"},
		{name: "explicit namespace, plain name", input: "- namespace: acme\n  name: widgets\n"},
	}
	for _, tc := range controls {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(tc.input), "https://default")
			collections := f.Collections
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.input, err)
			}
			if len(collections) != 1 || collections[0].Namespace != "acme" || collections[0].Name != "widgets" {
				t.Fatalf("Parse(%q) = %#v", tc.input, collections)
			}
		})
	}
}

// rejectedRequirementNameCase is one row of
// TestParseCollectionsRejectsNamesOutsideTheAlphabet.
type rejectedRequirementNameCase struct {
	name  string
	input string
}

// rejectedRequirementNameCases covers the forged-line shape in each of the two
// places a requirements entry can declare an identity, plus the two ordinary
// alphabet violations.
func rejectedRequirementNameCases() []rejectedRequirementNameCase {
	return []rejectedRequirementNameCase{
		{name: "forged line in an explicit namespace", input: "- namespace: \"acme\\n[CRITICAL] X\"\n  name: widgets\n"},
		{name: "forged line in a dotted name", input: "- name: \"acme.widgets\\n[CRITICAL] X\"\n"},
		{name: "uppercase", input: "- name: Acme.Widgets\n"},
		{name: "hyphen", input: "- name: acme.my-widgets\n"},
	}
}
