package collections

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"go.yaml.in/yaml/v3"
)

// installedTree builds a collections tree by hand: version-scoped sidecars and
// each collection's MANIFEST.json, written separately so a test can produce
// the disagreeing pair a stale sidecar is.
type installedTree struct {
	t    *testing.T
	path string
}

func newInstalledTree(t *testing.T) *installedTree {
	t.Helper()
	return &installedTree{t: t, path: t.TempDir()}
}

// sidecar writes one <namespace>.<name>-<version>.info/GALAXY.yml, filed
// under the directory name doc's own fields compose - the agreement
// scanInstalledCollection requires. sidecarAt is the way to break it.
func (tr *installedTree) sidecar(doc GalaxyYAML) {
	tr.t.Helper()
	tr.sidecarAt(fmt.Sprintf("%s.%s-%s%s", doc.Namespace, doc.Name, doc.Version, infoDirSuffix), doc)
}

// sidecarAt writes doc into the sidecar directory named dir, whatever doc
// says about itself.
func (tr *installedTree) sidecarAt(dir string, doc GalaxyYAML) {
	tr.t.Helper()
	data, err := yaml.Marshal(&doc)
	if err != nil {
		tr.t.Fatalf("marshal sidecar: %v", err)
	}
	tr.writeUnder(filepath.Join(collectionsDirName, dir, galaxyYAMLFileName), data)
}

// manifest writes the MANIFEST.json of an installed <ns>/<name> tree at the
// version given.
func (tr *installedTree) manifest(namespace, name, version string) {
	tr.t.Helper()
	body := fmt.Sprintf(`{"collection_info":{"namespace":%q,"name":%q,"version":%q}}`, namespace, name, version)
	tr.writeUnder(filepath.Join(collectionsDirName, namespace, name, helpers.ManifestFileName), []byte(body))
}

// install writes both halves at one version: what a completed install leaves.
func (tr *installedTree) install(doc GalaxyYAML) {
	tr.t.Helper()
	tr.sidecar(doc)
	tr.manifest(doc.Namespace, doc.Name, doc.Version)
}

// installFrom is install for a git or url collection: both halves, plus the
// provenanceFileName that says which of the two it came from.
func (tr *installedTree) installFrom(doc GalaxyYAML, prov sidecarProvenance) {
	tr.t.Helper()
	tr.install(doc)
	data, err := yaml.Marshal(&prov)
	if err != nil {
		tr.t.Fatalf("marshal provenance: %v", err)
	}
	dir := fmt.Sprintf("%s.%s-%s%s", doc.Namespace, doc.Name, doc.Version, infoDirSuffix)
	tr.writeUnder(filepath.Join(collectionsDirName, dir, provenanceFileName), data)
}

