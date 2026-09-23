package collections_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// The url scenario's one collection: acme.kafka@0.24.0, served as a tarball
// at a release-asset-shaped path, depending on acme.lib from Galaxy.
const (
	urlKafkaPath    = "acme/kafka/releases/download/0.24.0/acme-kafka-0.24.0.tar.gz"
	urlKafkaVersion = "0.24.0"
)

// urlFixture is the url scenario: a fakegalaxy serving the v3 API for the
// Galaxy dependency and the tarball path, with a real url download client.
type urlFixture struct {
	galaxy       *fakegalaxy.Server
	cfg          *config.Config
	runtime      *infra.Infra
	printer      *warnCapturingPrinter
	tarballURL   string
	tarballSHA   string
	reqPath      string
	cacheDir     string
	downloadPath string
}

func newURLFixture(t *testing.T) *urlFixture {
	t.Helper()
	root := t.TempDir()
	f := &urlFixture{
		galaxy:       fakegalaxy.New(t),
		printer:      &warnCapturingPrinter{},
		reqPath:      filepath.Join(root, "requirements.yml"),
		cacheDir:     filepath.Join(root, "cache"),
		downloadPath: filepath.Join(root, "install"),
	}
	f.galaxy.AddVersion("acme", "lib", testVersion100, nil)
	data, sha := fakegalaxy.BuildArtifact("acme", "kafka", urlKafkaVersion, map[string]string{"acme.lib": ">=1.0.0"})
	f.tarballURL = f.galaxy.AddTarball(urlKafkaPath, data)
	f.tarballSHA = sha

	f.cfg = &config.Config{
		Server:           f.galaxy.URL(),
		CacheDir:         f.cacheDir,
		DownloadPath:     f.downloadPath,
		RequirementsFile: f.reqPath,
		Workers:          4,
		Timeout:          e2eTimeout,
	}
	f.runtime = infra.New(f.printer, f.galaxy.Client())
	f.runtime.URLHTTP = fetch.NewURLDownload(e2eTimeout, false, nil)
	return f
}

func (f *urlFixture) writeRequirements(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(f.reqPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write requirements: %v", err)
	}
}

func (f *urlFixture) install(t *testing.T) error {
	t.Helper()
	return collections.Start(context.Background(), f.cfg, f.runtime)
}

func (f *urlFixture) mustInstall(t *testing.T) {
	t.Helper()
	if err := f.install(t); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func (f *urlFixture) locator() string {
	return urlsource.Locator{URL: f.tarballURL, SHA256: f.tarballSHA}.String()
}

func (f *urlFixture) lockfile(t *testing.T) *lockfile.File {
	t.Helper()
	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lf, err := lockfile.Load(lockfile.ResolveDefaultPath(f.reqPath, ""))
	if err != nil {
		t.Fatalf("load lockfile: %v", err)
	}
	return lf
}

// TestURLInstallFromTarball pins one url root end to end: one download, the
// artifact under its locator key, url provenance on the record and sidecar,
// the Galaxy dependency installed, and a rerun that never reaches the origin.
func TestURLInstallFromTarball(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.mustInstall(t)

	assertManifestInstalled(t, f.downloadPath, "kafka")
	assertManifestInstalled(t, f.downloadPath, "lib")
	if got := readManifestVersion(t, f.downloadPath, "kafka"); got != urlKafkaVersion {
		t.Fatalf("installed acme.kafka version = %q, want %s", got, urlKafkaVersion)
	}
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.kafka@"+urlKafkaVersion)
	if entry.Source != f.locator() {
		t.Fatalf("installed record Source = %q, want locator %q", entry.Source, f.locator())
	}
	if entry.ArtifactSHA256 != f.tarballSHA {
		t.Fatalf("installed record sha = %q, want the origin bytes' %q", entry.ArtifactSHA256, f.tarballSHA)
	}
	assertArtifactFilePresent(t, f.cacheDir, f.locator(), acmeArtifactFilename("kafka", urlKafkaVersion))
	assertInstalledProvenance(t, f.downloadPath, "kafka", urlKafkaVersion, f.tarballURL, "url_sha256: "+f.tarballSHA)
	assertTempFilesGone(t, f.cacheDir)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("tarball downloads = %d, want 1", got)
	}

	f.mustInstall(t)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("rerun reached the origin: downloads = %d, want still 1", got)
	}
}

