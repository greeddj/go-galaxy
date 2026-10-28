package extractmarker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// checkSHA is the sha every Check case below writes its marker under.
const checkSHA = "6666666666666666666666666666666666666666666666666666666666666666"

// TestRelRefusesAnUnsafeSHA pins that Rel builds no path for a sha that is not
// sha256 hex, a traversal included, and joins a valid one under dir.
func TestRelRefusesAnUnsafeSHA(t *testing.T) {
	t.Parallel()
	for _, sha := range []string{"", "../../etc/passwd", strings.Repeat("g", 64), strings.Repeat("a", 63)} {
		if rel, ok := Rel("dir", sha); ok || rel != "" {
			t.Errorf("Rel(dir, %q) = (%q, %t), want (\"\", false)", sha, rel, ok)
		}
	}
	if rel, ok := Rel("dir", checkSHA); !ok || rel != "dir/"+helpers.ExtractMarkerPrefix+checkSHA {
		t.Errorf("Rel(dir, valid) = (%q, %t)", rel, ok)
	}
}

// TestParseIsStrict pins that only Format's exact output parses: a wrong tag
// or field count, a renamed key, a bad count and trailing garbage are refused.
func TestParseIsStrict(t *testing.T) {
	t.Parallel()
	want := Tally{Entries: 4, Dirs: 3, Bytes: 23}
	if got, ok := parse(Format(want)); !ok || got != want {
		t.Fatalf("parse(Format(%+v)) = (%+v, %t)", want, got, ok)
	}
	for _, content := range []string{
		"ok\n",
		"go-galaxy-extract-0 entries=4 dirs=3 bytes=23\n",
		"go-galaxy-extract-1 entries=4 dirs=3\n",
		"go-galaxy-extract-1 entries=4 dirs=3 bytes=23 extra=1\n",
		"go-galaxy-extract-1 files=4 dirs=3 bytes=23\n",
		"go-galaxy-extract-1 entries=4 folders=3 bytes=23\n",
		"go-galaxy-extract-1 entries=4 dirs=3 size=23\n",
		"go-galaxy-extract-1 entries=-4 dirs=3 bytes=23\n",
		"go-galaxy-extract-1 entries=4 dirs=0x3 bytes=23\n",
		"go-galaxy-extract-1 entries=4 dirs=3 bytes=99999999999999999999\n",
		"go-galaxy-extract-1 entries=4 dirs=3 bytes=23x\n",
		"go-galaxy-extract-1  entries=4 dirs=3 bytes=23\n",
	} {
		if got, ok := parse(content); ok {
			t.Errorf("parse(%q) = %+v, want a refusal", content, got)
		}
	}
}

// checkCase is one Check outcome: what to plant at the marker path and in the
// tree, and the status Check must report.
type checkCase struct {
	plant func(t *testing.T, dir string, tally Tally)
	name  string
	sha   string
	tree  string
	want  Status
}

func checkCases() []checkCase {
	writeMarker := func(content string) func(t *testing.T, dir string, _ Tally) {
		return func(t *testing.T, dir string, _ Tally) {
			t.Helper()
			writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+checkSHA), []byte(content))
		}
	}
	formatted := func(t *testing.T, dir string, tally Tally) {
		t.Helper()
		writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+checkSHA), []byte(Format(tally)))
	}
	return []checkCase{
		{name: "matching marker", plant: formatted, want: StatusMatches},
		{name: "unsafe sha", sha: "../" + checkSHA, plant: formatted, want: StatusUnsafeSHA},
		{name: "no marker", plant: func(*testing.T, string, Tally) {}, want: StatusMissing},
		{name: "oversized marker", plant: writeMarker(strings.Repeat("x", maxReadSize+1)), want: StatusMissing},
		{
			name: "directory at the marker path",
			plant: func(t *testing.T, dir string, _ Tally) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(dir, helpers.ExtractMarkerPrefix+checkSHA), helpers.DirMod); err != nil {
					t.Fatalf("mkdir marker: %v", err)
				}
			},
			want: StatusMissing,
		},
		{name: "legacy marker", plant: writeMarker("ok\n"), want: StatusMalformed},
		{name: "tree that does not exist", tree: "absent", plant: formatted, want: StatusScanFailed},
		{
			name: "file grown in place",
			plant: func(t *testing.T, dir string, tally Tally) {
				t.Helper()
				formatted(t, dir, tally)
				writeTestFile(t, filepath.Join(dir, "file0.txt"), []byte("root-file, now longer"))
			},
			want: StatusDrifted,
		},
	}
}

