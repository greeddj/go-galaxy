package commands

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

// These tests drive the [tool.go-galaxy] table of a real galaxy.toml through
// the commands' own flag sets: install's precedence chain per key, cleanup's
// discovery, and the lock_file that hash, tree and explain read.

// projTOMLHead opens every galaxy.toml projWriteTOML writes: an empty project,
// so the [tool.go-galaxy] body under it is all a row differs in.
const projTOMLHead = "[project]\ncollections = []\n\n[tool.go-galaxy]\n"

// projFlagEnvKeys returns the variables that make a flag count as set and so
// outrank galaxy.toml, which every row here must start without.
func projFlagEnvKeys() []string {
	return []string{
		"GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE",
		"GO_GALAXY_CACHE_DIR", "ANSIBLE_GALAXY_CACHE_DIR", "GO_GALAXY_LOCK_FILE", "GO_GALAXY_METRICS_FILE",
		"GO_GALAXY_WORKERS", "GO_GALAXY_DOWNLOAD_WORKERS", "GO_GALAXY_OFFLINE",
		"GO_GALAXY_S3_BUCKET", "GO_GALAXY_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID",
		"GO_GALAXY_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY",
		"GO_GALAXY_SERVER", "GO_GALAXY_TOKEN", "ANSIBLE_GALAXY_SERVER", "ANSIBLE_GALAXY_SERVER_LIST",
	}
}

// projUnsetEnv unsets each key for the test through t.Setenv, which restores
// it on cleanup; an exported-empty value counts as set, so Setenv("") alone
// would not do.
func projUnsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

// projSetup moves into a fresh working directory, with ansible.cfg discovery
// neutralized or finding ansibleCfg as ./ansible.cfg when it is non-empty,
// and clears every variable that would outrank galaxy.toml.
func projSetup(t *testing.T, ansibleCfg string) {
	t.Helper()
	if ansibleCfg == "" {
		neutralizeAnsibleDiscovery(t)
	} else {
		writeCWDAnsibleConfig(t, ansibleCfg)
	}
	projUnsetEnv(t, projFlagEnvKeys()...)
}

// projWriteTOML writes a galaxy.toml at path, parents created, holding an
// empty [project] and tool as the [tool.go-galaxy] body.
func projWriteTOML(t *testing.T, path, tool string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	writeTestFile(t, path, []byte(projTOMLHead+tool))
}

// projInstallFlags is install's flag set with S3: the widest surface a
// galaxy.toml key can be outranked on.
func projInstallFlags() []cli.Flag {
	return append(cliflags.CollectionFlags(), cliflags.S3Flags()...)
}

// projBuild builds the install config over projInstallFlags, failing the test
// on an error.
func projBuild(t *testing.T, args ...string) *config.Config {
	t.Helper()
	cfg, err := buildConfigFor(t, "install", projInstallFlags(), args)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	return cfg
}

// projWarning returns the queued warning containing substr, or "" when none,
// so a row asserts on one line rather than on a slice position.
func projWarning(cfg *config.Config, substr string) string {
	for _, w := range cfg.Warnings {
		if strings.Contains(w, substr) {
			return w
		}
	}
	return ""
}

// projAssertUsed fails unless ProjectSettingsUsed is exactly want, in order,
// since the order is what the debug line prints.
func projAssertUsed(t *testing.T, cfg *config.Config, want ...string) {
	t.Helper()
	if !slices.Equal(cfg.ProjectSettingsUsed, want) {
		t.Errorf("ProjectSettingsUsed = %v, want %v", cfg.ProjectSettingsUsed, want)
	}
}

// projAssertNoPlaintext fails when secret appears in any rendering of cfg a
// log line, a crash dump or a report could carry: the three fmt verbs, JSON,
// and the queued warnings.
func projAssertNoPlaintext(t *testing.T, cfg *config.Config, secret string) {
	t.Helper()
	encoded, err := json.Marshal(cfg) //nolint:musttag // Config is a runtime type, not a serialization contract.
	if err != nil {
		t.Fatalf("json.Marshal(cfg): %v", err)
	}
	dumps := map[string]string{
		"%v": fmt.Sprintf("%v", cfg), "%+v": fmt.Sprintf("%+v", cfg), "%#v": fmt.Sprintf("%#v", cfg),
		"json.Marshal": string(encoded), "Warnings": strings.Join(cfg.Warnings, "\n"),
	}
	for name, dump := range dumps {
		if strings.Contains(dump, secret) {
			t.Errorf("%s of the Config carries the plaintext secret", name)
		}
	}
}

