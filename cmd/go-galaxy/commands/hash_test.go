package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

// writeTestFile writes content to path, failing the test on error. It exists
// purely to keep test case bodies terse and free of repeated error checks.
func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// hashTestCase is one row of TestComputeHash: setup prepares the temp
// directory and returns the paths computeHash should receive, check asserts
// on the returned hash/error pair.
type hashTestCase struct {
	setup func(t *testing.T, dir string) (reqPath, lockPath string)
	check func(t *testing.T, got string, err error)
	name  string
}

// testLockSHA is a well-formed Galaxy pin, as lockfile.Load requires.
const testLockSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// setupValidLockfile writes both a requirements file and a well-formed
// lockfile; computeHash must prefer the lockfile hash.
func setupValidLockfile(t *testing.T, dir string) (string, string) {
	t.Helper()
	reqPath := filepath.Join(dir, "requirements.yml")
	writeTestFile(t, reqPath, []byte("collections: []\n"))
	lockPath := filepath.Join(dir, lockfile.DefaultName)
	lf := &lockfile.File{
		Server:        "https://galaxy.ansible.com",
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "ns.name", Version: "1.0.0", Source: "galaxy", DownloadURL: testDownloadURL("ns.name", "1.0.0"), SHA256: testLockSHA},
		},
	}
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}
	return reqPath, lockPath
}

// checkValidLockfile asserts the returned hash matches the saved lockfile's
// own canonical hash.
func checkValidLockfile(t *testing.T, got string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("computeHash() error = %v, want nil", err)
	}
	// Re-derive the expected hash from the same lockfile content rather than
	// hardcoding it, so the test stays correct if the canonical form changes.
	lf := &lockfile.File{
		Server:        "https://galaxy.ansible.com",
		SchemaVersion: lockfile.SchemaVersion,
		Collections: []lockfile.Entry{
			{Name: "ns.name", Version: "1.0.0", Source: "galaxy", DownloadURL: testDownloadURL("ns.name", "1.0.0"), SHA256: testLockSHA},
		},
	}
	wantHash, hashErr := lf.Hash()
	if hashErr != nil {
		t.Fatalf("lf.Hash(): %v", hashErr)
	}
	if want := "sha256:" + wantHash; got != want {
		t.Errorf("computeHash() = %q, want %q", got, want)
	}
}

// reqOnlyContent is the requirements content shared by the fallback case's
// setup and check functions. Kept as a const (not a package-level var) to
// avoid gochecknoglobals.
const reqOnlyContent = "collections:\n  - name: ns.name\n    version: 1.0.0\n"

// setupLockfileAbsent writes only a requirements file; the lockfile path is
// never created so computeHash must fall back to hashing requirements.yml.
func setupLockfileAbsent(t *testing.T, dir string) (string, string) {
	t.Helper()
	reqPath := filepath.Join(dir, "requirements.yml")
	writeTestFile(t, reqPath, []byte(reqOnlyContent))
	lockPath := filepath.Join(dir, lockfile.DefaultName)
	return reqPath, lockPath
}

// checkLockfileAbsentFallback asserts the returned hash is the digest of the
// parsed requirements, not of their bytes.
func checkLockfileAbsentFallback(t *testing.T, got string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("computeHash() error = %v, want nil", err)
	}
	file, parseErr := requirements.Parse([]byte(reqOnlyContent), "")
	if parseErr != nil {
		t.Fatalf("requirements.Parse: %v", parseErr)
	}
	if want := "sha256:" + file.Hash(); got != want {
		t.Errorf("computeHash() = %q, want %q", got, want)
	}
}

// setupCorruptLockfile writes a requirements file plus a lockfile containing
// malformed YAML (an unterminated flow sequence).
func setupCorruptLockfile(t *testing.T, dir string) (string, string) {
	t.Helper()
	reqPath := filepath.Join(dir, "requirements.yml")
	writeTestFile(t, reqPath, []byte("collections: []\n"))
	lockPath := filepath.Join(dir, lockfile.DefaultName)
	writeTestFile(t, lockPath, []byte("schema_version: 1\ncollections: [\n"))
	return reqPath, lockPath
}

// setupUnsupportedSchemaLockfile writes a requirements file plus a
// syntactically valid lockfile whose schema_version is not supported.
func setupUnsupportedSchemaLockfile(t *testing.T, dir string) (string, string) {
	t.Helper()
	reqPath := filepath.Join(dir, "requirements.yml")
	writeTestFile(t, reqPath, []byte("collections: []\n"))
	lockPath := filepath.Join(dir, lockfile.DefaultName)
	writeTestFile(t, lockPath, []byte("schema_version: 999\ncollections: []\n"))
	return reqPath, lockPath
}

