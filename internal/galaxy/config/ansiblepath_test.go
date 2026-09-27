package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// underTestAnsibleConfigDir is rel joined under the directory of
// testAnsibleConfigPath, a relative file name, which is the physical working
// directory: where a relative ansible.cfg value must land.
func underTestAnsibleConfigDir(t *testing.T, rel string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v, want nil", err)
	}
	return filepath.Join(physicalDir(t, wd), rel)
}

// pathEnvKeys are every variable that sets --download-path, --roles-path or
// --cache-dir, which each test here starts without.
func pathEnvKeys() []string {
	return []string{
		"GO_GALAXY_COLLECTIONS_PATH", "GO_GALAXY_DOWNLOAD_PATH", "ANSIBLE_COLLECTIONS_PATH",
		"GO_GALAXY_ROLES_PATH", "ANSIBLE_ROLES_PATH", "GO_GALAXY_CACHE_DIR", "ANSIBLE_GALAXY_CACHE_DIR",
	}
}

// ansiblePathsRow is one ansible.cfg's three path values and the Config paths
// they must resolve to.
type ansiblePathsRow struct {
	name                                     string
	collections, roles, cache                string
	wantCollections, wantRoles, wantCacheDir string
}

// TestAnsibleConfigPathsResolveUnderTheFile pins the three ansible.cfg paths
// to ansible's resolution: $VAR, then ~, expanded, and a relative result
// joined under the file's directory. Not parallel: t.Setenv.
func TestAnsibleConfigPathsResolveUnderTheFile(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	conf := filepath.Join(root, "conf")
	cfgPath := filepath.Join(conf, "ansible.cfg")
	t.Setenv("HOME", home)
	t.Setenv("GG_CFG_ROOT", "/opt/root")
	psUnsetEnv(t, "GG_CFG_UNSET")

	rows := []ansiblePathsRow{
		{
			name: "relative values join under the file's directory", collections: "./collections", roles: "roles", cache: ".cache",
			wantCollections: filepath.Join(conf, "collections"), wantRoles: filepath.Join(conf, "roles"),
			wantCacheDir: filepath.Join(conf, ".cache"),
		},
		{
			name: "a parent reference leaves that directory", collections: "../shared/c", roles: "../shared/r", cache: "..",
			wantCollections: filepath.Join(root, "shared", "c"), wantRoles: filepath.Join(root, "shared", "r"), wantCacheDir: root,
		},
		{
			name: "absolute values are kept", collections: "/abs/c", roles: "/abs/r", cache: "/abs/cache",
			wantCollections: "/abs/c", wantRoles: "/abs/r", wantCacheDir: "/abs/cache",
		},
		{
			name: "absolute values are normalized lexically", collections: "/abs/link/../c", roles: "/abs//r/", cache: "/abs/./cache",
			wantCollections: "/abs/c", wantRoles: "/abs/r", wantCacheDir: "/abs/cache",
		},
		{
			name: "variables expand", collections: "$GG_CFG_ROOT/c", roles: "${GG_CFG_ROOT}/r", cache: "$GG_CFG_ROOT",
			wantCollections: "/opt/root/c", wantRoles: "/opt/root/r", wantCacheDir: "/opt/root",
		},
		{
			name: "an unset variable stays as written", collections: "$GG_CFG_UNSET/c", roles: "r/$GG_CFG_UNSET", cache: "${GG_CFG_UNSET}",
			wantCollections: filepath.Join(conf, "$GG_CFG_UNSET", "c"), wantRoles: filepath.Join(conf, "r", "$GG_CFG_UNSET"),
			wantCacheDir: filepath.Join(conf, "${GG_CFG_UNSET}"),
		},
		{
			name: "~ expands", collections: "~/c", roles: "~/r", cache: "~",
			wantCollections: filepath.Join(home, "c"), wantRoles: filepath.Join(home, "r"), wantCacheDir: home,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ansCfg := ansibleConfig{
				Defaults: ansibleDefaultsConfig{CollectionsPath: row.collections, RolesPath: row.roles},
				Galaxy:   ansibleGalaxyConfig{CacheDir: row.cache},
			}
			got := &Config{}
			applyAnsibleConfig(got, newApplyAnsibleConfigCmd(t, nil), ansCfg, cfgPath)
			assertConfigPaths(t, got, row.wantCollections, row.wantRoles, row.wantCacheDir)
			if !got.AnsibleCollectionsPathUsed || !got.AnsibleRolesPathUsed || !got.AnsibleCacheDirUsed {
				t.Errorf("ansible.cfg credit = %t, %t, %t, want all true",
					got.AnsibleCollectionsPathUsed, got.AnsibleRolesPathUsed, got.AnsibleCacheDirUsed)
			}
		})
	}
}