// TestURLCachingProxyPathInstallsAndLocks pins the http://front/<upstream-url>
// shape: it installs, keeps the full proxy URL as locator, record and lockfile
// source, and replays its pin without reaching the origin again.
func TestURLCachingProxyPathInstallsAndLocks(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	data, sha := fakegalaxy.BuildArtifact("acme", "kafka", urlKafkaVersion, map[string]string{"acme.lib": ">=1.0.0"})
	f.tarballURL = f.galaxy.AddTarball("https://github.com/"+urlKafkaPath, data)
	f.tarballSHA = sha
	if !strings.Contains(f.tarballURL, "/https://github.com/") {
		t.Fatalf("proxy-shaped URL lost its embedded upstream: %q", f.tarballURL)
	}
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.mustInstall(t)

	assertManifestInstalled(t, f.downloadPath, "kafka")
	assertManifestInstalled(t, f.downloadPath, "lib")
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.kafka@"+urlKafkaVersion)
	if entry.Source != f.locator() {
		t.Fatalf("installed record Source = %q, want locator %q", entry.Source, f.locator())
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("tarball downloads = %d, want 1", got)
	}
	f.mustInstall(t)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("rerun reached the origin: downloads = %d, want still 1", got)
	}
	assertURLLockEntries(t, f)
}

// TestURLVersionAssert pins that a version: key is judged against the manifest
// on both the fresh path and the pin replay: a match installs, and a mismatch,
// even one edited in after an install, fails ErrURLCollectionVersionMismatch.
func TestURLVersionAssert(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - name: "+f.tarballURL+"\n    type: url\n    version: "+urlKafkaVersion+"\n")
	f.mustInstall(t)

	wrong := newURLFixture(t)
	wrong.writeRequirements(t, "collections:\n  - name: "+wrong.tarballURL+"\n    version: 9.9.9\n")
	if err := wrong.install(t); !errors.Is(err, helpers.ErrURLCollectionVersionMismatch) {
		t.Fatalf("fresh mismatch: %v, want ErrURLCollectionVersionMismatch", err)
	}

	// The pin is recorded from the first install; the edited assertion must
	// fail on replay too, not ride the recorded answer.
	f.writeRequirements(t, "collections:\n  - name: "+f.tarballURL+"\n    version: 9.9.9\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrURLCollectionVersionMismatch) {
		t.Fatalf("replay mismatch: %v, want ErrURLCollectionVersionMismatch", err)
	}
}

// TestURLRootOwnsTheFQDN pins the duplicate refusal: a url root and a Galaxy
// root naming the same fqdn are two sources for one collection.
func TestURLRootOwnsTheFQDN(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.galaxy.AddVersion("acme", "kafka", "1.0.0", nil)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n  - acme.kafka\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrDuplicateCollectionRequirement) {
		t.Fatalf("install error = %v, want ErrDuplicateCollectionRequirement", err)
	}
}

// TestURLLockAndFrozenInstall pins a schema-4 url lock entry and a frozen
// install that replays it, re-downloads on a cache miss, and fails with
// ErrSHA256Mismatch once the origin serves different bytes.
func TestURLLockAndFrozenInstall(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	assertURLLockEntries(t, f)

	f.cfg.Frozen = true
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "kafka")
	downloadsAfterInstall := f.galaxy.Count(fakegalaxy.EndpointTarball)

	// A frozen cache miss re-downloads the pinned URL.
	f.evictURLArtifactAndTree(t)
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "kafka")
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != downloadsAfterInstall+1 {
		t.Fatalf("frozen miss downloads = %d, want %d", got, downloadsAfterInstall+1)
	}

	// The origin now serves different bytes under the same URL: the frozen
	// pin fails closed.
	tampered, _ := fakegalaxy.BuildArtifact("acme", "kafka", "0.25.0", nil)
	f.galaxy.AddTarball(urlKafkaPath, tampered)
	f.evictURLArtifactAndTree(t)
	if err := f.install(t); !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("tampered origin under frozen: %v, want ErrSHA256Mismatch", err)
	}
}