// projAssertUsageError fails unless err carries sentinel, reads msg somewhere
// in its text and exits as the usage error every galaxy.toml refusal is.
func projAssertUsageError(t *testing.T, err, sentinel error, msg string) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want errors.Is %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), msg) {
		t.Errorf("error = %q, want it to read %q", err.Error(), msg)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitUsage {
		t.Errorf("exitcode.FromError(err) = %d, want ExitUsage (%d)", got, exitcode.ExitUsage)
	}
}

// projCacheDirRow is one link of the cache_dir chain: the ansible.cfg planted
// in the working directory, the toml body and where it is written, the flag
// or variable set beside it, and what CacheDir must resolve to.
type projCacheDirRow struct {
	env         map[string]string
	name        string
	ansibleCfg  string
	tool        string
	tomlPath    string
	want        string
	args        []string
	wantUsed    bool
	wantAnsible bool
}

// projCacheDirRows returns the chain: ansible.cfg alone as the control, then
// toml over it, the flag and the variable over toml, and a relative path
// resolved under the file's own directory, discovered or named with -r.
func projCacheDirRows(tomlDir, flagDir string) []projCacheDirRow {
	const ansibleCfg = "[galaxy]\ncache_dir = /from/ansible\n"
	const relative = "cache_dir = \"cache/local\"\n"
	tool := fmt.Sprintf("cache_dir = %q\n", tomlDir)
	return []projCacheDirRow{
		{name: "[galaxy] cache_dir alone is read", ansibleCfg: ansibleCfg, want: "/from/ansible", wantAnsible: true},
		{name: "cache_dir outranks [galaxy] cache_dir", ansibleCfg: ansibleCfg, tool: tool, want: tomlDir, wantUsed: true},
		{name: "--cache-dir outranks cache_dir", tool: tool, args: []string{"--cache-dir=" + flagDir}, want: flagDir},
		{name: "GO_GALAXY_CACHE_DIR outranks cache_dir", tool: tool, env: map[string]string{"GO_GALAXY_CACHE_DIR": flagDir},
			want: flagDir},
		{name: "a relative cache_dir resolves under the discovered file's directory", tool: relative,
			want: filepath.Join("cache", "local"), wantUsed: true},
		{name: "a relative cache_dir resolves under the directory -r names", tool: relative,
			tomlPath: "sub/dir/galaxy.toml", args: []string{"-r", "sub/dir/galaxy.toml"},
			want: filepath.Join("sub", "dir", "cache", "local"), wantUsed: true},
	}
}

// TestProjectCacheDirPrecedence pins the cache_dir chain through install's
// real flag set: flag or variable, then galaxy.toml, then [galaxy] cache_dir,
// with the file's credit withdrawn once the project file wins.
func TestProjectCacheDirPrecedence(t *testing.T) {
	tomlDir := filepath.Join(t.TempDir(), "from-toml")
	flagDir := filepath.Join(t.TempDir(), "from-flag")
	for _, row := range projCacheDirRows(tomlDir, flagDir) {
		t.Run(row.name, func(t *testing.T) {
			projSetup(t, row.ansibleCfg)
			for key, value := range row.env {
				t.Setenv(key, value)
			}
			projWriteTOML(t, cmp.Or(row.tomlPath, helpers.RequirementsTOMLName), row.tool)

			cfg := projBuild(t, row.args...)
			assertConfigField(t, "CacheDir", cfg.CacheDir, row.want)
			assertConfigField(t, "AnsibleCacheDirUsed", cfg.AnsibleCacheDirUsed, row.wantAnsible)
			assertConfigField(t, "cache_dir credited to the file", slices.Contains(cfg.ProjectSettingsUsed, "cache_dir"), row.wantUsed)
		})
	}
}

