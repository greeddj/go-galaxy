package commands

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// buildConfigFor runs config.BuildCollectionConfig through a two-level command
// tree mirroring main.go: cliflags.CommonFlags on the root and subFlags on the
// subcommand sub, whose action only captures the result.
func buildConfigFor(t *testing.T, sub string, subFlags []cli.Flag, args []string) (*config.Config, error) {
	t.Helper()

	var gotCfg *config.Config
	var gotErr error
	app := &cli.Command{
		Name:  "go-galaxy",
		Flags: cliflags.CommonFlags(),
		Commands: []*cli.Command{
			{
				Name:  sub,
				Flags: subFlags,
				Action: func(_ context.Context, c *cli.Command) error {
					gotCfg, gotErr = config.BuildCollectionConfig(c)
					return nil
				},
			},
		},
	}

	fullArgs := append([]string{"go-galaxy", sub}, args...)
	if err := app.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("app.Run() error = %v, want nil", err)
	}
	return gotCfg, gotErr
}

// neutralizeAnsibleDiscovery hides ./ansible.cfg and ~/.ansible.cfg. It cannot
// hide /etc/ansible/ansible.cfg, so tests assert only values a flag or env
// source set explicitly, which outrank any ansible.cfg value.
func neutralizeAnsibleDiscovery(t *testing.T) {
	t.Helper()
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "absent.cfg"))
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
}

// assertConfigField fails the test unless got == want, naming field in the
// failure message. Kept generic so each call site stays a single line,
// which is what keeps the surface tests below within funlen/gocognit.
func assertConfigField[T comparable](t *testing.T, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}

// TestCleanupConfigSurface pins that cleanup, registering only its own flag
// set beside the globals, still builds a Config: unregistered flags read as
// zero values, safe because cleanup consumes only DryRun, CacheDir and S3Cache.
func TestCleanupConfigSurface(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	cacheDir := t.TempDir()

	t.Run("local cache", func(t *testing.T) {
		args := []string{"--cache-dir=" + cacheDir, "--dry-run"}
		cfg, err := buildConfigFor(t, "cleanup", Cleanup().Flags, args)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if cfg == nil {
			t.Fatal("BuildCollectionConfig() cfg = nil, want non-nil")
		}
		assertConfigField(t, "CacheDir", cfg.CacheDir, cacheDir)
		assertConfigField(t, "DryRun", cfg.DryRun, true)
		assertConfigField(t, "S3Cache.Enabled", cfg.S3Cache.Enabled, false)
	})

	t.Run("S3 cache", func(t *testing.T) {
		args := []string{
			"--cache-dir=" + cacheDir,
			"--s3-bucket=b",
			"--s3-access-key=k",
			"--s3-secret-key=s",
		}
		cfg, err := buildConfigFor(t, "cleanup", Cleanup().Flags, args)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		assertConfigField(t, "S3Cache.Enabled", cfg.S3Cache.Enabled, true)
		assertConfigField(t, "S3Cache.Bucket", cfg.S3Cache.Bucket, "b")
	})
}

// TestCollectionCommandConfigSurface pins that every flag of the
// CollectionFlags+S3Flags union shared by install, lock, warm and outdated
// round-trips into its Config field.
func TestCollectionCommandConfigSurface(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	cacheDir := t.TempDir()

	flags := append(cliflags.CollectionFlags(), cliflags.S3Flags()...)
	args := []string{
		"--server=https://explicit.example",
		"--download-path=/explicit/collections",
		"--roles-path=/explicit/roles",
		"--requirements-file=req.yml",
		"--lock-file=req.lock.yml",
		"--timeout=45s",
		"--workers=1", // the one value inside the accepted range on every machine; see TestWorkersEnvShapes
		"--offline",
		"--cache-dir=" + cacheDir,
	}
	cfg, err := buildConfigFor(t, "install", flags, args)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	if cfg == nil {
		t.Fatal("BuildCollectionConfig() cfg = nil, want non-nil")
	}

	assertConfigField(t, "Server", cfg.Server, "https://explicit.example")
	assertConfigField(t, "DownloadPath", cfg.DownloadPath, "/explicit/collections")
	assertConfigField(t, "RolesPath", cfg.RolesPath, "/explicit/roles")
	assertConfigField(t, "RequirementsFile", cfg.RequirementsFile, "req.yml")
	assertConfigField(t, "LockFile", cfg.LockFile, "req.lock.yml")
	assertConfigField(t, "Timeout", cfg.Timeout, 45*time.Second)
	assertConfigField(t, "Workers", cfg.Workers, 1)
	assertConfigField(t, "Offline", cfg.Offline, true)
	assertConfigField(t, "CacheDir", cfg.CacheDir, cacheDir)
}