func (tr *installedTree) writeUnder(rel string, data []byte) {
	tr.t.Helper()
	full := filepath.Join(tr.path, rel)
	if err := os.MkdirAll(filepath.Dir(full), helpers.DirMod); err != nil {
		tr.t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	mustWriteFile(tr.t, full, data)
}

// fallbackServer is the run's configured server, the one a sidecar naming no
// server falls back to, spelled apart from every per-collection server here.
const fallbackServer = "https://fallback.example"

// scan runs the unit under test against this tree.
func (tr *installedTree) scan() (installedScan, *capturingPrinter, error) {
	tr.t.Helper()
	printer := &capturingPrinter{}
	cfg := &config.Config{DownloadPath: tr.path, Server: fallbackServer}
	scan, err := scanInstalledTree(cfg, infra.New(printer, nil))
	return scan, printer, err
}

// galaxyDoc is the sidecar a Galaxy install writes: identity, version, and
// the server it came from.
func galaxyDoc(namespace, name, version, server string) GalaxyYAML {
	return GalaxyYAML{FormatVer: "1.0.0", Namespace: namespace, Name: name, Version: version, Server: server}
}

// TestScanInstalledTreeBuildsEntriesFromSidecars pins that one tree holding a
// Galaxy, a serverless, a url and a git install yields lockfile entries with
// each collection's own server, and lists the git install as skipped.
func TestScanInstalledTreeBuildsEntriesFromSidecars(t *testing.T) {
	t.Parallel()
	tr := newInstalledTree(t)
	tr.install(galaxyDoc("acme", "widgets", "1.0.0", "https://hub.example"))
	// No server of its own: an install that predates the field, or one whose
	// sidecar was written before a server was configured.
	tr.install(galaxyDoc("acme", "gadgets", "2.1.0", ""))
	tr.installFrom(galaxyDoc("acme", "fetched", "3.0.0", "https://files.example/c.tar.gz"),
		sidecarProvenance{URLSHA256: strings.Repeat("a", 64)})
	tr.installFrom(galaxyDoc("acme", "cloned", "4.0.0", "https://git.example/acme/c.git"),
		sidecarProvenance{GitCommit: strings.Repeat("b", 40)})

	scan, printer, err := tr.scan()
	if err != nil {
		t.Fatalf("scanInstalledTree: %v", err)
	}

	want := []lockfile.Entry{
		{Name: "acme.fetched", Version: "3.0.0", Source: "https://files.example/c.tar.gz", Type: lockfile.TypeURL},
		{Name: "acme.gadgets", Version: "2.1.0", Source: fallbackServer},
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://hub.example"},
	}
	assertEntriesEqual(t, scan.entries, want)
	if len(scan.skippedGit) != 1 || scan.skippedGit[0] != "acme.cloned" {
		t.Fatalf("skippedGit = %v, want [acme.cloned]", scan.skippedGit)
	}
	if len(printer.warns) != 0 {
		t.Errorf("a well-formed tree warned about nothing in particular: %v", printer.warns)
	}
}

// TestScanInstalledTreeDropsAStaleSidecar pins that a sidecar naming a version
// MANIFEST.json does not is dropped; the surviving row proves the drop is the
// disagreement rather than a collection the scan failed to read.
func TestScanInstalledTreeDropsAStaleSidecar(t *testing.T) {
	t.Parallel()
	tr := newInstalledTree(t)
	tr.sidecar(galaxyDoc("acme", "widgets", "1.0.0", "https://hub.example"))
	tr.install(galaxyDoc("acme", "widgets", "2.0.0", "https://hub.example"))

	scan, _, err := tr.scan()
	if err != nil {
		t.Fatalf("scanInstalledTree: %v", err)
	}
	assertEntriesEqual(t, scan.entries, []lockfile.Entry{
		{Name: "acme.widgets", Version: "2.0.0", Source: "https://hub.example"},
	})
}

// TestScanInstalledTreeDropsASidecarWithNoInstalledTree covers the other
// half of the same check: a sidecar whose collection has no MANIFEST.json at
// all names a version nothing is running, so it contributes nothing.
func TestScanInstalledTreeDropsASidecarWithNoInstalledTree(t *testing.T) {
	t.Parallel()
	tr := newInstalledTree(t)
	tr.sidecar(galaxyDoc("acme", "widgets", "1.0.0", "https://hub.example"))

	scan, _, err := tr.scan()
	if err != nil {
		t.Fatalf("scanInstalledTree: %v", err)
	}
	if len(scan.entries) != 0 {
		t.Fatalf("entries = %+v, want none", scan.entries)
	}
}

// TestScanInstalledTreeRefusesASidecarFiledUnderAnotherName pins that a
// sidecar counts only when its own fields recompose the directory it sits in,
// so a planted file cannot attribute a version to another collection.
func TestScanInstalledTreeRefusesASidecarFiledUnderAnotherName(t *testing.T) {
	t.Parallel()
	tr := newInstalledTree(t)
	tr.sidecarAt("acme.widgets-1.0.0"+infoDirSuffix, galaxyDoc("evil", "pkg", "9.9.9", "https://hub.example"))
	tr.manifest("evil", "pkg", "9.9.9")
	tr.manifest("acme", "widgets", "1.0.0")

	scan, printer, err := tr.scan()
	if err != nil {
		t.Fatalf("scanInstalledTree: %v", err)
	}
	if len(scan.entries) != 0 {
		t.Fatalf("entries = %+v, want none", scan.entries)
	}
	assertWarnedAbout(t, printer, "not the collection it is filed under")
}

// TestScanInstalledTreeRefusesAnUnusableIdentity pins the alphabet check on a
// sidecar's name and version, values that would otherwise reach a request URL.
func TestScanInstalledTreeRefusesAnUnusableIdentity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  GalaxyYAML
	}{
		{name: "namespace outside the alphabet", doc: galaxyDoc("Acme!", "widgets", "1.0.0", "https://hub.example")},
		{name: "version is not exact", doc: galaxyDoc("acme", "widgets", "1.x", "https://hub.example")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newInstalledTree(t)
			tr.install(tc.doc)

			scan, printer, err := tr.scan()
			if err != nil {
				t.Fatalf("scanInstalledTree: %v", err)
			}
			if len(scan.entries) != 0 {
				t.Fatalf("entries = %+v, want none", scan.entries)
			}
			assertWarnedAbout(t, printer, "names no collection this tool can look up")
		})
	}
}