// projPathRow is one row of TestProjectLockAndMetricsFiles: the toml body and
// where it is written, the flags set beside it, and the two paths plus the
// keys credited to the file that the Config must carry.
type projPathRow struct {
	name        string
	tool        string
	tomlPath    string
	wantLock    string
	wantMetrics string
	args        []string
	wantUsed    []string
}

func projPathRows() []projPathRow {
	const tool = "lock_file = \"locks/galaxy.lock\"\nmetrics_file = \"out/metrics.json\"\n"
	return []projPathRow{
		{name: "relative paths resolve under the discovered file's directory", tool: tool,
			wantLock: filepath.Join("locks", "galaxy.lock"), wantMetrics: filepath.Join("out", "metrics.json"),
			wantUsed: []string{"lock_file", "metrics_file"}},
		{name: "relative paths resolve under the directory -r names", tool: tool, tomlPath: "sub/dir/galaxy.toml",
			args:     []string{"-r", "sub/dir/galaxy.toml"},
			wantLock: filepath.Join("sub", "dir", "locks", "galaxy.lock"), wantMetrics: filepath.Join("sub", "dir", "out", "metrics.json"),
			wantUsed: []string{"lock_file", "metrics_file"}},
		{name: "an absolute path is kept", tool: "lock_file = \"/abs/galaxy.lock\"\n", wantLock: "/abs/galaxy.lock",
			wantUsed: []string{"lock_file"}},
		{name: "--lock-file outranks lock_file", tool: tool, args: []string{"--lock-file=/flag/galaxy.lock"},
			wantLock: "/flag/galaxy.lock", wantMetrics: filepath.Join("out", "metrics.json"), wantUsed: []string{"metrics_file"}},
	}
}

// TestProjectLockAndMetricsFiles pins lock_file and metrics_file through
// install's flag set: resolved against the file's own directory, kept when
// absolute, and outranked by the flag.
func TestProjectLockAndMetricsFiles(t *testing.T) {
	for _, row := range projPathRows() {
		t.Run(row.name, func(t *testing.T) {
			projSetup(t, "")
			projWriteTOML(t, cmp.Or(row.tomlPath, helpers.RequirementsTOMLName), row.tool)

			cfg := projBuild(t, row.args...)
			assertConfigField(t, "LockFile", cfg.LockFile, row.wantLock)
			assertConfigField(t, "MetricsFile", cfg.MetricsFile, row.wantMetrics)
			projAssertUsed(t, cfg, row.wantUsed...)
		})
	}
}

// projWorkersRow is one row of TestProjectWorkers: the toml body, the flag set
// beside it, the two pool sizes the Config must carry, the keys credited to
// the file, and the warning expected, or "" for none.
type projWorkersRow struct {
	name         string
	tool         string
	wantWarn     string
	args         []string
	wantUsed     []string
	wantWorkers  int
	wantDownload int
}

// projWorkersRows returns the rows; procs is what the process may use, which
// sizes both defaults and the range the out-of-range warning names.
func projWorkersRows(procs int) []projWorkersRow {
	derived := helpers.DefaultInstallWorkers(procs)
	download := helpers.DefaultDownloadWorkers(procs)
	outOfRange := fmt.Sprintf("[tool.go-galaxy] workers in galaxy.toml = 1000000 is outside 1..%d, the range this machine "+
		"accepts (the ceiling is the CPU this process is permitted to use, at least %d); using %d instead",
		helpers.MaxAcceptedInstallWorkers(procs), helpers.MinDefaultInstallWorkers, derived)
	return []projWorkersRow{
		{name: "workers inside the range is read", tool: "workers = 1\n",
			wantWorkers: 1, wantDownload: download, wantUsed: []string{"workers"}},
		{name: "workers outside the range warns naming the file and reads the default", tool: "workers = 1000000\n",
			wantWorkers: derived, wantDownload: download, wantWarn: outOfRange},
		{name: "--workers outranks workers", tool: "workers = 1000000\n", args: []string{"--workers=1"},
			wantWorkers: 1, wantDownload: download},
		{name: "download_workers is read", tool: "download_workers = 3\n",
			wantWorkers: derived, wantDownload: 3, wantUsed: []string{"download_workers"}},
		{name: "a non-positive download_workers is passed over silently", tool: "download_workers = 0\n",
			wantWorkers: derived, wantDownload: download},
	}
}

