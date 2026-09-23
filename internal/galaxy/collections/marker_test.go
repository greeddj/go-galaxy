package collections

// Unit tests for the extract-marker tally, driven against a flat installTarget
// (newFlatInstallTarget, rel ".") rather than the ansible_collections layout,
// so every expected path equals target.path.

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// validMarkerSHA is a syntactically valid 64-character lowercase hex digest
// used wherever a marker test needs a sha that passes helpers.IsSHA256Hex
// without caring what content it is a hash of.
const validMarkerSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// seedValidExtractMarker writes a real marker for target through
// writeExtractMarker, so its tally matches the tree: canSkipInstall rejects a
// marker that merely exists.
func seedValidExtractMarker(t *testing.T, target installTarget, sha string) {
	t.Helper()
	// extractTree creates the marker's directory before it writes the
	// marker; a seed standing in for that extraction does the same.
	if err := target.root.MkdirAll(target.marker, helpers.DirMod); err != nil {
		t.Fatalf("create the marker directory: %v", err)
	}
	if err := writeExtractMarker(target, sha); err != nil {
		t.Fatalf("seed extract marker: %v", err)
	}
}

// mustMkdirAll creates path (and parents), failing the test on error.
func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// buildFixedMarkerTree creates a small deterministic tree and returns a flat
// installTarget at it plus the exact treeTally it must produce.
func buildFixedMarkerTree(t *testing.T) (installTarget, treeTally) {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "file0.txt"), []byte("root-file")) // 9 bytes
	mustMkdirAll(t, filepath.Join(root, "dirA"))
	mustWriteFile(t, filepath.Join(root, "dirA", "file1.txt"), []byte("hello"))  // 5 bytes
	mustWriteFile(t, filepath.Join(root, "dirA", "file2.txt"), []byte("world!")) // 6 bytes
	mustMkdirAll(t, filepath.Join(root, "dirB", "nested"))
	mustWriteFile(t, filepath.Join(root, "dirB", "nested", "file3.txt"), []byte("abc")) // 3 bytes
	// entries: file0, file1, file2, file3 = 4; dirs: dirA, dirB, dirB/nested = 3;
	// bytes: 9 + 5 + 6 + 3 = 23.
	return newFlatInstallTarget(t, root), treeTally{Entries: 4, Dirs: 3, Bytes: 23}
}

// TestExtractMarkerRoundTrip pins the exact on-disk marker bytes as well as the
// round trip, since a format change made on both the write and parse sides
// would still round-trip.
func TestExtractMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	target, want := buildFixedMarkerTree(t)
	const sha = "1111111111111111111111111111111111111111111111111111111111111111"

	if err := writeExtractMarker(target, sha); err != nil {
		t.Fatalf("writeExtractMarker: %v", err)
	}

	marker := filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha)
	got, err := os.ReadFile(marker) //nolint:gosec // marker path is test-controlled
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	wantContent := fmt.Sprintf("go-galaxy-extract-1 entries=%d dirs=%d bytes=%d\n", want.Entries, want.Dirs, want.Bytes)
	if string(got) != wantContent {
		t.Fatalf("marker content = %q, want %q", got, wantContent)
	}

	printer := &capturingPrinter{}
	if !verifyExtractMarker(printer, target, sha) {
		t.Fatalf("expected verifyExtractMarker to accept a freshly written marker")
	}
}

// extractMarkerMutationCase is one row of TestExtractMarkerMutationCases: a
// mutation of a freshly seeded tree, the expected verifyExtractMarker verdict,
// and an optional extra check.
type extractMarkerMutationCase struct {
	mutate     func(t *testing.T, target installTarget)
	extraCheck func(t *testing.T, target installTarget, sha string, printer *capturingPrinter)
	name       string
	wantVerify bool
}

