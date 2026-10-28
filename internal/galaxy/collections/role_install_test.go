package collections

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// TestOpenRolesRootShapes pins openRolesRoot's answers: an empty path is
// named, a dry run opens an existing directory, and a regular file fails in
// both modes.
func TestOpenRolesRootShapes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "roles-file")
	mustWriteFile(t, file, []byte("not a directory"))

	if _, err := openRolesRoot("  ", true); !errors.Is(err, errEmptyRolesPath) {
		t.Fatalf("openRolesRoot(blank) error = %v, want errEmptyRolesPath", err)
	}

	root, err := openRolesRoot(dir, false)
	if err != nil || root == nil {
		t.Fatalf("openRolesRoot(existing, dry run) = (%v, %v), want an open root", root, err)
	}
	_ = root.Close()

	for _, create := range []bool{false, true} {
		root, err := openRolesRoot(file, create)
		if err == nil || root != nil || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("openRolesRoot(regular file, create=%v) = (%v, %v), want an error that is not absence", create, root, err)
		}
	}
}

// newRoleInstallDeps is newRefetchDeps plus a roles root under a fresh
// RolesPath, the shape installRole needs.
func newRoleInstallDeps(t *testing.T, client gitsource.Client) (installDeps, *capturingPrinter) {
	t.Helper()
	deps, printer := newRefetchDeps(t, client)
	deps.cfg.RolesPath = filepath.Join(t.TempDir(), "roles")
	root, err := openRolesRoot(deps.cfg.RolesPath, true)
	if err != nil {
		t.Fatalf("openRolesRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	deps.rolesRoot = root
	return deps, printer
}

// refetchRole is the role "web" pinned to refetchCommit on refetchRef.
func refetchRole() resolvedRole {
	return resolvedRole{
		Name: "web", Version: refetchRef, Ref: refetchRef, Kind: typeGit,
		Source: gitsource.Locator{URL: refetchRepoURL, Commit: refetchCommit}.String(),
	}
}

type gitRoleFetchRefusalCase struct {
	acquireErr   error
	wantErr      error
	name         string
	source       string
	ref          string
	servedCommit string
	wantAcquires int
	wantReleased int
	noClient     bool
}

func gitRoleFetchRefusalCases() []gitRoleFetchRefusalCase {
	return []gitRoleFetchRefusalCase{
		{name: "no git client wired", noClient: true, wantErr: helpers.ErrConfigIsNil},
		{name: "source is not a git locator", source: "https://hub.example/", wantErr: helpers.ErrInvalidGitLocator},
		{
			name:    "locator without a commit",
			source:  gitsource.Locator{URL: refetchRepoURL}.String(),
			wantErr: helpers.ErrInvalidGitLocator,
		},
		{name: "abbreviated commit as ref", ref: "abcdef1", wantErr: helpers.ErrGitAbbreviatedCommit},
		{name: "remote lost the commit", acquireErr: helpers.ErrGitCommitNotFound, wantErr: helpers.ErrGitCommitNotFound, wantAcquires: 1},
		{
			name:         "remote serves another commit",
			servedCommit: refetchOther,
			wantErr:      helpers.ErrRoleArtifactIdentityMismatch,
			wantAcquires: 1,
			wantReleased: 1,
		},
	}
}

// runGitRoleFetchRefusal runs one refusal case against a scripted remote
// serving tt.servedCommit, its build counting its own release.
func runGitRoleFetchRefusal(t *testing.T, tt gitRoleFetchRefusalCase) refetchOutcome {
	t.Helper()
	out := refetchOutcome{}
	client := &scriptedGitClient{
		acquireErr: tt.acquireErr,
		roleResult: gitsource.RoleResult{Commit: tt.servedCommit, Cleanup: func() { out.released++ }},
	}
	deps, _ := newRefetchDeps(t, client)
	if tt.noClient {
		deps.runtime.Git = nil
	}
	r := refetchRole()
	if tt.source != "" {
		r.Source = tt.source
	}
	if tt.ref != "" {
		r.Ref = tt.ref
	}
	got, err := roleFetchToCache(context.Background(), deps, r, true)
	out.got, out.err, out.acquires = got, err, client.acquires
	return out
}

// TestGitRoleFetchToCacheRefusals pins the role refetch's refusals: wiring,
// locator and ref before any fetch, and a remote answering with another
// commit than the pin, whose build is released.
func TestGitRoleFetchToCacheRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range gitRoleFetchRefusalCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertRefetchRefused(t, runGitRoleFetchRefusal(t, tt), tt.wantErr, tt.wantAcquires, tt.wantReleased)
		})
	}
}

