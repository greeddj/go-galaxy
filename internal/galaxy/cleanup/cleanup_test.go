package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// noopPrinter is an output.Printer stub for tests that need an Infra but do
// not care about the rendered output.
type noopPrinter struct{}

func (noopPrinter) Printf(string, ...any)                        {}
func (noopPrinter) PersistentPrintf(string, ...any)              {}
func (noopPrinter) Okf(string, ...any)                           {}
func (noopPrinter) OkVersionf(string, string, ...any)            {}
func (noopPrinter) Updatef(string, ...any)                       {}
func (noopPrinter) Errorf(string, ...any)                        {}
func (noopPrinter) ErrorVersionf(string, string, string, ...any) {}
func (noopPrinter) Warnf(string, ...any)                         {}
func (noopPrinter) Debugf(string, ...any)                        {}
func (noopPrinter) DebugSincef(time.Time, string, ...any)        {}

// recordingPrinter is an output.Printer stub that records Warnf, Printf and
// Errorf lines so a test can assert a warning, a dry-run report line or a
// non-fatal error surfaced. It is safe for concurrent use.
type recordingPrinter struct {
	noopPrinter

	warnings []string
	prints   []string
	errs     []string
	mu       sync.Mutex
}

func (p *recordingPrinter) Warnf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warnings = append(p.warnings, fmt.Sprintf(format, args...))
}

func (p *recordingPrinter) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prints = append(p.prints, fmt.Sprintf(format, args...))
}

func (p *recordingPrinter) Errorf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, fmt.Sprintf(format, args...))
}

// hasWarningContaining reports whether any recorded warning contains substr.
func (p *recordingPrinter) hasWarningContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// hasPrintContaining reports whether any recorded Printf line contains substr.
func (p *recordingPrinter) hasPrintContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.prints {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasErrorContaining reports whether any recorded Errorf line contains substr.
func (p *recordingPrinter) hasErrorContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.errs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// testManifestJSON is a minimal, valid MANIFEST.json for ns.name@1.0.0. The
// scan reads only its version; namespace and name come from the walked dirs.
const testManifestJSON = `{
	"collection_info": {
		"namespace": "ns",
		"name": "name",
		"version": "1.0.0"
	}
}`

// TestStartAbortsOnCorruptRegistryBeforeDeletion pins that an undecodable
// project registry aborts Start through the real local backend with
// ErrCorruptProjectRegistry (ExitCacheCorrupt) before anything is deleted.
func TestStartAbortsOnCorruptRegistryBeforeDeletion(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	writeCorruptRegistry(t, cacheDir)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitCacheCorrupt {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitCacheCorrupt (%d)", got, exitcode.ExitCacheCorrupt)
	}
	assertManifestSurvives(t, installDir)
}

// TestStartTakesNoOpPathOnEmptyRegistry pins that an empty registry takes
// Start's no-op branch, deleting nothing, and that the early return still
// releases the instance lock.
func TestStartTakesNoOpPathOnEmptyRegistry(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected nil error for an empty registry, got %v", err)
	}
	assertManifestSurvives(t, installDir)
	assertLockIsFree(t, cfg, runtime)
}

// assertLockIsFree proves the cache dir's instance lock is acquirable, which
// it is only if a prior Start released it. It opens a fresh backend, acquires
// the lock, and releases it again.
func assertLockIsFree(t *testing.T, cfg *config.Config, runtime *infra.Infra) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build a fresh backend: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open a fresh backend: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close the fresh backend: %v", err)
		}
	}()
	_, release, err := backend.Lock(t.Context())
	if err != nil {
		t.Fatalf("expected the lock to be free after an empty-registry cleanup, got %v", err)
	}
	if err := release(); err != nil {
		t.Errorf("failed to release the re-acquired lock: %v", err)
	}
}

// TestInitCleanupCacheBackendNewFailure pins that a cacheBackend.New failure
// (a nil config) propagates out of Start before anything is opened or locked.
func TestInitCleanupCacheBackendNewFailure(t *testing.T) {
	t.Parallel()
	runtime := newTestRuntime()
	if err := Start(t.Context(), nil, runtime); err == nil {
		t.Fatal("expected Start to fail when cfg is nil")
	}
}

// TestInitCleanupOpenFailure pins that backend.Open failing (the cache dir's
// parent is a regular file) propagates out of Start and leaves that file intact.
func TestInitCleanupOpenFailure(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "afile")
	if err := os.WriteFile(blocker, []byte("x"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write blocker file: %v", err)
	}

	cfg := &config.Config{CacheDir: filepath.Join(blocker, "sub"), DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err == nil {
		t.Fatal("expected Start to fail when the cache dir's parent is a regular file")
	}
	info, statErr := os.Stat(blocker)
	if statErr != nil {
		t.Fatalf("expected the blocker file to survive, stat error: %v", statErr)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("expected the blocker to remain a regular file, got mode %v", info.Mode())
	}
}

// TestInitCleanupLockFailure pins that a lock already held on the cache dir
// fails Start with ErrAnotherInstanceIsRunning (ExitCacheBusy) and that Start
// succeeds once it is released; flock conflicts per open file, not per process.
func TestInitCleanupLockFailure(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	holder, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build the lock-holding backend: %v", err)
	}
	if err := holder.Open(t.Context()); err != nil {
		t.Fatalf("failed to open the lock-holding backend: %v", err)
	}
	_, release, err := holder.Lock(t.Context())
	if err != nil {
		t.Fatalf("failed to acquire the holding lock: %v", err)
	}

	startErr := Start(t.Context(), cfg, runtime)
	if !errors.Is(startErr, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("expected ErrAnotherInstanceIsRunning, got %v", startErr)
	}
	if got := exitcode.FromError(startErr); got != exitcode.ExitCacheBusy {
		t.Fatalf("exitcode.FromError(startErr) = %d, want ExitCacheBusy (%d)", got, exitcode.ExitCacheBusy)
	}

	if err := release(); err != nil {
		t.Fatalf("failed to release the holding lock: %v", err)
	}
	if err := holder.Close(t.Context()); err != nil {
		t.Fatalf("failed to close the lock-holding backend: %v", err)
	}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once the lock is released, got %v", err)
	}
}

// TestInitCleanupLoadStoreFailure pins that a garbage Bolt file fails Start
// with ErrCorruptSnapshotStore (ExitCacheCorrupt), still releases the lock,
// and that Start succeeds once the file is removed.
func TestInitCleanupLoadStoreFailure(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}
	dbPath := filepath.Join(cacheDir, helpers.StoreDBLocal)
	if err := os.WriteFile(dbPath, []byte("not a bolt database"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write a garbage store db: %v", err)
	}

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if err == nil {
		t.Fatal("expected Start to fail when the local store db is corrupt")
	}
	if !errors.Is(err, helpers.ErrCorruptSnapshotStore) {
		t.Fatalf("expected ErrCorruptSnapshotStore, got %v", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitCacheCorrupt {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitCacheCorrupt (%d)", got, exitcode.ExitCacheCorrupt)
	}
	assertLockIsFree(t, cfg, runtime)
	assertInitCleanupReturnsHolderContext(t, cfg, runtime)

	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("failed to remove the corrupt store db: %v", err)
	}
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once the corrupt store db is removed, got %v", err)
	}
}

// assertInitCleanupReturnsHolderContext pins that a failing initCleanup still
// returns the lock's holder context, which runCleanup hands to
// cacheManager.LockLostError to tell a stolen lock from a bad snapshot.
func assertInitCleanupReturnsHolderContext(t *testing.T, cfg *config.Config, runtime *infra.Infra) {
	t.Helper()
	ctx := t.Context()
	lockCtx, state, err := initCleanup(ctx, cfg, runtime)
	if err == nil {
		t.Fatalf("expected initCleanup to fail against the corrupt store db, got state %+v", state)
	}
	if lockCtx != ctx {
		t.Fatalf("initCleanup returned holder context %v, want the ctx it was handed", lockCtx)
	}
}

// newTestRuntime builds an Infra wired with a no-op printer and the default
// HTTP client, matching the pattern used by the collections package's own
// tests for constructing an Infra without caring about rendered output.
func newTestRuntime() *infra.Infra {
	return newTestRuntimeWith(nil)
}

// newTestRuntimeWith builds an Infra wired with printer, or with noopPrinter
// when printer is nil, and the default HTTP client.
func newTestRuntimeWith(printer *recordingPrinter) *infra.Infra {
	if printer == nil {
		return infra.New(noopPrinter{}, http.DefaultClient)
	}
	return infra.New(printer, http.DefaultClient)
}

// openTestWorkspace opens collectionsPath's os.Root the way
// openProjectWorkspace does, for tests calling the rooted scan functions
// directly. The caller must close the returned workspace's root.
func openTestWorkspace(t *testing.T, collectionsPath string) workspace {
	t.Helper()
	root, err := os.OpenRoot(collectionsPath)
	if err != nil {
		t.Fatalf("failed to open root at %s: %v", collectionsPath, err)
	}
	return workspace{root: root, fsys: root.FS(), path: collectionsPath}
}

// seedInstallTree writes ns.name@1.0.0's MANIFEST.json under
// <downloadPath>/ansible_collections/ns/name and returns that install dir.
func seedInstallTree(t *testing.T, downloadPath string) string {
	t.Helper()
	installDir := filepath.Join(downloadPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir: %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}
	return installDir
}

// writeCorruptRegistry writes unparseable bytes directly at the project
// registry path, bypassing store.RecordProject (which only ever round-trips
// valid JSON) so the file is corrupt from the very first read.
func writeCorruptRegistry(t *testing.T, cacheDir string) {
	t.Helper()
	path := filepath.Join(cacheDir, helpers.StoreDBProjects)
	if err := os.WriteFile(path, []byte("{invalid"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write corrupt registry: %v", err)
	}
}

// assertManifestSurvives fails the test unless installDir's MANIFEST.json
// is still present, proving no deletion touched it.
func assertManifestSurvives(t *testing.T, installDir string) {
	t.Helper()
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected the pre-seeded install tree to survive, stat error: %v", err)
	}
}

// writeProjectRegistry writes registry as the project registry JSON under
// cacheDir, bypassing store.RecordProject so the test controls every field.
func writeProjectRegistry(t *testing.T, cacheDir string, registry *store.ProjectRegistry) {
	t.Helper()
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatalf("failed to marshal project registry: %v", err)
	}
	path := filepath.Join(cacheDir, helpers.StoreDBProjects)
	if err := os.WriteFile(path, data, helpers.FileMod); err != nil {
		t.Fatalf("failed to write project registry: %v", err)
	}
}

// writeOutsideSentinel creates a file at outsideRoot/name/keep.txt,
// simulating a target that a successful path-traversal exploit could reach
// if it escaped the collections tree, and returns the file's path.
func writeOutsideSentinel(t *testing.T, outsideRoot, name string) string {
	t.Helper()
	sentinelDir := filepath.Join(outsideRoot, name)
	if err := os.MkdirAll(sentinelDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create outside sentinel dir: %v", err)
	}
	sentinelFile := filepath.Join(sentinelDir, "keep.txt")
	if err := os.WriteFile(sentinelFile, []byte("keep me"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write outside sentinel file: %v", err)
	}
	return sentinelFile
}

// writeMaliciousManifestTree seeds a MANIFEST.json under downloadPath whose
// version is a path-traversal payload, and returns the manifest's path.
func writeMaliciousManifestTree(t *testing.T, downloadPath string) string {
	t.Helper()
	installDir := filepath.Join(downloadPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir: %v", err)
	}
	maliciousManifest := `{
		"collection_info": {
			"namespace": "ns",
			"name": "name",
			"version": "../../../../pwn"
		}
	}`
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(maliciousManifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write malicious MANIFEST.json: %v", err)
	}
	return manifestPath
}

// registerCleanupProject writes a project registry entry pointing at
// downloadPath with an empty requirements file (nothing reachable), so that
// any indexed collection would be a removal candidate.
func registerCleanupProject(t *testing.T, cacheDir, downloadPath string) {
	t.Helper()
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)
}

// registerCleanupProjectAt registers a project at downloadPath whose
// requirements file is reqPath, which the caller may leave missing or corrupt.
func registerCleanupProjectAt(t *testing.T, cacheDir, downloadPath, reqPath string) {
	t.Helper()
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"proj": {
				RequirementsFile: reqPath,
				CollectionsPath:  downloadPath,
				LastRun:          time.Now().UTC(),
			},
		},
	})
}

// TestRemoveInstalledRejectsTraversalVersion pins that a manifest version
// carrying a traversal payload is rejected at ingestion with a warning, so
// neither the collection's own tree nor anything outside it is deleted.
func TestRemoveInstalledRejectsTraversalVersion(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	sentinelFile := writeOutsideSentinel(t, t.TempDir(), "pwn")
	manifestPath := writeMaliciousManifestTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	if _, err := os.Stat(sentinelFile); err != nil {
		t.Fatalf("expected outside sentinel to survive, stat error: %v", err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected the collection's own install dir to survive since ingestion rejected it, stat error: %v", err)
	}
	if !printer.hasWarningContaining("unsafe identifier") {
		t.Fatalf("expected a warning about an unsafe identifier, got: %v", printer.warnings)
	}
}

// TestRemoveUnusedCannotForgeAReportLine pins that a manifest version carrying
// a newline is rejected at ingestion by helpers.IsPathElement, so it can never
// forge a report line; an ordinary sibling collection is the removal control.
func TestRemoveUnusedCannotForgeAReportLine(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	const forgedVersion = "1.0.0\nforged plain-text line"
	seedManifestAt(t, downloadPath, "ns", "hostile", forgedVersion)
	seedManifestAt(t, downloadPath, "ns", "ordinary", "1.0.0")
	registerCleanupProject(t, cacheDir, downloadPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	// Checked first: a raw newline in any recorded line is the forged-line
	// defect itself, reported before its downstream symptoms.
	for _, line := range append(append([]string{}, printer.warnings...), printer.prints...) {
		if strings.Contains(line, "\n") {
			t.Fatalf("recorded output line contains a raw newline, forged-line defect is not closed: %q", line)
		}
	}

	// This is what actually closes the defect: the hostile collection was
	// never indexed, so its own on-disk tree was never a removal candidate
	// and survives untouched.
	hostileManifest := filepath.Join(downloadPath, "ansible_collections", "ns", "hostile", "MANIFEST.json")
	if _, err := os.Stat(hostileManifest); err != nil {
		t.Fatalf("expected the hostile collection's install dir to survive since ingestion rejected it, stat error: %v", err)
	}
	if !printer.hasWarningContaining("unsafe identifier") {
		t.Fatalf("expected a warning about an unsafe identifier, got: %v", printer.warnings)
	}

	// Positive control: the ordinary, unreferenced sibling collection is a
	// genuine removal candidate and must actually be removed, with its
	// report line rendered normally.
	ordinaryManifest := filepath.Join(downloadPath, "ansible_collections", "ns", "ordinary", "MANIFEST.json")
	if _, err := os.Stat(ordinaryManifest); !os.IsNotExist(err) {
		t.Fatalf("expected the ordinary, unreferenced collection to be removed, stat error: %v", err)
	}
	if !printer.hasPrintContaining("Removed ns.ordinary@1.0.0") {
		t.Fatalf("expected a removal report line for ns.ordinary@1.0.0, got prints: %v", printer.prints)
	}
}

// TestRemoveInstalledContainmentGuard pins that removeInstalled refuses a
// Version containing "/" with ErrUnsafeRemovalPath before building any path,
// so nothing outside the collections dir is removed.
func TestRemoveInstalledContainmentGuard(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, t.TempDir(), "evil")

	inst := installedCollection{
		Key:            "ns.name@../../evil",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "../../evil",
		InstallPath:    filepath.Join(collectionsDir, "ansible_collections", "ns", "name"),
		CollectionsDir: collectionsDir,
	}

	err := removeInstalled(t.Context(), inst, nil, "")
	if !errors.Is(err, helpers.ErrUnsafeRemovalPath) {
		t.Fatalf("expected ErrUnsafeRemovalPath, got %v", err)
	}
	if _, statErr := os.Stat(sentinelFile); statErr != nil {
		t.Fatalf("expected outside sentinel to survive, stat error: %v", statErr)
	}
}

// TestRemoveInstalledRejectsInstallPathEscape pins the WithinDir guard: with
// valid ns, name and Version but an InstallPath outside CollectionsDir,
// removeInstalled refuses with ErrUnsafeRemovalPath and deletes nothing.
func TestRemoveInstalledRejectsInstallPathEscape(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, t.TempDir(), "victim")

	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Dir(sentinelFile),
		CollectionsDir: collectionsDir,
	}

	err := removeInstalled(t.Context(), inst, nil, "")
	if !errors.Is(err, helpers.ErrUnsafeRemovalPath) {
		t.Fatalf("expected ErrUnsafeRemovalPath, got %v", err)
	}
	if _, statErr := os.Stat(sentinelFile); statErr != nil {
		t.Fatalf("expected outside sentinel to survive, stat error: %v", statErr)
	}
}