// extractMarkerMutationCases lists the post-extraction mutations the tally
// must catch, plus the equal-size edit it deliberately misses.
func extractMarkerMutationCases() []extractMarkerMutationCase {
	return []extractMarkerMutationCase{
		{
			// A deleted file drops the entry count; the rejection warns and
			// removes the marker.
			name: "deletes a file",
			mutate: func(t *testing.T, target installTarget) {
				t.Helper()
				if err := os.Remove(filepath.Join(target.path, "dirA", "file1.txt")); err != nil {
					t.Fatalf("remove file: %v", err)
				}
			},
			extraCheck: func(t *testing.T, target installTarget, sha string, printer *capturingPrinter) {
				t.Helper()
				assertPathAbsent(t, filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha))
				if len(printer.warns) == 0 {
					t.Fatalf("expected a Warnf line for a current-format tally mismatch, got none")
				}
			},
			wantVerify: false,
		},
		{
			// Proves a file added under the install tree after extraction -
			// e.g. a stray write into the shared cache - is caught the same
			// way a deletion is.
			name: "adds a file",
			mutate: func(t *testing.T, target installTarget) {
				t.Helper()
				mustWriteFile(t, filepath.Join(target.path, "dirA", "extra.txt"), []byte("new"))
			},
			wantVerify: false,
		},
		{
			// The direct answer to "an in-place edit is detected": rewriting a
			// file with different-length content changes its lstat size, which
			// changes the tally's byte sum, which fails the comparison.
			name: "resizes a file",
			mutate: func(t *testing.T, target installTarget) {
				t.Helper()
				mustWriteFile(t, filepath.Join(target.path, "dirA", "file1.txt"), []byte("this content is longer than the original"))
			},
			wantVerify: false,
		},
		{
			// Pins the tally's documented limit: a same-length in-place edit
			// is invisible, since only counts and sizes are compared.
			name: "misses an equal-size edit",
			mutate: func(t *testing.T, target installTarget) {
				t.Helper()
				// "hello" -> "HELLO": identical length (5 bytes), different content.
				mustWriteFile(t, filepath.Join(target.path, "dirA", "file1.txt"), []byte("HELLO"))
			},
			wantVerify: true,
		},
	}
}

// TestExtractMarkerMutationCases checks verifyExtractMarker's verdict for each
// mutation on its own tree; only the deletion row also checks the marker's
// removal and the Warnf line.
func TestExtractMarkerMutationCases(t *testing.T) {
	t.Parallel()

	for _, tc := range extractMarkerMutationCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target, _ := buildFixedMarkerTree(t)
			seedValidExtractMarker(t, target, validMarkerSHA)

			tc.mutate(t, target)

			printer := &capturingPrinter{}
			if got := verifyExtractMarker(printer, target, validMarkerSHA); got != tc.wantVerify {
				t.Fatalf("verifyExtractMarker = %v, want %v", got, tc.wantVerify)
			}
			if tc.extraCheck != nil {
				tc.extraCheck(t, target, validMarkerSHA, printer)
			}
		})
	}
}

// TestExtractMarkerLegacyFormatInvalidates pins that the legacy "ok" marker is
// rejected and removed with a Debugf line only, since every upgraded install
// carries one and a warning would storm.
func TestExtractMarkerLegacyFormatInvalidates(t *testing.T) {
	t.Parallel()
	target, _ := buildFixedMarkerTree(t)
	const sha = "6666666666666666666666666666666666666666666666666666666666666666"
	marker := filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha)
	mustWriteFile(t, marker, []byte("ok"))

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, sha) {
		t.Fatalf("expected verifyExtractMarker to reject the legacy \"ok\" marker")
	}
	if len(printer.warns) != 0 {
		t.Fatalf("expected no Warnf for a legacy marker, got %v", printer.warns)
	}
	if !printer.hasDebugContaining("legacy or corrupt") {
		t.Fatalf("expected a Debugf line reporting the unparseable legacy marker, got %v", printer.debugs)
	}
	assertPathAbsent(t, marker)
}

// TestExtractMarkerOversizedAndGarbageInvalidate pins that an oversized marker
// and a truncated one are both rejected quietly and removed.
func TestExtractMarkerOversizedAndGarbageInvalidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []byte
	}{
		{name: "oversized marker", content: bytes.Repeat([]byte("a"), 300)},
		{name: "truncated marker", content: []byte("go-galaxy-extract-1 entries=x")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, _ := buildFixedMarkerTree(t)
			const sha = "7777777777777777777777777777777777777777777777777777777777777777"
			marker := filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha)
			mustWriteFile(t, marker, tt.content)

			printer := &capturingPrinter{}
			if verifyExtractMarker(printer, target, sha) {
				t.Fatalf("expected verifyExtractMarker to reject %s", tt.name)
			}
			if len(printer.warns) != 0 {
				t.Fatalf("expected no Warnf for an unparseable marker (%s), got %v", tt.name, printer.warns)
			}
			assertPathAbsent(t, marker)
		})
	}
}

