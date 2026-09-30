package collections_test

// This file pins that a version constraint semver refuses at the resolve, from
// requirements.yml or from a server's metadata, is named without the userinfo
// of a URL in it, on the solving path and under --frozen alike.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// credentialedConstraint is a URL with a password where a version constraint
// belongs; it is concatenated so no literal reads as a hardcoded credential.
const credentialedConstraint = "https://u:" + "s3cret@h.example/x"

// assertConstraintRefusalHidesUserinfo fails the test unless err names the
// refused constraint by its host and path and carries no password.
func assertConstraintRefusalHidesUserinfo(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Start succeeded, want a refused constraint")
	}
	if msg := err.Error(); !strings.Contains(msg, "h.example/x") || strings.Contains(msg, "s3cret") {
		t.Fatalf("Start error = %q, want the constraint named without its password", msg)
	}
}

// writeCredentialedRoot points f's requirements.yml at acme.app with the
// credentialed constraint quoted, the shape load leaves to the resolve.
func writeCredentialedRoot(t *testing.T, f *e2eFixture) {
	t.Helper()
	content := "collections:\n  - name: acme.app\n    version: \"" + credentialedConstraint + "\"\n"
	if err := os.WriteFile(f.cfg.RequirementsFile, []byte(content), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
}

// TestResolveRefusedRootConstraintHidesUserinfo pins the solving path for a
// constraint written in requirements.yml.
func TestResolveRefusedRootConstraintHidesUserinfo(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	writeCredentialedRoot(t, f)
	assertConstraintRefusalHidesUserinfo(t, collections.Start(context.Background(), f.cfg, f.runtime))
}

// TestResolveRefusedDependencyConstraintHidesUserinfo pins the solving path
// for a constraint a server's metadata names for a dependency.
func TestResolveRefusedDependencyConstraintHidesUserinfo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirements(t, reqPath, "acme.app")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "app", testVersion100, map[string]string{"acme.dep": credentialedConstraint})
	s.AddVersion("acme", "dep", testVersion100, nil)
	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}
	assertConstraintRefusalHidesUserinfo(t, collections.Start(context.Background(), cfg, infra.New(noopPrinter{}, s.Client())))
}

// TestFrozenRefusedConstraintHidesUserinfo pins the frozen root check: the
// refusal is still a lockfile mismatch, and names no password.
func TestFrozenRefusedConstraintHidesUserinfo(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	newFrozenPinFixture(t, f)
	writeCredentialedRoot(t, f)
	err := collections.Start(context.Background(), f.cfg, f.runtime)
	assertConstraintRefusalHidesUserinfo(t, err)
	if !errors.Is(err, helpers.ErrLockfileMismatch) {
		t.Fatalf("Start error = %v, want ErrLockfileMismatch", err)
	}
}
