package collections

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// presignedQuery is the shape of a real object-storage presigned URL's query
// string: a time-limited bearer capability that must never be persisted into
// the collections tree.
const presignedQuery = "?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeefcafe&X-Amz-Expires=600"

// acmeArtifactURL is the credential-free, query-free download URL every test
// in this file expects to survive a cut, and the base each of them decorates
// with a query, a userinfo, or both.
const acmeArtifactURL = "https://objects.example.com/artifacts/acme-widgets-1.0.0.tar.gz"

// newVersionInfo builds the version metadata writeGalaxyInfo consumes, with
// the given download and version URLs.
func newVersionInfo(downloadURL, href string) *types.GalaxyCollectionVersionInfo {
	info := &types.GalaxyCollectionVersionInfo{}
	info.DownloadURL = downloadURL
	info.Href = href
	info.Name = "widgets"
	info.Namespace.Name = "acme"
	info.Version = testVersion100
	return info
}

// TestBuildGalaxyYAMLStripsPresignedQuery asserts the capability-bearing
// query string of a presigned URL is dropped from both download_url and
// version_url, while the scheme, host and path survive.
func TestBuildGalaxyYAMLStripsPresignedQuery(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Server: "https://hub.example.com/api/automation-hub"}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	meta := newVersionInfo(
		acmeArtifactURL+presignedQuery,
		"https://hub.example.com/api/automation-hub/v3/collections/acme/widgets/versions/1.0.0/"+presignedQuery,
	)

	g := buildGalaxyYAML(cfg, col, meta)

	if g.DownloadURL != acmeArtifactURL {
		t.Errorf("download_url = %q, want %q", g.DownloadURL, acmeArtifactURL)
	}
	if want := "https://hub.example.com/api/automation-hub/v3/collections/acme/widgets/versions/1.0.0/"; g.VersionURL != want {
		t.Errorf("version_url = %q, want %q", g.VersionURL, want)
	}
}

// TestBuildGalaxyYAMLKeepsQuerylessURLs asserts the strip is a no-op for the
// public Galaxy shape, whose download URL carries no query at all.
func TestBuildGalaxyYAMLKeepsQuerylessURLs(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Server: "https://galaxy.ansible.com"}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	downloadURL := "https://galaxy.ansible.com/download/acme-widgets-1.0.0.tar.gz"
	href := "https://galaxy.ansible.com/api/v3/collections/acme/widgets/versions/1.0.0/"
	meta := newVersionInfo(downloadURL, href)

	g := buildGalaxyYAML(cfg, col, meta)

	if g.DownloadURL != downloadURL {
		t.Errorf("download_url = %q, want it unchanged: %q", g.DownloadURL, downloadURL)
	}
	if g.VersionURL != href {
		t.Errorf("version_url = %q, want it unchanged: %q", g.VersionURL, href)
	}
}

// TestBuildGalaxyYAMLNilMetaUnchanged asserts the artifact-cache-hit path,
// which has no version metadata, still writes identity and server and leaves
// both URL fields empty rather than a cut form of something.
func TestBuildGalaxyYAMLNilMetaUnchanged(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Server: "https://galaxy.ansible.com"}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}

	g := buildGalaxyYAML(cfg, col, nil)

	if g.DownloadURL != "" || g.VersionURL != "" {
		t.Errorf("download_url = %q and version_url = %q, want both empty", g.DownloadURL, g.VersionURL)
	}
	if g.Namespace != "acme" || g.Name != "widgets" || g.Version != "1.0.0" {
		t.Errorf("identity = %s.%s-%s, want acme.widgets-1.0.0", g.Namespace, g.Name, g.Version)
	}
	if g.Server != cfg.Server {
		t.Errorf("server = %q, want %q", g.Server, cfg.Server)
	}
}

// urlPassword is the credential the userinfo tests smuggle into a
// server-supplied URL, distinctive enough that a substring search for it in a
// rendered document or a written file cannot match by coincidence.
const urlPassword = "pa55w0rd-must-not-be-persisted"