// TestProjectWorkers pins workers and download_workers through install's flag
// set: read from the file, range-checked as the flag is with a warning that
// names galaxy.toml, and outranked by the flag.
func TestProjectWorkers(t *testing.T) {
	for _, row := range projWorkersRows(runtime.GOMAXPROCS(0)) {
		t.Run(row.name, func(t *testing.T) {
			projSetup(t, "")
			projWriteTOML(t, helpers.RequirementsTOMLName, row.tool)

			cfg := projBuild(t, row.args...)
			assertConfigField(t, "Workers", cfg.Workers, row.wantWorkers)
			assertConfigField(t, "DownloadWorkers", cfg.DownloadWorkers, row.wantDownload)
			assertConfigField(t, "workers warning", projWarning(cfg, "workers"), row.wantWarn)
			projAssertUsed(t, cfg, row.wantUsed...)
		})
	}
}

// projS3Row is one row of TestProjectS3: the [tool.go-galaxy.s3] body, the
// variables and flags set beside it, and either the refusal expected or the
// secret the Config must reveal and the keys credited to the file.
type projS3Row struct {
	env          map[string]string
	wantErr      error
	name         string
	s3           string
	wantMsg      string
	wantSecret   string
	args         []string
	wantUsed     []string
	pathStyleOff bool
}

// projS3Rows returns the rows around one bucket named in the file. The AWS row
// is satisfied on two channels at once, since the flags read AWS_* too, so the
// file is credited with the bucket alone; the row after it isolates the file.
func projS3Rows() []projS3Row {
	const bucket = "bucket = \"b\"\n"
	const plaintext = "toml-secret-plaintext"
	return []projS3Row{
		{name: "bucket from the file with keys from the environment", s3: bucket + "path_style_disabled = true\n",
			env:        map[string]string{"GO_GALAXY_S3_ACCESS_KEY": "k", "GO_GALAXY_S3_SECRET_KEY": "env-secret-plaintext"},
			wantSecret: "env-secret-plaintext", wantUsed: []string{"s3.bucket", "s3.path_style_disabled"}, pathStyleOff: true},
		{name: "bucket from the file with no keys", s3: bucket, wantErr: helpers.ErrS3EmptyCreds,
			wantMsg: "s3 cache requires access and secret keys when an S3 bucket is configured"},
		{name: "keys through the AWS variables the flags also read",
			s3:         bucket + "access_key = \"${AWS_ACCESS_KEY_ID}\"\nsecret_key = \"${AWS_SECRET_ACCESS_KEY}\"\n",
			env:        map[string]string{"AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": plaintext},
			wantSecret: plaintext, wantUsed: []string{"s3.bucket"}},
		{name: "keys through variables only the file reads",
			s3:         bucket + "access_key = \"${PROJ_S3_KEY_ID}\"\nsecret_key = \"${PROJ_S3_SECRET}\"\n",
			env:        map[string]string{"PROJ_S3_KEY_ID": "k", "PROJ_S3_SECRET": plaintext},
			wantSecret: plaintext, wantUsed: []string{"s3.bucket", "s3.access_key", "s3.secret_key"}},
		{name: "--offline with a bucket and keys from the file", args: []string{"--offline"},
			s3: bucket + "access_key = \"k\"\nsecret_key = \"" + plaintext + "\"\n", wantErr: helpers.ErrS3CacheOffline,
			wantMsg: "--offline cannot be combined with an S3 cache bucket: the S3 cache is reached over the network"},
		{name: "an unset variable in the file", s3: bucket + "secret_key = \"${MISSING}\"\n", wantErr: helpers.ErrProjectFileEnvUnset,
			wantMsg: "galaxy.toml: project file references unset environment variables: MISSING"},
	}
}