// TestBuildInstalledRecordRejectsSeparators pins buildInstalledRecord's
// verdicts: unsafe elements fail with ErrUnsafeCollectionIdentifier, an empty
// one is a benign skip, and valid ones, semver build metadata included, pass.
func TestBuildInstalledRecordRejectsSeparators(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		ns, coll, version string
		wantOK            bool
		wantErr           bool
	}{
		{"valid identifiers", "ns", "name", "1.0.0", true, false},
		{"valid semver with prerelease and build", "ns", "name", "1.0.0-rc.1+build", true, false},
		{"namespace is traversal", "..", "name", "1.0.0", false, true},
		{"name contains slash", "ns", "a/b", "1.0.0", false, true},
		{"version is traversal", "ns", "name", "../../../../pwn", false, true},
		{"version is absolute path", "ns", "name", "/etc/passwd", false, true},
		// A newline in a version would forge an extra line in a report this
		// package prints with a bare %s, so it is rejected at ingestion.
		{"version carries a newline", "ns", "name", "1.0.0\nforged plain-text line", false, true},
		{"namespace is empty", "", "name", "1.0.0", false, false},
		{"name is empty", "ns", "", "1.0.0", false, false},
		{"version is empty", "ns", "name", "", false, false},
		{"version is dot", "ns", "name", ".", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var manifest types.GalaxyCollectionVersionInfoManifest
			manifest.CollectionInfo.Version = tc.version

			record, key, ok, err := buildInstalledRecord(
				"/collections", "/collections/ansible_collections/x/MANIFEST.json", tc.ns, tc.coll, manifest,
			)

			if tc.wantErr {
				if !errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) {
					t.Fatalf("expected ErrUnsafeCollectionIdentifier, got %v (ok=%v)", err, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("expected ok=%v, got %v (record=%+v key=%q)", tc.wantOK, ok, record, key)
			}
		})
	}
}

// TestStartFailsOnUnreadableRequirementsCorrupt pins that an unparseable
// requirements file aborts the run with ErrProjectRequirementsUnreadable
// rather than contributing no roots and exposing installs to deletion.
func TestStartFailsOnUnreadableRequirementsCorrupt(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("{invalid"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write corrupt requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}
	assertManifestSurvives(t, installDir)
}

// TestStartToleratesMissingRequirementsAsStaleEntry pins that a requirements
// file that no longer exists is a stale entry, warned about and contributing no
// roots; only a present but unreadable file has unknown roots and aborts.
func TestStartToleratesMissingRequirementsAsStaleEntry(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "does-not-exist.yml")
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	printer := &recordingPrinter{}
	runtime := newTestRuntimeWith(printer)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed with a stale (missing) requirements file, got %v", err)
	}
	if !printer.hasWarningContaining("no longer exists") {
		t.Fatalf("expected a warning about the missing requirements file, got: %v", printer.warnings)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to still be removed despite the stale entry, stat error: %v", statErr)
	}
}

// TestStartDeletesUnreferencedWithValidRequirements is the control for
// TestStartFailsOnUnreadableRequirementsCorrupt: a valid requirements file
// naming nothing lets cleanup delete the unreferenced collection.
func TestStartDeletesUnreferencedWithValidRequirements(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed with a valid requirements file, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced collection to be deleted, stat error: %v", statErr)
	}
}

// buildNonRegularRequirementsFixture registers a "gated-project" whose
// requirements file buildReqFile shapes (and may skip on) and a healthy
// "other-project" with one unreferenced install; it returns both install dirs.
func buildNonRegularRequirementsFixture(
	t *testing.T,
	cacheDir string,
	buildReqFile func(t *testing.T, reqPath string),
) (string, string) {
	t.Helper()
	gatedDownloadPath := t.TempDir()
	gatedInstallDir := seedInstallTree(t, gatedDownloadPath)
	gatedReqPath := filepath.Join(t.TempDir(), "requirements.yml")
	buildReqFile(t, gatedReqPath)

	otherDownloadPath := t.TempDir()
	otherInstallDir := seedInstallTree(t, otherDownloadPath)
	otherReqPath := filepath.Join(t.TempDir(), "requirements-other.yml")
	if err := os.WriteFile(otherReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write other project's requirements file: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"gated-project": {RequirementsFile: gatedReqPath, CollectionsPath: gatedDownloadPath, LastRun: time.Now().UTC()},
			"other-project": {RequirementsFile: otherReqPath, CollectionsPath: otherDownloadPath, LastRun: time.Now().UTC()},
		},
	})
	return gatedInstallDir, otherInstallDir
}

// TestStartAbortsOnFifoRequirementsFile pins loadRequirements's regular-file
// gate: a named pipe aborts with ErrProjectRequirementsUnreadable instead of
// blocking in open() with the lock held. Start is bounded so a regression fails.
func TestStartAbortsOnFifoRequirementsFile(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	_, otherInstallDir := buildNonRegularRequirementsFixture(t, cacheDir, func(t *testing.T, reqPath string) {
		t.Helper()
		if err := syscall.Mkfifo(reqPath, 0o644); err != nil {
			t.Skipf("named pipes unavailable on this platform: %v", err)
		}
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := startBoundedErr(t, cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}

	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); statErr != nil {
		t.Fatalf("expected the other project's install to survive the abort untouched, stat error: %v", statErr)
	}
}

// TestStartAbortsOnFifoRequirementsFilePositiveControl swaps the pipe for a
// valid regular file on the same fixture: Start succeeds and removes both
// projects' unreferenced installs.
func TestStartAbortsOnFifoRequirementsFilePositiveControl(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	gatedInstallDir, otherInstallDir := buildNonRegularRequirementsFixture(t, cacheDir, func(t *testing.T, reqPath string) {
		t.Helper()
		if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
			t.Fatalf("failed to write requirements file: %v", err)
		}
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := startBoundedErr(t, cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	gatedManifest := filepath.Join(gatedInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(gatedManifest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the formerly-gated project's unreferenced install to be removed, stat error: %v", statErr)
	}
	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the other project's unreferenced install to be removed, stat error: %v", statErr)
	}
}

// TestStartRemovesLegacyAndScopedArtifactKeys pins that one run removes an
// unreferenced collection's artifact under both the server-scoped key, from
// its recorded Source, and the pre-multi-server legacyArtifactKey.
func TestStartRemovesLegacyAndScopedArtifactKeys(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedInstallTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	const source = "https://galaxy.example.com/api"
	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
		"ns.name@1.0.0": {Source: source, ArtifactSHA256: "deadbeef"},
	})

	const filename = "ns-name-1.0.0.tar.gz"
	legacyPath := filepath.Join(cacheDir, legacyArtifactKey("ns", "name", "1.0.0"))
	scopedPath := filepath.Join(cacheDir, helpers.ArtifactKey(source, filename))
	if err := os.WriteFile(legacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed legacy artifact: %v", err)
	}
	if err := os.WriteFile(scopedPath, []byte("scoped-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed scoped artifact: %v", err)
	}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected the legacy-keyed artifact to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(scopedPath); !os.IsNotExist(err) {
		t.Fatalf("expected the scoped artifact to be removed, stat error: %v", err)
	}
}

// TestStartSweepsLegacyArtifactForReachableCollection pins that
// sweepLegacyArtifacts ignores reachability: a reachable collection keeps its
// tree and scoped artifact but loses its legacy-keyed one.
func TestStartSweepsLegacyArtifactForReachableCollection(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - name: ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	const source = "https://galaxy.example.com/api"
	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
		"ns.name@1.0.0": {Source: source, ArtifactSHA256: "deadbeef"},
	})

	const filename = "ns-name-1.0.0.tar.gz"
	legacyPath := filepath.Join(cacheDir, legacyArtifactKey("ns", "name", "1.0.0"))
	scopedPath := filepath.Join(cacheDir, helpers.ArtifactKey(source, filename))
	if err := os.WriteFile(legacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed legacy artifact: %v", err)
	}
	if err := os.WriteFile(scopedPath, []byte("scoped-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed scoped artifact: %v", err)
	}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	manifestPath := filepath.Join(downloadPath, "ansible_collections", "ns", "name", "MANIFEST.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected the still-referenced collection's workspace to survive, stat error: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected the legacy-keyed artifact to be swept even though the collection is reachable, stat error: %v", err)
	}
	if _, err := os.Stat(scopedPath); err != nil {
		t.Fatalf("expected the scoped artifact of a reachable collection to survive, stat error: %v", err)
	}
}

// TestSweepLegacyArtifactsSkipsForgedScopedCollision pins that a namespace
// named "<fingerprint>.acme", whose legacy key equals a live scoped key, never
// gets that scoped artifact swept; a genuine legacy key is the control.
func TestSweepLegacyArtifactsSkipsForgedScopedCollision(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()

	const source = "https://galaxy.ansible.com/api"
	const filename = "acme-app-1.0.0.tar.gz"
	scopedKey := helpers.ArtifactKey(source, filename)
	fp := scopedKey[:helpers.ArtifactKeyFingerprintLen]
	hostileNS := fp + ".acme"

	hostileDownloadPath := t.TempDir()
	seedManifestAt(t, hostileDownloadPath, hostileNS, "app", "1.0.0")
	hostileReqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(hostileReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write hostile project's requirements file: %v", err)
	}

	// forgedLegacyKey is what legacyArtifactKey builds for the hostile
	// namespace/name/version. If this fails, the fixture itself no longer
	// demonstrates the collision this test exists to guard against.
	forgedLegacyKey := legacyArtifactKey(hostileNS, "app", "1.0.0")
	if forgedLegacyKey != scopedKey {
		t.Fatalf("fixture assumption broken: forged legacy key %q does not equal scoped key %q", forgedLegacyKey, scopedKey)
	}
	scopedArtifactPath := filepath.Join(cacheDir, scopedKey)
	if err := os.WriteFile(scopedArtifactPath, []byte("scoped-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed the scoped artifact: %v", err)
	}

	// Positive control: an ordinary project with a genuinely legacy-keyed
	// artifact.
	controlDownloadPath := t.TempDir()
	seedManifestAt(t, controlDownloadPath, "ctrl", "coll", "1.0.0")
	controlReqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(controlReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write control project's requirements file: %v", err)
	}
	controlLegacyKey := legacyArtifactKey("ctrl", "coll", "1.0.0")
	controlLegacyPath := filepath.Join(cacheDir, controlLegacyKey)
	if err := os.WriteFile(controlLegacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed the control project's legacy artifact: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"hostile-project": {RequirementsFile: hostileReqPath, CollectionsPath: hostileDownloadPath, LastRun: time.Now().UTC()},
			"control-project": {RequirementsFile: controlReqPath, CollectionsPath: controlDownloadPath, LastRun: time.Now().UTC()},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	if _, err := os.Stat(scopedArtifactPath); err != nil {
		t.Fatalf("expected the scoped artifact to survive the legacy sweep, stat error: %v", err)
	}
	if _, err := os.Stat(controlLegacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected the genuinely legacy-keyed control artifact to be swept, stat error: %v", err)
	}
}

// seedSnapshotInstalled saves a store snapshot at cfg.CacheDir whose
// Installed set is exactly entries, via the real local backend (SaveStore),
// mirroring how a prior install run would have persisted it.
func seedSnapshotInstalled(t *testing.T, cfg *config.Config, runtime *infra.Infra, entries map[string]store.InstalledEntry) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend for seeding: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend for seeding: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close seeding backend: %v", err)
		}
	}()
	st := store.New()
	for key, entry := range entries {
		st.SetInstalled(key, entry)
	}
	if err := backend.SaveStore(t.Context(), st); err != nil {
		t.Fatalf("failed to save seeded store: %v", err)
	}
}

// resaveLoadedSnapshot loads the persisted snapshot, applies mutate and saves
// it back, carrying forward what the snapshot recorded about itself, which the
// fresh store.New() seedSnapshotInstalled saves would drop.
func resaveLoadedSnapshot(t *testing.T, cfg *config.Config, runtime *infra.Infra, mutate func(*store.Store)) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend for resaving: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend for resaving: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close resaving backend: %v", err)
		}
	}()
	st, err := backend.LoadStore(t.Context())
	if err != nil {
		t.Fatalf("failed to load store for resaving: %v", err)
	}
	mutate(st)
	if err := backend.SaveStore(t.Context(), st); err != nil {
		t.Fatalf("failed to resave store: %v", err)
	}
}