// TestGitRoleFetchToCacheWithoutCache pins the --no-cache arm: the pinned
// commit is asked for, and the build is handed back with its warnings and
// bytes reported.
func TestGitRoleFetchToCacheWithoutCache(t *testing.T) {
	t.Parallel()
	client := &scriptedGitClient{}
	deps, printer := newRefetchDeps(t, client)
	path, sha := writeBuiltArtifact(t, t.TempDir(), "web.tar.gz")
	client.roleResult = gitsource.RoleResult{
		Commit: refetchCommit, ArtifactPath: path, ArtifactSHA: sha, BytesFetched: 512, Warnings: []string{"large file"},
	}

	got, err := roleFetchToCache(context.Background(), deps, refetchRole(), false)
	if err != nil || got.Path != path || got.SHA != sha {
		t.Fatalf("roleFetchToCache() = (%q, %q, %v), want the build (%q, %q)", got.Path, got.SHA, err, path, sha)
	}
	if req := client.lastRole; req == nil || req.Commit != refetchCommit || req.Ref.Name != refetchRef {
		t.Fatalf("AcquireRole request = %+v, want commit %s on %s", req, refetchCommit, refetchRef)
	}
	assertRefetchReported(t, printer, deps.runtime.Metrics.Totals(), "large file", 512, 0)
}

// TestGitRoleFetchToCacheReleasesAFailedCommit pins that a commit the store
// refuses releases the role build and reports the store's error.
func TestGitRoleFetchToCacheReleasesAFailedCommit(t *testing.T) {
	t.Parallel()
	released := 0
	client := &scriptedGitClient{}
	deps, _ := newRefetchDeps(t, client)
	deps.artifacts = local.NewArtifacts(filepath.Join(t.TempDir(), "absent"))
	path, sha := writeBuiltArtifact(t, t.TempDir(), "web.tar.gz")
	client.roleResult = gitsource.RoleResult{
		Commit: refetchCommit, ArtifactPath: path, ArtifactSHA: sha, Cleanup: func() { released++ },
	}

	if _, err := roleFetchToCache(context.Background(), deps, refetchRole(), true); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("roleFetchToCache() error = %v, want the store's fs.ErrNotExist", err)
	}
	if released != 1 {
		t.Fatalf("released %d times, want the build released once", released)
	}
}

type roleDirRefusalCase struct {
	wantErr error
	setup   func(t *testing.T, rolesPath string)
	name    string
	role    string
	remedy  string
}

// roleDirRefusalCases are the directory checks installRole makes before any
// fetch; remedy is text the error must carry, when set.
func roleDirRefusalCases() []roleDirRefusalCase {
	return []roleDirRefusalCase{
		{name: "unsafe install name", role: "../web", wantErr: helpers.ErrUnsafeCollectionIdentifier},
		{
			name: "regular file in place of the role",
			role: "web",
			setup: func(t *testing.T, rolesPath string) {
				t.Helper()
				mustWriteFile(t, filepath.Join(rolesPath, "web"), []byte("not a role"))
			},
			wantErr: syscall.ENOTDIR,
		},
		{
			name: "role directory symlinked outside roles_path",
			role: "web",
			setup: func(t *testing.T, rolesPath string) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), filepath.Join(rolesPath, "web")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			wantErr: helpers.ErrCollectionsPathEscape,
			remedy:  "--roles-path",
		},
	}
}