// TestProjectS3 pins [tool.go-galaxy.s3] through install's flag set with S3:
// a bucket from the file needs keys from any source, ${VAR} keys expand, the
// secrets are wrapped, and --offline and an unset variable are refused.
func TestProjectS3(t *testing.T) {
	for _, row := range projS3Rows() {
		t.Run(row.name, func(t *testing.T) {
			projSetup(t, "")
			projUnsetEnv(t, "MISSING")
			for key, value := range row.env {
				t.Setenv(key, value)
			}
			projWriteTOML(t, helpers.RequirementsTOMLName, "\n[tool.go-galaxy.s3]\n"+row.s3)

			cfg, err := buildConfigFor(t, "install", projInstallFlags(), row.args)
			if row.wantErr != nil {
				projAssertUsageError(t, err, row.wantErr, row.wantMsg)
				if strings.Contains(err.Error(), "secret-plaintext") {
					t.Errorf("error = %q carries a secret's plaintext", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
			}
			assertConfigField(t, "S3Cache.Enabled", cfg.S3Cache.Enabled, true)
			assertConfigField(t, "S3Cache.Bucket", cfg.S3Cache.Bucket, "b")
			assertConfigField(t, "S3Cache.AccessKey", cfg.S3Cache.AccessKey, "k")
			assertConfigField(t, "S3Cache.SecretKey.Reveal()", cfg.S3Cache.SecretKey.Reveal(), row.wantSecret)
			assertConfigField(t, "S3Cache.PathStyle", cfg.S3Cache.PathStyle, !row.pathStyleOff)
			projAssertUsed(t, cfg, row.wantUsed...)
			projAssertNoPlaintext(t, cfg, row.wantSecret)
		})
	}
}

// projOverlongLine is one byte past what the ansible.cfg reader's scanner
// accepts on a line: the one shape that makes the file unreadable rather
// than merely odd, since its grammar refuses nothing else.
const projOverlongLine = bufio.MaxScanTokenSize + 1

// TestProjectFileNotTOMLIsReportedFirst pins that a galaxy.toml that is not
// TOML fails BuildCollectionConfig before ansible.cfg is read and before
// --timeout is parsed; each control shows the other fault refused on its own.
func TestProjectFileNotTOMLIsReportedFirst(t *testing.T) {
	brokenCfg := "[galaxy]\ncache_dir = " + strings.Repeat("x", projOverlongLine) + "\n"

	t.Run("the planted ansible.cfg is refused on its own", func(t *testing.T) {
		projSetup(t, brokenCfg)
		projWriteTOML(t, helpers.RequirementsTOMLName, "")

		_, err := buildConfigFor(t, "install", projInstallFlags(), nil)
		if !errors.Is(err, helpers.ErrAnsibleConfigUnreadable) {
			t.Fatalf("BuildCollectionConfig() error = %v, want errors.Is helpers.ErrAnsibleConfigUnreadable", err)
		}
	})

	t.Run("a galaxy.toml that is not TOML is reported ahead of it", func(t *testing.T) {
		projSetup(t, brokenCfg)
		writeTestFile(t, helpers.RequirementsTOMLName, []byte(notTOMLContent))

		_, err := buildConfigFor(t, "install", projInstallFlags(), nil)
		projAssertUsageError(t, err, helpers.ErrInvalidRequirementsTOML, "galaxy.toml: requirements file is not valid TOML")
		if errors.Is(err, helpers.ErrAnsibleConfigUnreadable) {
			t.Errorf("error = %v also carries the ansible.cfg failure, want the toml failure alone", err)
		}
	})

	t.Run("--timeout=0 is refused on its own", func(t *testing.T) {
		projSetup(t, "")
		projWriteTOML(t, helpers.RequirementsTOMLName, "")

		_, err := buildConfigFor(t, "install", projInstallFlags(), []string{"--timeout=0"})
		if !errors.Is(err, helpers.ErrInvalidTimeout) {
			t.Fatalf("BuildCollectionConfig() error = %v, want errors.Is helpers.ErrInvalidTimeout", err)
		}
	})

	t.Run("a galaxy.toml that is not TOML is reported ahead of --timeout", func(t *testing.T) {
		projSetup(t, "")
		writeTestFile(t, helpers.RequirementsTOMLName, []byte(notTOMLContent))

		_, err := buildConfigFor(t, "install", projInstallFlags(), []string{"--timeout=0"})
		projAssertUsageError(t, err, helpers.ErrInvalidRequirementsTOML, "galaxy.toml: requirements file is not valid TOML")
		if errors.Is(err, helpers.ErrInvalidTimeout) {
			t.Errorf("error = %v also carries the --timeout failure, want the toml failure alone", err)
		}
	})
}

// projCleanupRow is one row of TestProjectCleanup: the files in the working
// directory, the flags, and the requirements path, cache_dir credit and
// discovery warning the Config must carry.
type projCleanupRow struct {
	name     string
	wantPath string
	args     []string
	files    []string
	wantToml bool
	wantWarn bool
}

func projCleanupRows() []projCleanupRow {
	both := []string{helpers.RequirementsTOMLName, helpers.RequirementsYAMLName}
	return []projCleanupRow{
		{name: "a discovered galaxy.toml names the cache to clean", files: []string{helpers.RequirementsTOMLName},
			wantPath: helpers.RequirementsTOMLName, wantToml: true},
		{name: "-r requirements.yml leaves the galaxy.toml beside it unread", files: both,
			args: []string{"-r", helpers.RequirementsYAMLName}, wantPath: helpers.RequirementsYAMLName},
		{name: "both present picks galaxy.toml with a warning", files: both,
			wantPath: helpers.RequirementsTOMLName, wantToml: true, wantWarn: true},
		{name: "an absent file named with -r contributes nothing", files: []string{helpers.RequirementsTOMLName},
			args: []string{"-r", "absent.toml"}, wantPath: "absent.toml"},
	}
}

// TestProjectCleanup pins that cleanup, mounting the requirements flag for
// this alone, takes cache_dir from a discovered galaxy.toml, ignores it under
// -r requirements.yml or an absent name, and warns when both files are present.
func TestProjectCleanup(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "from-toml")
	for _, row := range projCleanupRows() {
		t.Run(row.name, func(t *testing.T) {
			projSetup(t, "")
			for _, name := range row.files {
				if name == helpers.RequirementsTOMLName {
					projWriteTOML(t, name, fmt.Sprintf("cache_dir = %q\n", cacheDir))
				} else {
					writeTestFile(t, name, []byte("collections: []\n"))
				}
			}
			wantWarn := ""
			if row.wantWarn {
				wantWarn = bothPresentWarning
			}

			cfg, err := buildConfigFor(t, "cleanup", Cleanup().Flags, row.args)
			if err != nil {
				t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
			}
			assertConfigField(t, "RequirementsFile", cfg.RequirementsFile, row.wantPath)
			assertConfigField(t, "CacheDir is the file's cache_dir", cfg.CacheDir == cacheDir, row.wantToml)
			assertConfigField(t, "cache_dir credited to the file", slices.Contains(cfg.ProjectSettingsUsed, "cache_dir"), row.wantToml)
			assertConfigField(t, "discovery warning", discoveryWarning(cfg), wantWarn)
		})
	}
}

// projLockedTOML is the galaxy.toml of the command rows: one root and a
// lock_file away from the default, so which lockfile a command read shows in
// what it prints.
const projLockedTOML = "[project]\ncollections = [\"acme.widgets\"]\n\n[tool.go-galaxy]\nlock_file = \"locks/galaxy.lock\"\n"

// projUnsetTOML names a variable no row exports under the one key hash reads,
// so the refusal reaches hash exactly as it reaches tree and explain.
const projUnsetTOML = "[project]\ncollections = [\"acme.widgets\"]\n\n[tool.go-galaxy]\nlock_file = \"${HASH_UNSET}/galaxy.lock\"\n"

// projCommandRow is one row of TestProjectLockFileThroughCommands: the
// command and its arguments, the galaxy.toml written, and the check over the
// fixture directory, stdout and the returned error.
type projCommandRow struct {
	command func() *cli.Command
	check   func(t *testing.T, dir, stdout string, err error)
	name    string
	toml    string
	args    []string
}

// projCommandFixture writes toml as galaxy.toml into a fresh directory with a
// lockfile under locks/ and a different one under other/, and returns the
// directory; nothing sits at the default galaxy.lock beside the file.
func projCommandFixture(t *testing.T, toml string) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, helpers.RequirementsTOMLName), []byte(toml))
	discoveryLockfile(t, filepath.Join(dir, "locks"))
	if err := lockfile.Save(filepath.Join(dir, "other", lockfile.DefaultName), roleLockfile()); err != nil {
		t.Fatalf("save other lockfile: %v", err)
	}
	return dir
}

