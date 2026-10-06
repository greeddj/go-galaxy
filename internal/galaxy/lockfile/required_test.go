package lockfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestRequiredErrorMapsAbsenceAlone pins RequiredError over each Load outcome:
// absence becomes ErrLockfileMissing naming the path and no longer reads as
// absence, a file that does not load keeps its own error, and success is nil.
func TestRequiredErrorMapsAbsenceAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	absent := filepath.Join(dir, "absent.lock")
	_, loadErr := Load(absent)
	missing := RequiredError(absent, loadErr)
	if !errors.Is(missing, helpers.ErrLockfileMissing) || errors.Is(missing, fs.ErrNotExist) {
		t.Errorf("RequiredError(absent) = %v, want ErrLockfileMissing and not fs.ErrNotExist", missing)
	}
	if !strings.Contains(missing.Error(), absent) {
		t.Errorf("RequiredError(absent) = %q, want it to name %s", missing, absent)
	}

	invalid := filepath.Join(dir, "invalid.lock")
	if err := os.WriteFile(invalid, []byte("schema_version: 9999\ncollections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", invalid, err)
	}
	_, loadErr = Load(invalid)
	if got := RequiredError(invalid, loadErr); got == nil || got.Error() != loadErr.Error() || !errors.Is(got, helpers.ErrLockfileInvalid) {
		t.Errorf("RequiredError(invalid) = %v, want Load's own error %v", got, loadErr)
	}

	if got := RequiredError(absent, nil); got != nil {
		t.Errorf("RequiredError(nil) = %v, want nil", got)
	}
}

// TestLoadRequiredReturnsRequiredError pins that LoadRequired and a Load
// followed by RequiredError fail alike, so lock --check, which loads once,
// fails exactly as LoadRequired would.
func TestLoadRequiredReturnsRequiredError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.lock")
	if err := os.WriteFile(invalid, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", invalid, err)
	}
	for _, path := range []string{filepath.Join(dir, "absent.lock"), invalid} {
		_, required := LoadRequired(path)
		_, loadErr := Load(path)
		if mapped := RequiredError(path, loadErr); required == nil || mapped == nil || required.Error() != mapped.Error() {
			t.Errorf("%s: LoadRequired = %v, RequiredError(Load) = %v; want the same error", path, required, mapped)
		}
	}
}