// assertConfigPaths checks the three install and cache paths of got at once,
// so one wrong mapping does not hide another.
func assertConfigPaths(t *testing.T, got *Config, wantCollections, wantRoles, wantCacheDir string) {
	t.Helper()
	if got.DownloadPath != wantCollections {
		t.Errorf("DownloadPath = %q, want %q", got.DownloadPath, wantCollections)
	}
	if got.RolesPath != wantRoles {
		t.Errorf("RolesPath = %q, want %q", got.RolesPath, wantRoles)
	}
	if got.CacheDir != wantCacheDir {
		t.Errorf("CacheDir = %q, want %q", got.CacheDir, wantCacheDir)
	}
}

// TestAnsibleConfigPathEdges pins what the joining leaves alone: an empty first
// entry stays empty for the refusal downstream, a flag stays literal, and a
// relative ansible.cfg path still yields a physical base. Not parallel: t.Chdir.
func TestAnsibleConfigPathEdges(t *testing.T) {
	t.Run("an empty first entry stays empty and the rest is warned about", func(t *testing.T) {
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: ":x", RolesPath: ":y"}}
		got := &Config{}
		applyAnsibleConfig(got, newApplyAnsibleConfigCmd(t, nil), ansCfg, filepath.Join(t.TempDir(), "ansible.cfg"))
		if got.DownloadPath != "" || got.RolesPath != "" {
			t.Fatalf("DownloadPath, RolesPath = %q, %q, want both empty", got.DownloadPath, got.RolesPath)
		}
		assertWarningMentions(t, got, "[x]")
	})

	t.Run("a flag value is taken as written", func(t *testing.T) {
		t.Setenv("GG_CFG_ROOT", "/opt/root")
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "c"}}
		c := newApplyAnsibleConfigCmd(t, []string{"--download-path=$GG_CFG_ROOT/~", "--cache-dir=~/cache"})
		got := &Config{}
		applyAnsibleConfig(got, c, ansCfg, filepath.Join(t.TempDir(), "ansible.cfg"))
		if got.DownloadPath != "$GG_CFG_ROOT/~" || got.CacheDir != "~/cache" {
			t.Fatalf("DownloadPath, CacheDir = %q, %q, want both as written", got.DownloadPath, got.CacheDir)
		}
	})

	t.Run("a relative ansible.cfg path joins under its absolute directory", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "c", RolesPath: "r"}}
		got := &Config{}
		applyAnsibleConfig(got, newApplyAnsibleConfigCmd(t, nil), ansCfg, filepath.Join("conf", "ansible.cfg"))
		conf := filepath.Join(physicalDir(t, root), "conf")
		assertConfigPaths(t, got, filepath.Join(conf, "c"), filepath.Join(conf, "r"), testDefaultCacheDir)
		if got.AnsibleConfigPath != filepath.Join("conf", "ansible.cfg") {
			t.Errorf("AnsibleConfigPath = %q, want the path as found", got.AnsibleConfigPath)
		}
	})

	t.Run("a symlinked ansible.cfg resolves from the link's directory", subtestSymlinkedAnsibleConfig)
	t.Run("the overlap warning compares absolute forms", subtestOverlapComparesAbsoluteForms)
}

// subtestSymlinkedAnsibleConfig pins that a relative value in a symlinked
// ansible.cfg joins under the link's directory, not its target's, as ansible's
// unfrackpath(follow=False) resolves it.
func subtestSymlinkedAnsibleConfig(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target", "ansible.cfg")
	link := filepath.Join(root, "link", "ansible.cfg")
	for _, dir := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("os.Mkdir(%q) error = %v, want nil", dir, err)
		}
	}
	writeAnsibleCfg(t, target, "https://link.example")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("os.Symlink() error = %v, want nil", err)
	}
	ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "./c"}}
	got := &Config{}
	applyAnsibleConfig(got, newApplyAnsibleConfigCmd(t, nil), ansCfg, link)
	if want := filepath.Join(root, "link", "c"); got.DownloadPath != want {
		t.Fatalf("DownloadPath = %q, want %q", got.DownloadPath, want)
	}
}