// seedSnapshotWarmed saves a snapshot whose Warmed set is exactly entries,
// written into the map directly rather than through Store.SetWarmed so a
// caller can seed a stale WarmedAt.
func seedSnapshotWarmed(t *testing.T, cfg *config.Config, runtime *infra.Infra, entries map[string]store.WarmedEntry) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend for seeding: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend for seeding: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close seeding backend: %v", err)
		}
	}()
	st := store.New()
	maps.Copy(st.Warmed, entries)
	if err := backend.SaveStore(t.Context(), st); err != nil {
		t.Fatalf("failed to save seeded store: %v", err)
	}
}

// seedExtractedDir creates <cacheDir>/extracted/<sha>/ with a ready marker,
// mirroring the on-disk layout extracted.Store.Ensure/Promote produce, so
// Sweep sees a real completed entry rather than an empty directory.
func seedExtractedDir(t *testing.T, cacheDir, sha string) {
	t.Helper()
	dir := filepath.Join(cacheDir, extracted.RootDirName, sha)
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create extracted dir for %s: %v", sha, err)
	}
	if err := os.WriteFile(filepath.Join(dir, extracted.ReadyMarker), []byte("ok"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write ready marker for %s: %v", sha, err)
	}
}

// recordAbsentWorkspaceProject records a project at downloadPath with no
// ansible_collections under it, so the scan skips it: the ephemeral CI shape
// whose snapshot entries are never scanned or pruned.
func recordAbsentWorkspaceProject(t *testing.T, cfg *config.Config, runtime *infra.Infra, downloadPath string) {
	t.Helper()
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend to record project: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend to record project: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close backend after recording project: %v", err)
		}
	}()
	if err := backend.RecordProject(t.Context(), reqPath, downloadPath, ""); err != nil {
		t.Fatalf("failed to record project: %v", err)
	}
}

// TestSweepKeepsCacheWhenWorkspaceAbsent pins that the extracted keep set
// comes from the snapshot, not the disk: a project with no workspace still
// keeps its installed entries' extracted trees.
func TestSweepKeepsCacheWhenWorkspaceAbsent(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	entries := map[string]store.InstalledEntry{
		"ns.name@1.0.0":  {ArtifactSHA256: "sha-keep-1"},
		"ns.other@2.0.0": {ArtifactSHA256: "sha-keep-2"},
	}
	seedSnapshotInstalled(t, cfg, runtime, entries)
	for _, entry := range entries {
		seedExtractedDir(t, cacheDir, entry.ArtifactSHA256)
	}
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	for _, entry := range entries {
		dir := filepath.Join(cacheDir, extracted.RootDirName, entry.ArtifactSHA256)
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("expected extracted dir %s to survive an absent workspace, stat error: %v", entry.ArtifactSHA256, statErr)
		}
	}
}

// seedSnapshotInstalledAndWarmed saves a snapshot with exactly installed and
// warmed in one SaveStore, since two separate seeding saves would each replace
// every bucket and wipe the other's.
func seedSnapshotInstalledAndWarmed(
	t *testing.T,
	cfg *config.Config,
	runtime *infra.Infra,
	installed map[string]store.InstalledEntry,
	warmed map[string]store.WarmedEntry,
) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend for seeding: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend for seeding: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close seeding backend: %v", err)
		}
	}()
	st := store.New()
	for key, entry := range installed {
		st.SetInstalled(key, entry)
	}
	maps.Copy(st.Warmed, warmed)
	if err := backend.SaveStore(t.Context(), st); err != nil {
		t.Fatalf("failed to save seeded store: %v", err)
	}
}

// TestSweepKeepsWarmedShaWithNoInstalledEntry pins that a warm-only machine
// (a Warmed entry, no Installed entry, no workspace) keeps its extracted tree:
// extractedKeepSet consults the Warmed set too.
func TestSweepKeepsWarmedShaWithNoInstalledEntry(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedSnapshotWarmed(t, cfg, runtime, map[string]store.WarmedEntry{
		"ns.name@1.0.0": {WarmedAt: time.Now().UTC(), ArtifactSHA256: "sha-warmed"},
	})
	seedExtractedDir(t, cacheDir, "sha-warmed")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertExtractedDirsSurvive(t, cacheDir, "sha-warmed")
}

// TestSweepPrunesStaleWarmedSha pins that a warmed entry older than
// helpers.WarmedEntryMaxAge no longer protects its extracted tree; the
// read-side filter alone is pinned by TestWarmedArtifactSHAByKeyExcludesStaleEntry.
func TestSweepPrunesStaleWarmedSha(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	staleAt := time.Now().UTC().Add(-40 * 24 * time.Hour)
	seedSnapshotWarmed(t, cfg, runtime, map[string]store.WarmedEntry{
		"ns.name@1.0.0": {WarmedAt: staleAt, ArtifactSHA256: "sha-stale-warmed"},
	})
	seedExtractedDir(t, cacheDir, "sha-stale-warmed")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	dir := filepath.Join(cacheDir, extracted.RootDirName, "sha-stale-warmed")
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the stale warmed entry's extracted dir to be swept, stat error: %v", statErr)
	}
}

// TestSweepKeepsWarmedShaWhenInstalledEntryRemovedAsUnreachable pins that
// extractedKeepSet's warmed half ignores the removal exclusion: a key removed
// as unreachable in this run but also warmed keeps its extracted tree.
func TestSweepKeepsWarmedShaWhenInstalledEntryRemovedAsUnreachable(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	const sha = "sha-shared"
	seedSnapshotInstalledAndWarmed(t, cfg, runtime,
		map[string]store.InstalledEntry{"ns.name@1.0.0": {ArtifactSHA256: sha}},
		map[string]store.WarmedEntry{"ns.name@1.0.0": {WarmedAt: time.Now().UTC(), ArtifactSHA256: sha}},
	)
	seedExtractedDir(t, cacheDir, sha)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced collection's install to be removed, stat error: %v", statErr)
	}
	assertExtractedDirsSurvive(t, cacheDir, sha)
}

// TestSweepDropsUnreferencedSha proves the snapshot-derived keep set still
// drops a genuinely orphaned extracted entry: one referenced by an installed
// snapshot entry survives, one with no referencing entry at all is removed.
func TestSweepDropsUnreferencedSha(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
		"ns.name@1.0.0": {ArtifactSHA256: "sha-keep"},
	})
	seedExtractedDir(t, cacheDir, "sha-keep")
	seedExtractedDir(t, cacheDir, "sha-orphan")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	keepDir := filepath.Join(cacheDir, extracted.RootDirName, "sha-keep")
	if _, statErr := os.Stat(keepDir); statErr != nil {
		t.Fatalf("expected referenced sha dir to survive, stat error: %v", statErr)
	}
	orphanDir := filepath.Join(cacheDir, extracted.RootDirName, "sha-orphan")
	if _, statErr := os.Stat(orphanDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected unreferenced orphan sha dir to be removed, stat error: %v", statErr)
	}
}

// TestScanIgnoresNestedManifest pins that only the fixed
// ansible_collections/<ns>/<name>/MANIFEST.json depth is scanned: a manifest
// nested deeper, as in a collection's test fixtures, is never indexed.
func TestScanIgnoresNestedManifest(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	nestedDir := filepath.Join(installDir, "tests", "fixtures")
	if err := os.MkdirAll(nestedDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create nested fixture dir: %v", err)
	}
	nestedManifest := `{
		"collection_info": {
			"namespace": "phantom",
			"name": "fixture",
			"version": "9.9.9"
		}
	}`
	nestedManifestPath := filepath.Join(nestedDir, "MANIFEST.json")
	if err := os.WriteFile(nestedManifestPath, []byte(nestedManifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write nested manifest: %v", err)
	}

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	runtime := newTestRuntime()
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertManifestSurvives(t, installDir)
	if _, statErr := os.Stat(nestedManifestPath); statErr != nil {
		t.Fatalf("expected the nested phantom manifest to survive since it is never scanned, stat error: %v", statErr)
	}
}

// TestScanSkipsDirWithoutManifest proves a <ns>/<name> directory with no
// MANIFEST.json at all (e.g. a partially cleaned or interrupted install) is
// silently skipped rather than treated as an IO error that aborts the scan.
func TestScanSkipsDirWithoutManifest(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	emptyDir := filepath.Join(downloadPath, "ansible_collections", "ns", "empty")
	if err := os.MkdirAll(emptyDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create manifest-less dir: %v", err)
	}
	registerCleanupProject(t, cacheDir, downloadPath)

	runtime := newTestRuntime()
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed with a manifest-less directory present, got %v", err)
	}
}

// TestRemovesAllCopiesInOneRun pins that one ns.name@version installed under
// two projects has both copies removed in a single run: installedByKey
// appends every project's copy rather than overwriting.
func TestRemovesAllCopiesInOneRun(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPathA := t.TempDir()
	downloadPathB := t.TempDir()
	installDirA := seedInstallTree(t, downloadPathA)
	installDirB := seedInstallTree(t, downloadPathB)

	// Both projects need a valid requirements file naming nothing, so the run
	// reaches removeUnused with ns.name unreachable from both.
	reqPathA := filepath.Join(t.TempDir(), "requirements-a.yml")
	if err := os.WriteFile(reqPathA, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project A: %v", err)
	}
	reqPathB := filepath.Join(t.TempDir(), "requirements-b.yml")
	if err := os.WriteFile(reqPathB, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project B: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"proj-a": {
				RequirementsFile: reqPathA,
				CollectionsPath:  downloadPathA,
				LastRun:          time.Now().UTC(),
			},
			"proj-b": {
				RequirementsFile: reqPathB,
				CollectionsPath:  downloadPathB,
				LastRun:          time.Now().UTC(),
			},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	manifestA := filepath.Join(installDirA, "MANIFEST.json")
	if _, statErr := os.Stat(manifestA); !os.IsNotExist(statErr) {
		t.Fatalf("expected project A's copy to be removed in this single run, stat error: %v", statErr)
	}
	manifestB := filepath.Join(installDirB, "MANIFEST.json")
	if _, statErr := os.Stat(manifestB); !os.IsNotExist(statErr) {
		t.Fatalf("expected project B's copy to be removed in this single run, stat error: %v", statErr)
	}
}

// TestReportsCorruptManifest pins that an unparseable MANIFEST.json is
// warned about and is neither a root nor a removal candidate, while a valid
// sibling is still removed and Start does not fail.
func TestReportsCorruptManifest(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	validInstallDir := seedInstallTree(t, downloadPath)

	corruptDir := filepath.Join(downloadPath, "ansible_collections", "corruptns", "corruptname")
	if err := os.MkdirAll(corruptDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create corrupt manifest dir: %v", err)
	}
	corruptManifestPath := filepath.Join(corruptDir, "MANIFEST.json")
	if err := os.WriteFile(corruptManifestPath, []byte("{invalid"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write corrupt manifest: %v", err)
	}

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected a corrupt manifest to be reported rather than fatal, got error: %v", err)
	}

	if !printer.hasWarningContaining("corrupt manifest") {
		t.Fatalf("expected a warning about a corrupt manifest, got: %v", printer.warnings)
	}
	if _, statErr := os.Stat(corruptManifestPath); statErr != nil {
		t.Fatalf("expected the corrupt manifest's tree to survive, stat error: %v", statErr)
	}
	validManifestPath := filepath.Join(validInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(validManifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced valid collection to still be removed normally, stat error: %v", statErr)
	}
}

// seedManifestAt writes a MANIFEST.json for ns.name@version under
// <root>/ansible_collections/<ns>/<name>, for tests that need more than one
// distinct installed collection under the same collections root.
func seedManifestAt(t *testing.T, root, ns, name, version string) {
	t.Helper()
	seedManifestWithDeps(t, root, ns, name, version, nil)
}

// manifestJSON builds a MANIFEST.json body for ns.name@version, rendering a
// non-empty deps map as collection_info.dependencies in sorted order.
func manifestJSON(ns, name, version string, deps map[string]string) string {
	fields := fmt.Sprintf(`"namespace": %q, "name": %q, "version": %q`, ns, name, version)
	if len(deps) > 0 {
		keys := make([]string, 0, len(deps))
		for depFQDN := range deps {
			keys = append(keys, depFQDN)
		}
		slices.Sort(keys)
		pairs := make([]string, 0, len(deps))
		for _, depFQDN := range keys {
			pairs = append(pairs, fmt.Sprintf("%q: %q", depFQDN, deps[depFQDN]))
		}
		fields += fmt.Sprintf(`, "dependencies": {%s}`, strings.Join(pairs, ", "))
	}
	return fmt.Sprintf(`{"collection_info": {%s}}`, fields)
}

// seedManifestWithDeps writes a MANIFEST.json for ns.name@version, with an
// optional declared dependencies map, under
// <root>/ansible_collections/<ns>/<name>.
func seedManifestWithDeps(t *testing.T, root, ns, name, version string, deps map[string]string) {
	t.Helper()
	installDir := filepath.Join(root, "ansible_collections", ns, name)
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir for %s.%s: %v", ns, name, err)
	}
	manifest := manifestJSON(ns, name, version, deps)
	if err := os.WriteFile(filepath.Join(installDir, "MANIFEST.json"), []byte(manifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write manifest for %s.%s: %v", ns, name, err)
	}
}

// assertManifestPresentAt fails the test unless the MANIFEST.json for
// ns.name still exists under root/ansible_collections.
func assertManifestPresentAt(t *testing.T, root, ns, name string) {
	t.Helper()
	path := filepath.Join(root, "ansible_collections", ns, name, "MANIFEST.json")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("expected manifest for %s.%s to survive, stat error: %v", ns, name, statErr)
	}
}

// assertManifestAbsentAt fails the test unless the MANIFEST.json for
// ns.name has been removed from under root/ansible_collections.
func assertManifestAbsentAt(t *testing.T, root, ns, name string) {
	t.Helper()
	path := filepath.Join(root, "ansible_collections", ns, name, "MANIFEST.json")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected manifest for %s.%s to be removed, stat error: %v", ns, name, statErr)
	}
}

// assertExtractedDirGone fails the test unless the named extracted entry
// under cacheDir/extracted has been removed from disk.
func assertExtractedDirGone(t *testing.T, cacheDir, sha string) {
	t.Helper()
	dir := filepath.Join(cacheDir, extracted.RootDirName, sha)
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("expected extracted dir %s to be swept, stat error: %v", sha, statErr)
	}
}

