package extracted

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// unknownSHA is a well-formed sha256 hex digest no fixture tarball hashes to.
const unknownSHA = "0000000000000000000000000000000000000000000000000000000000000000"

// errPlannedWithoutStore marks a SweepPlan that listed removals for a cache
// directory that does not exist.
var errPlannedWithoutStore = errors.New("SweepPlan planned removals without a store")

// skipAsRoot skips a test whose refusal comes from permission bits, which
// root bypasses.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not block the operation")
	}
}

// lockDir drops dir's write bit for the rest of the test, so nothing can be
// created in or removed from it, and restores it before cleanup runs.
func lockDir(t *testing.T, dir string) {
	t.Helper()
	//nolint:gosec // read-only on purpose: 0500 keeps the directory listable but unwritable.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	t.Cleanup(func() {
		//nolint:gosec // restoring a test directory's own default mode before cleanup.
		_ = os.Chmod(dir, 0o755)
	})
}

// seedUnremovable creates dir holding a locked subdirectory with one file,
// which RemoveAll cannot delete.
func seedUnremovable(t *testing.T, dir string) {
	t.Helper()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, helpers.DirMod); err != nil {
		t.Fatalf("mkdir %s: %v", locked, err)
	}
	if err := os.WriteFile(filepath.Join(locked, "pinned"), []byte("x"), helpers.FileMod); err != nil {
		t.Fatalf("write pinned file: %v", err)
	}
	lockDir(t, locked)
}

// assertNoStoreTemps fails when the store root holds an "ingest-" or ".tmp"
// directory, the leftovers a failed extraction must not leave behind.
func assertNoStoreTemps(t *testing.T, store *Store) {
	t.Helper()
	entries, err := os.ReadDir(store.Root())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read store root: %v", err)
	}
	for _, e := range entries {
		if isTempEntryName(e.Name()) {
			t.Errorf("a failed extraction left %s behind", e.Name())
		}
	}
}

// TestNilStoreRefusesWork pins the "caching disabled" contract for the write
// paths: a nil *Store refuses with ErrStoreNotConfigured and drains a stream.
func TestNilStoreRefusesWork(t *testing.T) {
	t.Parallel()
	var s *Store

	if _, err := s.Ensure(context.Background(), unknownSHA, "unused", SHAFromRecord); !errors.Is(err, ErrStoreNotConfigured) {
		t.Errorf("Ensure error = %v, want ErrStoreNotConfigured", err)
	}
	stream := bytes.NewReader([]byte("an upstream body that must be drained"))
	if _, err := s.IngestReader(context.Background(), stream); !errors.Is(err, ErrStoreNotConfigured) {
		t.Errorf("IngestReader error = %v, want ErrStoreNotConfigured", err)
	}
	if stream.Len() != 0 {
		t.Errorf("IngestReader left %d bytes unread on a nil store", stream.Len())
	}
	if _, err := s.Promote("unused", unknownSHA); !errors.Is(err, ErrStoreNotConfigured) {
		t.Errorf("Promote error = %v, want ErrStoreNotConfigured", err)
	}
}

// TestNilStoreIsANoOp pins the read and removal paths of a nil *Store: no
// root, nothing ready, and nothing to discard, remove or sweep.
func TestNilStoreIsANoOp(t *testing.T) {
	t.Parallel()
	var s *Store

	if got := s.Root(); got != "" {
		t.Errorf("Root() = %q, want empty", got)
	}
	if s.Ready(unknownSHA) {
		t.Error("Ready on a nil store reported true")
	}
	if err := s.Discard("unused"); err != nil {
		t.Errorf("Discard error = %v, want nil", err)
	}
	if err := s.Remove(unknownSHA); err != nil {
		t.Errorf("Remove error = %v, want nil", err)
	}
	if planned, err := s.SweepPlan(nil); planned != nil || err != nil {
		t.Errorf("SweepPlan = (%v, %v), want (nil, nil)", planned, err)
	}
	if err := s.Sweep(t.Context(), nil); err != nil {
		t.Errorf("Sweep error = %v, want nil", err)
	}
}