// TestScanInstalledTreeNamesAnUnreadableSidecar pins that a sidecar that does
// not parse is named in a warning, while a .info directory holding no sidecar
// is skipped silently.
func TestScanInstalledTreeNamesAnUnreadableSidecar(t *testing.T) {
	t.Parallel()
	tr := newInstalledTree(t)
	tr.writeUnder(filepath.Join(collectionsDirName, "acme.widgets-1.0.0"+infoDirSuffix, galaxyYAMLFileName),
		[]byte("namespace: [not, a, string\n"))
	tr.writeUnder(filepath.Join(collectionsDirName, "empty"+infoDirSuffix, "README"), []byte("no sidecar here\n"))

	scan, printer, err := tr.scan()
	if err != nil {
		t.Fatalf("scanInstalledTree: %v", err)
	}
	if len(scan.entries) != 0 {
		t.Fatalf("entries = %+v, want none", scan.entries)
	}
	if len(printer.warns) != 1 {
		t.Fatalf("warns = %v, want exactly the unreadable sidecar", printer.warns)
	}
	assertWarnedAbout(t, printer, "acme.widgets-1.0.0"+infoDirSuffix)
}

// TestOutdatedInputPrefersTheLockfile pins that a present lockfile wins over a
// tree beside it holding another version, since only the lockfile covers roles
// and git refs.
func TestOutdatedInputPrefersTheLockfile(t *testing.T) {
	t.Parallel()
	tr := newInstalledTree(t)
	tr.install(galaxyDoc("acme", "widgets", "9.9.9", "https://hub.example"))
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	lockPath := lockfile.ResolveDefaultPath(reqPath, "")
	if err := lockfile.Save(lockPath, &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        "https://hub.example",
		Collections:   []lockfile.Entry{{Name: "acme.widgets", Version: "1.0.0", Source: "https://hub.example"}},
	}); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}

	cfg := &config.Config{RequirementsFile: reqPath, DownloadPath: tr.path}
	src, err := outdatedInput(cfg, infra.New(&capturingPrinter{}, nil))
	if err != nil {
		t.Fatalf("outdatedInput: %v", err)
	}
	if src.label != lockPath {
		t.Errorf("label = %q, want the lockfile path %q", src.label, lockPath)
	}
	assertEntriesEqual(t, src.collections, []lockfile.Entry{
		{Name: "acme.widgets", Version: "1.0.0", Source: "https://hub.example"},
	})
}