// TestBuildGalaxyYAMLStripsUserinfo asserts a credential a server embedded in
// either URL is dropped, each field compared with its userinfo-free twin so the
// cut must yield exactly the URL a credential-free server would have sent.
func TestBuildGalaxyYAMLStripsUserinfo(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Server: "https://hub.example.com/api/automation-hub"}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	const wantVersion = "https://hub.example.com/api/automation-hub/v3/collections/acme/widgets/versions/1.0.0/"
	meta := newVersionInfo(
		"https://u:"+urlPassword+"@objects.example.com/artifacts/acme-widgets-1.0.0.tar.gz",
		"https://u:"+urlPassword+"@hub.example.com/api/automation-hub/v3/collections/acme/widgets/versions/1.0.0/",
	)

	g := buildGalaxyYAML(cfg, col, meta)

	if g.DownloadURL != acmeArtifactURL {
		t.Errorf("download_url = %q, want %q", g.DownloadURL, acmeArtifactURL)
	}
	if g.VersionURL != wantVersion {
		t.Errorf("version_url = %q, want %q", g.VersionURL, wantVersion)
	}
}

// TestBuildGalaxyYAMLStripsUserinfoAndQueryTogether asserts the two cuts
// compose: a presigned download URL that also embeds a credential loses both
// the query and the userinfo, not only whichever cut ran last.
func TestBuildGalaxyYAMLStripsUserinfoAndQueryTogether(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Server: "https://hub.example.com/api/automation-hub"}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	meta := newVersionInfo("https://u:"+urlPassword+"@objects.example.com/artifacts/acme-widgets-1.0.0.tar.gz"+presignedQuery, "")

	g := buildGalaxyYAML(cfg, col, meta)

	if g.DownloadURL != acmeArtifactURL {
		t.Errorf("download_url = %q, want %q", g.DownloadURL, acmeArtifactURL)
	}
}

// TestWriteGalaxyInfoPersistsNoUserinfo asserts the GALAXY.yml bytes, which
// outlive the run as a CI artifact, carry the credential in no field at all
// while download_url itself survives in its cut form.
func TestWriteGalaxyInfoPersistsNoUserinfo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://hub.example.com/api/automation-hub", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	meta := newVersionInfo("https://u:"+urlPassword+"@objects.example.com/artifacts/acme-widgets-1.0.0.tar.gz", "")
	target := newTestInstallTarget(t, cfg, col)

	if err := writeGalaxyInfo(target, cfg, col, meta); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	path := filepath.Join(root, "ansible_collections", "acme.widgets-1.0.0.info", "GALAXY.yml")
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, marker := range []string{urlPassword, "u:", "@"} {
		if strings.Contains(string(data), marker) {
			t.Errorf("GALAXY.yml contains %q, want the userinfo stripped:\n%s", marker, data)
		}
	}

	var g GalaxyYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal GALAXY.yml: %v", err)
	}
	if g.DownloadURL != acmeArtifactURL {
		t.Errorf("download_url = %q, want %q", g.DownloadURL, acmeArtifactURL)
	}
}

// TestWriteGalaxyInfoPersistsNoSignature asserts the GALAXY.yml bytes, which
// outlive the run as a CI artifact, contain no part of a presigned query.
func TestWriteGalaxyInfoPersistsNoSignature(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://hub.example.com/api/automation-hub", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	meta := newVersionInfo(acmeArtifactURL+presignedQuery, "")
	target := newTestInstallTarget(t, cfg, col)

	if err := writeGalaxyInfo(target, cfg, col, meta); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	path := filepath.Join(root, "ansible_collections", "acme.widgets-1.0.0.info", "GALAXY.yml")
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, marker := range []string{"X-Amz-Signature", "X-Amz-Algorithm", "X-Amz-Expires", "?"} {
		if strings.Contains(string(data), marker) {
			t.Errorf("GALAXY.yml contains %q, want the presigned query stripped:\n%s", marker, data)
		}
	}

	var g GalaxyYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal GALAXY.yml: %v", err)
	}
	if g.DownloadURL != acmeArtifactURL {
		t.Errorf("download_url = %q, want %q", g.DownloadURL, acmeArtifactURL)
	}
}