// subtestOverlapComparesAbsoluteForms pins that a roles_path joined under the
// file's directory and a relative --download-path naming the same directory
// still draw the shared-directory warning.
func subtestOverlapComparesAbsoluteForms(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{RolesPath: "shared"}}
	c := newApplyAnsibleConfigCmd(t, []string{"--download-path=conf/shared"})
	got := &Config{}
	applyAnsibleConfig(got, c, ansCfg, filepath.Join(root, "conf", "ansible.cfg"))
	assertRoleWarningMentions(t, got, "roles_path and collections_path are the same directory")
}

// runPathFlagsCommand runs the tree main.go builds, cliflags.CommonFlags on the
// root and cliflags.CollectionFlags on install, with args, and returns the
// install command its action captured.
func runPathFlagsCommand(t *testing.T, args []string) *cli.Command {
	t.Helper()
	var captured *cli.Command
	app := &cli.Command{
		Name:  "go-galaxy",
		Flags: cliflags.CommonFlags(),
		Commands: []*cli.Command{{
			Name:  "install",
			Flags: cliflags.CollectionFlags(),
			Action: func(_ context.Context, c *cli.Command) error {
				captured = c
				return nil
			},
		}},
	}
	if err := app.Run(context.Background(), append([]string{"go-galaxy", "install"}, args...)); err != nil {
		t.Fatalf("app.Run() error = %v, want nil", err)
	}
	return captured
}

// pathEnvFixture is an ansible.cfg holding all three paths, read through the
// real flag set, and the home directory ~ expands to.
type pathEnvFixture struct {
	home    string
	cfgPath string
	file    ansibleConfig
}

// newPathEnvFixture builds a pathEnvFixture in fresh temporary directories.
func newPathEnvFixture(t *testing.T) pathEnvFixture {
	t.Helper()
	return pathEnvFixture{
		file: ansibleConfig{
			Defaults: ansibleDefaultsConfig{CollectionsPath: "file-c", RolesPath: "file-r"},
			Galaxy:   ansibleGalaxyConfig{CacheDir: "file-cache"},
		},
		home:    t.TempDir(),
		cfgPath: filepath.Join(t.TempDir(), "ansible.cfg"),
	}
}

// setup clears every path variable, points HOME at the fixture's home and
// exports GG_ENV_NAME=leaf, leaving GG_ENV_UNSET unset.
func (f pathEnvFixture) setup(t *testing.T) {
	t.Helper()
	psUnsetEnv(t, append(pathEnvKeys(), "GG_ENV_UNSET")...)
	t.Setenv("HOME", f.home)
	t.Setenv("GG_ENV_NAME", "leaf")
}

// build applies the fixture's ansible.cfg, then project, over install's flags
// parsed from args, in BuildCollectionConfig's order.
func (f pathEnvFixture) build(t *testing.T, project projectSettings, args ...string) *Config {
	t.Helper()
	c := runPathFlagsCommand(t, args)
	got := &Config{}
	applyAnsibleConfig(got, c, f.file, f.cfgPath)
	applyProjectSettings(got, c, project)
	return got
}