// TestOutdatedInputPropagatesAMalformedLockfile pins that a lockfile that
// exists and fails to load is returned, never hidden by the tree fallback.
func TestOutdatedInputPropagatesAMalformedLockfile(t *testing.T) {
	t.Parallel()
	tr := newInstalledTree(t)
	tr.install(galaxyDoc("acme", "widgets", "1.0.0", "https://hub.example"))
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	mustWriteFile(t, filepath.Join(dir, lockfile.DefaultName), []byte("schema_version: [broken\n"))

	cfg := &config.Config{RequirementsFile: reqPath, DownloadPath: tr.path}
	if _, err := outdatedInput(cfg, infra.New(&capturingPrinter{}, nil)); err == nil {
		t.Fatal("outdatedInput over a malformed lockfile: err = nil, want the load failure")
	}
}

// TestOutdatedInputWithNeitherSourceStaysALockfileError pins that a run with
// neither a lockfile nor a tree fails as helpers.ErrLockfileMissing and names
// the tree it also looked in.
func TestOutdatedInputWithNeitherSourceStaysALockfileError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	treePath := filepath.Join(dir, "collections")

	cfg := &config.Config{RequirementsFile: reqPath, DownloadPath: treePath}
	_, err := outdatedInput(cfg, infra.New(&capturingPrinter{}, nil))
	if !errors.Is(err, helpers.ErrLockfileMissing) {
		t.Fatalf("err = %v, want errors.Is helpers.ErrLockfileMissing", err)
	}
	if !strings.Contains(err.Error(), treePath) {
		t.Errorf("err = %v, want it to name the tree it also looked in (%s)", err, treePath)
	}
}

// TestOutdatedReadsTheInstalledTreeWithoutALockfile pins the fallback end to
// end: an unlocked tree behind a server with a newer version reports the drift
// and a summary line naming the tree.
func TestOutdatedReadsTheInstalledTreeWithoutALockfile(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)
	srv.AddVersion("acme", "widgets", "2.0.0", nil)

	tr := newInstalledTree(t)
	tr.install(galaxyDoc("acme", "widgets", "1.0.0", srv.URL()))
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	printer := &capturingPrinter{}
	cfg := &config.Config{
		Server: srv.URL(), RequirementsFile: reqPath, DownloadPath: tr.path, Workers: 1,
	}
	if err := Outdated(context.Background(), cfg, infra.New(printer, srv.Client())); err != nil {
		t.Fatalf("Outdated: %v", err)
	}

	if !printer.hasUpdateContaining("Outdated: acme.widgets 1.0.0 -> 2.0.0") {
		t.Fatalf("report lacks the drift line: %v", printer.updates)
	}
	// The summary line names the tree the answer came from, not a lockfile
	// path that does not exist.
	if !printer.hasPersistentPrintContaining(tr.path + ": 0 up to date, 1 outdated, 0 failed") {
		t.Fatalf("summary line does not name the tree: %v", printer.persists)
	}
}

// TestReportInstalledGapsNamesWhatItDidNotCheck pins the warnings naming the
// skipped git installs and the installed role count, neither of which the tree
// can answer for.
func TestReportInstalledGapsNamesWhatItDidNotCheck(t *testing.T) {
	t.Parallel()
	printer := &capturingPrinter{}
	reportInstalledGaps(infra.New(printer, nil), installedScan{skippedGit: []string{"acme.cloned"}}, 2)

	joined := strings.Join(printer.warns, "\n")
	for _, want := range []string{"acme.cloned", "2 installed role(s)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings lack %q: %v", want, printer.warns)
		}
	}
}

// TestReportInstalledGapsSaysNothingWhenThereIsNoGap is the negative control
// for the test above: a collections-only project that locks nothing must not
// be told about roles it does not have.
func TestReportInstalledGapsSaysNothingWhenThereIsNoGap(t *testing.T) {
	t.Parallel()
	printer := &capturingPrinter{}
	reportInstalledGaps(infra.New(printer, nil), installedScan{}, 0)
	if len(printer.warns) != 0 {
		t.Fatalf("warns = %v, want none", printer.warns)
	}
}