// projLockfileHash returns the line hash prints for the lockfile at path.
func projLockfileHash(t *testing.T, path string) string {
	t.Helper()
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	hash, err := lf.Hash()
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return "sha256:" + hash + "\n"
}

// projCommandRows returns the rows. With --lock-file set a galaxy.toml is never
// loaded for its lock_file, so neither an unset variable nor bytes that are not
// TOML stop hash, and tree and explain read their roots without expanding.
func projCommandRows() []projCommandRow {
	locks := []string{"--lock-file", filepath.Join("locks", lockfile.DefaultName)}
	return []projCommandRow{
		{name: "hash keys the lock_file lockfile", command: Hash, toml: projLockedTOML, check: projCheckHashIsLockFile},
		{name: "hash --lock-file outranks lock_file", command: Hash, toml: projLockedTOML,
			args: []string{"--lock-file", filepath.Join("other", lockfile.DefaultName)}, check: projCheckHashIsFlagLockfile},
		{name: "tree reads the lock_file lockfile", command: Tree, toml: projLockedTOML, check: projCheckTreeReadsLockFile},
		{name: "explain credits the root from the lock_file lockfile", command: Explain, toml: projLockedTOML,
			args: []string{"acme.widgets"}, check: projCheckExplainReadsLockFile},
		{name: "hash refuses an unset variable", command: Hash, toml: projUnsetTOML, check: projCheckUnsetVariable},
		{name: "tree refuses an unset variable", command: Tree, toml: projUnsetTOML, check: projCheckUnsetVariable},
		{name: "explain refuses an unset variable", command: Explain, toml: projUnsetTOML,
			args: []string{"acme.widgets"}, check: projCheckUnsetVariable},
		{name: "hash --lock-file passes over an unset variable", command: Hash, toml: projUnsetTOML,
			args: locks, check: projCheckHashIsLockFile},
		{name: "tree --lock-file passes over an unset variable", command: Tree, toml: projUnsetTOML,
			args: locks, check: projCheckTreeReadsLockFile},
		{name: "explain --lock-file passes over an unset variable", command: Explain, toml: projUnsetTOML,
			args: append(slices.Clone(locks), "acme.widgets"), check: projCheckExplainReadsLockFile},
		{name: "hash --lock-file naming no file keys a galaxy.toml that is not TOML", command: Hash, toml: notTOMLContent,
			args: []string{"--lock-file", filepath.Join("absent", lockfile.DefaultName)}, check: projCheckHashIsRawBytes},
	}
}