// checkErrLockfileInvalid asserts computeHash surfaces the lockfile parse
// error instead of silently falling back to hashing requirements.yml.
func checkErrLockfileInvalid(t *testing.T, got string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("computeHash() = %q, error = nil, want non-nil", got)
	}
	if !errors.Is(err, helpers.ErrLockfileInvalid) {
		t.Errorf("computeHash() error = %v, want errors.Is match with ErrLockfileInvalid", err)
	}
}

// setupBothAbsent points at a requirements file and a lockfile that neither
// exist.
func setupBothAbsent(_ *testing.T, dir string) (string, string) {
	reqPath := filepath.Join(dir, "requirements.yml")
	lockPath := filepath.Join(dir, lockfile.DefaultName)
	return reqPath, lockPath
}

// checkAnyError asserts only that computeHash failed, without pinning the
// exact error (a missing requirements file surfaces a plain os.ReadFile error).
func checkAnyError(t *testing.T, got string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("computeHash() = %q, error = nil, want non-nil", got)
	}
}

// notYAMLContent is a requirements file whose bytes are not YAML, which the
// fallback refuses as every command reading it does.
const notYAMLContent = "collections:\n  - name: [unclosed\n"

// setupRequirementsNotYAML writes only a requirements file that is not YAML.
func setupRequirementsNotYAML(t *testing.T, dir string) (string, string) {
	t.Helper()
	reqPath := filepath.Join(dir, "requirements.yml")
	writeTestFile(t, reqPath, []byte(notYAMLContent))
	return reqPath, filepath.Join(dir, lockfile.DefaultName)
}

// checkRequirementsNotYAMLRefused asserts a file that does not parse yields
// no key: ErrInvalidRequirementsYAML, exit 2.
func checkRequirementsNotYAMLRefused(t *testing.T, got string, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrInvalidRequirementsYAML) {
		t.Fatalf("computeHash() = %q, error = %v, want errors.Is helpers.ErrInvalidRequirementsYAML", got, err)
	}
	if code := exitcode.FromError(err); code != exitcode.ExitUsage {
		t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", code, exitcode.ExitUsage)
	}
}

// fileSourceContent names a type: file source, and refusedRoleContent a role
// the loader refuses; both parse as YAML, so only the loader stops them.
const (
	fileSourceContent  = "collections:\n  - name: acme.app\n    type: file\n"
	refusedRoleContent = "collections:\n  - acme.app\nroles:\n  - name: \"bad name!\"\n    src: \"not a role\"\n"
)

// setupRequirements returns a setup writing content as the only file.
func setupRequirements(content string) func(t *testing.T, dir string) (string, string) {
	return func(t *testing.T, dir string) (string, string) {
		t.Helper()
		reqPath := filepath.Join(dir, "requirements.yml")
		writeTestFile(t, reqPath, []byte(content))
		return reqPath, filepath.Join(dir, lockfile.DefaultName)
	}
}

// checkRefusedWithUsage returns a check that the loader refused the file with
// want: no key, and exit 2 as install gives the same file.
func checkRefusedWithUsage(want error) func(t *testing.T, got string, err error) {
	return func(t *testing.T, got string, err error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("computeHash() error = %v, want errors.Is %v", err, want)
		}
		if got != "" {
			t.Errorf("computeHash() = %q, want no key", got)
		}
		if code := exitcode.FromError(err); code != exitcode.ExitUsage {
			t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", code, exitcode.ExitUsage)
		}
	}
}

// notTOMLContent is a .toml requirements file whose bytes are not TOML, which
// is decoded for its lock_file before any key is computed and so refused.
const notTOMLContent = "[project]\ncollections = [\n"

// TestHashRefusesRequirementsNotTOML runs the command over a broken.toml with
// no lockfile beside it: ErrInvalidRequirementsTOML and exit 2.
func TestHashRefusesRequirementsNotTOML(t *testing.T) {
	t.Parallel()
	reqPath := filepath.Join(t.TempDir(), "broken.toml")
	writeTestFile(t, reqPath, []byte(notTOMLContent))

	err := Hash().Run(context.Background(), []string{"hash", "-r", reqPath})
	if !errors.Is(err, helpers.ErrInvalidRequirementsTOML) {
		t.Fatalf("hash over a broken .toml: error = %v, want errors.Is helpers.ErrInvalidRequirementsTOML", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", got, exitcode.ExitUsage)
	}
}

// setupRequirementsDirectory puts a directory where the requirements file
// should be, with no lockfile, so the fallback read fails.
func setupRequirementsDirectory(t *testing.T, dir string) (string, string) {
	t.Helper()
	reqPath := filepath.Join(dir, "requirements.yml")
	if err := os.Mkdir(reqPath, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", reqPath, err)
	}
	return reqPath, filepath.Join(dir, lockfile.DefaultName)
}

// checkErrRequirementsNotRegular asserts the fallback read's failure carries
// the usage sentinel the requirements loader uses for the same file.
func checkErrRequirementsNotRegular(t *testing.T, got string, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrRequirementsNotRegular) {
		t.Fatalf("computeHash() = %q, error = %v, want errors.Is helpers.ErrRequirementsNotRegular", got, err)
	}
}

