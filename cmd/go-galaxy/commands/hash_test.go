package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
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
			{Name: "ns.name", Version: "1.0.0", Source: "galaxy", SHA256: "deadbeef"},
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
			{Name: "ns.name", Version: "1.0.0", Source: "galaxy", SHA256: "deadbeef"},
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

// checkLockfileAbsentFallback asserts the returned hash matches a direct
// SHA256 of the requirements file content.
func checkLockfileAbsentFallback(t *testing.T, got string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("computeHash() error = %v, want nil", err)
	}
	sum := sha256.Sum256([]byte(reqOnlyContent))
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
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
// fallback hashes as it is.
const notYAMLContent = "collections:\n  - name: [unclosed\n"

// setupRequirementsNotYAML writes only a requirements file that is not YAML.
func setupRequirementsNotYAML(t *testing.T, dir string) (string, string) {
	t.Helper()
	reqPath := filepath.Join(dir, "requirements.yml")
	writeTestFile(t, reqPath, []byte(notYAMLContent))
	return reqPath, filepath.Join(dir, lockfile.DefaultName)
}

// checkRequirementsNotYAMLHashed asserts the fallback key is the SHA256 of
// the raw bytes: hash never parses the file it keys on.
func checkRequirementsNotYAMLHashed(t *testing.T, got string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("computeHash() error = %v, want nil", err)
	}
	sum := sha256.Sum256([]byte(notYAMLContent))
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Errorf("computeHash() = %q, want %q", got, want)
	}
}

// notTOMLContent is a .toml requirements file whose bytes are not TOML; the
// fallback hashes it as it is, exactly as it does the not-YAML file.
const notTOMLContent = "[project]\ncollections = [\n"

// setupRequirementsNotTOML writes only a broken.toml, so the fallback reads a
// file the TOML parser would refuse.
func setupRequirementsNotTOML(t *testing.T, dir string) (string, string) {
	t.Helper()
	reqPath := filepath.Join(dir, "broken.toml")
	writeTestFile(t, reqPath, []byte(notTOMLContent))
	return reqPath, filepath.Join(dir, lockfile.DefaultName)
}

// checkRequirementsNotTOMLHashed asserts the fallback key is the SHA256 of the
// raw bytes: a .toml path does not make hash decode the file it keys on.
func checkRequirementsNotTOMLHashed(t *testing.T, got string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("computeHash() error = %v, want nil", err)
	}
	sum := sha256.Sum256([]byte(notTOMLContent))
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Errorf("computeHash() = %q, want %q", got, want)
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

// checkErrRequirementsUnreadable asserts the fallback read's failure carries
// the usage sentinel the requirements loader uses for the same file.
func checkErrRequirementsUnreadable(t *testing.T, got string, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrRequirementsUnreadable) {
		t.Fatalf("computeHash() = %q, error = %v, want errors.Is helpers.ErrRequirementsUnreadable", got, err)
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
		{name: "lockfile absent, requirements not YAML still hashed", setup: setupRequirementsNotYAML, check: checkRequirementsNotYAMLHashed},
		{
			name:  "lockfile absent, .toml requirements not TOML still hashed",
			setup: setupRequirementsNotTOML,
			check: checkRequirementsNotTOMLHashed,
		},
		{
			name:  "lockfile absent, requirements unreadable surfaces ErrRequirementsUnreadable",
			setup: setupRequirementsDirectory,
			check: checkErrRequirementsUnreadable,
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
