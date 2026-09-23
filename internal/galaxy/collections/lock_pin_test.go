package collections

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// noopPrinter is a minimal output.Printer stub for tests that need an Infra
// but do not care about the rendered progress output.
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

// renderVersionLine renders an OkVersionf or ErrorVersionf call the way a
// recording printer double stores it: message, version tag and cause without
// color, so an assertion on a line sees all of it.
func renderVersionLine(version, cause, format string, args ...any) string {
	line := fmt.Sprintf(format, args...)
	if version != "" {
		line += " == " + version
	}
	if cause != "" {
		line += " " + cause
	}
	return line
}

// sidecarFor renders the GALAXY.yml an install of col leaves beside it. It
// must name col: matchingInstalledRecord parses the document and does not
// count one that describes another collection as evidence of an install.
func sidecarFor(col collection) []byte {
	return []byte(fmt.Sprintf("format_version: 1.0.0\nnamespace: %s\nname: %s\nversion: %s\n",
		col.Namespace, col.Name, col.Version))
}

func TestVerifyPinnedSHA(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "ns", Name: "name", Version: "1.0.0"}

	tests := []struct {
		name    string
		pin     string
		actual  string
		wantErr bool
	}{
		{name: "empty pin is a no-op", pin: "", actual: "deadbeef", wantErr: false},
		{name: "matching pin passes", pin: "abc123", actual: "abc123", wantErr: false},
		{name: "mismatched pin fails", pin: "abc123", actual: "def456", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := col
			c.SHA256 = tt.pin
			err := verifyPinnedSHA(c, tt.actual)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !errors.Is(err, helpers.ErrSHA256Mismatch) {
				t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
			}
			if !strings.Contains(err.Error(), c.key()) {
				t.Fatalf("expected error to contain key %q, got %v", c.key(), err)
			}
		})
	}
}

// newTestInstallDeps builds installDeps under cfg's temp paths with a real
// collections root, since installCollection fails closed with
// helpers.ErrUnsafeCollectionIdentifier on a nil one.
func newTestInstallDeps(t *testing.T, cfg *config.Config) installDeps {
	t.Helper()
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()
	artifacts := local.NewArtifacts(cfg.CacheDir)
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		root:           root,
	}
}

func TestInstallCollectionCacheHitPinMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0", SHA256: "not-the-real-hash"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	content := []byte("arbitrary tarball bytes for cache-hit test")
	if err := os.WriteFile(artifactPath, content, helpers.FileMod); err != nil {
		t.Fatalf("seed cached artifact: %v", err)
	}
	if realSHA := sha256Hex(content); realSHA == col.SHA256 {
		t.Fatalf("test setup bug: pin accidentally matches real hash")
	}

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      true,
	}
	deps := newTestInstallDeps(t, cfg)

	err := installCollection(context.Background(), col, deps, nil, nil, downloadResult{})
	if err == nil {
		t.Fatalf("expected pin mismatch error, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	if _, statErr := os.Stat(installPath); statErr == nil {
		t.Fatalf("install path %s was created despite the pin mismatch", installPath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on install path: %v", statErr)
	}

	// Offline mode must never evict: with no way to refetch, deleting the
	// only local copy would be pure data loss, so the cached artifact must
	// still be there after the failed, offline install.
	if _, statErr := os.Stat(artifactPath); statErr != nil {
		t.Fatalf("expected the cached artifact to survive an offline pin mismatch, stat error: %v", statErr)
	}
}

// TestInstallCollectionCacheHitPinIgnoresSidecarAndHashesRealBytes pins that
// a pinned cache hit hashes the bytes on disk and fails closed, even when the
// artifact's sha256 sidecar agrees with the pin.
func TestInstallCollectionCacheHitPinIgnoresSidecarAndHashesRealBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	pin := strings.Repeat("1", 64)
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0", SHA256: pin}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	content := []byte("real on-disk tarball bytes that differ from both the pin and the sidecar")
	if err := os.WriteFile(artifactPath, content, helpers.FileMod); err != nil {
		t.Fatalf("seed cached artifact: %v", err)
	}
	if realSHA := sha256Hex(content); realSHA == pin {
		t.Fatalf("test setup bug: pin accidentally matches real hash")
	}
	// A sidecar that falsely agrees with the pin: trusting it would let the
	// mismatch below through.
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(pin), helpers.FileMod); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      true,
	}
	deps := newTestInstallDeps(t, cfg)

	err := installCollection(context.Background(), col, deps, nil, nil, downloadResult{})
	if err == nil {
		t.Fatalf("expected pin mismatch error, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	if _, statErr := os.Stat(installPath); statErr == nil {
		t.Fatalf("install path %s was created despite the pin mismatch", installPath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on install path: %v", statErr)
	}
}

// TestInstallCollectionCacheHitPinIntactBytesSucceeds pins that a pinned
// cache hit whose bytes match the pin installs even when its sidecar
// disagrees, since the frozen path never consults the sidecar.
func TestInstallCollectionCacheHitPinIntactBytesSucceeds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	content := buildMinimalTarGz(t)
	realSHA := sha256Hex(content)
	col := collection{Namespace: "acme", Name: "gizmos", Version: "1.0.0", SHA256: realSHA}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	if err := os.WriteFile(artifactPath, content, helpers.FileMod); err != nil {
		t.Fatalf("seed cached artifact: %v", err)
	}
	// The sidecar disagrees with the real hash; the pinned path must ignore
	// it entirely and still succeed because the actual bytes match the pin.
	wrongSidecar := strings.Repeat("f", 64)
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	if err := os.WriteFile(sidecarPath, []byte(wrongSidecar), helpers.FileMod); err != nil {
		t.Fatalf("seed mismatched sidecar: %v", err)
	}

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      true,
	}
	deps := newTestInstallDeps(t, cfg)

	if err := installCollection(context.Background(), col, deps, nil, nil, downloadResult{}); err != nil {
		t.Fatalf("expected the pinned install to succeed, got %v", err)
	}

	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	if _, statErr := os.Stat(installPath); statErr != nil {
		t.Fatalf("expected install path %s to exist, stat error: %v", installPath, statErr)
	}
}

// buildTarGzWithEntry builds a valid gzip+tar stream holding one regular
// file named name with body, so fixtures can differ only in the entry name.
func buildTarGzWithEntry(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(body)), Mode: 0o644}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// buildMinimalTarGz builds a minimal but valid gzip+tar stream containing a
// single small regular file, suitable for archive.ExtractTarGz.
func buildMinimalTarGz(t *testing.T) []byte {
	t.Helper()
	return buildTarGzWithEntry(t, "README.md", []byte("# widgets\n"))
}

// buildEscapingTarGz builds a well-formed tar.gz whose entry escapes the
// destination: it passes the cache's shape probe, then fails extraction with
// helpers.ErrArchiveEntryEscapesDestination, so a test reaches that recovery arm.
func buildEscapingTarGz(t *testing.T) []byte {
	t.Helper()
	return buildTarGzWithEntry(t, "../escape.txt", []byte("outside\n"))
}