// TestNewInstallTargetRefusesTraversingVersionBeforeWriteGalaxyInfo asserts
// newInstallTarget refuses a version escaping DownloadPath before any write. It
// takes five "..", since path.Join fuses the first into "acme.widgets-..".
func TestNewInstallTargetRefusesTraversingVersionBeforeWriteGalaxyInfo(t *testing.T) {
	t.Parallel()

	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "project", "collections")
	victim := filepath.Join(sandbox, "victim")
	mustMkdirAll(t, victim)
	mustMkdirAll(t, downloadPath)

	cfg := &config.Config{DownloadPath: downloadPath}
	col := collection{Namespace: "acme", Name: "widgets", Version: "../../../../../victim/pwned"}
	root := newTestCollectionsRoot(t, downloadPath)

	if _, ok := newInstallTarget(root, cfg, col); ok {
		t.Fatal("expected newInstallTarget to reject a traversing version, got ok=true")
	}
	// (b) pins that nothing was ever created under the escape target: with no
	// chokepoint call, the join would land inside victim and this assertion is
	// what actually discriminates a deleted guard from a working one.
	if _, statErr := os.Stat(filepath.Join(victim, "pwned.info")); !os.IsNotExist(statErr) {
		t.Fatalf("expected %s/pwned.info to not exist, stat error: %v", victim, statErr)
	}
	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatalf("read dir %s: %v", victim, err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected %s to remain empty, got %d entries", victim, len(entries))
	}
}

// TestWriteGalaxyInfoIgnoresMetaIdentity asserts the sidecar's path and
// identity come from col, not from meta, even when meta carries a different
// identity whose fields are all valid path elements.
func TestWriteGalaxyInfoIgnoresMetaIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{Server: "https://galaxy.ansible.com", DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	meta := newVersionInfo("https://objects.example.com/artifacts/evil-other-9.9.9.tar.gz"+presignedQuery, "")
	meta.Name = "other"
	meta.Namespace.Name = "evil"
	meta.Version = "9.9.9"
	target := newTestInstallTarget(t, cfg, col)

	if err := writeGalaxyInfo(target, cfg, col, meta); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	wantPath := filepath.Join(root, "ansible_collections", "acme.widgets-1.0.0.info", "GALAXY.yml")
	data, err := os.ReadFile(wantPath) // #nosec G304 -- path is built from this test's own t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", wantPath, err)
	}
	metaPath := filepath.Join(root, "ansible_collections", "evil.other-9.9.9.info")
	if _, statErr := os.Stat(metaPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected %s to not exist, stat error: %v", metaPath, statErr)
	}

	var g GalaxyYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal GALAXY.yml: %v", err)
	}
	if g.Namespace != col.Namespace || g.Name != col.Name || g.Version != col.Version {
		t.Errorf("identity = %s.%s-%s, want %s.%s-%s", g.Namespace, g.Name, g.Version, col.Namespace, col.Name, col.Version)
	}
	if want := "https://objects.example.com/artifacts/evil-other-9.9.9.tar.gz"; g.DownloadURL != want {
		t.Errorf("download_url = %q, want %q (meta-sourced fields must survive)", g.DownloadURL, want)
	}
}

// TestInstallRecordMatchesAgreesWithWriteGalaxyInfo asserts the skip check
// finds the sidecar writeGalaxyInfo wrote when meta's version differs from
// col's: both must build the .info path from col's identity.
func TestInstallRecordMatchesAgreesWithWriteGalaxyInfo(t *testing.T) {
	t.Parallel()

	sandbox := t.TempDir()
	downloadPath := filepath.Join(sandbox, "install")
	cfg := &config.Config{DownloadPath: downloadPath}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100, Source: "https://galaxy.example.com"}
	target := newTestInstallTarget(t, cfg, col)
	mustMkdirAll(t, target.path)
	seedValidExtractMarker(t, target, validMarkerSHA)

	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		Source:         col.Source,
		ArtifactSHA256: validMarkerSHA,
		InstalledAt:    time.Now().UTC(),
	})

	// meta's version deliberately disagrees with col's - benign server-side
	// normalization, the very case the design forbids comparing against.
	meta := newVersionInfo("https://galaxy.example.com/download/acme-widgets-1.0.0.tar.gz", "")
	meta.Version = "9.9.9"

	if err := writeGalaxyInfo(target, cfg, col, meta); err != nil {
		t.Fatalf("writeGalaxyInfo: %v", err)
	}

	if !installRecordMatches(target, col, st) {
		t.Fatal("expected installRecordMatches to agree with the GALAXY.yml writeGalaxyInfo actually wrote")
	}
}