// TestExtractMarkerIgnoresSiblingMarkers pins that a top-level file carrying
// the marker prefix, such as another sha's marker, never changes the tally.
func TestExtractMarkerIgnoresSiblingMarkers(t *testing.T) {
	t.Parallel()
	target, want := buildFixedMarkerTree(t)

	before, err := scanTree(target)
	if err != nil {
		t.Fatalf("scanTree before: %v", err)
	}
	if before != want {
		t.Fatalf("scanTree before sibling = %+v, want %+v", before, want)
	}

	mustWriteFile(t, filepath.Join(target.path, helpers.ExtractMarkerPrefix+"deadbeef"), bytes.Repeat([]byte("x"), 128))

	after, err := scanTree(target)
	if err != nil {
		t.Fatalf("scanTree after: %v", err)
	}
	if after != want {
		t.Fatalf("scanTree after sibling marker = %+v, want unchanged %+v", after, want)
	}
}

// TestScanTreeDoesNotFollowSymlinks pins that a symlink to an outside
// directory counts as one entry and is never descended into, so a loop cannot
// hang the walk and a link cannot inflate the tally.
func TestScanTreeDoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "real.txt"), []byte("data"))

	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "a.txt"), []byte("aaaa"))
	mustWriteFile(t, filepath.Join(outside, "b.txt"), []byte("bbbbb"))
	mustMkdirAll(t, filepath.Join(outside, "sub"))

	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	target := newFlatInstallTarget(t, root)
	got, err := scanTree(target)
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}
	// real.txt plus the symlink itself: 2 non-directory entries. The
	// symlinked directory's own contents (2 files, 1 subdirectory) must
	// never be descended into or counted.
	if got.Entries != 2 {
		t.Fatalf("Entries = %d, want 2 (real.txt + the symlink itself, not its target's contents)", got.Entries)
	}
	if got.Dirs != 0 {
		t.Fatalf("Dirs = %d, want 0 (a symlink to a directory must not be descended into)", got.Dirs)
	}
}

// TestExtractMarkerOutcomeZeroValueFailsClosed pins that a zero
// extractMarkerOutcome never matches, so a path that forgets to set status
// forces re-extraction.
func TestExtractMarkerOutcomeZeroValueFailsClosed(t *testing.T) {
	t.Parallel()
	if (extractMarkerOutcome{}).matches() {
		t.Fatal("expected a zero-value extractMarkerOutcome{} to never satisfy matches()")
	}
}

// TestCheckExtractMarkerScanFailedAndMissing covers a scan failure and an
// absent marker: checkExtractMarker leaves the marker alone, while
// verifyExtractMarker logs at Debugf and removes it.
func TestCheckExtractMarkerScanFailedAndMissing(t *testing.T) {
	t.Run("scan failed", testCheckExtractMarkerScanFailed)
	t.Run("missing", testCheckExtractMarkerMissing)
}

// testCheckExtractMarkerScanFailed makes the tree unreadable after seeding a
// marker and pins that the scan failure is reported, logged at Debugf rather
// than Warnf, and the marker still removed.
func testCheckExtractMarkerScanFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	installPath := t.TempDir()
	target := newFlatInstallTarget(t, installPath)
	const sha = "deadbeefcafedeadbeefcafedeadbeefcafedeadbeefcafedeadbeefcafedead"
	seedValidExtractMarker(t, target, sha)
	markerPath := filepath.Join(installPath, helpers.ExtractMarkerPrefix+sha)

	// 0o311 (no read bit) makes scanTree's listing fail while the write bit
	// still lets the marker be unlinked, so the removal assertion is real.
	//nolint:gosec // G302: 0o311 is this test's own fixture permission; see comment above.
	if err := os.Chmod(installPath, 0o311); err != nil {
		t.Fatalf("chmod installPath: %v", err)
	}
	// Restored before t.TempDir's own cleanup runs, which needs to list (and
	// remove) installPath itself.
	t.Cleanup(func() {
		if err := os.Chmod(installPath, helpers.DirMod); err != nil {
			t.Errorf("restore installPath perms: %v", err)
		}
	})

	assertScanFailedOutcome(t, checkExtractMarker(target, sha), markerPath)
	assertVerifyExtractMarkerScanFailed(t, target, sha, markerPath)
}