// ansibleCfgWithServerList writes an ansible.cfg with a two-entry server_list
// and points $ANSIBLE_CONFIG at it, overriding the nonexistent path
// neutralizeAnsibleDiscovery sets.
func ansibleCfgWithServerList(t *testing.T) {
	t.Helper()
	neutralizeAnsibleDiscovery(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ansible.cfg")
	body := "[galaxy]\nserver_list = hub, pub\n\n" +
		"[galaxy_server.hub]\nurl = https://hub.example/\n\n" +
		"[galaxy_server.pub]\nurl = https://pub.example/\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write ansible.cfg: %v", err)
	}
	t.Setenv("ANSIBLE_CONFIG", path)
}

// serverIDs returns the resolved server ids in order, so a row can assert the
// whole list rather than one field of one entry.
func serverIDs(cfg *config.Config) []string {
	ids := make([]string, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		ids = append(ids, s.ID)
	}
	return ids
}

// TestAnsibleGalaxyServerDoesNotCollapseServerList pins that
// ANSIBLE_GALAXY_SERVER, like [galaxy] server, yields to a server_list; the
// control rows show GO_GALAXY_SERVER and --server still collapse it.
func TestAnsibleGalaxyServerDoesNotCollapseServerList(t *testing.T) {
	t.Run("the ansible env spelling leaves server_list intact", func(t *testing.T) {
		ansibleCfgWithServerList(t)
		t.Setenv("ANSIBLE_GALAXY_SERVER", "https://forced.example/")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if got := serverIDs(cfg); !slices.Equal(got, []string{"hub", "pub"}) {
			t.Fatalf("server ids = %v, want [hub pub]", got)
		}
	})

	t.Run("the go-galaxy env spelling still collapses it", func(t *testing.T) {
		ansibleCfgWithServerList(t)
		t.Setenv("GO_GALAXY_SERVER", "https://forced.example/")

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if got := len(cfg.Servers); got != 1 {
			t.Fatalf("server count = %d (%v), want 1", got, serverIDs(cfg))
		}
	})

	t.Run("the flag still collapses it", func(t *testing.T) {
		ansibleCfgWithServerList(t)

		cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), []string{"--server=https://forced.example/"})
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if got := len(cfg.Servers); got != 1 {
			t.Fatalf("server count = %d (%v), want 1", got, serverIDs(cfg))
		}
	})
}

// aliasCfg builds the install config with no CLI arguments at all, so a value
// a row below asserts on can only have arrived through an env source.
func aliasCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	return cfg
}