// TestMarkReachableFollowsTransitiveDependency pins that a manifest-declared
// dependency of a root (ns.a to ns.b) survives with its extracted tree, while
// the unreferenced ns.c and its tree are removed.
func TestMarkReachableFollowsTransitiveDependency(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	seedManifestWithDeps(t, downloadPath, "ns", "a", "1.0.0", map[string]string{"ns.b": ">=1.0.0"})
	seedManifestWithDeps(t, downloadPath, "ns", "b", "1.0.0", nil)
	seedManifestWithDeps(t, downloadPath, "ns", "c", "1.0.0", nil)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	seedSnapshotInstalled(t, cfg, newTestRuntime(), map[string]store.InstalledEntry{
		"ns.b@1.0.0": {ArtifactSHA256: "sha-b"},
		"ns.c@1.0.0": {ArtifactSHA256: "sha-c"},
	})
	seedExtractedDir(t, cacheDir, "sha-b")
	seedExtractedDir(t, cacheDir, "sha-c")

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.a\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	runtime := newTestRuntime()
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertManifestPresentAt(t, downloadPath, "ns", "a")
	assertManifestPresentAt(t, downloadPath, "ns", "b")
	assertManifestAbsentAt(t, downloadPath, "ns", "c")

	assertExtractedDirsSurvive(t, cacheDir, "sha-b")
	assertExtractedDirGone(t, cacheDir, "sha-c")
}

// assertExtractedDirsSurvive fails the test unless every named extracted
// entry under cacheDir/extracted is still present on disk.
func assertExtractedDirsSurvive(t *testing.T, cacheDir string, shas ...string) {
	t.Helper()
	for _, sha := range shas {
		dir := filepath.Join(cacheDir, extracted.RootDirName, sha)
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("expected extracted dir %s to survive, stat error: %v", sha, statErr)
		}
	}
}

// TestDryRunReportsExtractedSweep pins that a dry run deletes nothing and
// reports what a real run would sweep: the orphan and the unreachable key's
// sha, excluded explicitly since a dry run leaves the snapshot unpruned.
func TestDryRunReportsExtractedSweep(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	seedManifestAt(t, downloadPath, "ns", "name", "1.0.0")
	seedManifestAt(t, downloadPath, "orphan", "name", "2.0.0")

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	seedSnapshotInstalled(t, cfg, newTestRuntime(), map[string]store.InstalledEntry{
		"ns.name@1.0.0":     {ArtifactSHA256: "sha-keep"},
		"orphan.name@2.0.0": {ArtifactSHA256: "sha-would-remove"},
	})
	for _, sha := range []string{"sha-keep", "sha-would-remove", "sha-orphan"} {
		seedExtractedDir(t, cacheDir, sha)
	}

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}

	// Primary, non-negotiable assertion: dry-run deletes nothing at all.
	assertExtractedDirsSurvive(t, cacheDir, "sha-keep", "sha-would-remove", "sha-orphan")

	// Secondary assertion: the report accurately reflects what a real run
	// would sweep - both the unreachable key's SHA and the true orphan, but
	// not the reachable, kept key's SHA.
	if !printer.hasPrintContaining("sha-would-remove") {
		t.Fatalf("expected a would-sweep report for sha-would-remove, got prints: %v", printer.prints)
	}
	if !printer.hasPrintContaining("sha-orphan") {
		t.Fatalf("expected a would-sweep report for sha-orphan, got prints: %v", printer.prints)
	}
	if printer.hasPrintContaining("sha-keep") {
		t.Fatalf("expected no would-sweep report for the reachable, kept sha-keep, got prints: %v", printer.prints)
	}
}

// TestDryRunReportsExtractedSweepExcludesWarmedSha pins that a dry run never
// reports a fresh warmed sha as a sweep candidate but still reports an orphan.
func TestDryRunReportsExtractedSweepExcludesWarmedSha(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	seedRuntime := newTestRuntime()
	seedSnapshotWarmed(t, cfg, seedRuntime, map[string]store.WarmedEntry{
		"ns.name@1.0.0": {WarmedAt: time.Now().UTC(), ArtifactSHA256: "sha-warmed"},
	})
	seedExtractedDir(t, cacheDir, "sha-warmed")
	seedExtractedDir(t, cacheDir, "sha-orphan")
	recordAbsentWorkspaceProject(t, cfg, seedRuntime, downloadPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}

	// Primary, non-negotiable assertion: dry-run deletes nothing at all.
	assertExtractedDirsSurvive(t, cacheDir, "sha-warmed", "sha-orphan")

	// Secondary assertion: the report never names a warmed, kept sha, but
	// still names the true orphan.
	if printer.hasPrintContaining("sha-warmed") {
		t.Fatalf("expected no would-sweep report for the warmed sha-warmed, got prints: %v", printer.prints)
	}
	if !printer.hasPrintContaining("sha-orphan") {
		t.Fatalf("expected a would-sweep report for sha-orphan, got prints: %v", printer.prints)
	}
}

// TestDryRunReportsLegacyArtifactSweep pins that a dry run deletes nothing
// and reports only legacy keys that exist on disk, checked against ns.other's
// exact key rather than a loose substring.
func TestDryRunReportsLegacyArtifactSweep(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedManifestAt(t, downloadPath, "ns", "name", "1.0.0")
	seedManifestAt(t, downloadPath, "ns", "other", "1.0.0")

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.name\n  - ns.other\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	legacyPath := filepath.Join(cacheDir, legacyArtifactKey("ns", "name", "1.0.0"))
	if err := os.WriteFile(legacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed legacy artifact: %v", err)
	}

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}

	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("expected dry-run to delete nothing, but the legacy artifact is gone: %v", err)
	}
	legacyKey := legacyArtifactKey("ns", "name", "1.0.0")
	if !printer.hasPrintContaining(legacyKey) {
		t.Fatalf("expected a would-sweep report naming the legacy key %q, got prints: %v", legacyKey, printer.prints)
	}
	otherLegacyKey := legacyArtifactKey("ns", "other", "1.0.0")
	if printer.hasPrintContaining(otherLegacyKey) {
		t.Fatalf("expected no would-sweep report for ns.other's own legacy key %q, got prints: %v", otherLegacyKey, printer.prints)
	}
}

// assertSelectedKeys fails the test unless the Key of every item in got is
// exactly the set of want, ignoring order.
func assertSelectedKeys(t *testing.T, got []installedCollection, want ...string) {
	t.Helper()
	gotKeys := make([]string, 0, len(got))
	for _, item := range got {
		gotKeys = append(gotKeys, item.Key)
	}
	slices.Sort(gotKeys)
	wantSorted := append([]string(nil), want...)
	slices.Sort(wantSorted)
	if len(gotKeys) != len(wantSorted) {
		t.Fatalf("selected keys = %v, want %v", gotKeys, wantSorted)
	}
	for i, k := range gotKeys {
		if k != wantSorted[i] {
			t.Fatalf("selected keys = %v, want %v", gotKeys, wantSorted)
		}
	}
}

// TestSelectInstalledConstraintCache pins selectInstalled over its constraint
// cache: a valid constraint filters (skipping unparsed versions), an empty or
// unparseable one matches all, and a repeated constraint hits the cache.
func TestSelectInstalledConstraintCache(t *testing.T) {
	t.Parallel()
	v15, err := semver.NewVersion("1.5.0")
	if err != nil {
		t.Fatalf("failed to parse 1.5.0: %v", err)
	}
	v20, err := semver.NewVersion("2.0.0")
	if err != nil {
		t.Fatalf("failed to parse 2.0.0: %v", err)
	}
	index := map[string][]installedCollection{
		"ns.name": {
			{Key: "ns.name@1.5.0", Version: "1.5.0", Parsed: v15},
			{Key: "ns.name@2.0.0", Version: "2.0.0", Parsed: v20},
			// An item whose version failed to parse (e.g. a git ref that
			// still passed the path-element safety check): Parsed is nil,
			// exactly as buildInstalledRecord would leave it.
			{Key: "ns.name@git-ref", Version: "git-ref", Parsed: nil},
		},
	}
	constraints := make(map[string]*semver.Constraints)

	// A valid constraint selects the matching parsed version and skips
	// both the out-of-range version and the unparseable-version item.
	selected := selectInstalled(index, constraints, "ns.name", ">=1.0.0,<2.0.0")
	assertSelectedKeys(t, selected, "ns.name@1.5.0")
	if len(constraints) != 1 {
		t.Fatalf("expected the constraint to be cached after first use, got %d entries", len(constraints))
	}

	// Calling again with the identical constraint string must hit the
	// cache (no new entry) and still yield the identical result set.
	selectedAgain := selectInstalled(index, constraints, "ns.name", ">=1.0.0,<2.0.0")
	assertSelectedKeys(t, selectedAgain, "ns.name@1.5.0")
	if len(constraints) != 1 {
		t.Fatalf("expected no new cache entry on a repeated constraint, got %d entries", len(constraints))
	}

	// An empty/"*" constraint is match-all without ever touching the cache.
	all := selectInstalled(index, constraints, "ns.name", "")
	assertSelectedKeys(t, all, "ns.name@1.5.0", "ns.name@2.0.0", "ns.name@git-ref")
	if len(constraints) != 1 {
		t.Fatalf("expected the match-all fast path not to populate the cache, got %d entries", len(constraints))
	}

	// An unparseable constraint is match-all, like semver.NewConstraint's
	// failure, and is cached as nil so it is parsed only once.
	unparseable := selectInstalled(index, constraints, "ns.name", "not a constraint !!")
	assertSelectedKeys(t, unparseable, "ns.name@1.5.0", "ns.name@2.0.0", "ns.name@git-ref")
	c, ok := constraints["not a constraint !!"]
	if !ok || c != nil {
		t.Fatalf("expected the unparseable constraint to be cached as nil, got ok=%v c=%v", ok, c)
	}
}

// TestRemoveUnusedAbortsOnRemoveAllError pins that an os.RemoveAll failure
// (a read-only parent blocks the final rmdir) aborts Start with an error
// instead of being swallowed. Skipped as root, which bypasses permissions.
func TestRemoveUnusedAbortsOnRemoveAllError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based removal guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	namespaceDir := filepath.Join(downloadPath, "ansible_collections", "ns")
	//nolint:gosec // G302: intentionally read-only (no write bit) to force os.RemoveAll to fail with a permission error.
	if err := os.Chmod(namespaceDir, 0o555); err != nil {
		t.Fatalf("failed to chmod namespace dir read-only: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(namespaceDir, helpers.DirMod); err != nil {
			t.Errorf("failed to restore namespace dir perms: %v", err)
		}
	})

	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err == nil {
		t.Fatalf("expected a non-nil error when os.RemoveAll fails, got nil")
	}
	// The final rmdir is what failed, so the directory itself is still present.
	if _, statErr := os.Stat(installDir); statErr != nil {
		t.Fatalf("expected the install directory to still be present after a failed removal, stat error: %v", statErr)
	}
}

// TestRemoveInstalledRemovesInfoDir pins that removal deletes both the install
// dir and its <ns>.<name>-<version>.info sidecar even when they hold read-only
// files: removal needs write permission only on the containing directory.
func TestRemoveInstalledRemovesInfoDir(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath) // ns.name@1.0.0, unreferenced below

	// Hardened extraction leaves installed files read-only; removal must not
	// depend on the file's own mode.
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	//nolint:gosec // read-only (not writable) is exactly the fixture this test needs, not a leak risk.
	if err := os.Chmod(manifestPath, 0o444); err != nil {
		t.Fatalf("failed to chmod MANIFEST.json read-only: %v", err)
	}

	infoDir := filepath.Join(downloadPath, "ansible_collections", "ns.name-1.0.0.info")
	if err := os.MkdirAll(filepath.Join(infoDir, "marker"), helpers.DirMod); err != nil {
		t.Fatalf("failed to create .info dir: %v", err)
	}
	// .info sidecar content is never CAS-backed, but removeInfoDir must be
	// equally indifferent to a read-only file underneath it.
	infoFile := filepath.Join(infoDir, "marker", "GALAXY.yml")
	//nolint:gosec // read-only (not writable) is exactly the fixture this test needs, not a leak risk.
	if err := os.WriteFile(infoFile, []byte("collection_info: {}\n"), 0o444); err != nil {
		t.Fatalf("failed to write read-only .info file: %v", err)
	}

	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if _, statErr := os.Stat(infoDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the .info directory to be removed, stat error: %v", statErr)
	}
	if _, statErr := os.Stat(installDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the install directory (with its read-only MANIFEST.json) to be removed, stat error: %v", statErr)
	}
}

// TestRemoveInstalledRefusesNamespaceSymlinkSwap pins that a namespace dir
// swapped for an escaping symlink between scan and removal is refused: the
// lexical WithinDir check still passes, so the os.Root is what refuses it.
func TestRemoveInstalledRefusesNamespaceSymlinkSwap(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()
	seedInstallTree(t, collectionsDir) // real ansible_collections/ns/name/MANIFEST.json

	outsideRoot := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, outsideRoot, "ns-decoy")

	// Simulate the scan-to-removal race: the real "ns" directory the scan
	// saw is gone by the time removeInstalled runs, replaced by a symlink to
	// a directory entirely outside collectionsDir.
	nsDir := filepath.Join(collectionsDir, "ansible_collections", "ns")
	if err := os.RemoveAll(nsDir); err != nil {
		t.Fatalf("failed to remove the real ns dir before swapping it: %v", err)
	}
	if err := os.Symlink(filepath.Join(outsideRoot, "ns-decoy"), nsDir); err != nil {
		t.Fatalf("failed to symlink ns to the outside decoy: %v", err)
	}

	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Join(nsDir, "name"),
		CollectionsDir: collectionsDir,
	}

	if err := removeInstalled(t.Context(), inst, nil, ""); err == nil {
		t.Fatalf("expected removeInstalled to refuse to follow the swapped ns symlink, got nil error")
	}
	if _, statErr := os.Stat(sentinelFile); statErr != nil {
		t.Fatalf("expected the outside decoy's sentinel file to survive, stat error: %v", statErr)
	}
}

// TestRemoveInstalledRefusesAnsibleCollectionsSymlinkSwap pins the same
// refusal for ansible_collections itself, which only a root at CollectionsDir,
// not one level deeper, can see.
func TestRemoveInstalledRefusesAnsibleCollectionsSymlinkSwap(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()

	outsideRoot := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, outsideRoot, "ac-decoy")

	// ansible_collections itself never exists as a real directory here - it
	// is a symlink to a directory entirely outside collectionsDir from the
	// start, standing in for the moment right after an attacker's swap.
	acDir := filepath.Join(collectionsDir, "ansible_collections")
	if err := os.Symlink(filepath.Join(outsideRoot, "ac-decoy"), acDir); err != nil {
		t.Fatalf("failed to symlink ansible_collections to the outside decoy: %v", err)
	}

	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Join(acDir, "ns", "name"),
		CollectionsDir: collectionsDir,
	}

	if err := removeInstalled(t.Context(), inst, nil, ""); err == nil {
		t.Fatalf("expected removeInstalled to refuse to follow the swapped ansible_collections symlink, got nil error")
	}
	if _, statErr := os.Stat(sentinelFile); statErr != nil {
		t.Fatalf("expected the outside decoy's sentinel file to survive, stat error: %v", statErr)
	}
}