// TestInstalledRoleCountCountsOnlyInstalledRoles pins that only a directory
// carrying meta/.galaxy_install_info counts as a role, and that an absent roles
// path counts zero rather than failing.
func TestInstalledRoleCountCountsOnlyInstalledRoles(t *testing.T) {
	t.Parallel()
	rolesPath := t.TempDir()
	for _, name := range []string{"geerlingguy.ntp", "acme.base"} {
		dir := filepath.Join(rolesPath, name, "meta")
		if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		mustWriteFile(t, filepath.Join(dir, ".galaxy_install_info"), []byte("version: 1.0.0\n"))
	}
	if err := os.MkdirAll(filepath.Join(rolesPath, "handwritten", "tasks"), helpers.DirMod); err != nil {
		t.Fatalf("mkdir handwritten: %v", err)
	}

	if got := installedRoleCount(&config.Config{RolesPath: rolesPath}); got != 2 {
		t.Errorf("installedRoleCount = %d, want 2", got)
	}
	if got := installedRoleCount(&config.Config{RolesPath: filepath.Join(rolesPath, "absent")}); got != 0 {
		t.Errorf("installedRoleCount over an absent roles path = %d, want 0", got)
	}
}

// assertEntriesEqual compares entries field by field, in order, naming the
// first row that differs rather than dumping both slices.
func assertEntriesEqual(t *testing.T, got, want []lockfile.Entry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// assertWarnedAbout fails unless some recorded warning contains substr.
func assertWarnedAbout(t *testing.T, printer *capturingPrinter, substr string) {
	t.Helper()
	if !printer.hasWarnContaining(substr) {
		t.Fatalf("warnings lack %q: %v", substr, printer.warns)
	}
}

// TestScanInstalledTreeAcceptsAMixedCaseURLCollection pins that a url
// install's mixed-case namespace passes the scan, while the same name from a
// Galaxy install, the control, is still refused.
func TestScanInstalledTreeAcceptsAMixedCaseURLCollection(t *testing.T) {
	t.Parallel()

	t.Run("url install", func(t *testing.T) {
		t.Parallel()
		tr := newInstalledTree(t)
		tr.installFrom(galaxyDoc("StephenSorriaux", "ansible_kafka_admin", "0.24.0", "https://files.example/k.tar.gz"),
			sidecarProvenance{URLSHA256: strings.Repeat("a", 64)})

		scan, printer, err := tr.scan()
		if err != nil {
			t.Fatalf("scanInstalledTree: %v", err)
		}
		assertEntriesEqual(t, scan.entries, []lockfile.Entry{{
			Name:    "StephenSorriaux.ansible_kafka_admin",
			Version: "0.24.0",
			Source:  "https://files.example/k.tar.gz",
			Type:    lockfile.TypeURL,
		}})
		if len(printer.warns) != 0 {
			t.Errorf("an installed url collection was reported as unusable: %v", printer.warns)
		}
	})

	t.Run("galaxy install", func(t *testing.T) {
		t.Parallel()
		tr := newInstalledTree(t)
		tr.install(galaxyDoc("StephenSorriaux", "ansible_kafka_admin", "0.24.0", "https://hub.example"))

		scan, printer, err := tr.scan()
		if err != nil {
			t.Fatalf("scanInstalledTree: %v", err)
		}
		if len(scan.entries) != 0 {
			t.Fatalf("entries = %+v, want none: a Galaxy server accepts no such name", scan.entries)
		}
		assertWarnedAbout(t, printer, "names no collection this tool can look up")
	})
}

// TestScanInstalledTreeAsksTheServerTheSidecarNames pins that a collection is
// looked up at the server its sidecar records, not at the run's default, which
// would answer 404 for a collection it never served.
func TestScanInstalledTreeAsksTheServerTheSidecarNames(t *testing.T) {
	t.Parallel()
	const entryServer = "https://hub.example/galaxy/internal"
	tr := newInstalledTree(t)
	tr.install(galaxyDoc("sc", "internal", "0.0.24", entryServer))

	scan, _, err := tr.scan()
	if err != nil {
		t.Fatalf("scanInstalledTree: %v", err)
	}
	assertEntriesEqual(t, scan.entries, []lockfile.Entry{
		{Name: "sc.internal", Version: "0.0.24", Source: entryServer},
	})
}
