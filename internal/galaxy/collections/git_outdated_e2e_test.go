package collections_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// TestGitOutdatedReportsCommitDrift asserts that once the locked branch moves,
// Outdated reports the git entry as drift between two full commits, reports the
// Galaxy dependency as up to date, and advertises to the remote exactly once.
func TestGitOutdatedReportsCommitDrift(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.writeRequirements(t, "collections:\n  - git+"+gitAppURL+",main\n")
	lf := f.lockfile(t)
	// The Galaxy dependency beside the git entry is what sets the schema.
	if lf.SchemaVersion != lockfile.SchemaVersionDownloadURL {
		t.Fatalf("schema = %d, want %d", lf.SchemaVersion, lockfile.SchemaVersionDownloadURL)
	}
	lib := findLockEntry(t, lf, "acme.lib")

	f.git.repos[gitAppURL].refs[gitMainRef] = fakeCommit("app-2")
	f.git.resetCounts()
	printer := &lineCapturingPrinter{}
	f.runtime = infra.New(printer, f.galaxy.Client())
	f.runtime.Git = f.git
	f.cfg.MetricsFile = filepath.Join(t.TempDir(), "metrics.json")
	// Verbose, because the lib's up-to-date line is printed only then.
	f.cfg.Verbose = true
	if err := collections.Outdated(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Outdated: %v", err)
	}

	wantApp := "Outdated: acme.app " + fakeCommit("app-1") + " -> " + fakeCommit("app-2")
	if !printer.hasLineContaining(wantApp) {
		t.Fatalf("report lacks %q:\n%v", wantApp, printer.snapshot())
	}
	if wantLib := "Up to date: acme.lib == " + lib.Version; !printer.hasLineContaining(wantLib) {
		t.Fatalf("report lacks %q:\n%v", wantLib, printer.snapshot())
	}
	lockPath := lockfile.ResolveDefaultPath(f.reqPath, "")
	if want := lockPath + ": 1 up to date, 1 outdated, 0 failed"; !printer.hasLineContaining(want) {
		t.Fatalf("report lacks summary %q:\n%v", want, printer.snapshot())
	}
	if adv, acq := f.git.counts(); adv != 1 || acq != 0 {
		t.Fatalf("outdated reached the remote advertises=%d acquires=%d, want 1 and 0", adv, acq)
	}
	assertMetricsCommand(t, f.cfg.MetricsFile, "outdated")
}
