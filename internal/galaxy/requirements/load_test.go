package requirements

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// brokenRequirementsYAML is a requirements file whose bytes are not YAML.
const brokenRequirementsYAML = "collections:\n  - name: [unclosed\n"

// loadFailureCase is one row of TestLoadClassifiesFileFailures: the sentinel
// Load's error must carry and one it must not.
type loadFailureCase struct {
	want    error
	notWant error
	name    string
	path    string
}

// TestLoadClassifiesFileFailures pins the sentinel of each way a file fails
// before its shape is judged; absence stays a bare fs.ErrNotExist, which
// cleanup tolerates where it aborts on an unreadable file.
func TestLoadClassifiesFileFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.yml")
	if err := os.WriteFile(broken, []byte(brokenRequirementsYAML), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", broken, err)
	}
	cases := []loadFailureCase{
		{name: "missing", path: filepath.Join(dir, "missing.yml"), want: fs.ErrNotExist, notWant: helpers.ErrRequirementsUnreadable},
		{name: "a directory", path: dir, want: helpers.ErrRequirementsUnreadable, notWant: helpers.ErrInvalidRequirementsYAML},
		{name: "not YAML", path: broken, want: helpers.ErrInvalidRequirementsYAML, notWant: helpers.ErrRequirementsUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(tc.path, "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Load(%q) error = %v, want errors.Is %v", tc.path, err, tc.want)
			}
			if errors.Is(err, tc.notWant) {
				t.Fatalf("Load(%q) error = %v, must not match %v", tc.path, err, tc.notWant)
			}
		})
	}
}

// TestLoadPermissionDeniedIsUnreadable pins a file this process may not open.
// Root opens it anyway, so the test skips there.
func TestLoadPermissionDeniedIsUnreadable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	path := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(path, []byte("collections: []\n"), 0o000); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	_, err := Load(path, "")
	if !errors.Is(err, helpers.ErrRequirementsUnreadable) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Load error = %v, want errors.Is helpers.ErrRequirementsUnreadable and fs.ErrPermission", err)
	}
}

// TestReadLeavesBytesUnparsed pins that Read, which hash keys on, returns a
// file that is not YAML as it is, so only Parse refuses it.
func TestReadLeavesBytesUnparsed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(path, []byte(brokenRequirementsYAML), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	data, err := Read(path)
	if err != nil {
		t.Fatalf("Read error = %v, want nil", err)
	}
	if string(data) != brokenRequirementsYAML {
		t.Fatalf("Read = %q, want %q", data, brokenRequirementsYAML)
	}
}
