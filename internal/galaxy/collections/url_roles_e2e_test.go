package collections_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// urlRoleFixture is the url fixture with a roles path beside the install
// path: url roles need no git client and no v1 API, only tarballs the fake
// serves.
type urlRoleFixture struct {
	*urlFixture

	rolesPath string
}

func newURLRoleFixture(t *testing.T) *urlRoleFixture {
	t.Helper()
	f := &urlRoleFixture{urlFixture: newURLFixture(t)}
	f.rolesPath = filepath.Join(filepath.Dir(f.downloadPath), "roles")
	f.cfg.RolesPath = f.rolesPath
	return f
}

func (f *urlRoleFixture) rolePath(name string) string {
	return filepath.Join(f.rolesPath, name)
}

// buildRoleTarGz renders a role tree as a tar.gz, every path prefixed with
// prefix when one is given - the GitHub-archive shape - and flat otherwise.
// A meta/main.yml is added unless files already carries one.
func buildRoleTarGz(t *testing.T, prefix string, files map[string]string) ([]byte, string) {
	t.Helper()
	if _, ok := files["meta/main.yml"]; !ok {
		withMeta := make(map[string]string, len(files)+1)
		maps.Copy(withMeta, files)
		withMeta["meta/main.yml"] = "dependencies: []\n"
		files = withMeta
	}
	return rawTarGz(t, prefix, files)
}

// rawTarGz renders files verbatim, adding nothing - the builder for archives
// that deliberately are not one role.
func rawTarGz(t *testing.T, prefix string, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	// Sorted for a deterministic artifact, so a test's sha expectations hold.
	for _, name := range sortedStrings(names) {
		full := name
		if prefix != "" {
			full = prefix + "/" + name
		}
		body := files[name]
		hdr := &tar.Header{Name: full, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", full, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("Write(%q): %v", full, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestURLRoleInstallFromTarball pins flat and wrapped role tarballs: one
// download, installed under roles_path with the sha-derived version label and
// the url locator, and a rerun that replays the pin without the origin.
func TestURLRoleInstallFromTarball(t *testing.T) {
	t.Parallel()
	for name, prefix := range map[string]string{"flat": "", "wrapped": "myrole-1.2.3"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newURLRoleFixture(t)
			data, originSHA := buildRoleTarGz(t, prefix, map[string]string{
				"tasks/main.yml": "- name: hello\n  debug: msg=hello\n",
			})
			roleURL := f.galaxy.AddTarball("dl/"+name+"/myrole-1.2.3.tar.gz", data)
			f.writeRequirements(t, "roles:\n  - src: "+roleURL+"\n    name: myrole\n")
			f.mustInstall(t)

			assertFileContains(t, filepath.Join(f.rolePath("myrole"), "tasks", "main.yml"), "hello")
			label := originSHA[:12]
			assertFileContains(t, filepath.Join(f.rolePath("myrole"), "meta", ".galaxy_install_info"), "version: "+label)
			locator := urlsource.Locator{URL: roleURL, SHA256: originSHA}.String()
			st := loadStoreSnapshot(t, f.cfg, f.runtime)
			entry, ok := st.GetInstalledRole("myrole")
			if !ok {
				t.Fatalf("no installed role record for myrole")
			}
			if entry.Source != locator || entry.Version != label || entry.InstallPath != f.rolePath("myrole") {
				t.Fatalf("installed role record = %+v, want locator %s at %s", entry, locator, f.rolePath("myrole"))
			}
			assertArtifactFilePresent(t, f.cacheDir, locator, helpers.RoleArtifactFilename("myrole", label))
			assertTempFilesGone(t, f.cacheDir)
			if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
				t.Fatalf("tarball downloads = %d, want 1", got)
			}

			f.mustInstall(t)
			if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != 1 {
				t.Fatalf("rerun reached the origin: downloads = %d, want still 1", got)
			}
		})
	}
}

// TestURLRoleVersionLabel pins the explicit version label: it names the
// install and the artifact key rather than the sha-derived default.
func TestURLRoleVersionLabel(t *testing.T) {
	t.Parallel()
	f := newURLRoleFixture(t)
	data, originSHA := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=x\n"})
	roleURL := f.galaxy.AddTarball("dl/labeled.tar.gz", data)
	f.writeRequirements(t, "roles:\n  - src: "+roleURL+"\n    name: labeled\n    version: 1.2.3\n")
	f.mustInstall(t)
	assertFileContains(t, filepath.Join(f.rolePath("labeled"), "meta", ".galaxy_install_info"), "version: 1.2.3")
	locator := urlsource.Locator{URL: roleURL, SHA256: originSHA}.String()
	assertArtifactFilePresent(t, f.cacheDir, locator, helpers.RoleArtifactFilename("labeled", "1.2.3"))
}

// TestURLRoleDependencyWalk proves a url role's meta dependencies are
// walked: a dependency naming another url tarball installs beside it, and a
// local name is left alone.
func TestURLRoleDependencyWalk(t *testing.T) {
	t.Parallel()
	f := newURLRoleFixture(t)
	depData, _ := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=dep\n"})
	depURL := f.galaxy.AddTarball("dl/dep-role.tar.gz", depData)
	mainData, _ := buildRoleTarGz(t, "", map[string]string{
		"meta/main.yml":  "dependencies:\n  - src: " + depURL + "\n    name: deprole\n  - common\n",
		"tasks/main.yml": "- debug: msg=main\n",
	})
	mainURL := f.galaxy.AddTarball("dl/main-role.tar.gz", mainData)
	f.writeRequirements(t, "roles:\n  - src: "+mainURL+"\n    name: mainrole\n")
	f.mustInstall(t)
	assertFileContains(t, filepath.Join(f.rolePath("mainrole"), "tasks", "main.yml"), "main")
	assertFileContains(t, filepath.Join(f.rolePath("deprole"), "tasks", "main.yml"), "dep")
	assertPathAbsent(t, f.rolePath("common"))
}