// TestAnsiblePathVariablesExpandAgainstTheWorkingDirectory pins the ANSIBLE_*
// path variables to ansible's reading, expanded and relative to the working
// directory, while a flag and GO_GALAXY_* stay literal. Not parallel: t.Setenv.
func TestAnsiblePathVariablesExpandAgainstTheWorkingDirectory(t *testing.T) {
	f := newPathEnvFixture(t)

	t.Run("the ansible spellings expand and outrank the file", func(t *testing.T) {
		f.setup(t)
		t.Setenv("ANSIBLE_COLLECTIONS_PATH", "envc/$GG_ENV_NAME")
		t.Setenv("ANSIBLE_ROLES_PATH", "~/envr")
		t.Setenv("ANSIBLE_GALAXY_CACHE_DIR", "$GG_ENV_UNSET/ec")
		got := f.build(t, projectSettings{})
		assertConfigPaths(t, got, "envc/leaf", filepath.Join(f.home, "envr"), "$GG_ENV_UNSET/ec")
		if got.AnsibleCollectionsPathUsed || got.AnsibleRolesPathUsed || got.AnsibleCacheDirUsed {
			t.Errorf("ansible.cfg credited for a value its variable supplied")
		}
	})

	t.Run("the go-galaxy spellings stay literal and outrank the ansible ones", func(t *testing.T) {
		f.setup(t)
		t.Setenv("GO_GALAXY_COLLECTIONS_PATH", "~/lit")
		t.Setenv("ANSIBLE_COLLECTIONS_PATH", "envc")
		t.Setenv("GO_GALAXY_ROLES_PATH", "$GG_ENV_NAME")
		t.Setenv("GO_GALAXY_CACHE_DIR", "~")
		assertConfigPaths(t, f.build(t, projectSettings{}), "~/lit", "$GG_ENV_NAME", "~")
	})

	t.Run("a flag stays literal and outranks every variable", func(t *testing.T) {
		f.setup(t)
		t.Setenv("ANSIBLE_COLLECTIONS_PATH", "envc")
		got := f.build(t, projectSettings{}, "--download-path=~/flag", "--roles-path=$GG_ENV_NAME", "--cache-dir=~")
		assertConfigPaths(t, got, "~/flag", "$GG_ENV_NAME", "~")
	})

	t.Run("an exported-empty ansible spelling still hides the file", func(t *testing.T) {
		f.setup(t)
		t.Setenv("ANSIBLE_COLLECTIONS_PATH", "")
		if got := f.build(t, projectSettings{}); got.DownloadPath != "" || got.AnsibleCollectionsPathUsed {
			t.Fatalf("DownloadPath = %q (ansible.cfg used %t), want empty from the variable",
				got.DownloadPath, got.AnsibleCollectionsPathUsed)
		}
	})
}

// TestAnsibleCacheDirKeepsItsPlaceAroundGalaxyToml pins that galaxy.toml's
// cache_dir still sits between ANSIBLE_GALAXY_CACHE_DIR and a relative
// [galaxy] cache_dir, now that both are resolved. Not parallel: t.Setenv.
func TestAnsibleCacheDirKeepsItsPlaceAroundGalaxyToml(t *testing.T) {
	f := newPathEnvFixture(t)
	project := projectSettings{CacheDir: "/project/cache"}

	t.Run("ANSIBLE_GALAXY_CACHE_DIR outranks galaxy.toml", func(t *testing.T) {
		f.setup(t)
		t.Setenv("ANSIBLE_GALAXY_CACHE_DIR", "~/ec")
		got := f.build(t, project)
		if want := filepath.Join(f.home, "ec"); got.CacheDir != want || len(got.ProjectSettingsUsed) != 0 {
			t.Fatalf("CacheDir = %q (galaxy.toml keys %v), want %q", got.CacheDir, got.ProjectSettingsUsed, want)
		}
	})

	t.Run("galaxy.toml outranks a relative [galaxy] cache_dir", func(t *testing.T) {
		f.setup(t)
		got := f.build(t, project)
		if got.CacheDir != "/project/cache" || got.AnsibleCacheDirUsed {
			t.Fatalf("CacheDir = %q (ansible.cfg used %t), want galaxy.toml's", got.CacheDir, got.AnsibleCacheDirUsed)
		}
	})
}

// ansibleConfigEnvFixture is a tree of ansible.cfg files for $ANSIBLE_CONFIG
// to name: in home, in repo above the working directory deploy, beside root,
// and under a directory literally named $GG_CFG_UNSET.
type ansibleConfigEnvFixture struct {
	root, home, repo, deploy string
}

// newAnsibleConfigEnvFixture writes that tree, points HOME and GG_CFG_DIR at
// it, clears GG_CFG_UNSET and GO_GALAXY_ANSIBLE_CONFIG, and enters deploy.
func newAnsibleConfigEnvFixture(t *testing.T) ansibleConfigEnvFixture {
	t.Helper()
	root := physicalDir(t, t.TempDir())
	f := ansibleConfigEnvFixture{
		root: root, home: filepath.Join(root, "home"), repo: filepath.Join(root, "repo"),
		deploy: filepath.Join(root, "repo", "deploy"),
	}
	for _, dir := range []string{f.home, f.deploy, filepath.Join(root, "empty"), filepath.Join(f.deploy, "$GG_CFG_UNSET")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("os.MkdirAll(%q) error = %v, want nil", dir, err)
		}
	}
	writeAnsibleCfg(t, filepath.Join(f.home, "my.cfg"), "https://tilde.example")
	writeAnsibleCfg(t, filepath.Join(f.repo, "ansible.cfg"), "https://repo.example")
	writeAnsibleCfg(t, filepath.Join(root, "env.cfg"), "https://var.example")
	writeAnsibleCfg(t, filepath.Join(f.deploy, "$GG_CFG_UNSET", "x.cfg"), "https://literal.example")
	t.Setenv("HOME", f.home)
	t.Setenv("GG_CFG_DIR", root)
	psUnsetEnv(t, "GG_CFG_UNSET", "GO_GALAXY_ANSIBLE_CONFIG")
	t.Chdir(f.deploy)
	return f
}

