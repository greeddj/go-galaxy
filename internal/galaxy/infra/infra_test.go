package infra

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// recordingPrinter is a minimal output.Printer stub that records every
// Debugf line verbatim (format expanded via fmt semantics), so a test can
// assert on the exact rendered bytes rather than on call shape alone.
type recordingPrinter struct {
	debugLines []string
}

func (p *recordingPrinter) Printf(string, ...any)                        {}
func (p *recordingPrinter) PersistentPrintf(string, ...any)              {}
func (p *recordingPrinter) Okf(string, ...any)                           {}
func (p *recordingPrinter) OkVersionf(string, string, ...any)            {}
func (p *recordingPrinter) Updatef(string, ...any)                       {}
func (p *recordingPrinter) Errorf(string, ...any)                        {}
func (p *recordingPrinter) ErrorVersionf(string, string, string, ...any) {}
func (p *recordingPrinter) Warnf(string, ...any)                         {}

func (p *recordingPrinter) Debugf(format string, args ...any) {
	p.debugLines = append(p.debugLines, fmt.Sprintf(format, args...))
}

func (p *recordingPrinter) DebugSincef(time.Time, string, ...any) {}

// assertContainsAll fails the test unless line contains every one of want,
// kept apart so the caller stays under the cyclomatic-complexity budget.
func assertContainsAll(t *testing.T, line string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(line, w) {
			t.Fatalf("debug line %q missing %q", line, w)
		}
	}
}

// TestDebugAnsibleConfigReportsServerListWithoutLeakingToken pins that each
// server's debug line names its id, URL and TLS policy and reports the token
// only as a presence boolean.
func TestDebugAnsibleConfigReportsServerListWithoutLeakingToken(t *testing.T) {
	t.Parallel()
	const secretToken = "tok3n-must-not-appear-in-debug-output"

	printer := &recordingPrinter{}
	i := New(printer, nil)
	cfg := &config.Config{
		Servers: []config.Server{
			{ID: "a", URL: "https://a.example", Token: config.NewSecret(secretToken)},
			{ID: "b", URL: "https://b.example", InsecureSkipTLSVerify: true},
			{URL: "https://c.example"},
		},
	}

	i.DebugAnsibleConfig(cfg)

	if len(printer.debugLines) != 3 {
		t.Fatalf("expected 3 debug lines, got %d: %v", len(printer.debugLines), printer.debugLines)
	}
	for _, line := range printer.debugLines {
		if strings.Contains(line, secretToken) {
			t.Fatalf("debug line leaked the token: %q", line)
		}
	}

	assertContainsAll(t, printer.debugLines[0], `"a"`, "url=https://a.example", "token=true", "insecure_skip_tls_verify=false")
	assertContainsAll(t, printer.debugLines[1], `"b"`, "url=https://b.example", "token=false", "insecure_skip_tls_verify=true")
	assertContainsAll(t, printer.debugLines[2], `""`, "url=https://c.example", "token=false", "insecure_skip_tls_verify=false")
}

// TestDebugAnsibleConfigReportsURLCredentialsWithoutLeakingToken pins the url
// credential debug line: id, binding URL and kind, never the token - not
// even as a presence boolean, since a binding without a token cannot exist.
func TestDebugAnsibleConfigReportsURLCredentialsWithoutLeakingToken(t *testing.T) {
	t.Parallel()
	const secretToken = "url-tok3n-must-not-appear-in-debug-output" //nolint:gosec // the fixture under test, not a credential

	printer := &recordingPrinter{}
	i := New(printer, nil)
	prefix, err := urlsource.ParsePrefix("https://artifacts.example/org")
	if err != nil {
		t.Fatalf("ParsePrefix: %v", err)
	}
	cfg := &config.Config{
		URLCredentials: []config.URLCredential{{ID: "hub", URL: prefix, Token: config.NewSecret(secretToken)}},
	}

	i.DebugAnsibleConfig(cfg)

	if len(printer.debugLines) != 1 {
		t.Fatalf("expected 1 debug line, got %d: %v", len(printer.debugLines), printer.debugLines)
	}
	line := printer.debugLines[0]
	if strings.Contains(line, secretToken) {
		t.Fatalf("debug line leaked the token: %q", line)
	}
	assertContainsAll(t, line, `"hub"`, "url=https://artifacts.example/org", "kind=bearer")
	if strings.Contains(line, "token=") {
		t.Fatalf("debug line renders token presence: %q", line)
	}
}