// TestInstallRoleRefusesBeforeFetching pins the directory checks that run
// before any fetch: an unsafe install name, a regular file where the role
// directory belongs, and a role directory symlinked out of roles_path.
func TestInstallRoleRefusesBeforeFetching(t *testing.T) {
	t.Parallel()
	for _, tt := range roleDirRefusalCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &scriptedGitClient{}
			deps, _ := newRoleInstallDeps(t, client)
			if tt.setup != nil {
				tt.setup(t, deps.cfg.RolesPath)
			}
			r := refetchRole()
			r.Name = tt.role
			err := installRole(context.Background(), deps, r)
			if !errors.Is(err, tt.wantErr) || !strings.Contains(err.Error(), tt.remedy) {
				t.Fatalf("installRole() error = %v, want %v carrying %q", err, tt.wantErr, tt.remedy)
			}
			if client.acquires != 0 {
				t.Fatalf("AcquireRole calls = %d, want 0", client.acquires)
			}
		})
	}
}

type badCacheSlotCase struct {
	wantErr error
	seed    func(t *testing.T, deps *installDeps, slot string)
	name    string
	text    string
}

// badCacheSlotCases are the cache-hit arm's failures: a slot that cannot be
// hashed, bytes that are not an archive, and a read past the deadline.
func badCacheSlotCases() []badCacheSlotCase {
	return []badCacheSlotCase{
		{
			name: "slot is a directory",
			seed: func(t *testing.T, _ *installDeps, slot string) {
				t.Helper()
				if err := os.MkdirAll(slot, helpers.DirMod); err != nil {
					t.Fatalf("mkdir slot: %v", err)
				}
			},
			wantErr: syscall.EISDIR,
		},
		{
			name: "bytes are not an archive",
			seed: func(t *testing.T, _ *installDeps, slot string) {
				t.Helper()
				mustWriteFile(t, slot, []byte("not a tar.gz"))
			},
			text: "failed to extract role web",
		},
		{
			name: "read outlives the deadline",
			seed: func(t *testing.T, deps *installDeps, slot string) {
				t.Helper()
				deps.artifacts = &blockingFetchArtifacts{Artifacts: local.NewArtifacts(deps.cfg.CacheDir), blocking: true}
				deps.runtime.ArtifactDownloadDeadline = 20 * time.Millisecond
				mustWriteFile(t, slot, []byte("never read"))
			},
			wantErr: helpers.ErrArtifactDownloadDeadline,
		},
	}
}

// TestInstallRoleFromABadCacheSlot pins the cache-hit arm's failures: none
// of them reaches the remote or records the role.
func TestInstallRoleFromABadCacheSlot(t *testing.T) {
	t.Parallel()
	for _, tt := range badCacheSlotCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &scriptedGitClient{}
			deps, _ := newRoleInstallDeps(t, client)
			r := refetchRole()
			tt.seed(t, &deps, filepath.Join(deps.cfg.CacheDir, roleArtifactKey(r)))

			err := installRole(context.Background(), deps, r)
			if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) || !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("installRole() error = %v, want %v carrying %q", err, tt.wantErr, tt.text)
			}
			if entry, ok := deps.st.GetInstalledRole(r.Name); ok || client.acquires != 0 {
				t.Fatalf("role recorded as (%+v, %v) after %d fetches, want no record and no fetch", entry, ok, client.acquires)
			}
		})
	}
}