// assertScanFailedOutcome checks checkExtractMarker's own return value for
// the scan-failed scenario, and that it never touched the marker file - it
// is a pure read.
func assertScanFailedOutcome(t *testing.T, outcome extractMarkerOutcome, markerPath string) {
	t.Helper()
	if outcome.status != extractMarkerScanFailed {
		t.Fatalf("outcome.status = %v, want extractMarkerScanFailed", outcome.status)
	}
	if outcome.scanErr == nil {
		t.Fatal("expected a non-nil scanErr")
	}
	if outcome.matches() {
		t.Fatal("expected matches() to be false")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected checkExtractMarker to leave the marker untouched, stat error: %v", err)
	}
}

// assertVerifyExtractMarkerScanFailed checks that verifyExtractMarker rejects
// the scan-failed tree with a Debugf line and no Warnf, and removes the marker.
func assertVerifyExtractMarkerScanFailed(t *testing.T, target installTarget, sha, markerPath string) {
	t.Helper()
	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, sha) {
		t.Fatal("expected verifyExtractMarker to return false")
	}
	wantMsg := "failed to scan " + target.path + " to verify its extract marker"
	if !printer.hasDebugContaining(wantMsg) {
		t.Errorf("expected a Debugf line containing %q, got %v", wantMsg, printer.debugs)
	}
	if len(printer.warns) != 0 {
		t.Errorf("expected no Warnf line for a scan failure, got %v", printer.warns)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("expected verifyExtractMarker's best-effort removal to have actually removed the marker, stat error = %v", err)
	}
}

// testCheckExtractMarkerMissing pins that an absent marker, the first-ever
// extraction's shape, is reported by checkExtractMarker and logged at Debugf
// by verifyExtractMarker.
func testCheckExtractMarkerMissing(t *testing.T) {
	installPath := t.TempDir()
	target := newFlatInstallTarget(t, installPath)
	const sha = "deadbeefcafedeadbeefcafedeadbeefcafedeadbeefcafedeadbeefcafedead"
	markerPath := filepath.Join(installPath, helpers.ExtractMarkerPrefix+sha)

	outcome := checkExtractMarker(target, sha)
	if outcome.status != extractMarkerMissing {
		t.Fatalf("outcome.status = %v, want extractMarkerMissing", outcome.status)
	}
	if outcome.matches() {
		t.Fatal("expected matches() to be false")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("checkExtractMarker must never create a marker; stat error = %v", err)
	}

	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, sha) {
		t.Fatal("expected verifyExtractMarker to return false")
	}
	wantMsg := "extract marker missing or unreadable at " + markerPath
	if !printer.hasDebugContaining(wantMsg) {
		t.Errorf("expected a Debugf line containing %q, got %v", wantMsg, printer.debugs)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("expected no marker to exist (there was nothing to remove), stat error = %v", err)
	}
}

// TestPrefetchScanUsesCheapCheck pins that shouldSchedulePrefetch uses
// installRecordMatches (marker presence only), not canSkipInstall's tally: a
// legacy "ok" marker still reads as installed there.
func TestPrefetchScanUsesCheapCheck(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0", Type: "galaxy"}
	installPath := filepath.Join(root, "ansible_collections", col.Namespace, col.Name)
	mustMkdirAll(t, installPath)

	const installedSHA = "8888888888888888888888888888888888888888888888888888888888888888"
	infoDir := filepath.Join(root, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	mustMkdirAll(t, infoDir)
	mustWriteFile(t, collectionMarkerPath(installPath, col, installedSHA), []byte("ok"))
	mustWriteFile(t, filepath.Join(infoDir, "GALAXY.yml"), sidecarFor(col))

	cfg := &config.Config{DownloadPath: root, Workers: 1}
	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    installPath,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})
	testRoot := newTestCollectionsRoot(t, root)
	deps := newPrefetchDeps(cfg, infra.New(noopPrinter{}, http.DefaultClient), st, &presenceArtifacts{}, testRoot)

	if schedule, _ := shouldSchedulePrefetch(t.Context(), deps, col); schedule {
		t.Fatalf("expected shouldSchedulePrefetch to report already-installed via the cheap check, even with a legacy marker")
	}
}