// errArtifactStoreStubNotImplemented is returned by recordingArtifactStore
// methods removeInstalled never calls, so an unexpected call fails loudly.
var errArtifactStoreStubNotImplemented = errors.New("stub: method not implemented")

// recordingArtifactStore is a minimal cacheManager.ArtifactStore test double
// that records every key passed to Delete. removeInstalled only ever calls
// Delete on an ArtifactStore, so the other methods are never exercised here.
type recordingArtifactStore struct {
	deleted []string
}

func (a *recordingArtifactStore) Has(context.Context, string) (bool, error) {
	return false, errArtifactStoreStubNotImplemented
}

func (a *recordingArtifactStore) Meta(context.Context, string) (map[string]string, bool, error) {
	return nil, false, errArtifactStoreStubNotImplemented
}

func (a *recordingArtifactStore) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errArtifactStoreStubNotImplemented
}

func (a *recordingArtifactStore) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errArtifactStoreStubNotImplemented
}

func (a *recordingArtifactStore) Commit(
	context.Context, string, string, map[string]string,
) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errArtifactStoreStubNotImplemented
}

// Delete records key and always succeeds, matching a real ArtifactStore's
// best-effort use from removeInstalled (its error is ignored there).
func (a *recordingArtifactStore) Delete(_ context.Context, key string) error {
	a.deleted = append(a.deleted, key)
	return nil
}

// TestRemoveInstalledDeletesArtifactWhenWorkspaceAbsent pins that the
// artifact purge does not depend on the workspace: with CollectionsDir absent,
// removeInstalled still deletes the server-scoped artifact key.
func TestRemoveInstalledDeletesArtifactWhenWorkspaceAbsent(t *testing.T) {
	t.Parallel()
	collectionsDir := filepath.Join(t.TempDir(), "does-not-exist")
	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Join(collectionsDir, "ansible_collections", "ns", "name"),
		CollectionsDir: collectionsDir,
	}
	artifacts := &recordingArtifactStore{}
	const source = "https://galaxy.example.com/api"

	if err := removeInstalled(t.Context(), inst, artifacts, source); err != nil {
		t.Fatalf("expected nil error for an absent workspace, got %v", err)
	}

	wantKey := helpers.ArtifactKey(source, "ns-name-1.0.0.tar.gz")
	if len(artifacts.deleted) != 1 || artifacts.deleted[0] != wantKey {
		t.Fatalf("expected Delete to be called once with key %q, got %v", wantKey, artifacts.deleted)
	}
}

// TestRemoveInstalledSkipsArtifactPurgeWhenSourceUnknown pins that an empty
// source means no artifact Delete at all rather than a key guessed from an
// empty server fingerprint; legacy keys are sweepLegacyArtifacts' job.
func TestRemoveInstalledSkipsArtifactPurgeWhenSourceUnknown(t *testing.T) {
	t.Parallel()
	collectionsDir := filepath.Join(t.TempDir(), "does-not-exist")
	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Join(collectionsDir, "ansible_collections", "ns", "name"),
		CollectionsDir: collectionsDir,
	}
	artifacts := &recordingArtifactStore{}

	if err := removeInstalled(t.Context(), inst, artifacts, ""); err != nil {
		t.Fatalf("expected nil error for an absent workspace, got %v", err)
	}
	if len(artifacts.deleted) != 0 {
		t.Fatalf("expected no Delete call when source is unknown, got %v", artifacts.deleted)
	}
}

// TestScanAbortsOnRealIOErrorReadingManifest pins that a non-ENOENT error
// reading MANIFEST.json aborts Start before anything is deleted, rather than
// being skipped or warned about. Skipped as root.
func TestScanAbortsOnRealIOErrorReadingManifest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.Chmod(manifestPath, 0o000); err != nil {
		t.Fatalf("failed to chmod manifest unreadable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(manifestPath, helpers.FileMod); err != nil {
			t.Errorf("failed to restore manifest perms: %v", err)
		}
	})

	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err == nil {
		t.Fatalf("expected Start to abort with a non-nil error on a real manifest read error, got nil")
	}
	// os.Stat only needs traversal permission on parent directories, not
	// read permission on the file itself, so this still proves the entry
	// survives untouched despite the file being unreadable.
	assertManifestSurvives(t, installDir)
}

// TestScanAbortsOnUnreadableCollectionDir pins that a non-ENOENT Lstat
// failure on MANIFEST.json (a mode-0 collection dir) aborts the run; "statat"
// tells it apart from the ReadFile ("openat") abort. Skipped as root.
func TestScanAbortsOnUnreadableCollectionDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	if err := os.Chmod(installDir, 0o000); err != nil {
		t.Fatalf("failed to chmod collection dir unreadable: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if err := os.Chmod(installDir, helpers.DirMod); err != nil {
			t.Errorf("failed to restore collection dir perms: %v", err)
		}
		restored = true
	}
	t.Cleanup(restore)

	registerCleanupProject(t, cacheDir, downloadPath)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if err == nil {
		t.Fatalf("expected Start to abort on an unreadable collection directory, got nil")
	}
	if !strings.Contains(err.Error(), downloadPath) {
		t.Fatalf("expected the error to name the collections path %s, got %v", downloadPath, err)
	}
	if !strings.Contains(err.Error(), "statat") {
		t.Fatalf("expected the error to name the failing statat call, distinguishing this abort from a ReadFile (openat) one, got %v", err)
	}

	// The abort precedes removeUnused; survival is checked only after
	// restore(), since a mode-0 directory blocks this test's own Stat too.
	restore()
	assertManifestSurvives(t, installDir)

	// Positive control: with the directory readable again the same fixture
	// completes and removes the unreferenced install.
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once the collection directory is readable again, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to be removed once the read succeeds, stat error: %v", statErr)
	}
}

// TestDryRunReportsSweepPlanError pins that a SweepPlan failure in a dry run
// is reported through Errorf without failing the run. Skipped as root.
func TestDryRunReportsSweepPlanError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedManifestAt(t, downloadPath, "ns", "name", "1.0.0")

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	seedSnapshotInstalled(t, cfg, newTestRuntime(), map[string]store.InstalledEntry{
		"ns.name@1.0.0": {ArtifactSHA256: "sha-keep"},
	})

	extractedRoot := filepath.Join(cacheDir, extracted.RootDirName)
	if err := os.MkdirAll(extractedRoot, helpers.DirMod); err != nil {
		t.Fatalf("failed to create extracted root: %v", err)
	}
	if err := os.Chmod(extractedRoot, 0o000); err != nil {
		t.Fatalf("failed to chmod extracted root unreadable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(extractedRoot, helpers.DirMod); err != nil {
			t.Errorf("failed to restore extracted root perms: %v", err)
		}
	})

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to still succeed despite a sweep-plan error, got %v", err)
	}
	if !printer.hasErrorContaining("Failed to plan extracted cache sweep") {
		t.Fatalf("expected an Errorf about the failed sweep plan, got: %v", printer.errs)
	}
}

// TestScanInstalledCollectionsMissingRootIsNotAnError pins that an
// ansible_collections vanishing between openProjectWorkspace's probe and the
// scan is a benign no-op, not an error.
func TestScanInstalledCollectionsMissingRootIsNotAnError(t *testing.T) {
	t.Parallel()
	collectionsPath := t.TempDir() // no ansible_collections subdirectory created
	ws := openTestWorkspace(t, collectionsPath)
	defer func() { _ = ws.root.Close() }()
	index := make(map[string][]installedCollection)
	byKey := make(map[string][]installedCollection)
	deps := make(map[string]map[string]string)

	if err := scanInstalledCollections(noopPrinter{}, ws, index, byKey, deps); err != nil {
		t.Fatalf("expected nil error for a missing ansible_collections root, got %v", err)
	}
	if len(index) != 0 || len(byKey) != 0 || len(deps) != 0 {
		t.Fatalf("expected no entries to be added, got index=%v byKey=%v deps=%v", index, byKey, deps)
	}
}

// TestScanNamespaceDirVanishedIsNotAnError pins that scanNamespaceDir treats
// a vanished namespace directory as a benign no-op.
func TestScanNamespaceDirVanishedIsNotAnError(t *testing.T) {
	t.Parallel()
	collectionsPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(collectionsPath, "ansible_collections"), helpers.DirMod); err != nil {
		t.Fatalf("failed to create ansible_collections root: %v", err)
	}
	ws := openTestWorkspace(t, collectionsPath) // root exists; the "does-not-exist" ns subdir under it does not
	defer func() { _ = ws.root.Close() }()
	index := make(map[string][]installedCollection)
	byKey := make(map[string][]installedCollection)
	deps := make(map[string]map[string]string)

	if err := scanNamespaceDir(noopPrinter{}, ws, "does-not-exist", index, byKey, deps); err != nil {
		t.Fatalf("expected nil error for a vanished namespace dir, got %v", err)
	}
	if len(index) != 0 || len(byKey) != 0 || len(deps) != 0 {
		t.Fatalf("expected no entries to be added, got index=%v byKey=%v deps=%v", index, byKey, deps)
	}
}

// seedEscapingWorkspaceFixture makes a collections path whose
// ansible_collections is an absolute symlink out of it to a real manifest, and
// returns its ProjectRecord and that manifest's path; skips without symlinks.
func seedEscapingWorkspaceFixture(t *testing.T, reqPath string) (store.ProjectRecord, string) {
	t.Helper()
	escapingCollectionsPath := t.TempDir()
	outsideDir := t.TempDir()
	outsideManifestDir := filepath.Join(outsideDir, "outside", "coll")
	if err := os.MkdirAll(outsideManifestDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create outside manifest dir: %v", err)
	}
	outsideManifest := `{
		"collection_info": {
			"namespace": "outside",
			"name": "coll",
			"version": "1.0.0"
		}
	}`
	outsideManifestPath := filepath.Join(outsideManifestDir, "MANIFEST.json")
	if err := os.WriteFile(outsideManifestPath, []byte(outsideManifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write outside manifest: %v", err)
	}

	acLink := filepath.Join(escapingCollectionsPath, "ansible_collections")
	if err := os.Symlink(outsideDir, acLink); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	return store.ProjectRecord{
		RequirementsFile: reqPath,
		CollectionsPath:  escapingCollectionsPath,
		LastRun:          time.Now().UTC(),
	}, outsideManifestPath
}

// TestStartSkipsProjectWhoseWorkspaceEscapes pins that a project whose
// ansible_collections symlinks out of its collections path is skipped with a
// warning, while every other project's cleanup still runs.
func TestStartSkipsProjectWhoseWorkspaceEscapes(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write shared requirements file: %v", err)
	}
	escapingProject, outsideManifestPath := seedEscapingWorkspaceFixture(t, reqPath)

	healthyDownloadPath := t.TempDir()
	installDir := seedInstallTree(t, healthyDownloadPath)

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"escaping-project": escapingProject,
			"healthy-project": {
				RequirementsFile: reqPath,
				CollectionsPath:  healthyDownloadPath,
				LastRun:          time.Now().UTC(),
			},
		},
	})

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	// openProjectWorkspace probes through the root: a plain os.Stat would
	// follow the escape, and the rooted scan would then abort the whole run.
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to finish despite the escaping workspace, got %v", err)
	}
	// The skip warning must name the project.
	if !printer.hasWarningContaining(`skipping project "escaping-project"`) {
		t.Fatalf("expected a warning naming the skipped project, got: %v", printer.warnings)
	}
	// removeWorkspaceFiles would refuse this removal on its own; the pinned
	// form is TestDryRunReportsNoCandidatesFromEscapingWorkspace.
	if _, statErr := os.Stat(outsideManifestPath); statErr != nil {
		t.Fatalf("expected the outside manifest to survive, stat error: %v", statErr)
	}
	// Positive control on the identical run: proves the fixture is capable
	// of a real removal, so the nil error above means the run actually did
	// its job rather than merely failing to crash.
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the healthy project's installed collection to be removed, stat error: %v", statErr)
	}
}

// TestStartScansWorkspaceBehindRelativeSymlink pins that a relative in-root
// ansible_collections symlink is scanned and cleaned normally, so the probe
// must never become a blanket rejection of symlinked workspaces.
func TestStartScansWorkspaceBehindRelativeSymlink(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	collectionsPath := t.TempDir()

	installDir := filepath.Join(collectionsPath, "real_ac", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create real_ac install dir: %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}

	acLink := filepath.Join(collectionsPath, "ansible_collections")
	if err := os.Symlink("real_ac", acLink); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	registerCleanupProject(t, cacheDir, collectionsPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed scanning through a relative in-root symlink, got %v", err)
	}
	if printer.hasWarningContaining("skipping project") {
		t.Fatalf("expected no skip warning for a relative in-root symlink, got: %v", printer.warnings)
	}
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the collection behind the relative symlink to be removed, stat error: %v", statErr)
	}
}

// TestStartScansThroughSymlinkedCollectionsPath pins that a registered
// collections path that is itself a symlink is scanned and cleaned normally;
// it breaks if the root moves down to ansible_collections.
func TestStartScansThroughSymlinkedCollectionsPath(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	realCollectionsDir := t.TempDir()
	installDir := seedInstallTree(t, realCollectionsDir)

	collectionsPathLink := filepath.Join(t.TempDir(), "collections-link")
	if err := os.Symlink(realCollectionsDir, collectionsPathLink); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	registerCleanupProject(t, cacheDir, collectionsPathLink)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed through a symlinked collections path, got %v", err)
	}
	if printer.hasWarningContaining("skipping project") {
		t.Fatalf("expected no skip warning for a symlinked collections path, got: %v", printer.warnings)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the collection behind the symlinked collections path to be removed, stat error: %v", statErr)
	}
}

// TestDryRunReportsNoCandidatesFromEscapingWorkspace pins that a dry run on
// the escaping fixture warns about the skip and previews no removal outside it.
func TestDryRunReportsNoCandidatesFromEscapingWorkspace(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	escapingProject, outsideManifestPath := seedEscapingWorkspaceFixture(t, reqPath)

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"escaping-project": escapingProject,
		},
	})

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to finish despite the escaping workspace, got %v", err)
	}
	// A candidate here would preview a removal no real run could perform.
	if printer.hasPrintContaining("would remove outside.coll@1.0.0") {
		t.Fatalf("expected no dry-run candidate for a collection outside the collections tree, got prints: %v", printer.prints)
	}
	// The skip warning must name the project.
	if !printer.hasWarningContaining(`skipping project "escaping-project"`) {
		t.Fatalf("expected a warning naming the skipped project, got: %v", printer.warnings)
	}
	if _, statErr := os.Stat(outsideManifestPath); statErr != nil {
		t.Fatalf("expected the outside manifest to survive, stat error: %v", statErr)
	}
}