// TestInstallRolesStopsOnCanceledContext pins that a canceled run dispatches
// no role install: nothing is fetched, written or counted as a failure.
func TestInstallRolesStopsOnCanceledContext(t *testing.T) {
	t.Parallel()
	client := &scriptedGitClient{}
	deps, printer := newRoleInstallDeps(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := refetchRole()
	failures := &failureRecorder{}

	installRoles(ctx, deps, roleResolution{roles: map[string]resolvedRole{r.Name: r}, order: []string{r.Name}}, failures)

	if n := failures.count(); n != 0 {
		t.Fatalf("failures = %d, want 0", n)
	}
	if client.acquires != 0 || len(printer.okLines()) != 0 {
		t.Fatalf("canceled run fetched %d times and printed %v, want neither", client.acquires, printer.okLines())
	}
	if _, err := os.Stat(filepath.Join(deps.cfg.RolesPath, r.Name)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("role directory stat error = %v, want nothing written", err)
	}
}

// TestRoleRecordMatchesNeedsEveryPiece pins the Stat-only skip check: no
// store, a non-digest sha or a missing install info each fail it; the
// complete set passes.
func TestRoleRecordMatchesNeedsEveryPiece(t *testing.T) {
	t.Parallel()
	deps, _ := newRoleInstallDeps(t, &scriptedGitClient{})
	r := refetchRole()
	target, ok := newRoleTarget(deps.rolesRoot, deps.cfg, r)
	if !ok {
		t.Fatal("newRoleTarget() refused a safe role name")
	}
	sha := strings.Repeat("a", 64)
	entry := store.InstalledRoleEntry{InstallPath: target.path, Source: r.Source, Version: r.Version, ArtifactSHA256: sha}
	marker, _ := markerRel(target, sha)
	markerPath := filepath.Join(deps.cfg.RolesPath, filepath.FromSlash(marker))
	if err := os.MkdirAll(filepath.Dir(markerPath), helpers.DirMod); err != nil {
		t.Fatalf("mkdir role: %v", err)
	}
	mustWriteFile(t, markerPath, nil)

	if _, ok := roleRecordMatches(target, r, nil); ok {
		t.Fatal("roleRecordMatches(nil store) = true, want false")
	}

	st := store.New()
	torn := entry
	torn.ArtifactSHA256 = "not-a-digest"
	st.SetInstalledRole(r.Name, torn)
	if _, ok := roleRecordMatches(target, r, st); ok {
		t.Fatal("roleRecordMatches(non-digest sha) = true, want false")
	}

	st.SetInstalledRole(r.Name, entry)
	if _, ok := roleRecordMatches(target, r, st); ok {
		t.Fatal("roleRecordMatches(no install info) = true, want false")
	}

	if err := writeGalaxyInstallInfo(target, r.Version, time.Now()); err != nil {
		t.Fatalf("writeGalaxyInstallInfo: %v", err)
	}
	if got, ok := roleRecordMatches(target, r, st); !ok || got.ArtifactSHA256 != sha {
		t.Fatalf("roleRecordMatches(complete) = (%+v, %v), want the record", got, ok)
	}
}

// installInfoNow is a non-UTC instant, so a record that is not converted to
// UTC shows in the rendered install_date.
func installInfoNow() time.Time {
	return time.Date(2026, time.March, 5, 7, 8, 9, 0, time.FixedZone("UTC+3", 3*60*60))
}

// TestWriteGalaxyInstallInfoReplacesTheRecord pins the record ansible-galaxy
// reads back: the C-locale date in UTC and the version, replacing any
// earlier file rather than writing through it.
func TestWriteGalaxyInstallInfoReplacesTheRecord(t *testing.T) {
	t.Parallel()
	deps, _ := newRoleInstallDeps(t, &scriptedGitClient{})
	target, _ := newRoleTarget(deps.rolesRoot, deps.cfg, refetchRole())
	info := filepath.Join(deps.cfg.RolesPath, "web", "meta", ".galaxy_install_info")
	if err := os.MkdirAll(filepath.Dir(info), helpers.DirMod); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	mustWriteFile(t, info, []byte("version: stale\n"))
	if err := writeGalaxyInstallInfo(target, "1.10", installInfoNow()); err != nil {
		t.Fatalf("writeGalaxyInstallInfo() error = %v", err)
	}
	assertFileContent(t, info, "install_date: Thu Mar  5 04:08:09 2026\nversion: \"1.10\"\n")
}

type installInfoRefusalCase struct {
	wantErr error
	setup   func(t *testing.T, roleDir string)
	name    string
	remedy  string
}

// installInfoRefusalCases are the trees writeGalaxyInstallInfo must refuse:
// meta as a file, the record as a non-empty directory, and meta symlinked
// out of roles_path, which names the roles path remedy.
func installInfoRefusalCases() []installInfoRefusalCase {
	return []installInfoRefusalCase{
		{
			name: "meta is a regular file",
			setup: func(t *testing.T, roleDir string) {
				t.Helper()
				mustWriteFile(t, filepath.Join(roleDir, "meta"), []byte("file"))
			},
			wantErr: fs.ErrExist,
		},
		{
			name: "install info is a non-empty directory",
			setup: func(t *testing.T, roleDir string) {
				t.Helper()
				info := filepath.Join(roleDir, "meta", ".galaxy_install_info")
				if err := os.MkdirAll(info, helpers.DirMod); err != nil {
					t.Fatalf("mkdir install info: %v", err)
				}
				mustWriteFile(t, filepath.Join(info, "keep"), nil)
			},
			wantErr: syscall.ENOTEMPTY,
		},
		{
			name: "meta symlinked outside roles_path",
			setup: func(t *testing.T, roleDir string) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), filepath.Join(roleDir, "meta")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			wantErr: helpers.ErrCollectionsPathEscape,
			remedy:  "--roles-path",
		},
	}
}

