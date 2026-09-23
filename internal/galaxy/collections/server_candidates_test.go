package collections

import (
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// TestRootMetadataURLCandidatesEmptyMemoMatchesFullSet pins that an empty or
// nil memo yields every API root in priority order, with and without a
// trailing slash, /api/v3 first so galaxy.ansible.com pays for no fallback.
func TestRootMetadataURLCandidatesEmptyMemoMatchesFullSet(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets"}
	const base = "https://galaxy.example.com"

	want := []rootMetaCandidate{
		{url: base + "/api/v3/collections/acme/widgets/", base: base, apiRoot: base + "/api/v3"},
		{url: base + "/api/v3/collections/acme/widgets", base: base, apiRoot: base + "/api/v3"},
		{url: base + "/v3/collections/acme/widgets/", base: base, apiRoot: base + "/v3"},
		{url: base + "/v3/collections/acme/widgets", base: base, apiRoot: base + "/v3"},
		{url: base + "/api/v2/collections/acme/widgets/", base: base, apiRoot: base + "/api/v2"},
		{url: base + "/api/v2/collections/acme/widgets", base: base, apiRoot: base + "/api/v2"},
		{url: base + "/v2/collections/acme/widgets/", base: base, apiRoot: base + "/v2"},
		{url: base + "/v2/collections/acme/widgets", base: base, apiRoot: base + "/v2"},
		{url: base + "/api/collections/acme/widgets/", base: base, apiRoot: base + "/api"},
		{url: base + "/api/collections/acme/widgets", base: base, apiRoot: base + "/api"},
	}

	for name, memo := range map[string]*apiRootMemo{"empty memo": newAPIRootMemo(), "nil memo": nil} {
		got := rootMetadataURLCandidates(base, col, memo)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: candidate set mismatch\n got: %+v\nwant: %+v", name, got, want)
		}
	}
}

// TestRootMetadataURLCandidatesWithRecordedWinner pins that a memoized winning
// API root yields only its two trailing-slash variants; the losers are dropped.
func TestRootMetadataURLCandidatesWithRecordedWinner(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets"}
	const base = "https://galaxy.example.com"
	const winningRoot = base + "/api/v2"

	memo := newAPIRootMemo()
	memo.recordWinner(base, winningRoot)

	got := rootMetadataURLCandidates(base, col, memo)
	want := []rootMetaCandidate{
		{url: winningRoot + "/collections/acme/widgets/", base: base, apiRoot: winningRoot},
		{url: winningRoot + "/collections/acme/widgets", base: base, apiRoot: winningRoot},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate set mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// apiRootCandidatesCase is one TestAPIRootCandidates table entry.
type apiRootCandidatesCase struct {
	base string
	want []string
}

// apiRootCandidatesCases is TestAPIRootCandidates' table, hoisted to keep the
// test function short.
//
//nolint:gochecknoglobals // test-only fixture, not runtime-mutable state.
var apiRootCandidatesCases = map[string]apiRootCandidatesCase{
	"bare base": {
		base: "https://galaxy.example.com",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"base ending /api": {
		base: "https://galaxy.example.com/api",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/api",
		},
	},
	"base ending /api/v3": {
		base: "https://galaxy.example.com/api/v3",
		want: []string{"https://galaxy.example.com/api/v3"},
	},
	"base ending /api/v2": {
		base: "https://galaxy.example.com/api/v2",
		want: []string{"https://galaxy.example.com/api/v2"},
	},
	"base ending /v3": {
		base: "https://hub.example.com/api/automation-hub/v3",
		want: []string{"https://hub.example.com/api/automation-hub/v3"},
	},
	"base ending /v2": {
		base: "https://hub.example.com/api/automation-hub/v2",
		want: []string{"https://hub.example.com/api/automation-hub/v2"},
	},
	"quoted value": {
		base: `"https://galaxy.example.com"`,
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"trailing-slash value": {
		base: "https://galaxy.example.com/",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"empty string": {
		base: "",
		want: nil,
	},
}

// TestAPIRootCandidates is a table test over apiRootCandidates' own suffix
// handling and normalization, independent of rootMetadataURLCandidates'
// collections/ns/name URL building.
func TestAPIRootCandidates(t *testing.T) {
	t.Parallel()
	for name, tc := range apiRootCandidatesCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := apiRootCandidates(tc.base)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("apiRootCandidates(%q) = %+v, want %+v", tc.base, got, tc.want)
			}
		})
	}
}