// TestProjectLockFileThroughCommands runs hash, tree and explain from the
// fixture directory: without --lock-file the discovered galaxy.toml's
// lock_file is the only road to the lockfile; with it, nothing is expanded.
func TestProjectLockFileThroughCommands(t *testing.T) {
	for _, row := range projCommandRows() {
		t.Run(row.name, func(t *testing.T) {
			projUnsetEnv(t, projFlagEnvKeys()...)
			projUnsetEnv(t, "HASH_UNSET")
			dir := projCommandFixture(t, row.toml)

			var err error
			stdout, stderr := captureStdIO(t, func() {
				err = runCommandInDir(t, dir, row.command(), row.args...)
			})
			row.check(t, dir, stdout, err)
			if stderr != "" {
				t.Errorf("%s wrote to stderr: %q", row.command().Name, stderr)
			}
		})
	}
}

// projCheckHashIsLockFile pins that the key is the lock_file lockfile's hash,
// which the requirements bytes' hash could never equal.
func projCheckHashIsLockFile(t *testing.T, dir, stdout string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	assertConfigField(t, "hash stdout", stdout, projLockfileHash(t, filepath.Join(dir, "locks", lockfile.DefaultName)))
}

func projCheckHashIsFlagLockfile(t *testing.T, dir, stdout string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	assertConfigField(t, "hash stdout", stdout, projLockfileHash(t, filepath.Join(dir, "other", lockfile.DefaultName)))
}

// projCheckHashIsRawBytes pins the fallback key over the requirements bytes as
// they are: with no lockfile at the flag's path, hash never parses the file.
func projCheckHashIsRawBytes(t *testing.T, _, stdout string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	sum := sha256.Sum256([]byte(notTOMLContent))
	assertConfigField(t, "hash stdout", stdout, "sha256:"+hex.EncodeToString(sum[:])+"\n")
}