// TestScanAbortsOnUnreadableWorkspaceRoot pins that a real IO error reading
// ansible_collections aborts the run, wrapped with the collections path,
// unlike an escaping symlink, which is skipped. Skipped as root.
func TestScanAbortsOnUnreadableWorkspaceRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	acDir := filepath.Join(downloadPath, "ansible_collections")

	if err := os.Chmod(acDir, 0o000); err != nil {
		t.Fatalf("failed to chmod ansible_collections unreadable: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if err := os.Chmod(acDir, helpers.DirMod); err != nil {
			t.Errorf("failed to restore ansible_collections perms: %v", err)
		}
		restored = true
	}
	t.Cleanup(restore)

	registerCleanupProject(t, cacheDir, downloadPath)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if err == nil {
		t.Fatalf("expected Start to abort on an unreadable ansible_collections root, got nil")
	}
	if !strings.Contains(err.Error(), downloadPath) {
		t.Fatalf("expected the error to name the collections path %s, got %v", downloadPath, err)
	}
	// The abort precedes removeUnused; survival is checked only after
	// restore(), since a mode-0 directory blocks this test's own Stat too.
	restore()
	assertManifestSurvives(t, installDir)

	// Positive control: with ansible_collections readable again the same
	// fixture completes and removes the unreferenced install.
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once ansible_collections is readable again, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to be removed once the read succeeds, stat error: %v", statErr)
	}
}

// buildNamespaceSymlinkFixture registers a project whose ns.name@1.0.0 sits
// under a real ansible_collections/ns (safe) or behind an in-root symlink from
// it to a sibling hidden_ns (unsafe), and returns the manifest's real path.
func buildNamespaceSymlinkFixture(t *testing.T, cacheDir string, safe bool) string {
	t.Helper()
	collectionsPath := t.TempDir()
	hiddenNsDir := filepath.Join(collectionsPath, "hidden_ns")
	hiddenInstallDir := filepath.Join(hiddenNsDir, "name")
	if err := os.MkdirAll(hiddenInstallDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create hidden namespace install dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hiddenInstallDir, "MANIFEST.json"), []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}

	acDir := filepath.Join(collectionsPath, "ansible_collections")
	if err := os.MkdirAll(acDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create ansible_collections dir: %v", err)
	}
	nsPath := filepath.Join(acDir, "ns")

	var manifestPath string
	if safe {
		if err := os.Rename(hiddenNsDir, nsPath); err != nil {
			t.Fatalf("failed to move the namespace dir into place: %v", err)
		}
		manifestPath = filepath.Join(nsPath, "name", "MANIFEST.json")
	} else {
		if err := os.Symlink("../hidden_ns", nsPath); err != nil {
			t.Skipf("symlinks unavailable on this platform: %v", err)
		}
		manifestPath = filepath.Join(hiddenNsDir, "name", "MANIFEST.json")
	}

	registerCleanupProject(t, cacheDir, collectionsPath)
	return manifestPath
}

// TestScanSkipsSymlinkedNamespaceEntry pins that a symlinked namespace entry
// is never indexed (DirEntry.IsDir is false for a symlink), which the rooting
// alone does not provide; a real namespace directory is the control.
func TestScanSkipsSymlinkedNamespaceEntry(t *testing.T) {
	t.Parallel()

	t.Run("symlinked namespace entry is not indexed", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		manifestPath := buildNamespaceSymlinkFixture(t, cacheDir, false)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		if _, statErr := os.Stat(manifestPath); statErr != nil {
			t.Fatalf("expected the manifest behind the symlinked namespace entry to survive unindexed, stat error: %v", statErr)
		}
	})

	t.Run("real namespace directory is indexed and removed", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		manifestPath := buildNamespaceSymlinkFixture(t, cacheDir, true)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
			t.Fatalf("expected the manifest behind a real namespace directory to be removed, stat error: %v", statErr)
		}
	})
}

// buildNameSymlinkFixture is buildNamespaceSymlinkFixture with the symlink at
// the name entry itself; it returns the manifest's real path and the
// ansible_collections/ns/name entry's own path.
func buildNameSymlinkFixture(t *testing.T, cacheDir string, safe bool) (string, string) {
	t.Helper()
	collectionsPath := t.TempDir()
	hiddenNameDir := filepath.Join(collectionsPath, "hidden_name")
	if err := os.MkdirAll(hiddenNameDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create hidden name install dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hiddenNameDir, "MANIFEST.json"), []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}

	nsDir := filepath.Join(collectionsPath, "ansible_collections", "ns")
	if err := os.MkdirAll(nsDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create ansible_collections/ns dir: %v", err)
	}
	namePath := filepath.Join(nsDir, "name")

	var manifestPath string
	if safe {
		if err := os.Rename(hiddenNameDir, namePath); err != nil {
			t.Fatalf("failed to move the name dir into place: %v", err)
		}
		manifestPath = filepath.Join(namePath, "MANIFEST.json")
	} else {
		if err := os.Symlink("../../hidden_name", namePath); err != nil {
			t.Skipf("symlinks unavailable on this platform: %v", err)
		}
		manifestPath = filepath.Join(hiddenNameDir, "MANIFEST.json")
	}

	registerCleanupProject(t, cacheDir, collectionsPath)
	return manifestPath, namePath
}

// TestScanSkipsSymlinkedNameEntry pins that a symlinked name entry is neither
// indexed nor reported removed. It is correctness, not containment: RemoveAll
// unlinks a leaf symlink, while a symlinked namespace would be traversed.
func TestScanSkipsSymlinkedNameEntry(t *testing.T) {
	t.Parallel()

	t.Run("symlinked name entry is not indexed", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		manifestPath, entryPath := buildNameSymlinkFixture(t, cacheDir, false)

		printer := &recordingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)
		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		// Without the name-level IsDir guard the symlink would be unlinked and
		// reported removed; the no-op printer could not see that line.
		if printer.hasPrintContaining("removed ns.name@1.0.0") {
			t.Fatalf("expected the symlinked name entry not to be reported as removed, got prints: %v", printer.prints)
		}
		// The entry itself must survive, not only go unreported.
		if _, statErr := os.Lstat(entryPath); statErr != nil {
			t.Fatalf("expected the symlink entry itself to survive, lstat error: %v", statErr)
		}
		// RemoveAll unlinks a leaf symlink rather than following it, so the
		// target survives either way.
		if _, statErr := os.Stat(manifestPath); statErr != nil {
			t.Fatalf("expected the manifest behind the symlinked name entry to survive, stat error: %v", statErr)
		}
	})

	t.Run("real name directory is indexed and removed", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		manifestPath, _ := buildNameSymlinkFixture(t, cacheDir, true)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
			t.Fatalf("expected the manifest behind a real name directory to be removed, stat error: %v", statErr)
		}
	})
}

// TestScanSkipsNonRegularManifest pins that a MANIFEST.json that is not a
// regular file by Lstat (any symlink, a directory, a named pipe) is warned
// about and skipped without failing the run, under a bound for the pipe case.
func TestScanSkipsNonRegularManifest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		// build shapes the hostile manifest entry under nameDir and returns its
		// path plus, for a symlink shape, its target's path.
		build func(t *testing.T, hostileCollectionsPath, nameDir string) (manifestPath, targetPath string)
		name  string
		// wantIndexed marks the positive control: a regular MANIFEST.json is
		// expected to be indexed and removed rather than warned about.
		wantIndexed bool
	}{
		{name: "absolute symlink targeting outside the root", build: buildManifestAbsoluteSymlinkOutsideRoot},
		{name: "absolute symlink resolving back inside the root", build: buildManifestAbsoluteSymlinkInsideRoot},
		{name: "relative in-root symlink", build: buildManifestRelativeInRootSymlink},
		// The directory shape is deliberately not skipped when symlinks are
		// unavailable: it needs no symlink support at all, and is the one
		// that must run on every platform.
		{name: "directory named MANIFEST.json", build: buildManifestAsDirectory},
		{name: "named pipe", build: buildManifestAsNamedPipe},
		{name: "positive control: regular file", build: buildManifestAsRegularFile, wantIndexed: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runNonRegularManifestCase(t, tc.build, tc.wantIndexed)
		})
	}
}

// buildManifestAbsoluteSymlinkOutsideRoot makes ansible_collections/ns/name/
// MANIFEST.json an absolute symlink to a real manifest file entirely outside
// the hostile project's own collections path.
func buildManifestAbsoluteSymlinkOutsideRoot(t *testing.T, _, nameDir string) (string, string) {
	t.Helper()
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "MANIFEST.json")
	if err := os.WriteFile(target, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write outside target manifest: %v", err)
	}
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	return manifestPath, target
}

// buildManifestAbsoluteSymlinkInsideRoot makes MANIFEST.json an absolute
// symlink that resolves back inside the collections path; os.Root refuses
// any absolute target, but the gate rejects it by Lstat regardless.
func buildManifestAbsoluteSymlinkInsideRoot(t *testing.T, hostileCollectionsPath, nameDir string) (string, string) {
	t.Helper()
	targetDir := filepath.Join(hostileCollectionsPath, "geometric-target")
	if err := os.MkdirAll(targetDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create geometric target dir: %v", err)
	}
	target := filepath.Join(targetDir, "MANIFEST.json")
	if err := os.WriteFile(target, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write geometrically-inside target manifest: %v", err)
	}
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	return manifestPath, target
}

// buildManifestRelativeInRootSymlink makes MANIFEST.json a relative in-root
// symlink, the one shape os.Root would follow if the gate did not reject it.
func buildManifestRelativeInRootSymlink(t *testing.T, hostileCollectionsPath, nameDir string) (string, string) {
	t.Helper()
	targetDir := filepath.Join(hostileCollectionsPath, "relative-target")
	if err := os.MkdirAll(targetDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create relative target dir: %v", err)
	}
	target := filepath.Join(targetDir, "MANIFEST.json")
	if err := os.WriteFile(target, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write relative target manifest: %v", err)
	}
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	rel, err := filepath.Rel(nameDir, target)
	if err != nil {
		t.Fatalf("failed to compute relative target: %v", err)
	}
	if err := os.Symlink(rel, manifestPath); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	return manifestPath, target
}

// buildManifestAsDirectory makes ansible_collections/ns/name/MANIFEST.json a
// directory instead of a file - the shape that needs no symlink support at
// all.
func buildManifestAsDirectory(t *testing.T, _, nameDir string) (string, string) {
	t.Helper()
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := os.MkdirAll(manifestPath, helpers.DirMod); err != nil {
		t.Fatalf("failed to create directory-shaped manifest: %v", err)
	}
	return manifestPath, ""
}

// buildManifestAsNamedPipe makes MANIFEST.json a named pipe, which Lstat
// classifies instantly but an unguarded ReadFile would block on in open().
func buildManifestAsNamedPipe(t *testing.T, _, nameDir string) (string, string) {
	t.Helper()
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := syscall.Mkfifo(manifestPath, 0o644); err != nil {
		t.Skipf("named pipes unavailable on this platform: %v", err)
	}
	return manifestPath, ""
}

// buildManifestAsRegularFile writes a genuine, valid MANIFEST.json at
// ansible_collections/ns/name/MANIFEST.json - the positive control every
// non-regular shape above is contrasted against.
func buildManifestAsRegularFile(t *testing.T, _, nameDir string) (string, string) {
	t.Helper()
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write regular manifest: %v", err)
	}
	return manifestPath, ""
}

// nonRegularManifestStartBound bounds runStartBounded so a regression that
// reaches the named pipe's blocking open() fails fast instead of hanging.
const nonRegularManifestStartBound = 10 * time.Second

// runStartBounded runs Start on its own goroutine and fails the test if it
// does not return within nonRegularManifestStartBound; a goroutine still
// blocked in open() is abandoned.
func runStartBounded(t *testing.T, cfg *config.Config, runtime *infra.Infra) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Start(t.Context(), cfg, runtime)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
	case <-time.After(nonRegularManifestStartBound):
		t.Fatalf(
			"Start did not return within %s: the manifest gate did not reject this shape, so the read blocked on open",
			nonRegularManifestStartBound,
		)
	}
}

// requirementsFifoStartBound bounds startBoundedErr so a regression that
// reaches a requirements-file pipe's blocking open() fails fast.
const requirementsFifoStartBound = 10 * time.Second

// startBoundedErr is runStartBounded's sibling that returns Start's error,
// failing the test if Start does not return within requirementsFifoStartBound.
func startBoundedErr(t *testing.T, cfg *config.Config, runtime *infra.Infra) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Start(t.Context(), cfg, runtime)
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(requirementsFifoStartBound):
		t.Fatalf(
			"Start did not return within %s: the requirements-file gate did not reject this shape, so the read blocked on open",
			requirementsFifoStartBound,
		)
		return nil // unreachable: t.Fatalf stops this goroutine before returning.
	}
}

// runNonRegularManifestCase runs Start over a hostile project shaped by build
// beside a healthy one, asserting the healthy install is removed and then
// either the positive-control or the warned-and-skipped outcome.
func runNonRegularManifestCase(
	t *testing.T,
	build func(t *testing.T, hostileCollectionsPath, nameDir string) (manifestPath, targetPath string),
	wantIndexed bool,
) {
	t.Helper()
	cacheDir := t.TempDir()

	hostileCollectionsPath := t.TempDir()
	nameDir := filepath.Join(hostileCollectionsPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(nameDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create hostile name dir: %v", err)
	}
	manifestPath, targetPath := build(t, hostileCollectionsPath, nameDir)

	healthyDownloadPath := t.TempDir()
	healthyInstallDir := seedInstallTree(t, healthyDownloadPath)

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write shared requirements file: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"hostile-project": {
				RequirementsFile: reqPath,
				CollectionsPath:  hostileCollectionsPath,
				LastRun:          time.Now().UTC(),
			},
			"healthy-project": {
				RequirementsFile: reqPath,
				CollectionsPath:  healthyDownloadPath,
				LastRun:          time.Now().UTC(),
			},
		},
	})

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	runStartBounded(t, cfg, runtime)

	// This is what proves the run actually did its job rather than merely
	// not crashing: the healthy project's own unreferenced collection must
	// still be removed despite the hostile project's manifest leaf.
	healthyManifestPath := filepath.Join(healthyInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(healthyManifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the healthy project's collection to be removed, stat error: %v", statErr)
	}

	if wantIndexed {
		assertHostileManifestIndexed(t, printer, manifestPath)
		return
	}
	assertHostileManifestWarnedAndSkipped(t, printer, manifestPath, targetPath)
}