// TestAnsibleConfigEnvResolvesAsAnsible pins $ANSIBLE_CONFIG to ansible's
// reading: $VAR and ~ expanded, made absolute, a directory standing for its
// ansible.cfg, and one without it passed over. Not parallel: t.Setenv.
func TestAnsibleConfigEnvResolvesAsAnsible(t *testing.T) {
	f := newAnsibleConfigEnvFixture(t)
	rows := []struct{ name, env, wantPath, wantServer string }{
		{name: "~ expands", env: "~/my.cfg", wantPath: filepath.Join(f.home, "my.cfg"), wantServer: "https://tilde.example"},
		{name: "a variable expands", env: "$GG_CFG_DIR/env.cfg", wantPath: filepath.Join(f.root, "env.cfg"), wantServer: "https://var.example"},
		{
			name: "a relative file is made absolute", env: "../ansible.cfg",
			wantPath: filepath.Join(f.repo, "ansible.cfg"), wantServer: "https://repo.example",
		},
		{
			name: "a directory stands for its ansible.cfg", env: "..",
			wantPath: filepath.Join(f.repo, "ansible.cfg"), wantServer: "https://repo.example",
		},
		{
			name: "an unset variable stays as written", env: "$GG_CFG_UNSET/x.cfg",
			wantPath: filepath.Join(f.deploy, "$GG_CFG_UNSET", "x.cfg"), wantServer: "https://literal.example",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("ANSIBLE_CONFIG", row.env)
			cfg, gotPath, _, err := loadAnsibleConfigFromCLI(newAnsibleConfigCmd(t, nil))
			assertAnsibleConfigLoaded(t, cfg, gotPath, err, row.wantPath, row.wantServer)
		})
	}

	t.Run("a directory without an ansible.cfg falls through", func(t *testing.T) {
		t.Setenv("ANSIBLE_CONFIG", filepath.Join(f.root, "empty"))
		_, gotPath, _, err := loadAnsibleConfigFromCLI(newAnsibleConfigCmd(t, nil))
		assertDiscoveryFallsThroughCleanly(t, gotPath, err)
	})
}

// TestExplicitAnsibleConfigStaysLiteral pins that --ansible-config and
// GO_GALAXY_ANSIBLE_CONFIG, go-galaxy's own spellings, expand nothing: a path
// that exists once expanded is still not found. Not parallel: t.Setenv.
func TestExplicitAnsibleConfigStaysLiteral(t *testing.T) {
	newAnsibleConfigEnvFixture(t)

	t.Run("--ansible-config", func(t *testing.T) {
		_, _, _, err := loadAnsibleConfigFromCLI(newAnsibleConfigCmd(t, []string{"--ansible-config=~/my.cfg"}))
		if !errors.Is(err, helpers.ErrAnsibleConfigNotFound) {
			t.Fatalf("error = %v, want helpers.ErrAnsibleConfigNotFound for the unexpanded path", err)
		}
	})

	t.Run("GO_GALAXY_ANSIBLE_CONFIG", func(t *testing.T) {
		t.Setenv("GO_GALAXY_ANSIBLE_CONFIG", "$GG_CFG_DIR/env.cfg")
		_, _, _, err := loadAnsibleConfigFromCLI(newAnsibleConfigCmd(t, nil))
		if !errors.Is(err, helpers.ErrAnsibleConfigNotFound) {
			t.Fatalf("error = %v, want helpers.ErrAnsibleConfigNotFound for the unexpanded path", err)
		}
	})
}

// physicalDir is dir with its symlinks resolved, the spelling Python's
// os.getcwd() reports for it; t.TempDir sits under a symlink on macOS.
func physicalDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(%q) error = %v, want nil", dir, err)
	}
	return resolved
}

// symlinkedCwdFixture is a working directory entered through a symlink:
// repo/deploy links to real/x, and real and repo each hold an ansible.cfg.
type symlinkedCwdFixture struct {
	root string
}