func projCheckTreeReadsLockFile(t *testing.T, _, stdout string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if !strings.HasPrefix(stdout, helpers.RequirementsTOMLName+"\n") {
		t.Errorf("tree header is not galaxy.toml; got:\n%s", stdout)
	}
	assertOutputHas(t, stdout, "└── acme.widgets 1.0.0\n")
}

func projCheckExplainReadsLockFile(t *testing.T, _, stdout string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	assertOutputHas(t, stdout, "acme.widgets 1.0.0\n", "    - galaxy.toml (root)\n")
}

// projCheckUnsetVariable pins the refusal every command shares: the sentinel,
// its text naming the variable and the file, exit 2 and nothing printed, so
// hash never yields a key over a file it could not read whole.
func projCheckUnsetVariable(t *testing.T, _, stdout string, err error) {
	t.Helper()
	projAssertUsageError(t, err, helpers.ErrProjectFileEnvUnset,
		"galaxy.toml: project file references unset environment variables: HASH_UNSET")
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing printed", stdout)
	}
}

// projServerRow is one row of TestProjectServers: the token of the one
// [[tool.go-galaxy.servers]] entry, the variables beside it, and either the
// refusal expected or the token the Config must reveal.
type projServerRow struct {
	env       map[string]string
	wantErr   error
	name      string
	token     string
	wantMsg   string
	wantToken string
}

// projServerRows returns the rows; hubEnvRef is the ${VAR} spelling, whose
// expansion makes the token the operator's rather than the file's.
func projServerRows() []projServerRow {
	const operatorToken = "s3cr3t-operator-token"
	const hubEnvRef = "${HUB_TOKEN}"
	return []projServerRow{
		{name: "a literal token is the file's own and is accepted", token: "s3cr3t-toml-token", wantToken: "s3cr3t-toml-token"},
		{name: "a ${VAR} token with the file's url is refused", token: hubEnvRef,
			env: map[string]string{"HUB_TOKEN": operatorToken}, wantErr: helpers.ErrTokenDestinationFromFile,
			wantMsg: "galaxy server token destination came from a configuration file: " +
				"server \"hub\" (https://hub.example:443) in galaxy.toml"},
		{name: "the documented remedy puts the address on the operator's channel", token: hubEnvRef,
			env:       map[string]string{"HUB_TOKEN": operatorToken, "ANSIBLE_GALAXY_SERVER_HUB_URL": "https://hub.example"},
			wantToken: operatorToken},
	}
}

// TestProjectServers pins [[tool.go-galaxy.servers]] through install's flag
// set: the entry is the server list, a literal token is the file's own, and a
// ${VAR} token is the operator's, refused beside the file's url.
func TestProjectServers(t *testing.T) {
	for _, row := range projServerRows() {
		t.Run(row.name, func(t *testing.T) {
			projSetup(t, "")
			projUnsetEnv(t, "HUB_TOKEN", "ANSIBLE_GALAXY_SERVER_HUB_URL")
			for key, value := range row.env {
				t.Setenv(key, value)
			}
			projWriteTOML(t, helpers.RequirementsTOMLName,
				fmt.Sprintf("\n[[tool.go-galaxy.servers]]\nid = \"hub\"\nurl = \"https://hub.example\"\ntoken = %q\n", row.token))

			cfg, err := buildConfigFor(t, "install", projInstallFlags(), nil)
			if row.wantErr != nil {
				projAssertUsageError(t, err, row.wantErr, row.wantMsg)
				if strings.Contains(err.Error(), "s3cr3t") {
					t.Errorf("error = %q carries the token's plaintext", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
			}
			if len(cfg.Servers) != 1 {
				t.Fatalf("Servers = %+v, want exactly one", cfg.Servers)
			}
			assertConfigField(t, "Servers[0].ID", cfg.Servers[0].ID, "hub")
			assertConfigField(t, "Servers[0].URL", cfg.Servers[0].URL, "https://hub.example")
			assertConfigField(t, "Servers[0].Token.Reveal()", cfg.Servers[0].Token.Reveal(), row.wantToken)
			projAssertUsed(t, cfg, "servers")
			projAssertNoPlaintext(t, cfg, row.wantToken)
		})
	}
}