// assertHostileManifestIndexed asserts the positive control: no non-regular
// warning, and the regular manifest was indexed and removed.
func assertHostileManifestIndexed(t *testing.T, printer *recordingPrinter, manifestPath string) {
	t.Helper()
	if printer.hasWarningContaining("skipping non-regular manifest") {
		t.Fatalf("expected no non-regular-manifest warning for a regular manifest, got: %v", printer.warnings)
	}
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the hostile project's own regular manifest to be indexed and removed, stat error: %v", statErr)
	}
}

// assertHostileManifestWarnedAndSkipped asserts a warning names the hostile
// manifest and, for a symlink shape, that its target survives.
func assertHostileManifestWarnedAndSkipped(t *testing.T, printer *recordingPrinter, manifestPath, targetPath string) {
	t.Helper()
	if !printer.hasWarningContaining(manifestPath) {
		t.Fatalf("expected a warning naming the hostile manifest path, got: %v", printer.warnings)
	}
	if targetPath == "" {
		return
	}
	if _, statErr := os.Stat(targetPath); statErr != nil {
		t.Fatalf("expected the symlink target to survive, stat error: %v", statErr)
	}
}

// TestSweepExtractedStoreNoopWhenCacheDirEmpty proves sweepExtractedStore's
// empty-CacheDir guard returns immediately without panicking or touching
// anything, called directly with an empty cfg.CacheDir.
func TestSweepExtractedStoreNoopWhenCacheDirEmpty(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{CacheDir: "", DryRun: false}
	runtime := newTestRuntime()
	st := store.New()

	sweepExtractedStore(t.Context(), cfg, runtime, st, map[string]bool{}, map[string][]installedCollection{}, roleReachability{})
}

// TestStartLeavesExtractedCacheWhenNoSnapshotPersisted pins that with no
// persisted snapshot the sweep is skipped: an empty keep set is no evidence,
// and sweeping on it would wipe the whole extracted store.
func TestStartLeavesExtractedCacheWhenNoSnapshotPersisted(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedExtractedDir(t, cacheDir, "sha-orphaned-by-no-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertExtractedDirsSurvive(t, cacheDir, "sha-orphaned-by-no-snapshot")
}

// TestStartDoesNotFabricatePersistedSnapshot pins that a run with no
// persisted snapshot saves none, since a persisted-and-empty one would read
// as evidence that nothing is installed or warmed.
func TestStartDoesNotFabricatePersistedSnapshot(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedExtractedDir(t, cacheDir, "sha-orphaned-by-no-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	reloaded := reloadStoreThroughFreshBackend(t, cfg, runtime)
	if reloaded.WasPersisted() {
		t.Fatal("expected no snapshot to have been persisted by a cleanup run that never loaded one")
	}
}

// reloadStoreThroughFreshBackend loads the store through a new backend, which
// is what the next run would see on disk.
func reloadStoreThroughFreshBackend(t *testing.T, cfg *config.Config, runtime *infra.Infra) *store.Store {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build a fresh backend: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open a fresh backend: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close the fresh backend: %v", err)
		}
	}()
	st, err := backend.LoadStore(t.Context())
	if err != nil {
		t.Fatalf("failed to load store through a fresh backend: %v", err)
	}
	return st
}

// TestStartTwiceWithNoSnapshotLeavesExtractedCacheIntact pins that the guard
// holds across runs: it needs both the read-side and the write-side halves,
// or the first run's fabricated snapshot lets the second one sweep.
func TestStartTwiceWithNoSnapshotLeavesExtractedCacheIntact(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedExtractedDir(t, cacheDir, "sha-orphaned-by-no-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected first Start to succeed, got %v", err)
	}
	assertExtractedDirsSurvive(t, cacheDir, "sha-orphaned-by-no-snapshot")

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected second Start to succeed, got %v", err)
	}
	assertExtractedDirsSurvive(t, cacheDir, "sha-orphaned-by-no-snapshot")
}

// TestStartSweepsOnlyWhenTheSnapshotEverRecordedContent pins that the sweep
// needs a snapshot that once recorded content, not merely a persisted one: a
// lock-written snapshot is empty without having looked at the disk.
func TestStartSweepsOnlyWhenTheSnapshotEverRecordedContent(t *testing.T) {
	t.Parallel()

	t.Run("never recorded content: kept", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		downloadPath := t.TempDir()
		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		// The shape a lock run leaves on a cache with no prior snapshot: the
		// save happened, so LastSnapshot is stamped, but nothing about on-disk
		// content was ever written.
		seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{})
		seedExtractedDir(t, cacheDir, "sha-orphan-never-recorded")
		recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		assertExtractedDirsSurvive(t, cacheDir, "sha-orphan-never-recorded")
	})

	t.Run("recorded content once, now empty: swept", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		downloadPath := t.TempDir()
		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		// A real install once recorded a collection here, and a later run
		// emptied the set. The snapshot genuinely knows nothing is installed,
		// which is evidence, so the sweep must still reclaim an orphan.
		seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
			"ns.name@1.0.0": {ArtifactSHA256: "sha-once-installed"},
		})
		resaveLoadedSnapshot(t, cfg, runtime, func(st *store.Store) {
			st.DeleteInstalled("ns.name@1.0.0")
		})
		seedExtractedDir(t, cacheDir, "sha-orphan-after-recorded-content")
		recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		assertExtractedDirGone(t, cacheDir, "sha-orphan-after-recorded-content")
	})
}

// TestStartRemovesUnreferencedInstallWithNoPersistedSnapshot pins that
// removeUnused does not depend on the snapshot: an unreferenced install is
// removed even when no snapshot was ever persisted.
func TestStartRemovesUnreferencedInstallWithNoPersistedSnapshot(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to be removed even with no persisted snapshot, stat error: %v", statErr)
	}
}

// TestDryRunReportsNoExtractedSweepWithNoPersistedSnapshot pins that the
// snapshot guard precedes the dry-run branch, so a dry run reports no sweep a
// real run would not perform.
func TestDryRunReportsNoExtractedSweepWithNoPersistedSnapshot(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	runtime := newTestRuntime()

	seedExtractedDir(t, cacheDir, "sha-orphaned-by-no-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	printer := &recordingPrinter{}
	dryRunRuntime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, dryRunRuntime); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}

	if printer.hasPrintContaining("would sweep extracted") {
		t.Fatalf("expected no would-sweep-extracted report with no persisted snapshot, got prints: %v", printer.prints)
	}
	assertExtractedDirsSurvive(t, cacheDir, "sha-orphaned-by-no-snapshot")
}

// The tests below pin two things: a scanned collection's namespace and name
// come from the walked ansible_collections/<ns>/<name> pair, never from its
// manifest; and buildReachable's two-phase, sorted-project reachability.

// seedManifestWithIdentity writes, under ansible_collections/<dirNs>/<dirName>,
// a MANIFEST.json that declares jsonNs.jsonName instead.
func seedManifestWithIdentity(t *testing.T, root, dirNs, dirName, jsonNs, jsonName, version string) {
	t.Helper()
	installDir := filepath.Join(root, "ansible_collections", dirNs, dirName)
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir for %s/%s: %v", dirNs, dirName, err)
	}
	manifest := manifestJSON(jsonNs, jsonName, version, nil)
	if err := os.WriteFile(filepath.Join(installDir, "MANIFEST.json"), []byte(manifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write manifest for %s/%s: %v", dirNs, dirName, err)
	}
}

// TestScanIdentityComesFromWalkedDirectoryNotManifest pins that a manifest
// at evil/pkg claiming victim.collection cannot redirect deletion at the real
// victim.collection; the hostile record is removed from its own directory.
func TestScanIdentityComesFromWalkedDirectoryNotManifest(t *testing.T) {
	t.Parallel()

	t.Run("hostile manifest cannot redirect deletion", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		downloadPath := t.TempDir()
		seedManifestAt(t, downloadPath, "victim", "collection", "1.0.0")
		seedManifestWithIdentity(t, downloadPath, "evil", "pkg", "victim", "collection", "9.9.9")

		reqPath := filepath.Join(t.TempDir(), "requirements.yml")
		reqYAML := "collections:\n  - name: victim.collection\n    version: \"==1.0.0\"\n"
		if err := os.WriteFile(reqPath, []byte(reqYAML), helpers.FileMod); err != nil {
			t.Fatalf("failed to write requirements file: %v", err)
		}
		registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}

		if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "victim", "collection", "MANIFEST.json")); statErr != nil {
			t.Fatalf("expected victim.collection to survive, stat error: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "evil", "pkg", "MANIFEST.json")); !os.IsNotExist(statErr) {
			t.Fatalf("expected the hostile fixture's own directory (evil/pkg) to be removed, stat error: %v", statErr)
		}
	})

	// Positive control: requiring evil.pkg keeps the hostile fixture, proving
	// its scanned identity is evil.pkg and not what its manifest claims.
	t.Run("positive control: evil.pkg survives its own requirement", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		downloadPath := t.TempDir()
		seedManifestAt(t, downloadPath, "victim", "collection", "1.0.0")
		seedManifestWithIdentity(t, downloadPath, "evil", "pkg", "victim", "collection", "9.9.9")

		reqPath := filepath.Join(t.TempDir(), "requirements.yml")
		if err := os.WriteFile(reqPath, []byte("collections:\n  - evil.pkg\n"), helpers.FileMod); err != nil {
			t.Fatalf("failed to write requirements file: %v", err)
		}
		registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}

		assertManifestPresentAt(t, downloadPath, "evil", "pkg")
		assertManifestAbsentAt(t, downloadPath, "victim", "collection")
	})
}

// scanHostileEvilPkgRecord scans the evil/pkg fixture through
// scanCollectionDir and returns its single record, failing unless it is keyed
// as evil.pkg.
func scanHostileEvilPkgRecord(t *testing.T, downloadPath string) installedCollection {
	t.Helper()
	ws := openTestWorkspace(t, downloadPath)
	defer func() { _ = ws.root.Close() }()
	index := make(map[string][]installedCollection)
	byKey := make(map[string][]installedCollection)
	deps := make(map[string]map[string]string)
	if err := scanCollectionDir(noopPrinter{}, ws, "evil", "pkg", index, byKey, deps); err != nil {
		t.Fatalf("failed to scan the hostile fixture: %v", err)
	}
	insts := byKey["evil.pkg@9.9.9"]
	if len(insts) != 1 {
		t.Fatalf("expected exactly one scanned record keyed evil.pkg@9.9.9, got %d: %+v", len(insts), insts)
	}
	inst := insts[0]
	if inst.Namespace != "evil" || inst.Name != "pkg" {
		t.Fatalf("expected the scanned record's identity to be evil/pkg (the walked directory), got ns=%q name=%q", inst.Namespace, inst.Name)
	}
	return inst
}

// seedEmptyDirs creates each of dirs as an empty directory, failing the test
// on any MkdirAll error.
func seedEmptyDirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
			t.Fatalf("failed to create dir %s: %v", dir, err)
		}
	}
}

// TestRemoveInstalledArtifactAndSidecarFollowWalkedIdentity pins that the
// artifact purge and the .info sidecar removal use the walked identity
// (evil.pkg), never the one the manifest claims.
func TestRemoveInstalledArtifactAndSidecarFollowWalkedIdentity(t *testing.T) {
	t.Parallel()
	downloadPath := t.TempDir()
	seedManifestWithIdentity(t, downloadPath, "evil", "pkg", "victim", "collection", "9.9.9")
	inst := scanHostileEvilPkgRecord(t, downloadPath)

	// One sidecar under the walked identity, one under the claimed one; only
	// the former may be removed.
	realSidecar := filepath.Join(downloadPath, "ansible_collections", "evil.pkg-9.9.9.info")
	falseSidecar := filepath.Join(downloadPath, "ansible_collections", "victim.collection-9.9.9.info")
	seedEmptyDirs(t, realSidecar, falseSidecar)

	st := store.New()
	const source = "https://galaxy.example.com/api"
	st.SetInstalled(inst.Key, store.InstalledEntry{Source: source, ArtifactSHA256: "deadbeef"})
	resolvedSource := installedSource(st, inst.Key)

	artifacts := &recordingArtifactStore{}
	if err := removeInstalled(t.Context(), inst, artifacts, resolvedSource); err != nil {
		t.Fatalf("expected removeInstalled to succeed, got %v", err)
	}

	wantKey := helpers.ArtifactKey(source, "evil-pkg-9.9.9.tar.gz")
	if len(artifacts.deleted) != 1 || artifacts.deleted[0] != wantKey {
		t.Fatalf("expected Delete to be called once with key %q, got %v", wantKey, artifacts.deleted)
	}
	if _, statErr := os.Stat(realSidecar); !os.IsNotExist(statErr) {
		t.Fatalf("expected the walked-identity sidecar to be removed, stat error: %v", statErr)
	}
	if _, statErr := os.Stat(falseSidecar); statErr != nil {
		t.Fatalf("expected the manifest-claimed sidecar to survive untouched, stat error: %v", statErr)
	}
}

// TestBuildReachablePhase2ReachesDirectCrossProjectRequirement pins that
// every workspace is scanned before any root resolves: proj-a's requirement
// keeps the foo.bar only proj-b holds, though proj-a sorts first.
func TestBuildReachablePhase2ReachesDirectCrossProjectRequirement(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPathA := t.TempDir()
	downloadPathB := t.TempDir()

	seedManifestAt(t, downloadPathA, "other", "thing", "1.0.0")
	seedManifestAt(t, downloadPathB, "foo", "bar", "1.0.0")

	reqPathA := filepath.Join(t.TempDir(), "requirements-a.yml")
	reqA := "collections:\n  - name: foo.bar\n    version: \">=1.0.0\"\n"
	if err := os.WriteFile(reqPathA, []byte(reqA), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project A: %v", err)
	}
	reqPathB := filepath.Join(t.TempDir(), "requirements-b.yml")
	if err := os.WriteFile(reqPathB, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project B: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"proj-a": {RequirementsFile: reqPathA, CollectionsPath: downloadPathA, LastRun: time.Now().UTC()},
			"proj-b": {RequirementsFile: reqPathB, CollectionsPath: downloadPathB, LastRun: time.Now().UTC()},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(downloadPathB, "ansible_collections", "foo", "bar", "MANIFEST.json")); statErr != nil {
		t.Fatalf("expected foo.bar to survive via project A's cross-project requirement, stat error: %v", statErr)
	}
	assertManifestAbsentAt(t, downloadPathA, "other", "thing")
}