// TestEmptySHAIsRefused pins that Ensure and Promote refuse an empty sha with
// ErrSHAEmpty, Promote dropping its temp, and that Remove of one is a no-op.
func TestEmptySHAIsRefused(t *testing.T) {
	t.Parallel()
	store := NewStore(filepath.Join(t.TempDir(), "cache"))

	if _, err := store.Ensure(context.Background(), "", "unused", SHAFromRecord); !errors.Is(err, ErrSHAEmpty) {
		t.Errorf("Ensure error = %v, want ErrSHAEmpty", err)
	}
	tmp := filepath.Join(store.Root(), ingestPrefix+"empty-sha")
	seedTempEntry(t, tmp)
	if _, err := store.Promote(tmp, ""); !errors.Is(err, ErrSHAEmpty) {
		t.Errorf("Promote error = %v, want ErrSHAEmpty", err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Promote kept the temp of a refused empty sha, stat error = %v", err)
	}
	if err := store.Remove(""); err != nil {
		t.Errorf("Remove(\"\") error = %v, want nil", err)
	}
}

// TestEnsureFromRecordNeedsTheTarball pins that a recorded sha is hashed
// before extraction: a missing tarball is its own error, and no entry is
// kept for that sha.
func TestEnsureFromRecordNeedsTheTarball(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "cache"))

	_, err := store.Ensure(context.Background(), unknownSHA, filepath.Join(dir, "missing.tar.gz"), SHAFromRecord)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Ensure error = %v, want fs.ErrNotExist", err)
	}
	if store.Ready(unknownSHA) {
		t.Fatal("Ensure left a ready entry for a tarball it could not hash")
	}
}

// TestFailedExtractionLeavesNoTemp pins that a stream the archive layer
// refuses, whether not gzip or an escaping entry, leaves no temp tree, no
// ready entry and no file outside the store.
func TestFailedExtractionLeavesNoTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	escaping := filepath.Join(dir, "escaping.tar.gz")
	writeTarball(t, escaping, map[string]string{"../escaped.txt": "out"})
	//nolint:gosec // path under t.TempDir().
	escapingBytes, err := os.ReadFile(escaping)
	if err != nil {
		t.Fatalf("read escaping tarball: %v", err)
	}

	for _, tc := range []struct {
		wantErr error
		name    string
		body    []byte
	}{
		{name: "not gzip", body: []byte("<html>an error page, not a tarball</html>")},
		{name: "escaping entry", body: escapingBytes, wantErr: helpers.ErrArchiveEntryEscapesDestination},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache := filepath.Join(t.TempDir(), "cache")
			store := NewStore(cache)

			stream := bytes.NewReader(tc.body)
			_, err := store.IngestReader(context.Background(), stream)
			assertExtractionRefused(t, err, tc.wantErr)
			if stream.Len() != 0 {
				t.Errorf("IngestReader left %d bytes unread after a refusal", stream.Len())
			}
			assertNoStoreTemps(t, store)

			tarPath := filepath.Join(t.TempDir(), "src.tar.gz")
			if err := os.WriteFile(tarPath, tc.body, helpers.FileMod); err != nil {
				t.Fatalf("write tarball: %v", err)
			}
			_, err = store.Ensure(context.Background(), unknownSHA, tarPath, SHASelfComputed)
			assertExtractionRefused(t, err, tc.wantErr)
			assertNoStoreTemps(t, store)
			if store.Ready(unknownSHA) {
				t.Error("a refused extraction is reported ready")
			}
			if _, err := os.Stat(filepath.Join(cache, "escaped.txt")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("an escaping entry landed beside the store, stat error = %v", err)
			}
		})
	}
}