func TestInstallCollectionFreshDownloadPinMismatch(t *testing.T) {
	t.Parallel()
	// A real tar.gz: this download arm probes an artifact's shape before
	// committing it, so shapeless bytes would fail before the pin check.
	content := buildMinimalTarGz(t)
	correctSHA := sha256Hex(content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "gadgets", Version: "2.0.0", SHA256: "wrong-pin-value"}

	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = correctSHA

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	deps := newTestInstallDeps(t, cfg)

	err := installCollection(context.Background(), col, deps, nil, meta, downloadResult{})
	if err == nil {
		t.Fatalf("expected pin mismatch error, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	if _, statErr := os.Stat(installPath); statErr == nil {
		t.Fatalf("install path %s was created despite the pin mismatch", installPath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on install path: %v", statErr)
	}
}

func TestCanSkipInstallPinGate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	const installedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	cfg := &config.Config{DownloadPath: downloadPath}
	target := newTestInstallTarget(t, cfg, col)
	if err := os.MkdirAll(target.path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir installPath: %v", err)
	}
	seedValidExtractMarker(t, target, installedSHA)
	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir infoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), sidecarFor(col), helpers.FileMod); err != nil {
		t.Fatalf("write GALAXY.yml: %v", err)
	}

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})

	matching := col
	matching.SHA256 = installedSHA
	if _, ok := canSkipInstall(target, matching, st, noopPrinter{}); !ok {
		t.Fatalf("expected canSkipInstall to return true when the pin matches the installed SHA")
	}

	mismatched := col
	mismatched.SHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, ok := canSkipInstall(target, mismatched, st, noopPrinter{}); ok {
		t.Fatalf("expected canSkipInstall to return false when the pin does not match the installed SHA")
	}
}

// TestCanSkipInstallSourceGate pins that an install recorded from one server
// is not kept when the same name@version now resolves from another, even with
// path and sha256 matching and no lockfile pin in play.
func TestCanSkipInstallSourceGate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0", Source: "https://a.example.com"}
	const installedSHA = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	cfg := &config.Config{DownloadPath: downloadPath}
	target := newTestInstallTarget(t, cfg, col)
	if err := os.MkdirAll(target.path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir installPath: %v", err)
	}
	seedValidExtractMarker(t, target, installedSHA)
	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir infoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), sidecarFor(col), helpers.FileMod); err != nil {
		t.Fatalf("write GALAXY.yml: %v", err)
	}

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		Source:         col.Source,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})

	if _, ok := canSkipInstall(target, col, st, noopPrinter{}); !ok {
		t.Fatalf("expected canSkipInstall to return true when the source is unchanged")
	}

	switched := col
	switched.Source = "https://b.example.com"
	if _, ok := canSkipInstall(target, switched, st, noopPrinter{}); ok {
		t.Fatalf("expected canSkipInstall to return false when the collection now resolves from a different server")
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestCanSkipInstallReadsTheSidecar pins that the skip check judges GALAXY.yml
// by what it says, not by its existence; every row shares the same record,
// marker and path, and the matching row is the positive control.
func TestCanSkipInstallReadsTheSidecar(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}

	cases := []struct {
		name     string
		sidecar  []byte
		wantSkip bool
	}{
		{name: "describes this collection", sidecar: sidecarFor(col), wantSkip: true},
		{name: "empty", sidecar: nil, wantSkip: false},
		{name: "no identity at all", sidecar: []byte("format_version: 1.0.0\n"), wantSkip: false},
		{
			name:    "another version",
			sidecar: sidecarFor(collection{Namespace: "acme", Name: "widgets", Version: "2.0.0"}),
		},
		{
			name:    "another collection",
			sidecar: sidecarFor(collection{Namespace: "evil", Name: "pkg", Version: testVersion100}),
		},
		{name: "not a document", sidecar: []byte("namespace: [broken\n"), wantSkip: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target, st := seedInstalledCollection(t, col, tc.sidecar)
			if _, ok := canSkipInstall(target, col, st, noopPrinter{}); ok != tc.wantSkip {
				t.Fatalf("canSkipInstall = %v, want %v", ok, tc.wantSkip)
			}
		})
	}
}

