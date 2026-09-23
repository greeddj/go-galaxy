package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustReadFile reads path and fails the test on error, giving gosec's G304 a
// single call site to annotate.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	//nolint:gosec // path is built from this test's own t.TempDir fixture, never external input.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return data
}

// TestWriteEmptyPathIsNoOp pins that Write("", ...) returns nil without
// touching the filesystem, so callers can pass cfg.MetricsFile unconditionally.
func TestWriteEmptyPathIsNoOp(t *testing.T) {
	t.Parallel()

	if err := Write("", Report{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// TestWriteProducesReadableReport asserts Write creates any missing parent
// directory and produces a report that round-trips through JSON.
func TestWriteProducesReadableReport(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "sub", "metrics.json")

	r := Report{
		Command:     "install",
		Server:      "https://galaxy.ansible.com",
		Collections: 3,
		Failures:    0,
		StartedAt:   time.Unix(1000, 0).UTC(),
		FinishedAt:  time.Unix(1010, 0).UTC(),
	}
	if err := Write(path, r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data := mustReadFile(t, path)
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Command != r.Command {
		t.Fatalf("Command = %q, want %q", got.Command, r.Command)
	}
	if got.Server != r.Server {
		t.Fatalf("Server = %q, want %q", got.Server, r.Server)
	}
	if got.Collections != r.Collections {
		t.Fatalf("Collections = %d, want %d", got.Collections, r.Collections)
	}
}

// TestWriteReplacesSymlinkTarget pins that a symlink planted at the metrics
// path is replaced by a regular report file and its target left untouched, so
// a Write regressed to os.WriteFile fails here.
func TestWriteReplacesSymlinkTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	sentinel := []byte("do-not-touch")
	// 0o600: this is scratch fixture content, not the operator-facing report
	// this test is about, so it does not need to match FileMod.
	if err := os.WriteFile(victim, sentinel, 0o600); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}

	path := filepath.Join(dir, "metrics.json")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	r := Report{Command: "warm", Collections: 1}
	if err := Write(path, r); err != nil {
		t.Fatalf("Write: %v", err)
	}

	gotVictim := mustReadFile(t, victim)
	if string(gotVictim) != string(sentinel) {
		t.Fatalf("victim content = %q, want unchanged %q", gotVictim, sentinel)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("metrics path is still a symlink after Write")
	}

	data := mustReadFile(t, path)
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Command != r.Command {
		t.Fatalf("Command = %q, want %q", got.Command, r.Command)
	}
}