// TestURLRoleLockAndFrozenInstall pins a schema-4 url role lock entry by
// origin sha256, a frozen cache miss that re-downloads and repacks, and an
// origin serving different bytes failing the integrity class.
func TestURLRoleLockAndFrozenInstall(t *testing.T) {
	t.Parallel()
	f := newURLRoleFixture(t)
	data, originSHA := buildRoleTarGz(t, "wrapped-role", map[string]string{"tasks/main.yml": "- debug: msg=x\n"})
	roleURL := f.galaxy.AddTarball("dl/frozen-role.tar.gz", data)
	f.writeRequirements(t, "roles:\n  - src: "+roleURL+"\n    name: frozenrole\n")
	label := originSHA[:12]
	assertURLRoleLockEntry(t, f, roleURL, originSHA, label)

	f.cfg.Frozen = true
	f.mustInstall(t)
	assertFileContains(t, filepath.Join(f.rolePath("frozenrole"), "meta", ".galaxy_install_info"), "version: "+label)
	downloads := f.galaxy.Count(fakegalaxy.EndpointTarball)

	// A frozen cache miss re-downloads the pinned URL and repacks.
	f.evictURLRole(t, roleURL, originSHA, label)
	f.mustInstall(t)
	assertFileContains(t, filepath.Join(f.rolePath("frozenrole"), "tasks", "main.yml"), "msg=x")
	if got := f.galaxy.Count(fakegalaxy.EndpointTarball); got != downloads+1 {
		t.Fatalf("frozen miss downloads = %d, want %d", got, downloads+1)
	}

	// The origin now serves different bytes under the same URL: the frozen
	// pin fails closed.
	tampered, _ := buildRoleTarGz(t, "", map[string]string{"tasks/main.yml": "- debug: msg=tampered\n"})
	f.galaxy.AddTarball("dl/frozen-role.tar.gz", tampered)
	f.evictURLRole(t, roleURL, originSHA, label)
	if err := f.install(t); !errors.Is(err, helpers.ErrURLArtifactSHA256Mismatch) {
		t.Fatalf("tampered origin under frozen: %v, want ErrURLArtifactSHA256Mismatch", err)
	}
}

// assertURLRoleLockEntry locks and checks the one url role entry's shape:
// schema 4, the URL as its source, the origin sha256 as its pin, and none
// of the fields that belong to the other role kinds.
func assertURLRoleLockEntry(t *testing.T, f *urlRoleFixture, roleURL, originSHA, label string) {
	t.Helper()
	lf := f.lockfile(t)
	if lf.SchemaVersion != lockfile.SchemaVersionURL {
		t.Fatalf("schema = %d, want %d", lf.SchemaVersion, lockfile.SchemaVersionURL)
	}
	if len(lf.Roles) != 1 {
		t.Fatalf("roles = %+v, want one entry", lf.Roles)
	}
	got := lf.Roles[0]
	want := lockfile.RoleEntry{Name: got.Name, Type: lockfile.RoleTypeURL, Version: label, Source: roleURL, SHA256: originSHA, Deps: got.Deps}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("url role lock entry = %+v, want %+v", got, want)
	}
}

// evictURLRole removes the cached repacked artifact and the installed roles
// tree, the frozen-miss setup both arms above share.
func (f *urlRoleFixture) evictURLRole(t *testing.T, roleURL, originSHA, label string) {
	t.Helper()
	locator := urlsource.Locator{URL: roleURL, SHA256: originSHA}.String()
	key := helpers.ArtifactKey(locator, helpers.RoleArtifactFilename("frozenrole", label))
	if err := os.Remove(filepath.Join(f.cacheDir, key)); err != nil {
		t.Fatalf("evict cached artifact: %v", err)
	}
	if err := os.RemoveAll(f.rolesPath); err != nil {
		t.Fatal(err)
	}
}

// TestURLRoleAmbiguousLayoutRefused pins the layout rule: an archive whose
// meta/main.yml sits under two top-level directories cannot become one role.
func TestURLRoleAmbiguousLayoutRefused(t *testing.T) {
	t.Parallel()
	f := newURLRoleFixture(t)
	combined, _ := rawTarGz(t, "", map[string]string{
		"one/meta/main.yml":  "dependencies: []\n",
		"one/tasks/main.yml": "- debug: msg=1\n",
		"two/meta/main.yml":  "dependencies: []\n",
		"two/tasks/main.yml": "- debug: msg=2\n",
	})
	roleURL := f.galaxy.AddTarball("dl/ambiguous.tar.gz", combined)
	f.writeRequirements(t, "roles:\n  - src: "+roleURL+"\n    name: ambiguous\n")
	if err := f.install(t); !errors.Is(err, helpers.ErrRoleTarballLayout) {
		t.Fatalf("install error = %v, want ErrRoleTarballLayout", err)
	}
}