// TestCanSkipInstallRepairsADriftedServer pins that the skip path rewrites a
// sidecar whose server fell behind the record and keeps download_url, which
// no metadata fetched on this path could rebuild.
func TestCanSkipInstallRepairsADriftedServer(t *testing.T) {
	t.Parallel()
	const stale = "https://hub.example/galaxy/ansible"
	const current = "https://hub.example/galaxy/internal"
	const downloadURL = "https://objects.example/acme-widgets-1.0.0.tar.gz"

	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100, Source: current}
	drifted := GalaxyYAML{
		FormatVer: "1.0.0", Namespace: col.Namespace, Name: col.Name, Version: col.Version,
		Server: stale, DownloadURL: downloadURL,
	}
	seeded, err := yaml.Marshal(&drifted)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	target, st := seedInstalledCollection(t, col, seeded)

	cfg := &config.Config{Server: stale}
	state, ok := canSkipInstall(target, col, st, noopPrinter{})
	if !ok {
		t.Fatalf("canSkipInstall = false; a drifted server must not force a reinstall")
	}
	reconcileGalaxyInfo(infra.New(noopPrinter{}, nil), target, cfg, col, state)

	repaired, _, ok := readGalaxyInfo(target)
	if !ok {
		t.Fatalf("sidecar unreadable after the repair")
	}
	if repaired.Server != current {
		t.Errorf("server = %q, want the server the record names, %q", repaired.Server, current)
	}
	if repaired.DownloadURL != downloadURL {
		t.Errorf("download_url = %q, want it preserved: %q", repaired.DownloadURL, downloadURL)
	}
}

// TestReconcileGalaxyInfoLeavesAnAgreeingSidecarAlone pins that a sidecar that
// already agrees is not rewritten: a skip that touches the tree on every run
// is no longer a skip.
func TestReconcileGalaxyInfoLeavesAnAgreeingSidecarAlone(t *testing.T) {
	t.Parallel()
	const server = "https://hub.example/galaxy/ansible"
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100, Source: server}
	doc := GalaxyYAML{
		FormatVer: "1.0.0", Namespace: col.Namespace, Name: col.Name, Version: col.Version, Server: server,
	}
	seeded, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	target, _ := seedInstalledCollection(t, col, seeded)
	rel := filepath.Join(target.path, "..", "..",
		col.Namespace+"."+col.Name+"-"+col.Version+".info", galaxyYAMLFileName)
	before := mustModTime(t, rel)

	reconcileGalaxyInfo(infra.New(noopPrinter{}, nil), target, &config.Config{Server: server}, col,
		installedState{info: doc, infoData: seeded})

	if after := mustModTime(t, rel); !after.Equal(before) {
		t.Errorf("sidecar was rewritten though it already agreed: %s -> %s", before, after)
	}
}

func mustModTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime()
}

// seedInstalledCollection seeds a completed install of col - store record,
// install path, extract marker and the sidecar bytes verbatim, empty included -
// and returns the target and store the skip check reads.
func seedInstalledCollection(t *testing.T, col collection, sidecar []byte) (installTarget, *store.Store) {
	t.Helper()
	const artifactSHA = "f16682b62c181adfc22929576413890a9e5b338d975c949948262e41345a5c04"
	downloadPath := t.TempDir()
	target := newTestInstallTarget(t, &config.Config{DownloadPath: downloadPath}, col)
	if err := os.MkdirAll(target.path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir install path: %v", err)
	}
	seedValidExtractMarker(t, target, artifactSHA)

	infoDir := filepath.Join(downloadPath, "ansible_collections",
		col.Namespace+"."+col.Name+"-"+col.Version+".info")
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir info dir: %v", err)
	}
	mustWriteFile(t, filepath.Join(infoDir, galaxyYAMLFileName), sidecar)

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath: target.path, Source: col.Source, ArtifactSHA256: artifactSHA,
	})
	return target, st
}

// BenchmarkMatchingInstalledRecord measures the skip check's per-collection
// cost on an unchanged tree, where the sidecar is read and parsed rather than
// merely stat-ed.
func BenchmarkMatchingInstalledRecord(b *testing.B) {
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target, st := seedInstalledCollection(&testing.T{}, col, sidecarFor(col))

	b.ReportAllocs()
	for b.Loop() {
		if _, ok := matchingInstalledRecord(target, col, st); !ok {
			b.Fatal("fixture does not match")
		}
	}
}