// TestNormalizeServerBase pins the order of steps: whitespace is trimmed, then
// one balanced pair of double quotes is stripped (an unbalanced quote is
// kept), then trailing slashes are removed.
func TestNormalizeServerBase(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		value string
		want  string
	}{
		"plain":                     {value: "https://hub.example.com", want: "https://hub.example.com"},
		"trailing slash":            {value: "https://hub.example.com/", want: "https://hub.example.com"},
		"trailing slash run":        {value: "https://hub.example.com///", want: "https://hub.example.com"},
		"surrounding whitespace":    {value: "  https://hub.example.com/  ", want: "https://hub.example.com"},
		"quoted":                    {value: "\"https://hub.example.com/\"", want: "https://hub.example.com"},
		"whitespace outside quotes": {value: " \"https://hub.example.com/\" ", want: "https://hub.example.com"},
		"unbalanced leading quote":  {value: "\"https://hub.example.com", want: "\"https://hub.example.com"},
		"unbalanced trailing quote": {value: "https://hub.example.com\"", want: "https://hub.example.com\""},
		"single quote character":    {value: "\"", want: "\""},
		"only one pair stripped":    {value: "\"\"https://hub.example.com\"\"", want: "\"https://hub.example.com\""},
		"empty quoted pair":         {value: "\"\"", want: ""},
		"empty":                     {value: "", want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeServerBase(tc.value); got != tc.want {
				t.Fatalf("normalizeServerBase(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestServerCandidateLabel pins serverCandidate.label's own precedence: a
// non-empty id always wins, else base.
func TestServerCandidateLabel(t *testing.T) {
	t.Parallel()
	if got := (serverCandidate{base: "https://a.example", id: "a"}).label(); got != "a" {
		t.Fatalf("label() = %q, want the id %q", got, "a")
	}
	if got := (serverCandidate{base: "https://a.example"}).label(); got != "https://a.example" {
		t.Fatalf("label() = %q, want the base when id is empty", got)
	}
}

// serverCandidatesCase is one TestServerCandidates table entry.
type serverCandidatesCase struct {
	cfg  *config.Config
	name string
	col  collection
	want []serverCandidate
}

// multiServerTestCfg is the shared two-server config most
// serverCandidatesCases entries pin against.
//
//nolint:gochecknoglobals // test-only fixture, not runtime-mutable state.
var multiServerTestCfg = &config.Config{
	Server: "https://a.example",
	Servers: []config.Server{
		{ID: "a", URL: "https://a.example"},
		{ID: "b", URL: "https://b.example"},
	},
}

// serverCandidatesCases is TestServerCandidates' table, hoisted to package
// level to keep the test function within the complexity budget.
//
//nolint:gochecknoglobals // test-only fixture, not runtime-mutable state.
var serverCandidatesCases = []serverCandidatesCase{
	{
		name: "pinned by id",
		cfg:  multiServerTestCfg,
		col:  collection{Namespace: "acme", Name: "widgets", Source: "b"},
		want: []serverCandidate{{base: "https://b.example", id: "b"}},
	},
	{
		name: "pinned by origin, different path",
		cfg:  multiServerTestCfg,
		col:  collection{Namespace: "acme", Name: "widgets", Source: "https://b.example/content/published"},
		want: []serverCandidate{{base: "https://b.example/content/published", id: "b"}},
	},
	{
		name: "pinned, matches nothing",
		cfg:  multiServerTestCfg,
		col:  collection{Namespace: "acme", Name: "widgets", Source: "https://other.example"},
		want: []serverCandidate{{base: "https://other.example"}},
	},
	{
		name: "unpinned walks every server in order, deduplicated",
		cfg: &config.Config{
			Servers: []config.Server{
				{ID: "a", URL: "https://a.example/"},
				{ID: "b", URL: "https://b.example"},
				{ID: "a2", URL: "https://a.example"},
			},
		},
		col: collection{Namespace: "acme", Name: "widgets"},
		want: []serverCandidate{
			{base: "https://a.example", id: "a"},
			{base: "https://b.example", id: "b"},
		},
	},
	{
		name: "Servers empty falls back to cfg.Server",
		cfg:  &config.Config{Server: "https://default.example"},
		col:  collection{Namespace: "acme", Name: "widgets"},
		want: []serverCandidate{{base: "https://default.example"}},
	},
	{
		name: "nil config yields nil",
		cfg:  nil,
		col:  collection{Namespace: "acme", Name: "widgets"},
		want: nil,
	},
}

// TestServerCandidates walks serverCandidatesCases, checking one
// representative scenario per rule serverCandidates documents.
func TestServerCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range serverCandidatesCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := serverCandidates(collectionDeps{cfg: tc.cfg}, tc.col)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("serverCandidates = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestUnmatchedSourceIsWarnedAboutOncePerHost pins that a source: on a host
// configured nowhere warns exactly once, while a match by id or by origin
// stays silent, so a warning that fired for every source would fail.
func TestUnmatchedSourceIsWarnedAboutOncePerHost(t *testing.T) {
	t.Parallel()

	for _, tc := range unmatchedSourceCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			printer := &capturingPrinter{}
			deps := newCollectionDeps(tc.cfg, infra.New(printer, nil), nil)
			// Called twice with the same source, which is what a lockfile
			// pinning several collections to one host produces: the count
			// below is what pins the deduplication, not just the warning.
			serverCandidates(deps, collection{Namespace: "acme", Name: "widgets", Source: tc.source})
			serverCandidates(deps, collection{Namespace: "acme", Name: "gadgets", Source: tc.source})

			var warned int
			for _, w := range printer.warns {
				if strings.Contains(w, "matches no configured Galaxy server") {
					warned++
				}
			}
			if warned != tc.wantWarnings {
				t.Fatalf("%d warnings, want %d: %v", warned, tc.wantWarnings, printer.warns)
			}
		})
	}
}

// unmatchedSourceCase is one row of TestUnmatchedSourceIsWarnedAboutOncePerHost.
type unmatchedSourceCase struct {
	cfg          *config.Config
	name         string
	source       string
	wantWarnings int
}

// unmatchedSourceCases pairs one configured server with the three kinds of
// source: value that can point at it, or not.
func unmatchedSourceCases() []unmatchedSourceCase {
	configured := &config.Config{
		Servers: []config.Server{{ID: "hub", URL: "https://hub.example/api/"}},
	}
	return []unmatchedSourceCase{
		{name: "matched by id", cfg: configured, source: "hub", wantWarnings: 0},
		{
			name:         "matched by origin under a repo-scoped path",
			cfg:          configured,
			source:       "https://hub.example/api/content/published/",
			wantWarnings: 0,
		},
		{
			name:         "host configured nowhere",
			cfg:          configured,
			source:       "https://attacker.example/api/",
			wantWarnings: 1,
		},
	}
}