// TestFlagNameEnvAliases pins GO_GALAXY_DOWNLOAD_PATH and GO_GALAXY_TIMEOUT
// second in their chains, below the older name and above the ANSIBLE_ one. The
// values avoid the flag defaults, and paths avoid ":", which the path split cuts.
func TestFlagNameEnvAliases(t *testing.T) {
	base := t.TempDir()
	pathA := filepath.Join(base, "a")
	pathB := filepath.Join(base, "b")
	pathC := filepath.Join(base, "c")

	t.Run("the flag-name spelling alone sets the path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_DOWNLOAD_PATH", pathB)

		assertConfigField(t, "DownloadPath", aliasCfg(t).DownloadPath, pathB)
	})

	t.Run("the name that shipped first outranks it for the path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_COLLECTIONS_PATH", pathA)
		t.Setenv("GO_GALAXY_DOWNLOAD_PATH", pathB)

		assertConfigField(t, "DownloadPath", aliasCfg(t).DownloadPath, pathA)
	})

	t.Run("it outranks the ansible spelling for the path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_DOWNLOAD_PATH", pathB)
		t.Setenv("ANSIBLE_COLLECTIONS_PATH", pathC)

		assertConfigField(t, "DownloadPath", aliasCfg(t).DownloadPath, pathB)
	})

	t.Run("the flag-name spelling alone sets the timeout", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_TIMEOUT", "90s")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 90*time.Second)
	})

	t.Run("the name that shipped first outranks it for the timeout", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_SERVER_TIMEOUT", "45s")
		t.Setenv("GO_GALAXY_TIMEOUT", "90s")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 45*time.Second)
	})

	t.Run("it outranks the ansible spelling for the timeout", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_TIMEOUT", "90s")
		t.Setenv("ANSIBLE_GALAXY_SERVER_TIMEOUT", "60")

		assertConfigField(t, "Timeout", aliasCfg(t).Timeout, 90*time.Second)
	})
}

// TestAnsibleRequirementsFileEnvIsStillRead pins a deliberate exception:
// ANSIBLE_GALAXY_REQUIREMENTS_FILE, no ansible name, is still read below
// GO_GALAXY_REQUIREMENTS_FILE; dropping it would silently install another file.
func TestAnsibleRequirementsFileEnvIsStillRead(t *testing.T) {
	const (
		ansiblePath  = "/from-ansible.yml"
		goGalaxyPath = "/from-go-galaxy.yml"
	)

	t.Run("the ansible-namespaced name is read", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("ANSIBLE_GALAXY_REQUIREMENTS_FILE", ansiblePath)

		assertConfigField(t, "RequirementsFile", aliasCfg(t).RequirementsFile, ansiblePath)
	})

	t.Run("go-galaxy's own name outranks it", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_REQUIREMENTS_FILE", goGalaxyPath)
		t.Setenv("ANSIBLE_GALAXY_REQUIREMENTS_FILE", ansiblePath)

		assertConfigField(t, "RequirementsFile", aliasCfg(t).RequirementsFile, goGalaxyPath)
	})
}

// workersEnvRow is one shape GO_GALAXY_WORKERS can arrive in from an ambient
// CI environment block: the exported value, the worker count the run must
// resolve to, and whether it must also have warned about the value it replaced.
type workersEnvRow struct {
	name     string
	value    string
	want     int
	wantWarn bool
}

// workersWarning returns the queued warning naming --workers, or "" when none,
// matching the flag name rather than a slice position; "--download-workers"
// does not match.
func workersWarning(cfg *config.Config) string {
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "--workers") {
			return w
		}
	}
	return ""
}

// TestWorkersEnvShapes pins how each GO_GALAXY_WORKERS shape resolves through
// the real cliflags.CollectionFlags(), which TestApplyWorkers cannot reach; "1"
// is the positive control, in range on every machine and never the default.
func TestWorkersEnvShapes(t *testing.T) {
	derived := galaxyhelpers.DefaultInstallWorkers(runtime.GOMAXPROCS(0))
	rows := []workersEnvRow{
		{name: "a value inside the range is read", value: "1", want: 1},
		{name: "a declared but empty value reads the flag default", value: "", want: derived},
		{name: "zero is replaced with a warning", value: "0", want: derived, wantWarn: true},
		{name: "a negative value is replaced with a warning", value: "-1", want: derived, wantWarn: true},
		{name: "a value far above the ceiling is replaced with a warning", value: "1000000", want: derived, wantWarn: true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			neutralizeAnsibleDiscovery(t)
			t.Setenv("GO_GALAXY_WORKERS", row.value)

			cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
			if err != nil {
				t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
			}
			assertConfigField(t, "Workers", cfg.Workers, row.want)

			warned := workersWarning(cfg)
			if (warned != "") != row.wantWarn {
				t.Fatalf("warned about --workers = %v, want %v", warned != "", row.wantWarn)
			}
			if row.wantWarn && !strings.Contains(warned, row.value) {
				t.Fatalf("warning does not name the supplied value %q: %q", row.value, warned)
			}
		})
	}

	// The cleanup shape on the real flag set: with no --workers flag registered,
	// no source supplied a value, so nothing may be warned about one.
	t.Run("a command registering no workers flag warns about none", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)

		cfg, err := buildConfigFor(t, "cleanup", cliflags.S3Flags(), nil)
		if err != nil {
			t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
		}
		if workersWarning(cfg) != "" {
			t.Fatalf("warned about --workers, want no such warning")
		}
	})
}