func assertExtractionRefused(t *testing.T, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatal("the extraction was accepted")
	}
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// TestReadyDirectoryInATarballIsNeverMarkedReady pins that a tarball whose
// ".ready" is a directory is refused by its sentinel on either path, and that
// the half-built temp is removed.
func TestReadyDirectoryInATarballIsNeverMarkedReady(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{
		"foo.txt":                helloFileContent,
		ReadyMarker + "/planted": ReadyMarkerPayload,
	})
	sha := mustHash(t, tarPath)

	ensureStore := NewStore(filepath.Join(dir, "ensure-cache"))
	_, err := ensureStore.Ensure(context.Background(), sha, tarPath, SHAFromRecord)
	if !errors.Is(err, helpers.ErrArchiveEntryShadowsReadyMarker) {
		t.Errorf("Ensure error = %v, want %v", err, helpers.ErrArchiveEntryShadowsReadyMarker)
	}
	if ensureStore.Ready(sha) {
		t.Error("Ensure left a ready entry")
	}
	assertNoStoreTemps(t, ensureStore)

	promoteStore := NewStore(filepath.Join(dir, "promote-cache"))
	//nolint:gosec // path under t.TempDir().
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tarball: %v", err)
	}
	defer func() { _ = f.Close() }()
	tmp, err := promoteStore.IngestReader(context.Background(), f)
	if err != nil {
		t.Fatalf("IngestReader: %v", err)
	}
	if _, err := promoteStore.Promote(tmp, sha); !errors.Is(err, helpers.ErrArchiveEntryShadowsReadyMarker) {
		t.Errorf("Promote error = %v, want %v", err, helpers.ErrArchiveEntryShadowsReadyMarker)
	}
	if promoteStore.Ready(sha) {
		t.Error("Promote left a ready entry")
	}
	assertNoStoreTemps(t, promoteStore)
}

// TestEmptyReadyDirectoryInATarballIsReplaced is the positive control beside
// the refusal: an empty ".ready" directory gives way to the marker.
func TestEmptyReadyDirectoryInATarballIsReplaced(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent, ReadyMarker + "/": ""})
	sha := mustHash(t, tarPath)
	store := NewStore(filepath.Join(dir, "cache"))
	if _, err := store.Ensure(context.Background(), sha, tarPath, SHAFromRecord); err != nil {
		t.Fatalf("Ensure error = %v, want nil", err)
	}
	if !store.Ready(sha) {
		t.Error("Ensure left no ready entry")
	}
}

// promoteRefusalCase is one TestPromoteRefusals row: the store and temp to
// promote, and the sentinel Promote must refuse with.
type promoteRefusalCase struct {
	wantErr error
	setup   func(t *testing.T, dir string) (*Store, string)
	name    string
}

func promoteRefusalCases() []promoteRefusalCase {
	return []promoteRefusalCase{
		{
			name:    "temp outside the cache directory",
			wantErr: ErrTempOutsideStore,
			setup: func(t *testing.T, dir string) (*Store, string) {
				t.Helper()
				outside := filepath.Join(dir, "outside")
				seedTempEntry(t, outside)
				t.Cleanup(func() {
					if _, err := os.Stat(filepath.Join(outside, "partial")); err != nil {
						t.Errorf("Promote touched a temp outside the store: %v", err)
					}
				})
				return NewStore(filepath.Join(dir, "cache")), outside
			},
		},
		{
			name:    "relative temp path",
			wantErr: ErrTempOutsideStore,
			setup: func(_ *testing.T, dir string) (*Store, string) {
				return NewStore(filepath.Join(dir, "cache")), filepath.Join(RootDirName, ingestPrefix+"relative")
			},
		},
		{
			name:    "temp inside the store that does not exist",
			wantErr: fs.ErrNotExist,
			setup: func(_ *testing.T, dir string) (*Store, string) {
				store := NewStore(filepath.Join(dir, "cache"))
				return store, filepath.Join(store.Root(), ingestPrefix+"vanished")
			},
		},
		{
			name:    "store directory is a regular file",
			wantErr: ErrStoreDirUnusable,
			setup: func(t *testing.T, dir string) (*Store, string) {
				t.Helper()
				cache := filepath.Join(dir, "cache")
				if err := os.MkdirAll(cache, helpers.DirMod); err != nil {
					t.Fatalf("mkdir cache: %v", err)
				}
				if err := os.WriteFile(filepath.Join(cache, RootDirName), []byte("x"), helpers.FileMod); err != nil {
					t.Fatalf("write blocking file: %v", err)
				}
				return NewStore(cache), filepath.Join(cache, RootDirName, ingestPrefix+"x")
			},
		},
	}
}