// TestDebugAnsibleConfigNilSafe checks that the nil-guard contract
// (nil Infra, nil Output, nil cfg) holds even though the method does
// more than the ansible.cfg-sourced branch.
func TestDebugAnsibleConfigNilSafe(t *testing.T) {
	t.Parallel()
	var nilInfra *Infra
	nilInfra.DebugAnsibleConfig(&config.Config{})

	i := &Infra{}
	i.DebugAnsibleConfig(&config.Config{})
	i.DebugAnsibleConfig(nil)
}

// TestArtifactDeadlineDefaultsToTheConstantAndHonorsAnOverride pins that a
// positive override is honored, while a nil Infra, a zero value, one from New
// or a negative override reports the constant, so no Infra gets a zero budget.
func TestArtifactDeadlineDefaultsToTheConstantAndHonorsAnOverride(t *testing.T) {
	t.Parallel()

	var nilInfra *Infra

	cases := []struct {
		infra *Infra
		name  string
		want  time.Duration
	}{
		{name: "New sets the real constant", infra: New(&recordingPrinter{}, nil), want: helpers.ArtifactDownloadDeadline},
		{name: "zero-value Infra falls back to the constant", infra: &Infra{}, want: helpers.ArtifactDownloadDeadline},
		{name: "nil Infra falls back to the constant", infra: nilInfra, want: helpers.ArtifactDownloadDeadline},
		{
			name:  "a negative override falls back to the constant",
			infra: &Infra{ArtifactDownloadDeadline: -1},
			want:  helpers.ArtifactDownloadDeadline,
		},
		{
			name:  "a positive override is honored verbatim",
			infra: &Infra{ArtifactDownloadDeadline: 7 * time.Millisecond},
			want:  7 * time.Millisecond,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.infra.ArtifactDeadline(); got != tc.want {
				t.Errorf("ArtifactDeadline() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestSignatureDeadlineFallsBackAndHonorsAnOverride pins the same fallback for
// SignatureDeadline, whose nil and zero-value rows keep
// verifyCollectionSignatures from wrapping a zero budget around its gather.
func TestSignatureDeadlineFallsBackAndHonorsAnOverride(t *testing.T) {
	t.Parallel()

	var nilInfra *Infra
	if got := nilInfra.SignatureDeadline(); got != helpers.SignatureFetchDeadline {
		t.Errorf("nil Infra: SignatureDeadline() = %s, want %s", got, helpers.SignatureFetchDeadline)
	}
	if got := (&Infra{}).SignatureDeadline(); got != helpers.SignatureFetchDeadline {
		t.Errorf("zero-value Infra: SignatureDeadline() = %s, want %s", got, helpers.SignatureFetchDeadline)
	}
	if got := New(&recordingPrinter{}, nil).SignatureDeadline(); got != helpers.SignatureFetchDeadline {
		t.Errorf("New: SignatureDeadline() = %s, want %s", got, helpers.SignatureFetchDeadline)
	}
	if got := (&Infra{SignatureFetchDeadline: -1}).SignatureDeadline(); got != helpers.SignatureFetchDeadline {
		t.Errorf("negative override: SignatureDeadline() = %s, want %s", got, helpers.SignatureFetchDeadline)
	}
	if got := (&Infra{SignatureFetchDeadline: 7 * time.Millisecond}).SignatureDeadline(); got != 7*time.Millisecond {
		t.Errorf("positive override: SignatureDeadline() = %s, want %s", got, 7*time.Millisecond)
	}
}
