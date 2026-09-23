package collections_test

// End-to-end coverage of the version solver through collections.Start: the
// installed version is the solver's own answer, and a conflict, online or
// offline, surfaces the solver's proof.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// e2eVersion200 is a fixture version literal, a const to satisfy goconst.
const e2eVersion200 = "2.0.0"

// writeRequirementsWithConstraint writes a requirements.yml at path requiring
// the one collection name at constraint.
func writeRequirementsWithConstraint(t *testing.T, path, name, constraint string) {
	t.Helper()
	content := "collections:\n  - name: " + name + "\n    version: \"" + constraint + "\"\n"
	if err := os.WriteFile(path, []byte(content), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}
}

// TestPlainInstallResolvesThroughSolverVersionSelection pins that install
// picks the highest satisfying dependency version, and that it is exactly what
// an independent solver.Solve over MetadataProvider answers.
func TestPlainInstallResolvesThroughSolverVersionSelection(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	f.server.AddVersion("acme", "lib", "1.5.0", nil)
	f.server.AddVersion("acme", "lib", e2eVersion200, nil)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
	assertManifestInstalled(t, f.downloadPath, "lib")
	installedLibVersion := readManifestVersion(t, f.downloadPath, "lib")

	provider := collections.NewMetadataProvider(f.cfg, f.runtime, store.New(), nil)
	result, err := solver.Solve(context.Background(), []solver.Requirement{{Package: "acme.app", Constraint: "*"}}, provider)
	if err != nil {
		t.Fatalf("solver.Solve: %v", err)
	}
	wantLibVersion := result.Versions["acme.lib"]
	if wantLibVersion == "" {
		t.Fatalf("solver.Solve did not resolve acme.lib at all: %v", result.Versions)
	}
	if installedLibVersion != wantLibVersion {
		t.Fatalf("installed acme.lib version = %q, want the solver's own choice %q", installedLibVersion, wantLibVersion)
	}
	if installedLibVersion != e2eVersion200 {
		t.Fatalf("installed acme.lib version = %q, want 2.0.0 (the highest registered version satisfying >=1.0.0)", installedLibVersion)
	}
}

// TestConflictingRequirementsSurfaceSolverProof pins that disjoint dependency
// ranges fail Start with the solver's *solver.ConflictError and its proof,
// exit code ExitResolution, before anything is installed.
func TestConflictingRequirementsSurfaceSolverProof(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")
	writeRequirementsMulti(t, reqPath, "acme.x", "acme.y")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "x", "1.0.0", map[string]string{"acme.z": "^1.0.0"})
	s.AddVersion("acme", "y", "1.0.0", map[string]string{"acme.z": "^2.0.0"})
	s.AddVersion("acme", "z", "1.0.0", nil)
	s.AddVersion("acme", "z", e2eVersion200, nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}
	runtime := infra.New(noopPrinter{}, s.Client())

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected a conflict, got a successful install")
	}
	var conflictErr *solver.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Start error is not a *solver.ConflictError: %v (%T)", err, err)
	}
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(err, ErrNoVersionSatisfiesConstraints) = false")
	}
	if got := exitcode.FromError(err); got != exitcode.ExitResolution {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitResolution (%d)", got, exitcode.ExitResolution)
	}
	if proof := conflictErr.Error(); !strings.Contains(proof, "version solving failed") {
		t.Fatalf("proof %q does not contain \"version solving failed\"", proof)
	}
	assertPathAbsent(t, installPathFor(downloadPath, "x"))
	assertPathAbsent(t, installPathFor(downloadPath, "y"))
}

// TestOfflineConflictCarriesTheOfflineNote pins that an offline conflict over
// cached metadata is a *solver.ConflictError carrying the offline note and
// still classified as ExitResolution.
func TestOfflineConflictCarriesTheOfflineNote(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")

	s := fakegalaxy.New(t)
	s.AddVersion("acme", "thing", "1.0.0", nil)
	s.AddVersion("acme", "thing", "1.5.0", nil)

	cfg := &config.Config{
		Server:           s.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}
	runtime := infra.New(noopPrinter{}, s.Client())

	// "!=1.5.0" excludes highest_version, forcing the full versions list into
	// the cache for the offline run below.
	writeRequirementsWithConstraint(t, reqPath, "acme.thing", "!=1.5.0")
	if err := collections.Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("first Start (online, populate the cache): %v", err)
	}
	assertManifestInstalled(t, downloadPath, "thing")

	// Now require a version no cached candidate satisfies, and go offline:
	// the solver must determine this is unsatisfiable using only the
	// already-cached versions list, never touching the network.
	writeRequirementsWithConstraint(t, reqPath, "acme.thing", ">=2.0.0")
	cfg.Offline = true
	runtime.HTTP = fetch.NewOffline(cfg.Timeout)
	if err := os.RemoveAll(downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the offline conflict run: %v", err)
	}

	err := collections.Start(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatal("expected an offline conflict, got a successful install")
	}
	if _, ok := errors.AsType[*solver.ConflictError](err); !ok {
		t.Fatalf("Start error is not a *solver.ConflictError: %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "offline mode restricts resolution to cached metadata") {
		t.Fatalf("error %q does not carry the offline note", err.Error())
	}
	if !errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) {
		t.Fatalf("errors.Is(err, ErrNoVersionSatisfiesConstraints) = false")
	}
	if got := exitcode.FromError(err); got != exitcode.ExitResolution {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitResolution (%d)", got, exitcode.ExitResolution)
	}
}