// TestCheckOutcomes pins the status Check reports for each way a marker can
// fail to vouch for a tree, and when ScanErr and the tallies are set.
func TestCheckOutcomes(t *testing.T) {
	t.Parallel()
	for _, tc := range checkCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, tally := buildFixedTree(t)
			tc.plant(t, dir, tally)
			sha, tree := tc.sha, tc.tree
			if sha == "" {
				sha = checkSHA
			}
			if tree == "" {
				tree = "."
			}

			assertCheckOutcome(t, Check(openTestRoot(t, dir), ".", sha, tree), tc.want, tally)
		})
	}
}

// assertCheckOutcome checks got against the wanted status, with ScanErr set
// only on a failed scan and the drift case carrying the recorded tally.
func assertCheckOutcome(t *testing.T, got Outcome, want Status, tally Tally) {
	t.Helper()
	if got.Status != want {
		t.Fatalf("Check status = %d, want %d (outcome %+v)", got.Status, want, got)
	}
	if got.Matches() != (want == StatusMatches) {
		t.Fatalf("Matches() = %t for status %d", got.Matches(), got.Status)
	}
	if (got.ScanErr != nil) != (want == StatusScanFailed) {
		t.Fatalf("ScanErr = %v for status %d", got.ScanErr, got.Status)
	}
	if want == StatusScanFailed && !errors.Is(got.ScanErr, fs.ErrNotExist) {
		t.Fatalf("ScanErr = %v, want fs.ErrNotExist", got.ScanErr)
	}
	if want == StatusDrifted && (got.Want != tally || got.Got.Bytes != tally.Bytes+12) {
		t.Fatalf("drift = want %+v got %+v, want the recorded %+v and 12 more bytes", got.Want, got.Got, tally)
	}
}

// TestScanSkipsATopLevelMarkerDirectory pins that a top-level directory with
// the marker prefix is skipped whole, contents included, while one deeper is
// counted like any other directory.
func TestScanSkipsATopLevelMarkerDirectory(t *testing.T) {
	t.Parallel()
	dir, want := buildFixedTree(t)
	writeTestFile(t, filepath.Join(dir, helpers.ExtractMarkerPrefix+"dir", "hidden.txt"), []byte("hidden"))
	root := openTestRoot(t, dir)

	got, err := Scan(root, ".")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != want {
		t.Fatalf("Scan = %+v, want %+v (the marker directory must not count)", got, want)
	}

	writeTestFile(t, filepath.Join(dir, "dirA", helpers.ExtractMarkerPrefix+"nested", "seen.txt"), []byte("seen"))
	got, err = Scan(root, ".")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if wantNested := (Tally{Entries: want.Entries + 1, Dirs: want.Dirs + 1, Bytes: want.Bytes + 4}); got != wantNested {
		t.Fatalf("Scan = %+v, want %+v (a nested marker-named directory counts)", got, wantNested)
	}
}

// TestScanReportsAMissingTree pins that Scan returns the walk's error and a
// zero tally rather than a partial count.
func TestScanReportsAMissingTree(t *testing.T) {
	t.Parallel()
	got, err := Scan(openTestRoot(t, t.TempDir()), "absent")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Scan error = %v, want fs.ErrNotExist", err)
	}
	if got != (Tally{}) {
		t.Fatalf("Scan tally = %+v, want zero beside an error", got)
	}
}
