package collections_test

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// noteE2ECommit is hand-written, since every fakeCommit value starts with the
// twelve zeros an install line's note would show for any of them.
const (
	noteE2ECommit  = "d4e99adaaa78873baaafbdb953a001315081c0f0"
	noteE2EShort   = "d4e99adaaa78"
	noteRoleURL    = "https://git.example/acme/ansible-role-grafana.git"
	noteCollection = "https://git.example/acme/note.git"
)

// captureLines points f's runtime at a fresh line-capturing printer, keeping
// the fake git client, and returns the printer.
func captureLines(f *gitFixture) *lineCapturingPrinter {
	printer := &lineCapturingPrinter{}
	f.runtime = infra.New(printer, f.galaxy.Client())
	f.runtime.Git = f.git
	return printer
}

// assertExactLine fails unless printer recorded want as a whole line, so a
// note that should be absent cannot hide behind a substring match.
func assertExactLine(t *testing.T, printer *lineCapturingPrinter, run, want string) {
	t.Helper()
	if !slices.Contains(printer.snapshot(), want) {
		t.Fatalf("%s: no line %q in:\n%v", run, want, printer.snapshot())
	}
}

// TestRoleInstallLineNamesTheCommitAMovingRefInstalled pins a branch role's
// note on an install, a skipping rerun and a frozen install; a version-like
// tag gets none.
func TestRoleInstallLineNamesTheCommitAMovingRefInstalled(t *testing.T) {
	t.Parallel()
	f := newRoleFixture(t)
	when := time.Date(2026, time.October, 29, 16, 55, 0, 0, time.FixedZone("", 3*60*60))
	repo := &fakeGitRepo{refs: map[string]string{"HEAD": noteE2ECommit, roleMainRef: noteE2ECommit}}
	repo.addRole(noteE2ECommit, fakeGitRole{roleName: "grafana", when: when})
	f.git.add(noteRoleURL, repo)
	f.writeRequirements(t, "roles:\n  - src: git+"+noteRoleURL+"\n    name: grafana\n"+
		"  - src: git+"+roleBaseURL+"\n    version: v1.0.0\n    name: pinned\n")
	moving := "Installed: role grafana == main (" + noteE2EShort + ", 2026-10-29T13:55:00Z)"
	pinned := "Installed: role pinned == v1.0.0"

	printer := captureLines(f.gitFixture)
	f.mustInstall(t)
	assertExactLine(t, printer, "install", moving)
	assertExactLine(t, printer, "install", pinned)

	printer = captureLines(f.gitFixture)
	f.mustInstall(t)
	if !printer.hasLineContaining("Skipping install, already installed: role grafana@main") {
		t.Fatalf("rerun: grafana was not skipped:\n%v", printer.snapshot())
	}
	assertExactLine(t, printer, "rerun", moving)

	f.lockfile(t)
	if err := os.RemoveAll(f.rolesPath); err != nil {
		t.Fatalf("remove roles: %v", err)
	}
	f.cfg.Frozen = true
	printer = captureLines(f.gitFixture)
	f.mustInstall(t)
	assertExactLine(t, printer, "frozen", moving)
	assertExactLine(t, printer, "frozen", pinned)
}

// TestGitInstallLineNamesTheCommitAMovingRefInstalled pins the note for a git
// collection at HEAD, whose line shows galaxy.yml's version; a version-like tag
// and a Galaxy collection get none.
func TestGitInstallLineNamesTheCommitAMovingRefInstalled(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	f.git.add(noteCollection, &fakeGitRepo{
		refs:    map[string]string{"HEAD": noteE2ECommit, gitMainRef: noteE2ECommit},
		commits: map[string][]fakeGitCollection{noteE2ECommit: {{namespace: "acme", name: "note", version: "1.0.0"}}},
	})
	f.writeRequirements(t, "collections:\n  - git+"+noteCollection+"\n  - git+"+gitAppURL+",v1\n")
	stamp := fakegalaxy.ArtifactModTime().UTC().Format(time.RFC3339)

	for _, run := range []string{"install", "rerun"} {
		printer := captureLines(f)
		f.mustInstall(t)
		if run == "rerun" && !printer.hasLineContaining("Skipping install, already installed: acme/note/1.0.0") {
			t.Fatalf("rerun: acme.note was not skipped:\n%v", printer.snapshot())
		}
		assertExactLine(t, printer, run, "Installed: acme.note == 1.0.0 ("+noteE2EShort+", "+stamp+")")
		assertExactLine(t, printer, run, "Installed: acme.app == 1.2.3")
		assertExactLine(t, printer, run, "Installed: acme.lib == 2.0.0")
	}
}