// buildTraversalFixture returns sandbox, containment and an installPath four
// elements below it: a naive join of six ".." (one fused into the prefix, one
// canceling it) lands at containment, and a leading "./" reaches sandbox.
func buildTraversalFixture(t *testing.T) (string, string, string) {
	t.Helper()
	sandbox := t.TempDir()
	containment := filepath.Join(sandbox, "root")
	installPath := filepath.Join(containment, "collections", "ansible_collections", "ns", "name")
	mustMkdirAll(t, installPath)
	return sandbox, containment, installPath
}

// TestVerifyExtractMarkerRefusesTraversalSHA pins that a traversal sha is
// refused before any path is built: the victim a naive join would reach
// survives, and exactly one Warnf and no Debugf is emitted.
func TestVerifyExtractMarkerRefusesTraversalSHA(t *testing.T) {
	t.Parallel()
	_, containment, installPath := buildTraversalFixture(t)
	target := newFlatInstallTarget(t, installPath)

	victim := filepath.Join(containment, "home", "ci", ".ssh", "authorized_keys")
	const victimContent = "ssh-ed25519 AAAA... ci@legit\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	const traversalSHA = "../../../../../../home/ci/.ssh/authorized_keys"
	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, traversalSHA) {
		t.Fatal("expected verifyExtractMarker to reject a traversal sha")
	}
	assertFileContent(t, victim, victimContent)
	if len(printer.warns) != 1 {
		t.Fatalf("expected exactly one Warnf line, got %v", printer.warns)
	}
	if len(printer.debugs) != 0 {
		t.Fatalf("expected no Debugf line for an unsafe sha, got %v", printer.debugs)
	}
}

// TestVerifyExtractMarkerRefusesUnboundedTraversalSHA covers the leading "./"
// variant, whose naive join escapes one level past containment to sandbox.
func TestVerifyExtractMarkerRefusesUnboundedTraversalSHA(t *testing.T) {
	t.Parallel()
	sandbox, _, installPath := buildTraversalFixture(t)
	target := newFlatInstallTarget(t, installPath)

	victim := filepath.Join(sandbox, "etc", "passwd")
	const victimContent = "root:x:0:0:root:/root:/bin/sh\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	const traversalSHA = "./../../../../../../etc/passwd"
	printer := &capturingPrinter{}
	if verifyExtractMarker(printer, target, traversalSHA) {
		t.Fatal("expected verifyExtractMarker to reject an unbounded traversal sha")
	}
	assertFileContent(t, victim, victimContent)
	if len(printer.warns) != 1 {
		t.Fatalf("expected exactly one Warnf line, got %v", printer.warns)
	}
}