// TestComputeHash covers computeHash's cases: a valid lockfile is preferred, a
// missing one falls back to the requirements file, a corrupt or unsupported one
// surfaces its error, and neither file present is an error.
func TestComputeHash(t *testing.T) {
	t.Parallel()
	tests := []hashTestCase{
		{name: "valid lockfile present", setup: setupValidLockfile, check: checkValidLockfile},
		{name: "lockfile absent, requirements present falls back", setup: setupLockfileAbsent, check: checkLockfileAbsentFallback},
		{name: "lockfile absent, requirements not YAML refused", setup: setupRequirementsNotYAML, check: checkRequirementsNotYAMLRefused},
		{
			name:  "lockfile absent, requirements naming a type: file source refused",
			setup: setupRequirements(fileSourceContent),
			check: checkRefusedWithUsage(helpers.ErrUnsupportedCollectionType),
		},
		{
			name:  "lockfile absent, a refused role refused",
			setup: setupRequirements(refusedRoleContent),
			check: checkRefusedWithUsage(helpers.ErrInvalidRoleName),
		},
		{
			name:  "lockfile absent, requirements a directory surfaces ErrRequirementsNotRegular",
			setup: setupRequirementsDirectory,
			check: checkErrRequirementsNotRegular,
		},
		{name: "corrupt lockfile surfaces ErrLockfileInvalid", setup: setupCorruptLockfile, check: checkErrLockfileInvalid},
		{
			name:  "unsupported schema_version surfaces ErrLockfileInvalid",
			setup: setupUnsupportedSchemaLockfile,
			check: checkErrLockfileInvalid,
		},
		{name: "both lockfile and requirements absent", setup: setupBothAbsent, check: checkAnyError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			reqPath, lockPath := tt.setup(t, dir)
			got, err := computeHash(reqPath, lockPath)
			tt.check(t, got, err)
		})
	}
}

// hashTwinYAML and hashTwinTOML ask for the same collections and roles in
// two formats, with comments, respelled constraints, reordered collections,
// a project name and a [tool.go-galaxy] table that no key may see.
const (
	hashTwinYAML = `---
# reviewed quarterly
collections:
  - community.general
  - name: ansible.utils
    version: ">= 6.0.0, <7.0.0"
  - name: acme.app
    version: "==1.4.0"
    source: https://hub.example.com/api/galaxy/
  - name: git+https://github.com/acme/mono.git#collections/app,main
roles:
  - name: geerlingguy.docker
    version: 8.0.0
  - src: https://github.com/acme/ansible-role-base.git
    scm: git
    version: v1.2.0
    name: base
`
	hashTwinTOML = `[project]
name = "infra"
collections = [
  "git+https://github.com/acme/mono.git#collections/app,main",
  "ansible.utils >=6.0.0,<7.0.0",
  { name = "acme.app", version = "1.4.0", source = "https://hub.example.com/api/galaxy/" },
  "community.general",
]
roles = [
  "geerlingguy.docker,8.0.0",
  { src = "git+https://github.com/acme/ansible-role-base.git", version = "v1.2.0", name = "base" },
]

[tool.go-galaxy]
workers = 8
`
)

// TestHashIsTheSameForRequirementsYAMLAndItsGalaxyTOML runs hash in two
// directories holding the two files: the keys must be equal.
func TestHashIsTheSameForRequirementsYAMLAndItsGalaxyTOML(t *testing.T) {
	t.Parallel()
	yamlDir, tomlDir := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(yamlDir, helpers.RequirementsYAMLName), []byte(hashTwinYAML))
	writeTestFile(t, filepath.Join(tomlDir, helpers.RequirementsTOMLName), []byte(hashTwinTOML))
	fromYAML, err := computeHash(filepath.Join(yamlDir, helpers.RequirementsYAMLName), filepath.Join(yamlDir, lockfile.DefaultName))
	if err != nil {
		t.Fatalf("hash requirements.yml: %v", err)
	}
	fromTOML, err := computeHash(filepath.Join(tomlDir, helpers.RequirementsTOMLName), filepath.Join(tomlDir, lockfile.DefaultName))
	if err != nil {
		t.Fatalf("hash galaxy.toml: %v", err)
	}
	if fromYAML != fromTOML {
		t.Errorf("hash of requirements.yml = %q, of its galaxy.toml = %q, want them equal", fromYAML, fromTOML)
	}
}
