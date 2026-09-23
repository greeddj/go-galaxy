package config

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// writeOverlongAnsibleCfg writes an ansible.cfg holding one line past the
// scanner's 64 KiB token limit, the one content parseAnsibleConfig fails on.
func writeOverlongAnsibleCfg(t *testing.T, path string) {
	t.Helper()
	content := "[defaults]\nx = " + strings.Repeat("a", bufio.MaxScanTokenSize) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
	}
}

// assertAnsibleConfigUnreadable checks that err carries
// helpers.ErrAnsibleConfigUnreadable and not the not-found sentinel.
func assertAnsibleConfigUnreadable(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrAnsibleConfigUnreadable) {
		t.Fatalf("error = %v, want errors.Is helpers.ErrAnsibleConfigUnreadable", err)
	}
	if errors.Is(err, helpers.ErrAnsibleConfigNotFound) {
		t.Fatalf("error = %v, an existing file must not read as not found", err)
	}
}

// TestLoadAnsibleConfigFromCLIUnreadable pins that an ansible.cfg that exists
// but cannot be read to the end is refused under its own sentinel, named or
// discovered. Not parallel: t.Setenv.
func TestLoadAnsibleConfigFromCLIUnreadable(t *testing.T) {
	t.Run("explicit directory", func(t *testing.T) {
		c := newAnsibleConfigCmd(t, []string{"--ansible-config=" + t.TempDir()})
		_, _, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigUnreadable(t, err)
	})

	t.Run("explicit line past the scanner limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "long.cfg")
		writeOverlongAnsibleCfg(t, path)
		c := newAnsibleConfigCmd(t, []string{"--ansible-config=" + path})
		_, _, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigUnreadable(t, err)
		if !errors.Is(err, bufio.ErrTooLong) || !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %v, want the scanner's cause and the path %q", err, path)
		}
	})

	t.Run("explicit permission denied", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		path := filepath.Join(t.TempDir(), "noread.cfg")
		if err := os.WriteFile(path, nil, 0o000); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
		}
		c := newAnsibleConfigCmd(t, []string{"--ansible-config=" + path})
		_, _, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigUnreadable(t, err)
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("error = %v, want errors.Is fs.ErrPermission", err)
		}
	})

	t.Run("discovered directory", func(t *testing.T) {
		t.Setenv("ANSIBLE_CONFIG", t.TempDir())
		c := newAnsibleConfigCmd(t, nil)
		_, _, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigUnreadable(t, err)
	})

	t.Run("discovered line past the scanner limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "long.cfg")
		writeOverlongAnsibleCfg(t, path)
		t.Setenv("ANSIBLE_CONFIG", path)
		c := newAnsibleConfigCmd(t, nil)
		_, _, _, err := loadAnsibleConfigFromCLI(c)
		assertAnsibleConfigUnreadable(t, err)
	})
}

// TestLoadAnsibleConfigMissingIsNotUnreadable pins that absence stays a bare
// fs.ErrNotExist, which the explicit path turns into not-found and discovery
// passes over.
func TestLoadAnsibleConfigMissingIsNotUnreadable(t *testing.T) {
	t.Parallel()
	_, _, err := loadAnsibleConfig(filepath.Join(t.TempDir(), "missing.cfg"))
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, helpers.ErrAnsibleConfigUnreadable) {
		t.Fatalf("loadAnsibleConfig() error = %v, want a bare fs.ErrNotExist", err)
	}
}