// TestRolesPathEnvAliases pins --roles-path's two env spellings and their
// order: GO_GALAXY_ROLES_PATH first, ANSIBLE_ROLES_PATH last, the rule every
// other GO_GALAXY_ name on a collection flag follows.
func TestRolesPathEnvAliases(t *testing.T) {
	base := t.TempDir()
	pathB := filepath.Join(base, "b")
	pathC := filepath.Join(base, "c")

	t.Run("the flag-name spelling alone sets the roles path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_ROLES_PATH", pathB)

		assertConfigField(t, "RolesPath", aliasCfg(t).RolesPath, pathB)
	})

	t.Run("it outranks the ansible spelling for the roles path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("GO_GALAXY_ROLES_PATH", pathB)
		t.Setenv("ANSIBLE_ROLES_PATH", pathC)

		assertConfigField(t, "RolesPath", aliasCfg(t).RolesPath, pathB)
	})

	t.Run("the ansible spelling alone sets the roles path", func(t *testing.T) {
		neutralizeAnsibleDiscovery(t)
		t.Setenv("ANSIBLE_ROLES_PATH", pathC)

		assertConfigField(t, "RolesPath", aliasCfg(t).RolesPath, pathC)
	})
}

// requirementsDiscoveryRow is one working-directory shape for the unset
// --requirements-file: the files planted there, the path the Config must
// carry, and whether the both-present warning must be queued.
type requirementsDiscoveryRow struct {
	name     string
	wantPath string
	files    []string
	wantWarn bool
}

// discoveryWarning returns the queued warning naming galaxy.toml, or "" when
// none, so a row asserts on the discovery line and not on a slice position.
func discoveryWarning(cfg *config.Config) string {
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "galaxy.toml") {
			return w
		}
	}
	return ""
}

// TestRequirementsFileDiscoveryConfigSurface pins that an unset
// --requirements-file reaches cfg.RequirementsFile through discovery: a
// regular ./galaxy.toml wins, warning only when ./requirements.yml sits beside it.
func TestRequirementsFileDiscoveryConfigSurface(t *testing.T) {
	const bothPresent = "galaxy.toml and requirements.yml are both present in the current directory; " +
		"using galaxy.toml and ignoring requirements.yml (name one with --requirements-file to choose)"
	rows := []requirementsDiscoveryRow{
		{name: "galaxy.toml alone is picked silently", files: []string{"galaxy.toml"}, wantPath: "galaxy.toml"},
		{name: "both present picks galaxy.toml with a warning", files: []string{"galaxy.toml", "requirements.yml"},
			wantPath: "galaxy.toml", wantWarn: true},
		{name: "neither present falls back to requirements.yml", wantPath: "requirements.yml"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			neutralizeAnsibleDiscovery(t)
			for _, name := range row.files {
				// Each file is valid in its own format: config now decodes a
				// discovered galaxy.toml for its settings, not only its path.
				body := "collections: []\n"
				if name == "galaxy.toml" {
					body = "[project]\ncollections = []\n"
				}
				if err := os.WriteFile(name, []byte(body), galaxyhelpers.FileMod); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}

			cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
			if err != nil {
				t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
			}
			assertConfigField(t, "RequirementsFile", cfg.RequirementsFile, row.wantPath)

			warned := discoveryWarning(cfg)
			if row.wantWarn {
				assertConfigField(t, "discovery warning", warned, bothPresent)
			} else if warned != "" {
				t.Fatalf("queued a discovery warning %q, want none", warned)
			}
		})
	}
}