// TestBuildReachablePhase2FollowsTransitiveDependencyEdge pins the same over
// a manifest dependency edge: proj-a's top.level keeps proj-b's dep.leaf,
// while proj-b's unrelated.extra is removed as the control.
func TestBuildReachablePhase2FollowsTransitiveDependencyEdge(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPathA := t.TempDir()
	downloadPathB := t.TempDir()

	seedManifestWithDeps(t, downloadPathA, "top", "level", "1.0.0", map[string]string{"dep.leaf": ">=1.0.0"})
	seedManifestAt(t, downloadPathB, "dep", "leaf", "1.0.0")
	seedManifestAt(t, downloadPathB, "unrelated", "extra", "1.0.0")

	reqPathA := filepath.Join(t.TempDir(), "requirements-a.yml")
	if err := os.WriteFile(reqPathA, []byte("collections:\n  - top.level\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project A: %v", err)
	}
	reqPathB := filepath.Join(t.TempDir(), "requirements-b.yml")
	if err := os.WriteFile(reqPathB, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project B: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"proj-a": {RequirementsFile: reqPathA, CollectionsPath: downloadPathA, LastRun: time.Now().UTC()},
			"proj-b": {RequirementsFile: reqPathB, CollectionsPath: downloadPathB, LastRun: time.Now().UTC()},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertManifestPresentAt(t, downloadPathA, "top", "level")
	if _, statErr := os.Stat(filepath.Join(downloadPathB, "ansible_collections", "dep", "leaf", "MANIFEST.json")); statErr != nil {
		t.Fatalf("expected dep.leaf to survive via top.level's transitive dependency, stat error: %v", statErr)
	}
	assertManifestAbsentAt(t, downloadPathB, "unrelated", "extra")
}

// buildSkippedProjectRootsFixture registers "aa-holder" holding foo.bar and a
// workspace-less "zz-requires-only" requiring foo.bar when requireFooBar; the
// skipped project sorts last, so ordering cannot explain a survival.
func buildSkippedProjectRootsFixture(t *testing.T, cacheDir string, requireFooBar bool) string {
	t.Helper()
	holderPath := t.TempDir()
	seedManifestAt(t, holderPath, "foo", "bar", "1.0.0")

	holderReqPath := filepath.Join(t.TempDir(), "requirements-holder.yml")
	if err := os.WriteFile(holderReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write holder requirements file: %v", err)
	}

	skippedCollectionsPath := t.TempDir()
	skippedReqPath := filepath.Join(t.TempDir(), "requirements-skipped.yml")
	skippedReqYAML := "collections: []\n"
	if requireFooBar {
		skippedReqYAML = "collections:\n  - foo.bar\n"
	}
	if err := os.WriteFile(skippedReqPath, []byte(skippedReqYAML), helpers.FileMod); err != nil {
		t.Fatalf("failed to write skipped project's requirements file: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"aa-holder":        {RequirementsFile: holderReqPath, CollectionsPath: holderPath, LastRun: time.Now().UTC()},
			"zz-requires-only": {RequirementsFile: skippedReqPath, CollectionsPath: skippedCollectionsPath, LastRun: time.Now().UTC()},
		},
	})
	return holderPath
}

// TestBuildReachableSkippedProjectStillContributesRoots pins that a project
// whose workspace is absent still contributes its requirements roots,
// keeping another project's copy alive.
func TestBuildReachableSkippedProjectStillContributesRoots(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	holderPath := buildSkippedProjectRootsFixture(t, cacheDir, true)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(holderPath, "ansible_collections", "foo", "bar", "MANIFEST.json")); statErr != nil {
		t.Fatalf("expected foo.bar to survive via the skipped project's own requirement, stat error: %v", statErr)
	}
}

// TestBuildReachableSkippedProjectRootsControl is the positive control: when
// the skipped project does not name foo.bar, the holder's copy is removed.
func TestBuildReachableSkippedProjectRootsControl(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	holderPath := buildSkippedProjectRootsFixture(t, cacheDir, false)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	assertManifestAbsentAt(t, holderPath, "foo", "bar")
}

// buildStaleRegistryFixture registers a "stale-project" whose requirements
// file holds staleContent (nil leaves it absent) and a healthy "other-project";
// it returns the stale download path and the other project's install dir.
func buildStaleRegistryFixture(t *testing.T, cacheDir string, staleContent []byte) (string, string) {
	t.Helper()
	staleDownloadPath := t.TempDir()
	seedManifestAt(t, staleDownloadPath, "stale", "coll", "1.0.0")
	staleReqPath := filepath.Join(t.TempDir(), "stale-requirements.yml")
	if staleContent != nil {
		if err := os.WriteFile(staleReqPath, staleContent, helpers.FileMod); err != nil {
			t.Fatalf("failed to write stale project's requirements file: %v", err)
		}
	}

	otherDownloadPath := t.TempDir()
	otherInstallDir := seedInstallTree(t, otherDownloadPath)
	otherReqPath := filepath.Join(t.TempDir(), "requirements-other.yml")
	if err := os.WriteFile(otherReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write other project's requirements file: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"stale-project": {RequirementsFile: staleReqPath, CollectionsPath: staleDownloadPath, LastRun: time.Now().UTC()},
			"other-project": {RequirementsFile: otherReqPath, CollectionsPath: otherDownloadPath, LastRun: time.Now().UTC()},
		},
	})
	return staleDownloadPath, otherInstallDir
}

// TestStaleRegistryEntryToleratedWithOtherProjectCleanup pins that a missing
// requirements file is a warned stale entry and other projects still clean up.
func TestStaleRegistryEntryToleratedWithOtherProjectCleanup(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	_, otherInstallDir := buildStaleRegistryFixture(t, cacheDir, nil)

	printer := &recordingPrinter{}
	runtime := newTestRuntimeWith(printer)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if !printer.hasWarningContaining("no longer exists") {
		t.Fatalf("expected a warning about the missing requirements file, got: %v", printer.warnings)
	}
	manifestPath := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the other project's unreferenced copy to still be removed, stat error: %v", statErr)
	}
}

// TestStaleRegistryEntryPositiveControlUnparseableAborts pins that on the
// same fixture an unparseable file aborts the run instead and nothing is
// deleted, the healthy project's unreferenced copy included.
func TestStaleRegistryEntryPositiveControlUnparseableAborts(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	staleDownloadPath, otherInstallDir := buildStaleRegistryFixture(t, cacheDir, []byte("{invalid"))

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}

	staleManifest := filepath.Join(staleDownloadPath, "ansible_collections", "stale", "coll", "MANIFEST.json")
	if _, statErr := os.Stat(staleManifest); statErr != nil {
		t.Fatalf("expected the stale project's own install to survive the abort, stat error: %v", statErr)
	}
	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); statErr != nil {
		t.Fatalf("expected the other project's install to survive the abort untouched, stat error: %v", statErr)
	}
}

// buildUnscannedProjectFixture registers a workspace-less project whose
// requirements file holds reqContent and a healthy one with an unreferenced
// install; it returns that requirements path and the healthy install dir.
func buildUnscannedProjectFixture(t *testing.T, cacheDir string, reqContent []byte) (string, string) {
	t.Helper()
	absentCollectionsPath := t.TempDir()
	reqPath := filepath.Join(t.TempDir(), "unparseable.yml")
	if err := os.WriteFile(reqPath, reqContent, helpers.FileMod); err != nil {
		t.Fatalf("failed to write the absent-workspace project's requirements file: %v", err)
	}

	otherDownloadPath := t.TempDir()
	otherInstallDir := seedInstallTree(t, otherDownloadPath)
	otherReqPath := filepath.Join(t.TempDir(), "requirements-other.yml")
	if err := os.WriteFile(otherReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write other project's requirements file: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"absent-workspace-project": {RequirementsFile: reqPath, CollectionsPath: absentCollectionsPath, LastRun: time.Now().UTC()},
			"other-project":            {RequirementsFile: otherReqPath, CollectionsPath: otherDownloadPath, LastRun: time.Now().UTC()},
		},
	})
	return reqPath, otherInstallDir
}

// TestUnparseableRequirementsAbortsEvenForUnscannedProject pins that an
// unparseable requirements file aborts the run even when its project's
// workspace was never scanned, and that the error names that file.
func TestUnparseableRequirementsAbortsEvenForUnscannedProject(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	unparseableReqPath, otherInstallDir := buildUnscannedProjectFixture(t, cacheDir, []byte("{invalid"))

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}
	if !strings.Contains(err.Error(), unparseableReqPath) {
		t.Fatalf("expected the error to name the failing requirements file %q, got %v", unparseableReqPath, err)
	}

	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); statErr != nil {
		t.Fatalf("expected the other project's install to survive the abort untouched, stat error: %v", statErr)
	}
}

// TestUnparseableRequirementsAbortsEvenForUnscannedProjectPositiveControl pins
// that a valid, empty file on the same fixture lets Start remove the other
// project's unreferenced install.
func TestUnparseableRequirementsAbortsEvenForUnscannedProjectPositiveControl(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	_, otherInstallDir := buildUnscannedProjectFixture(t, cacheDir, []byte("collections: []\n"))

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the other project's unreferenced install to be removed, stat error: %v", statErr)
	}
}

// TestScanNamespaceDirAbortsOnUnreadableNamespaceDir pins that a mode-0
// namespace dir aborts the run at scanNamespaceDir's ReadDir, told apart from
// the manifest-level aborts by "openat ansible_collections/ns:". Skipped as root.
func TestScanNamespaceDirAbortsOnUnreadableNamespaceDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	nsDir := filepath.Join(downloadPath, "ansible_collections", "ns")
	if err := os.Chmod(nsDir, 0o000); err != nil {
		t.Fatalf("failed to chmod namespace dir unreadable: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if err := os.Chmod(nsDir, helpers.DirMod); err != nil {
			t.Errorf("failed to restore namespace dir perms: %v", err)
		}
		restored = true
	}
	t.Cleanup(restore)

	registerCleanupProject(t, cacheDir, downloadPath)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if err == nil {
		t.Fatalf("expected Start to abort on an unreadable namespace directory, got nil")
	}
	if !strings.Contains(err.Error(), "openat ansible_collections/ns:") {
		t.Fatalf("expected the error to name the failing openat call on ansible_collections/ns, got %v", err)
	}

	// The abort precedes removeUnused; survival is checked only after
	// restore(), since a mode-0 directory blocks this test's own Stat too.
	restore()
	assertManifestSurvives(t, installDir)

	// Positive control: with the namespace directory readable again the same
	// fixture completes and removes the unreferenced install.
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once the namespace directory is readable again, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to be removed once the read succeeds, stat error: %v", statErr)
	}
}

// TestScanCollectionDirManifestMissingVersionIsBenignSkip pins that a
// manifest without a version is skipped silently and survives; only the
// relevant warnings are checked, since the no-snapshot warning always fires.
func TestScanCollectionDirManifestMissingVersionIsBenignSkip(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	installDir := filepath.Join(downloadPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir: %v", err)
	}
	manifestNoVersion := `{"collection_info": {"namespace": "ns", "name": "name"}}`
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(manifestNoVersion), helpers.FileMod); err != nil {
		t.Fatalf("failed to write manifest without a version: %v", err)
	}
	registerCleanupProject(t, cacheDir, downloadPath)

	printer := &recordingPrinter{}
	runtime := newTestRuntimeWith(printer)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if _, statErr := os.Stat(manifestPath); statErr != nil {
		t.Fatalf("expected the version-less install to survive as a benign skip, stat error: %v", statErr)
	}
	if printer.hasWarningContaining("unsafe identifier") {
		t.Fatalf("expected no unsafe-identifier warning for a manifest missing only its version, got: %v", printer.warnings)
	}
	if printer.hasWarningContaining("corrupt manifest") {
		t.Fatalf("expected no corrupt-manifest warning for a manifest missing only its version, got: %v", printer.warnings)
	}
}

// TestScanCollectionDirManifestMissingVersionPositiveControl pins that the
// same manifest with a version is indexed and, unreferenced, removed.
func TestScanCollectionDirManifestMissingVersionPositiveControl(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath) // ns.name@1.0.0, version present
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the version-complete install to be indexed and removed, stat error: %v", statErr)
	}
}

// buildCollectionsPathFallbackFixture registers a project with an empty
// CollectionsPath and an install under <project>/<fallbackDirName>, and
// returns that install's directory.
func buildCollectionsPathFallbackFixture(t *testing.T, cacheDir, fallbackDirName string) string {
	t.Helper()
	projectDir := t.TempDir()
	fallbackRoot := filepath.Join(projectDir, fallbackDirName)
	installDir := seedInstallTree(t, fallbackRoot)

	reqPath := filepath.Join(projectDir, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			projectDir: {RequirementsFile: reqPath, CollectionsPath: "", LastRun: time.Now().UTC()},
		},
	})
	return installDir
}

// TestOpenProjectWorkspaceFallsBackToDotCollections pins that an empty
// CollectionsPath falls back to <project>/.collections, scanned and cleaned.
func TestOpenProjectWorkspaceFallsBackToDotCollections(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	installDir := buildCollectionsPathFallbackFixture(t, cacheDir, ".collections")

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed scanning the .collections fallback, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install under .collections to be removed, stat error: %v", statErr)
	}
}

// TestOpenProjectWorkspaceFallsBackToCollections is
// TestOpenProjectWorkspaceFallsBackToDotCollections's sibling for the second,
// non-dotfile fallback candidate ("collections").
func TestOpenProjectWorkspaceFallsBackToCollections(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	installDir := buildCollectionsPathFallbackFixture(t, cacheDir, "collections")

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed scanning the collections fallback, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install under collections to be removed, stat error: %v", statErr)
	}
}

// TestOpenProjectWorkspacePrefersRecordedCollectionsPathOverFallback pins
// candidate order: the recorded CollectionsPath is opened first, so an
// unreferenced collection under <project>/.collections survives.
func TestOpenProjectWorkspacePrefersRecordedCollectionsPathOverFallback(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	projectDir := t.TempDir()

	recordedPath := t.TempDir()
	recordedInstallDir := seedInstallTree(t, recordedPath) // ns.name@1.0.0

	fallbackRoot := filepath.Join(projectDir, ".collections")
	fallbackInstallDir := filepath.Join(fallbackRoot, "ansible_collections", "sibling", "other")
	if err := os.MkdirAll(fallbackInstallDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create sibling fallback install dir: %v", err)
	}
	sibling := `{"collection_info": {"namespace": "sibling", "name": "other", "version": "1.0.0"}}`
	fallbackManifestPath := filepath.Join(fallbackInstallDir, "MANIFEST.json")
	if err := os.WriteFile(fallbackManifestPath, []byte(sibling), helpers.FileMod); err != nil {
		t.Fatalf("failed to write sibling manifest: %v", err)
	}

	reqPath := filepath.Join(projectDir, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			projectDir: {RequirementsFile: reqPath, CollectionsPath: recordedPath, LastRun: time.Now().UTC()},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	recordedManifest := filepath.Join(recordedInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(recordedManifest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the recorded CollectionsPath's unreferenced install to be removed, stat error: %v", statErr)
	}
	if _, statErr := os.Stat(fallbackManifestPath); statErr != nil {
		t.Fatalf("expected the .collections sibling to survive untouched, stat error: %v", statErr)
	}
}