// TestPromoteRefusals pins Promote's refusals of a temp it did not create or
// cannot use, each leaving the sha unfinalized.
func TestPromoteRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range promoteRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, tmp := tc.setup(t, t.TempDir())
			if _, err := store.Promote(tmp, unknownSHA); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Promote error = %v, want %v", err, tc.wantErr)
			}
			if store.Ready(unknownSHA) {
				t.Fatal("a refused Promote finalized the sha")
			}
		})
	}
}

// TestDiscardRefusesARelativeTemp pins that Discard judges a relative path
// outside the store rather than resolving it against the working directory.
func TestDiscardRefusesARelativeTemp(t *testing.T) {
	t.Parallel()
	store := NewStore(filepath.Join(t.TempDir(), "cache"))
	if err := store.Discard(filepath.Join(RootDirName, ingestPrefix+"relative")); !errors.Is(err, ErrTempOutsideStore) {
		t.Fatalf("Discard error = %v, want ErrTempOutsideStore", err)
	}
	if err := store.Discard(""); err != nil {
		t.Fatalf("Discard(\"\") error = %v, want nil", err)
	}
}

// TestMissingOrBrokenCacheDirectory pins how the read and removal paths treat
// a cache directory that is absent (nothing to do) or a regular file (an
// error, never a silent success).
func TestMissingOrBrokenCacheDirectory(t *testing.T) {
	t.Parallel()
	ops := []struct {
		run  func(t *testing.T, s *Store) error
		name string
	}{
		{name: "Discard", run: func(_ *testing.T, s *Store) error {
			return s.Discard(filepath.Join(s.Root(), ingestPrefix+"x"))
		}},
		{name: "Remove", run: func(_ *testing.T, s *Store) error { return s.Remove(unknownSHA) }},
		{name: "SweepPlan", run: func(_ *testing.T, s *Store) error {
			planned, err := s.SweepPlan(nil)
			if planned != nil {
				return errPlannedWithoutStore
			}
			return err
		}},
		{name: "Sweep", run: func(t *testing.T, s *Store) error {
			t.Helper()
			return s.Sweep(t.Context(), nil)
		}},
		{name: "SweepTemp", run: func(_ *testing.T, s *Store) error { return s.SweepTemp() }},
	}
	for _, op := range ops {
		t.Run(op.name+"/missing", func(t *testing.T) {
			t.Parallel()
			store := NewStore(filepath.Join(t.TempDir(), "absent"))
			if err := op.run(t, store); err != nil {
				t.Fatalf("%s on a missing cache directory = %v, want nil", op.name, err)
			}
			if _, err := os.Stat(store.Root()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s created the cache directory, stat error = %v", op.name, err)
			}
		})
		t.Run(op.name+"/regular file", func(t *testing.T) {
			t.Parallel()
			cache := filepath.Join(t.TempDir(), "cache")
			if err := os.WriteFile(cache, []byte("not a directory"), helpers.FileMod); err != nil {
				t.Fatalf("write cache file: %v", err)
			}
			err := op.run(t, NewStore(cache))
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s on a cache directory that is a file = %v, want a real error", op.name, err)
			}
		})
	}
}

// TestWritePathsReportACacheDirectoryUnderAFile pins that Ensure and
// IngestReader surface the cache directory's own creation failure rather than
// misnaming it as an unusable store directory.
func TestWritePathsReportACacheDirectoryUnderAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), helpers.FileMod); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	tarPath := filepath.Join(dir, "src.tar.gz")
	writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
	store := NewStore(filepath.Join(blocker, "cache"))

	_, err := store.Ensure(context.Background(), mustHash(t, tarPath), tarPath, SHAFromRecord)
	if !errors.Is(err, syscall.ENOTDIR) || errors.Is(err, ErrStoreDirUnusable) {
		t.Errorf("Ensure error = %v, want ENOTDIR and not ErrStoreDirUnusable", err)
	}
	stream := bytes.NewReader([]byte("body"))
	if _, err := store.IngestReader(context.Background(), stream); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("IngestReader error = %v, want ENOTDIR", err)
	}
	if stream.Len() != 0 {
		t.Errorf("IngestReader left %d bytes unread", stream.Len())
	}
}