// assertURLLockEntries locks and checks the shape of the two entries a url
// root plus its Galaxy dependency leave: the url one carries the URL as its
// source and the origin sha256, the Galaxy one keeps its own shape.
func assertURLLockEntries(t *testing.T, f *urlFixture) {
	t.Helper()
	lf := f.lockfile(t)
	if lf.SchemaVersion != lockfile.SchemaVersionURL {
		t.Fatalf("schema = %d, want %d", lf.SchemaVersion, lockfile.SchemaVersionURL)
	}
	entry := findLockEntry(t, lf, "acme.kafka")
	if !entry.IsURL() || entry.Source != f.tarballURL || entry.SHA256 != f.tarballSHA ||
		entry.Version != urlKafkaVersion || entry.Ref != "" || entry.Commit != "" {
		t.Fatalf("url lock entry = %+v", entry)
	}
	lib := findLockEntry(t, lf, "acme.lib")
	if lib.IsURL() || lib.SHA256 == "" {
		t.Fatalf("galaxy entry lost its shape: %+v", lib)
	}
}

// evictURLArtifactAndTree removes the cached url artifact and the installed
// tree, the frozen-miss setup both arms above share.
func (f *urlFixture) evictURLArtifactAndTree(t *testing.T) {
	t.Helper()
	key := helpers.ArtifactKey(f.locator(), acmeArtifactFilename("kafka", urlKafkaVersion))
	if err := os.Remove(filepath.Join(f.cacheDir, key)); err != nil {
		t.Fatalf("evict cached artifact: %v", err)
	}
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatal(err)
	}
}

// TestURLOfflineAndRefresh pins that --offline replays a recorded pin and
// fails a cold one, and that --refresh re-downloads: unchanged bytes keep the
// install, changed bytes become a new pin and a new install.
func TestURLOfflineAndRefresh(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.mustInstall(t)

	f.cfg.Offline = true
	f.mustInstall(t)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("offline replay reached the origin: downloads = %d, want 1", got)
	}
	cold := newURLFixture(t)
	cold.writeRequirements(t, "collections:\n  - "+cold.tarballURL+"\n")
	cold.cfg.Offline = true
	if err := cold.install(t); !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("cold offline: %v, want ErrOfflineMode", err)
	}

	f.cfg.Offline = false
	f.cfg.Refresh = true
	f.mustInstall(t)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 2 {
		t.Fatalf("refresh downloads = %d, want 2 (one re-download)", got)
	}
	if got := readManifestVersion(t, f.downloadPath, "kafka"); got != urlKafkaVersion {
		t.Fatalf("refresh of unchanged bytes reinstalled version %q", got)
	}

	newData, newSHA := fakegalaxy.BuildArtifact("acme", "kafka", "0.25.0", nil)
	f.galaxy.AddTarball(urlKafkaPath, newData)
	f.mustInstall(t)
	if got := readManifestVersion(t, f.downloadPath, "kafka"); got != "0.25.0" {
		t.Fatalf("refresh of changed bytes installed version %q, want 0.25.0", got)
	}
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.kafka@0.25.0")
	want := urlsource.Locator{URL: f.tarballURL, SHA256: newSHA}.String()
	if entry.Source != want {
		t.Fatalf("installed record Source = %q, want the new pin %q", entry.Source, want)
	}
}