// An unsafe col.Version never reaches installRecordMatches: newInstallTarget,
// the chokepoint for the install and the .info path, refuses it first (see
// TestNewInstallTargetRejectsUnsafeIdentifiers).

// TestWriteGalaxyInfoIfPresentWarnsOnCollectionsPathEscape asserts an
// ansible_collections symlinked outside DownloadPath is reported on the Warnf
// tier, which survives --quiet, and never on the best-effort Printf tier.
func TestWriteGalaxyInfoIfPresentWarnsOnCollectionsPathEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(root, "ansible_collections")); err != nil {
		t.Fatalf("remove ansible_collections: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "ansible_collections")); err != nil {
		t.Fatalf("symlink ansible_collections: %v", err)
	}

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	writeGalaxyInfoIfPresent(runtime, target, cfg, col, nil)

	if len(printer.warns) != 1 {
		t.Fatalf("warns = %v, want exactly one warning", printer.warns)
	}
	if !strings.Contains(printer.warns[0], "Refusing to write GALAXY.yml") {
		t.Errorf("warns[0] = %q, want it to mention refusing to write GALAXY.yml", printer.warns[0])
	}
	if len(printer.prints) != 0 {
		t.Errorf("prints = %v, want none: a path escape must not also hit the ordinary-failure tier", printer.prints)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("expected nothing written outside DownloadPath, entries=%v err=%v", entries, err)
	}
}

// TestWriteGalaxyInfoIfPresentPrintsOnOrdinaryFailure asserts an ordinary I/O
// failure lands on Printf, never Warnf. It makes ansible_collections read-only,
// since writeGalaxyInfo would simply remove a file planted at target.info.
func TestWriteGalaxyInfoIfPresentPrintsOnOrdinaryFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based write guard cannot be tested")
	}

	root := t.TempDir()
	cfg := &config.Config{DownloadPath: root}
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	target := newTestInstallTarget(t, cfg, col)

	collectionsDir := filepath.Join(root, "ansible_collections")
	//nolint:gosec // G302: 0o555 is this test's own fixture permission, restored in t.Cleanup below.
	if err := os.Chmod(collectionsDir, 0o555); err != nil {
		t.Fatalf("chmod ansible_collections: %v", err)
	}
	// Restored before t.TempDir's own cleanup runs, which needs to remove
	// ansible_collections itself.
	t.Cleanup(func() {
		if err := os.Chmod(collectionsDir, helpers.DirMod); err != nil {
			t.Errorf("restore ansible_collections perms: %v", err)
		}
	})

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	writeGalaxyInfoIfPresent(runtime, target, cfg, col, nil)

	if len(printer.prints) != 1 {
		t.Fatalf("prints = %v, want exactly one", printer.prints)
	}
	if !strings.Contains(printer.prints[0], "Failed to write GALAXY.yml") {
		t.Errorf("prints[0] = %q, want it to mention the write failure", printer.prints[0])
	}
	if len(printer.warns) != 0 {
		t.Errorf("warns = %v, want none: an ordinary I/O failure must not also hit the security-signal tier", printer.warns)
	}
}

// TestBuildGalaxyYAMLRecordsTheResolvingServer asserts the sidecar's server is
// the one the collection resolved from, since outdated asks that server, and
// falls back to the run's server only when no source was stamped.
func TestBuildGalaxyYAMLRecordsTheResolvingServer(t *testing.T) {
	t.Parallel()

	const runServer = "https://hub.example/galaxy/ansible"
	const entryServer = "https://hub.example/galaxy/internal"
	cfg := &config.Config{Server: runServer}

	resolved := buildGalaxyYAML(cfg, collection{
		Namespace: "sc", Name: "internal", Version: testVersion100, Source: entryServer,
	}, nil)
	if resolved.Server != entryServer {
		t.Errorf("server = %q, want the collection's own source %q", resolved.Server, entryServer)
	}

	unstamped := buildGalaxyYAML(cfg, collection{
		Namespace: "acme", Name: "widgets", Version: testVersion100,
	}, nil)
	if unstamped.Server != runServer {
		t.Errorf("server with no source stamped = %q, want the run's server %q", unstamped.Server, runServer)
	}
}