// TestWriteGalaxyInstallInfoRefusals pins each refused tree's error, and
// that nothing but a symlink escape is reported as one.
func TestWriteGalaxyInstallInfoRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range installInfoRefusalCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deps, _ := newRoleInstallDeps(t, &scriptedGitClient{})
			target, _ := newRoleTarget(deps.rolesRoot, deps.cfg, refetchRole())
			roleDir := filepath.Join(deps.cfg.RolesPath, "web")
			if err := os.MkdirAll(roleDir, helpers.DirMod); err != nil {
				t.Fatalf("mkdir role: %v", err)
			}
			tt.setup(t, roleDir)
			err := writeGalaxyInstallInfo(target, refetchRef, installInfoNow())
			if !errors.Is(err, tt.wantErr) || !strings.Contains(err.Error(), tt.remedy) {
				t.Fatalf("writeGalaxyInstallInfo() error = %v, want %v carrying %q", err, tt.wantErr, tt.remedy)
			}
			if tt.remedy == "" && errors.Is(err, helpers.ErrCollectionsPathEscape) {
				t.Fatalf("writeGalaxyInstallInfo() error = %v, must not read as a symlink escape", err)
			}
		})
	}
}

// TestWriteGalaxyInstallInfoIntoReadOnlyMeta pins that a meta directory the
// run cannot write is reported as a permission failure.
func TestWriteGalaxyInstallInfoIntoReadOnlyMeta(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	deps, _ := newRoleInstallDeps(t, &scriptedGitClient{})
	target, _ := newRoleTarget(deps.rolesRoot, deps.cfg, refetchRole())
	meta := filepath.Join(deps.cfg.RolesPath, "web", "meta")
	if err := os.MkdirAll(meta, helpers.DirMod); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	//nolint:gosec // G302: 0o555 is this test's own fixture permission, restored in t.Cleanup below.
	if err := os.Chmod(meta, 0o555); err != nil {
		t.Fatalf("chmod meta: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(meta, helpers.DirMod) })
	if err := writeGalaxyInstallInfo(target, refetchRef, installInfoNow()); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("writeGalaxyInstallInfo() error = %v, want fs.ErrPermission", err)
	}
}

// TestRecordRoleInstall pins that a run with no snapshot (a nil store)
// records nothing, and that a record keeps the role's fields in UTC.
func TestRecordRoleInstall(t *testing.T) {
	t.Parallel()
	recordRoleInstall(nil, refetchRole(), "/roles/web", strings.Repeat("a", 64), time.Now())

	st := store.New()
	r := refetchRole()
	r.Deps = []string{"base"}
	installedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.FixedZone("UTC+1", 60*60))
	recordRoleInstall(st, r, "/roles/web", strings.Repeat("a", 64), installedAt)
	entry, ok := st.GetInstalledRole("web")
	if !ok || entry.Source != r.Source || entry.Version != refetchRef || !entry.InstalledAt.Equal(installedAt) ||
		entry.InstalledAt.Location() != time.UTC || len(entry.Deps) != 1 {
		t.Fatalf("recorded role = (%+v, %v), want the role's source, version, deps and a UTC time", entry, ok)
	}
}
