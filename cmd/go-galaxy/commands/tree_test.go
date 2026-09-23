package commands

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// hostileLockfileName and hostileLockfileSource are a path-traversal name and
// a value carrying NUL, an ANSI escape and CRLF, bytes a hand-edited lockfile
// can put in any Entry string field; the lockfile package tests share the shape.
const (
	hostileLockfileName   = "../../../../etc/passwd"
	hostileLockfileSource = "https://x.example\x00\x1b[31m\r\nInstalled: totally.fine"
)

// TestPrintTree checks that the header line prints the actual requirements
// path passed in (not a hardcoded "requirements.yml"), and that the tree body
// reflects the dependency structure.
func TestPrintTree(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "community.general", Version: "11.5.0", Deps: []string{"ansible.posix"}},
			{Name: "ansible.posix", Version: "2.0.0"},
			{Name: "ansible.utils", Version: "6.0.2"},
		},
	}
	roots := []string{"community.general", "ansible.utils"}
	reqPath := "custom/dir/req.yml"

	var buf strings.Builder
	printTree(&buf, reqPath, lf, roots)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("printTree produced no output")
	}
	if lines[0] != reqPath {
		t.Errorf("first output line = %q, want %q", lines[0], reqPath)
	}

	out := buf.String()
	wantRows := []string{
		"community.general 11.5.0",
		"ansible.posix 2.0.0",
		"ansible.utils 6.0.2",
	}
	for _, want := range wantRows {
		if !strings.Contains(out, want) {
			t.Errorf("printTree() output missing row %q; got:\n%s", want, out)
		}
	}
}

// TestPrintTreeMissingDependency checks that a dependency absent from the
// lockfile is flagged inline instead of being silently skipped.
func TestPrintTreeMissingDependency(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "community.general", Version: "11.5.0", Deps: []string{"ns.ghost"}},
		},
	}
	roots := []string{"community.general"}

	var buf strings.Builder
	printTree(&buf, "requirements.yml", lf, roots)

	if !strings.Contains(buf.String(), "ns.ghost (missing in lockfile)") {
		t.Errorf("printTree() output missing dependency marker; got:\n%s", buf.String())
	}
}

// TestPrintTreeSanitizesLockfileText pins that printTree's safeout wrap reaches
// the root line and the "(missing in lockfile)" branch: no raw ESC, CR or NUL,
// valid UTF-8, and the branch decoration kept as the positive control.
func TestPrintTreeSanitizesLockfileText(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: hostileLockfileName, Version: "1.0.0", Deps: []string{hostileLockfileSource}},
		},
	}
	roots := []string{hostileLockfileName}

	var buf strings.Builder
	printTree(&buf, "requirements.yml", lf, roots)
	out := buf.String()

	for _, b := range []byte{0x1b, '\r', 0x00} {
		if strings.IndexByte(out, b) != -1 {
			t.Fatalf("printTree() output contains raw byte %#x; got:\n%s", b, out)
		}
	}
	if !strings.ContainsRune(out, '\ufffd') {
		t.Fatalf("printTree() output missing U+FFFD replacement; got:\n%s", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("printTree() output is not valid UTF-8; got:\n%s", out)
	}
	if !strings.Contains(out, "\u2514\u2500\u2500 ") {
		t.Fatalf("printTree() output missing its own branch decoration; got:\n%s", out)
	}
}
