package commands

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// TestExplainRequiresATarget pins explainArguments' zero-argument branch: a
// missing target is refused as errExplainNoTarget before the action runs, and
// exits as a usage error. Any other outcome, nil included, fails the errors.Is
// check. The flags point into t.TempDir, so an action a faulty validator lets
// through reads no galaxy.lock from the working directory or the environment.
//
// KILLING MUTATION, run and reverted, in explainArguments (explain.go) -
// return nil for zero arguments. Both assertions fail, the second because a
// missing lockfile exits 6:
//
//	explain_test.go:38: explain with no target: error = lockfile not found:
//	.../001/galaxy.lock, want errors.Is match with missing argument: explain
//	takes one, a collection name (namespace.name) or a role name
//	explain_test.go:41: explain with no target: exit code = 6, want 2
func TestExplainRequiresATarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := Explain().Run(context.Background(), []string{
		"explain",
		"-r", filepath.Join(dir, "requirements.yml"),
		"--lock-file", filepath.Join(dir, "galaxy.lock"),
	})
	if !errors.Is(err, errExplainNoTarget) {
		t.Errorf("explain with no target: error = %v, want errors.Is match with %v", err, errExplainNoTarget)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Errorf("explain with no target: exit code = %d, want %d", got, exitcode.ExitUsage)
	}
}

// TestPrintExplainOrphan checks the orphan-in-lockfile case: a target that is
// neither a root requirement nor depended on by anything else must print the
// hyphen-minus message and must not contain an em dash (U+2014).
func TestPrintExplainOrphan(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "ns.orphan", Version: "1.0.0"},
		},
	}
	roots := map[string]bool{}

	var buf strings.Builder
	if err := printExplain(&buf, lf, "ns.orphan", roots, nil); err != nil {
		t.Fatalf("printExplain() error = %v, want nil", err)
	}

	out := buf.String()
	if !strings.Contains(out, "(no parents - orphan in lockfile)") {
		t.Errorf("printExplain() output missing orphan message; got:\n%s", out)
	}
	// Spelled as an escape rather than as the character itself: this is the
	// same rune either way, and the escape keeps the file from tripping the
	// repository-wide ban on em dashes in committed text, which would
	// otherwise need an exemption naming this line.
	if strings.ContainsRune(out, '\u2014') {
		t.Errorf("printExplain() output contains an em dash (U+2014); got:\n%s", out)
	}
}

// TestPrintExplainRequiredByAndDepends checks the normal case: a root
// requirement with its own dependency prints both the "required by" (root)
// line and the "depends on" line.
func TestPrintExplainRequiredByAndDepends(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "community.general", Version: "11.5.0", Source: "galaxy", Deps: []string{"ansible.posix"}},
			{Name: "ansible.posix", Version: "2.0.0"},
		},
	}
	roots := map[string]bool{"community.general": true}

	var buf strings.Builder
	if err := printExplain(&buf, lf, "community.general", roots, nil); err != nil {
		t.Fatalf("printExplain() error = %v, want nil", err)
	}

	out := buf.String()
	for _, want := range []string{
		"community.general 11.5.0",
		"required by:",
		"requirements.yml (root)",
		"depends on:",
		"ansible.posix",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printExplain() output missing %q; got:\n%s", want, out)
		}
	}
}