// TestSweepKeepingEverythingRemovesNothing pins that a keep set naming every
// entry is a no-op.
func TestSweepKeepingEverythingRemovesNothing(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	kept := seedFinalizedEntry(t, store, "kept")
	if err := store.Sweep(t.Context(), map[string]bool{"kept": true}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("Sweep removed a kept entry: %v", err)
	}
}

// TestSweepGoesOnPastAFailedRemoval pins that Sweep still removes the later
// entries after one it cannot delete, and returns that first failure.
func TestSweepGoesOnPastAFailedRemoval(t *testing.T) {
	t.Parallel()
	skipAsRoot(t)
	store := NewStore(t.TempDir())
	stuck := seedFinalizedEntry(t, store, "a-stuck")
	seedUnremovable(t, stuck)
	removable := seedFinalizedEntry(t, store, "b-removable")

	if err := store.Sweep(t.Context(), nil); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Sweep error = %v, want fs.ErrPermission", err)
	}
	if _, err := os.Stat(removable); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Sweep stopped at the failed entry, the later one survives: %v", err)
	}
}

// TestSweepTempReportsAFailedRemoval pins that SweepTemp returns the error of
// a temp it cannot delete instead of reporting a clean sweep.
func TestSweepTempReportsAFailedRemoval(t *testing.T) {
	t.Parallel()
	skipAsRoot(t)
	store := NewStore(t.TempDir())
	seedUnremovable(t, filepath.Join(store.Root(), unknownSHA+tmpSuffix))

	if err := store.SweepTemp(); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("SweepTemp error = %v, want fs.ErrPermission", err)
	}
}

// TestEnsureReportsWhatBlocksTheExtraction pins Ensure's checked removals and
// creations: each failure is returned, nothing is finalized, and a temp Ensure
// itself created is gone.
func TestEnsureReportsWhatBlocksTheExtraction(t *testing.T) {
	t.Parallel()
	skipAsRoot(t)
	for _, tc := range []struct {
		block       func(t *testing.T, store *Store, sha string)
		name        string
		ownTempGone bool
	}{
		{
			name: "stale temp that cannot be removed",
			block: func(t *testing.T, store *Store, sha string) {
				t.Helper()
				seedUnremovable(t, filepath.Join(store.Root(), sha+tmpSuffix))
			},
		},
		{
			name:        "store directory without a write bit",
			ownTempGone: true,
			block: func(t *testing.T, store *Store, _ string) {
				t.Helper()
				if err := os.MkdirAll(store.Root(), helpers.DirMod); err != nil {
					t.Fatalf("mkdir store root: %v", err)
				}
				lockDir(t, store.Root())
			},
		},
		{
			name:        "legacy tree that cannot be replaced",
			ownTempGone: true,
			block: func(t *testing.T, store *Store, sha string) {
				t.Helper()
				legacy := filepath.Join(store.Root(), sha)
				seedUnremovable(t, legacy)
				if err := os.WriteFile(filepath.Join(legacy, ReadyMarker), []byte("ok"), helpers.FileMod); err != nil {
					t.Fatalf("write legacy marker: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tarPath := filepath.Join(dir, "src.tar.gz")
			writeTarball(t, tarPath, map[string]string{"foo.txt": helloFileContent})
			sha := mustHash(t, tarPath)
			store := NewStore(filepath.Join(dir, "cache"))
			tc.block(t, store, sha)

			if _, err := store.Ensure(context.Background(), sha, tarPath, SHAFromRecord); !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("Ensure error = %v, want fs.ErrPermission", err)
			}
			if store.Ready(sha) {
				t.Fatal("a blocked Ensure finalized the tree")
			}
			_, err := os.Stat(filepath.Join(store.Root(), sha+tmpSuffix))
			if tc.ownTempGone && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Ensure left its own temp behind, stat error = %v", err)
			}
		})
	}
}