// TestURLDryRunAndNoCache proves a dry run downloads (it has to, to learn
// the identity) but commits no artifact and installs nothing, and that a
// --no-cache run downloads once and hands the bytes to the install phase.
func TestURLDryRunAndNoCache(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.cfg.DryRun = true
	f.mustInstall(t)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("dry run downloads = %d, want 1", got)
	}
	key := helpers.ArtifactKey(f.locator(), acmeArtifactFilename("kafka", urlKafkaVersion))
	if _, err := os.Stat(filepath.Join(f.cacheDir, key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run committed the artifact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.downloadPath, "ansible_collections")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run installed something: %v", err)
	}
	assertTempFilesGone(t, f.cacheDir)

	nc := newURLFixture(t)
	nc.writeRequirements(t, "collections:\n  - "+nc.tarballURL+"\n")
	nc.cfg.NoCache = true
	nc.mustInstall(t)
	assertManifestInstalled(t, nc.downloadPath, "kafka")
	if got := nc.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("no-cache downloads = %d, want 1 (prebuilt handoff)", got)
	}
	ncKey := helpers.ArtifactKey(nc.locator(), acmeArtifactFilename("kafka", urlKafkaVersion))
	if _, err := os.Stat(filepath.Join(nc.cacheDir, ncKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-cache committed the artifact: %v", err)
	}
}

// TestURLDryRunBannerNamesTheDownload pins that the banner of a dry run which
// downloads an unpinned url tarball says so, rather than promising no download.
func TestURLDryRunBannerNamesTheDownload(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.cfg.DryRun = true
	f.mustInstall(t)
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("dry run downloads = %d, want 1", got)
	}
	var banner string
	for _, line := range f.printer.warns {
		if strings.HasPrefix(line, "--dry-run is active:") {
			banner = line
		}
	}
	for _, want := range []string{
		"no Galaxy artifact will be downloaded",
		"url source with no usable recorded pin is still fetched",
		"then discarded",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("dry-run banner %q does not say %q", banner, want)
		}
	}
}

// TestURLRedirectFollowed proves the release-asset shape end to end: the
// requirements entry names a URL that answers 302, and the artifact installs
// from the redirect target.
func TestURLRedirectFollowed(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	redirectURL := f.galaxy.AddRedirect("acme/kafka/releases/download/latest/kafka.tar.gz", f.tarballURL)
	f.writeRequirements(t, "collections:\n  - "+redirectURL+"\n")
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "kafka")
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.kafka@"+urlKafkaVersion)
	want := urlsource.Locator{URL: redirectURL, SHA256: f.tarballSHA}.String()
	if entry.Source != want {
		t.Fatalf("installed record Source = %q, want the requirement's own URL pinned: %q", entry.Source, want)
	}
}

// TestURLBearerTokenReachesTheOrigin proves the auth seam end to end: a
// binding for the origin attaches the Bearer token to the tarball download,
// and the fake, demanding exactly that header, serves it.
func TestURLBearerTokenReachesTheOrigin(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.galaxy.RequireAuth("Bearer url-e2e-token")
	origin := strings.TrimSuffix(f.galaxy.URL(), "/")
	f.runtime.URLHTTP = fetch.NewURLDownload(e2eTimeout, false, []fetch.URLBinding{
		{Origin: originOfURL(t, origin), Token: "url-e2e-token"}, //nolint:gosec // the fixture under test, not a credential
	})
	// No Galaxy dependency: RequireAuth gates every endpoint, and the Galaxy
	// client carries no such token - the url root must resolve alone.
	data, sha := fakegalaxy.BuildArtifact("acme", "solo", "1.0.0", nil)
	soloURL := f.galaxy.AddTarball("dl/acme-solo-1.0.0.tar.gz", data)
	f.writeRequirements(t, "collections:\n  - "+soloURL+"\n")
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "solo")
	if auth, ok := f.galaxy.SeenAuth(fakegalaxy.EndpointTarball); !ok || auth != "Bearer url-e2e-token" {
		t.Fatalf("tarball download Authorization = %q (present=%t), want the Bearer token", auth, ok)
	}
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "acme.solo@1.0.0")
	if entry.ArtifactSHA256 != sha {
		t.Fatalf("installed sha = %q, want %q", entry.ArtifactSHA256, sha)
	}
}

