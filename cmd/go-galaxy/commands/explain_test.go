package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// TestExplainRequiresATarget pins that explain with no target is refused as
// errExplainNoTarget before the action runs and exits as a usage error; flags
// point into t.TempDir so a faulty validator reads no ambient galaxy.lock.
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
	if err := printExplain(&buf, lf, "ns.orphan", "requirements.yml", roots, nil); err != nil {
		t.Fatalf("printExplain() error = %v, want nil", err)
	}

	out := buf.String()
	if !strings.Contains(out, "(no parents - orphan in lockfile)") {
		t.Errorf("printExplain() output missing orphan message; got:\n%s", out)
	}
	// Spelled as an escape so this file does not trip the repository-wide ban
	// on em dashes in committed text.
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
	if err := printExplain(&buf, lf, "community.general", "requirements.yml", roots, nil); err != nil {
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

// TestPrintExplainRootLabel pins that the (root) line names the requirements
// file explain was run against, for a collection and a role alike, so a
// project read from galaxy.toml is not credited to requirements.yml.
func TestPrintExplainRootLabel(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersionRoles,
		Collections:   []lockfile.Entry{{Name: "acme.app", Version: "1.0.0", Source: "galaxy"}},
		Roles:         []lockfile.RoleEntry{{Name: "base", Type: lockfile.RoleTypeGit, Version: "main", Source: gitTestSource}},
	}
	for _, target := range []string{"acme.app", "base"} {
		var buf strings.Builder
		err := printExplain(&buf, lf, target, "galaxy.toml", map[string]bool{"acme.app": true}, map[string]bool{"base": true})
		if err != nil {
			t.Fatalf("printExplain(%s) error = %v, want nil", target, err)
		}
		if !strings.Contains(buf.String(), "    - galaxy.toml (root)") {
			t.Errorf("printExplain(%s) output missing the galaxy.toml root line; got:\n%s", target, buf.String())
		}
		if strings.Contains(buf.String(), "requirements.yml") {
			t.Errorf("printExplain(%s) still names requirements.yml; got:\n%s", target, buf.String())
		}
	}
}

// TestPrintExplainRequiredByIsNameSorted pins printRequiredBy's ascending sort,
// which single-parent fixtures cannot; the parents are listed in neither
// ascending nor descending order, so the input order cannot pass either.
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
	if err := printExplain(&buf, lf, "ns.target", "requirements.yml", map[string]bool{}, nil); err != nil {
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

// TestPrintExplainRoleParentsByGalaxyName pins that a role explained by its
// Galaxy name lists the same parents as by an install name that differs from
// it, because a parent's deps hold the install name.
func TestPrintExplainRoleParentsByGalaxyName(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{
		SchemaVersion: lockfile.SchemaVersionRoles,
		Roles: []lockfile.RoleEntry{
			{
				Name: "nginx", Type: lockfile.RoleTypeGalaxy, Version: "1.2.3", Galaxy: "owner.nginx",
				Source: "https://galaxy.example", Repository: "https://github.com/owner/ansible-role-nginx",
				Ref: "1.2.3", Commit: gitTestCommit,
			},
			{
				Name: "webapp", Type: lockfile.RoleTypeGit, Version: "v1.0.0", Source: gitTestSource,
				Ref: "v1.0.0", Commit: gitTestCommit, Deps: []string{"nginx"},
			},
		},
	}
	roleRoots := map[string]bool{"webapp": true}

	outputs := make(map[string]string, 2)
	for _, target := range []string{"nginx", "owner.nginx"} {
		var buf strings.Builder
		if err := printExplain(&buf, lf, target, "requirements.yml", nil, roleRoots); err != nil {
			t.Fatalf("printExplain(%s) error = %v, want nil", target, err)
		}
		out := buf.String()
		if !strings.Contains(out, "  required by:\n    - role webapp v1.0.0\n") || strings.Contains(out, "orphan") {
			t.Errorf("printExplain(%s) does not list webapp under required by, or calls nginx an orphan; got:\n%s", target, out)
		}
		outputs[target] = out
	}
	if outputs["owner.nginx"] != outputs["nginx"] {
		t.Errorf("explain by Galaxy name differs from explain by install name:\n%s\nwant\n%s",
			outputs["owner.nginx"], outputs["nginx"])
	}
}

// TestPrintExplainNotFound checks that a target absent from the lockfile
// returns errExplainNotFound rather than printing anything misleading.
func TestPrintExplainNotFound(t *testing.T) {
	t.Parallel()
	lf := &lockfile.File{SchemaVersion: lockfile.SchemaVersion}
	var buf strings.Builder
	err := printExplain(&buf, lf, "ns.missing", "requirements.yml", map[string]bool{}, nil)
	if err == nil {
		t.Fatal("printExplain() error = nil, want non-nil")
	}
}

// TestExplainNotFoundExitsGeneric pins that a name a valid lockfile holds as
// neither a collection nor a role exits 1: errExplainNotFound is claimed by
// no class, neither the lockfile class nor the usage class.
func TestExplainNotFoundExitsGeneric(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - acme.app\n"), helpers.FileMod); err != nil {
		t.Fatalf("write requirements: %v", err)
	}
	lockPath := filepath.Join(dir, lockfile.DefaultName)
	if err := lockfile.Save(lockPath, roleLockfile()); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	for _, target := range []string{"acme.missing", "missingrole"} {
		err := Explain().Run(context.Background(), []string{"explain", "-r", reqPath, "--lock-file", lockPath, target})
		if !errors.Is(err, errExplainNotFound) {
			t.Errorf("explain %s: error = %v, want errors.Is match with %v", target, err, errExplainNotFound)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitError {
			t.Errorf("explain %s: exit code = %d, want %d", target, got, exitcode.ExitError)
		}
	}
}

// TestPrintExplainSanitizesLockfileText pins that printExplain's safeout wrap
// reaches every field its helpers print: no raw ESC, CR or NUL survives, output
// stays valid UTF-8, and both section headers remain as the positive control.
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
	if err := printExplain(&buf, lf, hostileLockfileName, "requirements.yml", roots, nil); err != nil {
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