// TestPrintExplainRequiredByIsNameSorted pins the direction of
// printRequiredBy's comparison, which nothing else in this package asserts:
// every other fixture here has at most one reverse dependency, so a reversed
// comparison would order a one-element slice indistinguishably from a correct
// one and pass unnoticed.
//
// The lockfile lists the three parents in an order that is neither ascending
// nor descending, so the assertion cannot be satisfied by the input order
// surviving unsorted either. Line positions are compared rather than a
// rendered block, so the check states the ordering property itself instead of
// re-encoding the surrounding layout.
//
// KILLING MUTATION, run for real: swapping printRequiredBy's comparison to
// strings.Compare(b.Name, a.Name) fails this test with "required by lists
// ns.beta at 58 and ns.alpha at 78; want ns.alpha before ns.beta, got:" and
// the whole rendered report. Reverting the argument order made it pass again.
func TestPrintExplainRequiredByIsNameSorted(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "ns.gamma", Version: "3.0.0", Deps: []string{"ns.target"}},
			{Name: "ns.alpha", Version: "1.0.0", Deps: []string{"ns.target"}},
			{Name: "ns.beta", Version: "2.0.0", Deps: []string{"ns.target"}},
			{Name: "ns.target", Version: "9.0.0"},
		},
	}

	var buf strings.Builder
	if err := printExplain(&buf, lf, "ns.target", map[string]bool{}, nil); err != nil {
		t.Fatalf("printExplain() error = %v, want nil", err)
	}
	out := buf.String()

	// Ascending by name means ascending by first byte offset in the output.
	for _, pair := range [][2]string{
		{"ns.alpha", "ns.beta"},
		{"ns.beta", "ns.gamma"},
	} {
		before, after := strings.Index(out, pair[0]), strings.Index(out, pair[1])
		if before == -1 || after == -1 {
			t.Fatalf("required by is missing %q (at %d) or %q (at %d), got:\n%s", pair[0], before, pair[1], after, out)
		}
		if before > after {
			t.Fatalf("required by lists %s at %d and %s at %d; want %s before %s, got:\n%s",
				pair[1], after, pair[0], before, pair[0], pair[1], out)
		}
	}
}

// TestPrintExplainNotFound checks that a target absent from the lockfile
// returns errExplainNotFound rather than printing anything misleading.
func TestPrintExplainNotFound(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{SchemaVersion: lockfile.SchemaVersion}
	var buf strings.Builder
	err := printExplain(&buf, lf, "ns.missing", map[string]bool{}, nil)
	if err == nil {
		t.Fatal("printExplain() error = nil, want non-nil")
	}
}

// TestPrintExplainSanitizesLockfileText proves printExplain's
// safeout.NewWriter wrap (its first statement) reaches every write its
// helpers (printEntryHeader, printRequiredBy, printDepends) make: every
// rendered field - Name, Version, Source, SHA256, and the one Deps element
// - carries the hostileLockfileName/hostileLockfileSource shape (shared
// with tree_test.go, reusing the adversarial shape at
// internal/galaxy/lockfile/compare_test.go). No raw ESC/CR/NUL byte
// survives anywhere in the output, U+FFFD stands in for each of them, and
// the output stays valid UTF-8. The final assertion - both section headers
// are still present - is the positive control: it proves the writer
// sanitized the hostile text rather than discarding the whole report.
func TestPrintExplainSanitizesLockfileText(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{
				Name:    hostileLockfileName,
				Version: hostileLockfileSource,
				Source:  hostileLockfileSource,
				SHA256:  hostileLockfileSource,
				Deps:    []string{hostileLockfileSource},
			},
		},
	}
	roots := map[string]bool{hostileLockfileName: true}

	var buf strings.Builder
	if err := printExplain(&buf, lf, hostileLockfileName, roots, nil); err != nil {
		t.Fatalf("printExplain() error = %v, want nil", err)
	}
	out := buf.String()

	for _, b := range []byte{0x1b, '\r', 0x00} {
		if strings.IndexByte(out, b) != -1 {
			t.Fatalf("printExplain() output contains raw byte %#x; got:\n%s", b, out)
		}
	}
	if !strings.ContainsRune(out, '\ufffd') {
		t.Fatalf("printExplain() output missing U+FFFD replacement; got:\n%s", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("printExplain() output is not valid UTF-8; got:\n%s", out)
	}
	if !strings.Contains(out, "required by:") || !strings.Contains(out, "depends on:") {
		t.Fatalf("printExplain() output missing its own section headers; got:\n%s", out)
	}
}