// TestWriteExtractMarkerRefusesTraversalSHA pins that writeExtractMarker
// refuses a traversal sha with helpers.ErrMalformedArtifactSHA256 before
// touching the filesystem.
func TestWriteExtractMarkerRefusesTraversalSHA(t *testing.T) {
	t.Parallel()
	_, containment, installPath := buildTraversalFixture(t)
	target := newFlatInstallTarget(t, installPath)

	victim := filepath.Join(containment, "home", "ci", ".ssh", "authorized_keys")
	const victimContent = "ssh-ed25519 AAAA... ci@legit\n"
	mustMkdirAll(t, filepath.Dir(victim))
	mustWriteFile(t, victim, []byte(victimContent))

	const traversalSHA = "../../../../../../home/ci/.ssh/authorized_keys"
	err := writeExtractMarker(target, traversalSHA)
	// t.Errorf, not t.Fatalf: the filesystem assertions below must still run
	// when the error class is wrong.
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Errorf("writeExtractMarker error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	assertFileContent(t, victim, victimContent)

	entries, readErr := os.ReadDir(installPath)
	if readErr != nil {
		t.Fatalf("read installPath: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected installPath to remain empty (no marker written), got %v", entries)
	}
}

// TestCheckExtractMarkerReportsUnsafeSHA pins that checkExtractMarker reports
// extractMarkerUnsafeSHA for a traversal sha and leaves installPath untouched.
func TestCheckExtractMarkerReportsUnsafeSHA(t *testing.T) {
	t.Parallel()
	_, _, installPath := buildTraversalFixture(t)
	target := newFlatInstallTarget(t, installPath)

	before, err := os.ReadDir(installPath)
	if err != nil {
		t.Fatalf("read installPath before: %v", err)
	}

	const traversalSHA = "../../../../../../home/ci/.ssh/authorized_keys"
	outcome := checkExtractMarker(target, traversalSHA)
	if outcome.status != extractMarkerUnsafeSHA {
		t.Fatalf("outcome.status = %v, want extractMarkerUnsafeSHA", outcome.status)
	}
	if outcome.matches() {
		t.Fatal("expected matches() to be false")
	}

	after, err := os.ReadDir(installPath)
	if err != nil {
		t.Fatalf("read installPath after: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("installPath directory listing changed: before=%v after=%v", before, after)
	}
}

// TestMarkerRelRejectsNonDigest pins that markerRel accepts only 64 lowercase
// hex characters and joins a valid digest under target.rel.
func TestMarkerRelRejectsNonDigest(t *testing.T) {
	t.Parallel()
	installPath := filepath.Join(t.TempDir(), "ansible_collections", "ns", "name")
	mustMkdirAll(t, installPath)
	target := newFlatInstallTarget(t, installPath)

	badCases := []struct {
		name string
		sha  string
	}{
		{"empty", ""},
		{"short but hex", "deadbeef"},
		{"uppercase", strings.ToUpper(validMarkerSHA)},
		{"multi-element path", "a/b"},
		{"double dot", ".."},
		{"traversal", "../../../../../../home/ci/.ssh/authorized_keys"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := markerRel(target, tc.sha); ok {
				t.Fatalf("markerRel(%q, %q) ok = true, want false", installPath, tc.sha)
			}
		})
	}

	got, ok := markerRel(target, validMarkerSHA)
	if !ok {
		t.Fatalf("markerRel with a valid 64-hex sha: ok = false, want true")
	}
	want := path.Join(target.rel, helpers.ExtractMarkerPrefix+validMarkerSHA)
	if got != want {
		t.Fatalf("markerRel = %q, want %q", got, want)
	}
}

// BenchmarkScanTree times one scanTree pass over a collection-sized tree: 500
// files of about 2000 bytes across 50 subdirectories.
func BenchmarkScanTree(b *testing.B) {
	root := b.TempDir()
	const subdirs = 50
	const filesPerSubdir = 10
	const fileSize = 2000
	content := bytes.Repeat([]byte("x"), fileSize)
	for i := range subdirs {
		dir := filepath.Join(root, fmt.Sprintf("sub%d", i))
		if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
			b.Fatalf("mkdir %s: %v", dir, err)
		}
		for j := range filesPerSubdir {
			p := filepath.Join(dir, fmt.Sprintf("file%d.dat", j))
			if err := os.WriteFile(p, content, helpers.FileMod); err != nil {
				b.Fatalf("write %s: %v", p, err)
			}
		}
	}

	osRoot, err := os.OpenRoot(root)
	if err != nil {
		b.Fatalf("os.OpenRoot(%s): %v", root, err)
	}
	b.Cleanup(func() {
		_ = osRoot.Close()
	})
	target := installTarget{root: osRoot, rel: ".", path: root}

	for b.Loop() {
		if _, err := scanTree(target); err != nil {
			b.Fatalf("scanTree: %v", err)
		}
	}
}

// collectionMarkerPath is where a collection at installPath keeps its marker
// for sha: the version's .info directory under ansible_collections, not the
// install directory.
func collectionMarkerPath(installPath string, col collection, sha string) string {
	infoDir := filepath.Join(filepath.Dir(filepath.Dir(installPath)), col.Namespace+"."+col.Name+"-"+col.Version+infoDirSuffix)
	return filepath.Join(infoDir, helpers.ExtractMarkerPrefix+sha)
}