// newSymlinkedCwdFixture writes that tree, clears the path and ansible.cfg
// variables, and enters repo/deploy by its link, as a shell's cd leaves $PWD.
func newSymlinkedCwdFixture(t *testing.T) symlinkedCwdFixture {
	t.Helper()
	f := symlinkedCwdFixture{root: physicalDir(t, t.TempDir())}
	for _, dir := range []string{filepath.Join(f.root, "real", "x"), filepath.Join(f.root, "repo")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("os.MkdirAll(%q) error = %v, want nil", dir, err)
		}
	}
	for _, name := range []string{"real", "repo"} {
		body := "[galaxy]\nserver = https://" + name + ".example\n\n[defaults]\ncollections_path = ./" + name + "-c\n"
		if err := os.WriteFile(filepath.Join(f.root, name, "ansible.cfg"), []byte(body), 0o600); err != nil {
			t.Fatalf("os.WriteFile() error = %v, want nil", err)
		}
	}
	link := filepath.Join(f.root, "repo", "deploy")
	if err := os.Symlink(filepath.Join(f.root, "real", "x"), link); err != nil {
		t.Fatalf("os.Symlink() error = %v, want nil", err)
	}
	psUnsetEnv(t, append(pathEnvKeys(), "GO_GALAXY_ANSIBLE_CONFIG", "ANSIBLE_CONFIG", "ANSIBLE_GALAXY_SERVER")...)
	t.Chdir(link)
	return f
}

// loadAndApply loads the ansible.cfg c selects and applies it, returning the
// Config and the path it was read from.
func loadAndApply(t *testing.T, c *cli.Command) (*Config, string) {
	t.Helper()
	ansCfg, gotPath, _, err := loadAnsibleConfigFromCLI(c)
	if err != nil {
		t.Fatalf("loadAnsibleConfigFromCLI() error = %v, want nil", err)
	}
	got := &Config{}
	applyAnsibleConfig(got, c, ansCfg, gotPath)
	return got, gotPath
}

// TestAnsibleConfigFromASymlinkedWorkingDirectory pins that a relative path
// climbs from the physical working directory, as Python's os.getcwd() and the
// kernel do, not from a symlinked $PWD. Not parallel: t.Setenv, t.Chdir.
func TestAnsibleConfigFromASymlinkedWorkingDirectory(t *testing.T) {
	t.Run("$ANSIBLE_CONFIG=../ansible.cfg reads the physical parent's file", func(t *testing.T) {
		f := newSymlinkedCwdFixture(t)
		t.Setenv("ANSIBLE_CONFIG", "../ansible.cfg")
		got, gotPath := loadAndApply(t, newAnsibleConfigCmd(t, nil))
		if want := filepath.Join(f.root, "real", "ansible.cfg"); gotPath != want || got.Server != "https://real.example" {
			t.Fatalf("path, Server = %q, %q, want %q, https://real.example", gotPath, got.Server, want)
		}
		if want := filepath.Join(f.root, "real", "real-c"); got.DownloadPath != want {
			t.Fatalf("DownloadPath = %q, want %q", got.DownloadPath, want)
		}
	})

	t.Run("--ansible-config ../ansible.cfg joins under the file it read", func(t *testing.T) {
		f := newSymlinkedCwdFixture(t)
		got, _ := loadAndApply(t, newAnsibleConfigCmd(t, []string{"--ansible-config=../ansible.cfg"}))
		if want := filepath.Join(f.root, "real", "real-c"); got.Server != "https://real.example" || got.DownloadPath != want {
			t.Fatalf("Server, DownloadPath = %q, %q, want https://real.example, %q", got.Server, got.DownloadPath, want)
		}
	})

	t.Run("a '..' in ./ansible.cfg climbs from the physical directory", func(t *testing.T) {
		f := newSymlinkedCwdFixture(t)
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{CollectionsPath: "../c"}}
		got := &Config{}
		applyAnsibleConfig(got, newApplyAnsibleConfigCmd(t, nil), ansCfg, cwdAnsibleCfgName)
		if want := filepath.Join(f.root, "real", "c"); got.DownloadPath != want {
			t.Fatalf("DownloadPath = %q, want %q", got.DownloadPath, want)
		}
	})

	t.Run("the overlap warning sees one directory through the link", func(t *testing.T) {
		f := newSymlinkedCwdFixture(t)
		ansCfg := ansibleConfig{Defaults: ansibleDefaultsConfig{RolesPath: "shared"}}
		c := newApplyAnsibleConfigCmd(t, []string{"--download-path=" + filepath.Join(f.root, "repo", "deploy", "shared")})
		got := &Config{}
		applyAnsibleConfig(got, c, ansCfg, cwdAnsibleCfgName)
		assertRoleWarningMentions(t, got, "roles_path and collections_path are the same directory")
	})
}