// TestIngestReaderReportsAnUnwritableStore pins that a store directory that
// refuses a new temp is an error, with the stream still drained.
func TestIngestReaderReportsAnUnwritableStore(t *testing.T) {
	t.Parallel()
	skipAsRoot(t)
	store := NewStore(t.TempDir())
	if err := os.MkdirAll(store.Root(), helpers.DirMod); err != nil {
		t.Fatalf("mkdir store root: %v", err)
	}
	lockDir(t, store.Root())

	stream := bytes.NewReader([]byte("body"))
	if _, err := store.IngestReader(context.Background(), stream); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("IngestReader error = %v, want fs.ErrPermission", err)
	}
	if stream.Len() != 0 {
		t.Fatalf("IngestReader left %d bytes unread", stream.Len())
	}
}

// TestReadyRejectsAMarkerItCannotTrust pins that a marker longer than the
// read cap, even one starting with the payload, and a directory named like
// the marker both read as not ready.
func TestReadyRejectsAMarkerItCannotTrust(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())

	oversized := seedFinalizedEntry(t, store, "oversized")
	long := ReadyMarkerPayload + strings.Repeat(" ", readyMarkerMaxReadSize)
	if err := os.WriteFile(filepath.Join(oversized, ReadyMarker), []byte(long), helpers.FileMod); err != nil {
		t.Fatalf("write oversized marker: %v", err)
	}
	if store.Ready("oversized") {
		t.Error("a marker past the read cap is reported ready")
	}

	dirMarker := filepath.Join(store.Root(), "dir-marker", ReadyMarker)
	if err := os.MkdirAll(dirMarker, helpers.DirMod); err != nil {
		t.Fatalf("mkdir marker directory: %v", err)
	}
	if store.Ready("dir-marker") {
		t.Error("a directory named like the marker is reported ready")
	}

	seedFinalizedEntry(t, store, "control")
	if !store.Ready("control") {
		t.Error("positive control: a current marker is not reported ready")
	}
}

// TestMaterializeMirrorsSymlinksAndSkipsSpecialFiles pins that Materialize
// recreates a symlink with its own target, replacing what stood at dst, and
// leaves out a fifo.
func TestMaterializeMirrorsSymlinksAndSkipsSpecialFiles(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "target.txt"), []byte(helloFileContent), helpers.FileMod); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(src, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "install")
	if err := os.MkdirAll(dst, helpers.DirMod); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	if err := os.Symlink("stale", filepath.Join(dst, "link")); err != nil {
		t.Fatalf("seed stale link: %v", err)
	}

	if err := Materialize(src, dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "target.txt" {
		t.Fatalf("link target = %q, want target.txt", target)
	}
	if _, err := os.Lstat(filepath.Join(dst, "pipe")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Materialize mirrored a fifo, lstat error = %v", err)
	}
}

// TestMaterializeRefusals pins that a destination it cannot create and a
// source tree that does not exist are both errors.
func TestMaterializeRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), helpers.FileMod); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if err := Materialize(dir, filepath.Join(blocker, "install")); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("Materialize under a file = %v, want ENOTDIR", err)
	}
	if err := Materialize(filepath.Join(dir, "absent"), filepath.Join(dir, "install")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Materialize of a missing source = %v, want fs.ErrNotExist", err)
	}
}

// TestCopyFallbackReportsItsFailures pins the byte-copy fallback's errors: a
// missing source, a source it cannot read as a file, and a destination
// directory refusing both the link and the copy.
func TestCopyFallbackReportsItsFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte(helloFileContent), helpers.FileMod); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := copyFile(filepath.Join(dir, "absent"), filepath.Join(dir, "a.txt"), 0o444); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("copyFile of a missing source = %v, want fs.ErrNotExist", err)
	}
	if err := copyFile(t.TempDir(), filepath.Join(dir, "b.txt"), 0o444); !errors.Is(err, syscall.EISDIR) {
		t.Errorf("copyFile of a directory = %v, want EISDIR", err)
	}

	t.Run("destination refuses link and copy", func(t *testing.T) {
		t.Parallel()
		skipAsRoot(t)
		locked := filepath.Join(t.TempDir(), "locked")
		if err := os.MkdirAll(locked, helpers.DirMod); err != nil {
			t.Fatalf("mkdir locked: %v", err)
		}
		lockDir(t, locked)
		if err := materializeFile(src, filepath.Join(locked, "dst.txt"), 0o444); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("materializeFile into a locked directory = %v, want fs.ErrPermission", err)
		}
	})
}