// TestURLCorruptCacheEvictsAndRefetches proves the one bounded recovery: a
// corrupted cached artifact fails its pin check, is evicted, and the refetch
// re-verifies the origin bytes against the locator.
func TestURLCorruptCacheEvictsAndRefetches(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.mustInstall(t)

	key := helpers.ArtifactKey(f.locator(), acmeArtifactFilename("kafka", urlKafkaVersion))
	if err := os.WriteFile(filepath.Join(f.cacheDir, key), []byte("corrupted"), 0o600); err != nil {
		t.Fatalf("corrupt cached artifact: %v", err)
	}
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatal(err)
	}
	f.mustInstall(t)
	assertManifestInstalled(t, f.downloadPath, "kafka")
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 2 {
		t.Fatalf("downloads = %d, want 2 (one recovery refetch)", got)
	}
}

// TestURLWarm proves warm acquires a url root's artifact during discovery
// and a second warm replays the pin with no download.
func TestURLWarm(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	assertArtifactFilePresent(t, f.cacheDir, f.locator(), acmeArtifactFilename("kafka", urlKafkaVersion))
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("warm downloads = %d, want 1", got)
	}
	if err := collections.Warm(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Warm: %v", err)
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
		t.Fatalf("second warm reached the origin: downloads = %d, want 1", got)
	}
}

// originOfURL renders raw's normalized origin the way a URLBinding carries
// it.
func originOfURL(t *testing.T, raw string) string {
	t.Helper()
	p, err := urlsource.ParsePrefix(raw)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", raw, err)
	}
	return p.Origin()
}

// TestURLOutdatedIsCurrentByConstruction pins outdated's url verdict: no
// network round trip for the url entry - its pin is content-addressed - and
// the run succeeds with the Galaxy dependency looked up as usual.
func TestURLOutdatedIsCurrentByConstruction(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	f.writeRequirements(t, "collections:\n  - "+f.tarballURL+"\n")
	f.lockfile(t)
	downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)
	if err := collections.Outdated(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Outdated: %v", err)
	}
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != downloads {
		t.Fatalf("outdated reached the url origin: downloads = %d, want %d", got, downloads)
	}
}

// TestURLUppercaseManifestIdentityInstalls pins the relaxed url identity
// alphabet: a MANIFEST.json declaring a mixed-case namespace installs,
// records and locks under it, as ansible-galaxy installs it.
func TestURLUppercaseManifestIdentityInstalls(t *testing.T) {
	t.Parallel()
	f := newURLFixture(t)
	data, sha := fakegalaxy.BuildArtifact("StephenSorriaux", "ansible_kafka_admin", "0.24.0", nil)
	upperURL := f.galaxy.AddTarball("dl/StephenSorriaux-ansible_kafka_admin-0.24.0.tar.gz", data)
	f.writeRequirements(t, "collections:\n  - "+upperURL+"\n")
	f.mustInstall(t)
	manifest := filepath.Join(f.downloadPath, "ansible_collections", "StephenSorriaux", "ansible_kafka_admin", "MANIFEST.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("mixed-case collection not installed: %v", err)
	}
	entry := loadInstalledEntry(t, f.cfg, f.runtime, "StephenSorriaux.ansible_kafka_admin@0.24.0")
	want := urlsource.Locator{URL: upperURL, SHA256: sha}.String()
	if entry.Source != want {
		t.Fatalf("installed record Source = %q, want %q", entry.Source, want)
	}
	lf := f.lockfile(t)
	locked := findLockEntry(t, lf, "StephenSorriaux.ansible_kafka_admin")
	if !locked.IsURL() || locked.SHA256 != sha {
		t.Fatalf("uppercase url lock entry = %+v", locked)
	}
}