// TestAnsibleConfigEmptyAfterExpansionIsSkipped pins that an $ANSIBLE_CONFIG
// expanding to "" is skipped like an empty one, so a world-writable cwd's
// ./ansible.cfg is still declined. Not parallel: t.Setenv, t.Chdir.
func TestAnsibleConfigEmptyAfterExpansionIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeAnsibleCfg(t, filepath.Join(dir, "ansible.cfg"), "https://cwd.example")
	chmodDir(t, dir, 0o777)
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GG_CFG_EMPTY", "")
	psUnsetEnv(t, "GO_GALAXY_ANSIBLE_CONFIG")

	for _, env := range []string{"$GG_CFG_EMPTY", "${GG_CFG_EMPTY}"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("ANSIBLE_CONFIG", env)
			cfg, gotPath, warnings, err := loadAnsibleConfigFromCLI(newAnsibleConfigCmd(t, nil))
			assertDiscoveryFallsThroughCleanly(t, gotPath, err)
			if cfg.Galaxy.Server == "https://cwd.example" {
				t.Fatalf("path = %q, want the world-writable cwd's ansible.cfg declined", gotPath)
			}
			if !warningMentions(warnings, "world-writable", dir) {
				t.Fatalf("warnings = %v, want one naming %q as world-writable", warnings, dir)
			}
		})
	}
}

// TestAnsiblePathVariableHoldingAColonIsSplit pins where ":" in an expanded
// value differs by source: ansible.cfg splits before expanding, as ansible
// does, while ANSIBLE_COLLECTIONS_PATH is split after. Not parallel: t.Setenv.
func TestAnsiblePathVariableHoldingAColonIsSplit(t *testing.T) {
	f := newPathEnvFixture(t)
	f.file.Defaults.CollectionsPath = "$GG_LIST"

	t.Run("ansible.cfg keeps the value as one path", func(t *testing.T) {
		f.setup(t)
		t.Setenv("GG_LIST", "/opt/a:/opt/b")
		if got := f.build(t, projectSettings{}); got.DownloadPath != "/opt/a:/opt/b" || len(got.Warnings) != 0 {
			t.Fatalf("DownloadPath = %q (warnings %v), want one path, no warning", got.DownloadPath, got.Warnings)
		}
	})

	t.Run("ANSIBLE_COLLECTIONS_PATH is split at that colon", func(t *testing.T) {
		f.setup(t)
		t.Setenv("GG_LIST", "/opt/a:/opt/b")
		t.Setenv("ANSIBLE_COLLECTIONS_PATH", "$GG_LIST")
		got := f.build(t, projectSettings{})
		if got.DownloadPath != "/opt/a" {
			t.Fatalf("DownloadPath = %q, want %q", got.DownloadPath, "/opt/a")
		}
		assertWarningMentions(t, got, "[/opt/b]")
	})
}

// TestAnsibleConfigFlagDotDotIsResolvedAsText pins that an absolute
// --ansible-config whose ".." follows a symlink reads the file its directory
// names, not the kernel's. Not parallel: t.Setenv, t.Chdir.
func TestAnsibleConfigFlagDotDotIsResolvedAsText(t *testing.T) {
	f := newSymlinkedCwdFixture(t)
	flag := "--ansible-config=" + filepath.Join(f.root, "repo", "deploy") + string(filepath.Separator) + filepath.Join("..", "ansible.cfg")
	got, gotPath := loadAndApply(t, newAnsibleConfigCmd(t, []string{flag}))
	if want := filepath.Join(f.root, "repo", "ansible.cfg"); gotPath != want || got.Server != "https://repo.example" {
		t.Fatalf("path, Server = %q, %q, want %q, https://repo.example", gotPath, got.Server, want)
	}
	if want := filepath.Join(f.root, "repo", "repo-c"); got.DownloadPath != want {
		t.Fatalf("DownloadPath = %q, want %q", got.DownloadPath, want)
	}
}
