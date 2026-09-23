package collections

// Tests driving collections.Start and collections.Warm end to end with a keyring,
// proving the verify context reaches the workers and pinning what a preview or
// an --offline run says about verification.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// verifyCommandTimeout is the HTTP client timeout these command-level fixtures
// use: long enough never to be the reason a row fails, short enough that a real
// hang still fails fast.
const verifyCommandTimeout = 30 * time.Second

// newVerifyCommandFixture builds a real Start or Warm run against a fake server
// publishing acme.app@1.0.0, with a valid keyring and optional signatures:
// sources; it returns the config, the runtime and the collections path.
func newVerifyCommandFixture(t *testing.T, sources []string) (*config.Config, *infra.Infra, string) {
	t.Helper()
	cfg, runtime := newVerifySetupFixture(t, writeTestKeyring(t), sources)

	return cfg, runtime, cfg.DownloadPath
}

// newVerifySetupFixture is newVerifyCommandFixture with the keyring path as a
// parameter, so a real command can be driven into each of newVerifyContext's
// setup refusals.
func newVerifySetupFixture(t *testing.T, keyringPath string, sources []string) (*config.Config, *infra.Infra) {
	t.Helper()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")
	reqPath := filepath.Join(root, "requirements.yml")

	var body strings.Builder
	body.WriteString("collections:\n  - name: acme.app\n    version: \"*\"\n")
	if len(sources) > 0 {
		body.WriteString("    signatures:\n")
		for _, source := range sources {
			body.WriteString("      - " + source + "\n")
		}
	}
	mustWriteFile(t, reqPath, []byte(body.String()))

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          2,
		DownloadWorkers:  2,
		Timeout:          verifyCommandTimeout,
		Signature: config.SignatureConfig{
			KeyringPath:   keyringPath,
			RequiredCount: "1",
		},
	}

	return cfg, infra.New(&capturingPrinter{}, srv.Client())
}

// writeUnverifiableSignature writes non-OpenPGP bytes and returns a file:// URL
// naming them: the source reads but never verifies, so a required count of one
// fails before any chain walk.
func writeUnverifiableSignature(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unverifiable.asc")
	mustWriteFile(t, path, []byte("these bytes are not an OpenPGP signature\n"))
	return "file://" + path
}

// capturedOutput returns runtime's underlying *capturingPrinter, failing the
// test rather than panicking if runtime was built with another printer.
func capturedOutput(t *testing.T, runtime *infra.Infra) *capturingPrinter {
	t.Helper()
	printer, ok := runtime.Output.(*capturingPrinter)
	if !ok {
		t.Fatalf("runtime.Output is %T, want *capturingPrinter", runtime.Output)
	}

	return printer
}

// TestInstallCommandVerifiesSignatures pins that a real Start run with a source
// that cannot verify fails with exit 10 and installs nothing; the same
// requirements without signatures: install cleanly.
func TestInstallCommandVerifiesSignatures(t *testing.T) {
	t.Parallel()

	t.Run("a signature that cannot verify fails the install", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, downloadPath := newVerifyCommandFixture(t, []string{writeUnverifiableSignature(t)})

		err := Start(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
		}
		if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "acme", "app", "MANIFEST.json")); statErr == nil {
			t.Fatal("the refused collection was installed anyway")
		}
	})

	t.Run("the same requirements without signatures install", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, downloadPath := newVerifyCommandFixture(t, nil)

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		assertExists(t, filepath.Join(downloadPath, "ansible_collections", "acme", "app", "MANIFEST.json"))
	})
}

// TestWarmCommandVerifiesSignatures pins the same for Warm, which mounts the
// verification flags and must honor them; the same requirements without
// signatures: warm cleanly.
func TestWarmCommandVerifiesSignatures(t *testing.T) {
	t.Parallel()

	t.Run("a signature that cannot verify fails the warm", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, []string{writeUnverifiableSignature(t)})

		err := Warm(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Warm() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
		}
	})

	t.Run("the same requirements without signatures warm", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)

		if err := Warm(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Warm() = %v, want nil", err)
		}
	})
}

// TestDryRunDisclosesThatVerificationWasNotExercised pins announceVerification's
// tier split: a dry run with a keyring warns that nothing is verified, a real
// run prints the setup on the persist tier, and no keyring prints neither.
func TestDryRunDisclosesThatVerificationWasNotExercised(t *testing.T) {
	t.Parallel()

	t.Run("keyring configured, dry run: the warn tier discloses and the persist tier is silent", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)
		cfg.DryRun = true

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		printer := capturedOutput(t, runtime)
		if !printer.hasWarnContaining("so the would-fail count covers no signature verdict") {
			t.Logf("warns = %v", printer.warns)
			t.Fatalf("no dry-run verification disclosure on the warn tier")
		}
		if printer.hasPersistentPrintContaining("Signature verification") {
			t.Logf("persists = %v", printer.persists)
			t.Fatalf("the persist tier carries a verification line on a dry run")
		}
	})

	t.Run("keyring configured, real run: the persist tier carries the real-run line, the warn tier is silent about it", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		printer := capturedOutput(t, runtime)
		want := fmt.Sprintf("Signature verification on: keyring %s, required count 1", cfg.Signature.KeyringPath)
		if !slices.Contains(printer.persists, want) {
			t.Fatalf("persists = %v, want it to contain %q", printer.persists, want)
		}
		if printer.hasWarnContaining("this preview validates the setup and verifies no collection") {
			t.Fatalf("the warn tier carries a dry-run disclosure on a real run: %v", printer.warns)
		}
	})

	t.Run("no keyring, dry run: neither tier carries a verification line", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)
		cfg.DryRun = true
		cfg.Signature.KeyringPath = ""

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		printer := capturedOutput(t, runtime)
		if printer.hasPersistentPrintContaining("Signature verification") ||
			printer.hasWarnContaining("signature verification is configured") {
			t.Fatalf("a verification line leaked with no keyring configured; persists=%v warns=%v", printer.persists, printer.warns)
		}
	})
}

// TestDryRunFetchesNoSignatureSource pins that install and warm --dry-run never
// fetch a declared signature source, while a real run on the same primed cache
// fetches it once. Subtests share state and run in order, not in parallel.
func TestDryRunFetchesNoSignatureSource(t *testing.T) {
	// hits is never reset, so a later subtest also sees an earlier one's fetch.
	var hits atomic.Int32
	sigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("these bytes are not an OpenPGP signature\n"))
	}))
	t.Cleanup(sigSrv.Close)

	cfg, runtime, _ := newVerifyCommandFixture(t, []string{sigSrv.URL + "/sig.asc"})

	// Prime through warm with verification off: it never reaches sigSrv and
	// records no install, so the real run below cannot skip via canSkipInstall.
	cfg.Signature.DisableGPGVerify = true
	if err := Warm(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("priming Warm() = %v, want nil", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("priming hit the signature source %d times, want 0 (verification was off)", got)
	}
	cfg.Signature.DisableGPGVerify = false

	t.Run("install --dry-run fetches nothing", func(t *testing.T) {
		cfg.DryRun = true
		defer func() { cfg.DryRun = false }()

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() (dry run) = %v, want nil", err)
		}
		if got := hits.Load(); got != 0 {
			t.Fatalf("install --dry-run hit the signature source %d times, want 0", got)
		}
	})

	t.Run("warm --dry-run fetches nothing", func(t *testing.T) {
		cfg.DryRun = true
		defer func() { cfg.DryRun = false }()

		if err := Warm(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Warm() (dry run) = %v, want nil", err)
		}
		if got := hits.Load(); got != 0 {
			t.Fatalf("warm --dry-run hit the signature source %d times, want 0", got)
		}
	})

	t.Run("positive control: a real run hits the same source exactly once", func(t *testing.T) {
		err := Start(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := hits.Load(); got != 1 {
			t.Fatalf("a real run hit the signature source %d times, want exactly 1", got)
		}
	})
}

// verifySetupFailureRow is a keyring path and declared sources that
// newVerifyContext, or loadRoots for an unfetchable source, refuses with
// wantErr.
type verifySetupFailureRow struct {
	wantErr     error
	name        string
	keyringPath string
	sources     []string
}

// verifySetupFailureRows builds
// TestDryRunRefusesEveryVerificationSetupFailureARealRunDoes's table,
// factored out to keep that test under the funlen budget.
func verifySetupFailureRows(t *testing.T) []verifySetupFailureRow {
	t.Helper()
	// A GnuPG keybox: the "KBXf" magic looksLikeKeybox reads sits at bytes
	// 8..11, and nothing else about the file's shape matters to that check.
	keybox := make([]byte, 12)
	copy(keybox[8:], "KBXf")
	badKeyboxPath := filepath.Join(t.TempDir(), "keybox.gpg")
	mustWriteFile(t, badKeyboxPath, keybox)

	return []verifySetupFailureRow{
		{
			name:        "absent keyring path",
			keyringPath: filepath.Join(t.TempDir(), "does-not-exist.asc"),
			wantErr:     helpers.ErrKeyringUnreadable,
		},
		{
			name:        "keyring is a GnuPG keybox",
			keyringPath: badKeyboxPath,
			wantErr:     helpers.ErrKeyringIsKeybox,
		},
		{
			name:    "signatures declared with no keyring configured",
			sources: []string{"https://example.invalid/acme-app.asc"},
			wantErr: helpers.ErrKeyringRequired,
		},
		{
			name:        "a signature source this tool does not fetch",
			keyringPath: writeTestKeyring(t),
			sources:     []string{"ftp://example.invalid/acme-app.asc"},
			wantErr:     helpers.ErrUnsupportedSignatureSource,
		},
	}
}

// TestDryRunRefusesEveryVerificationSetupFailureARealRunDoes pins that each
// verification setup refusal exits 2 identically with and without --dry-run,
// since it is reached before the dry-run branch; a good keyring installs.
func TestDryRunRefusesEveryVerificationSetupFailureARealRunDoes(t *testing.T) {
	t.Parallel()

	for _, row := range verifySetupFailureRows(t) {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			for _, dryRun := range []bool{true, false} {
				t.Run(fmt.Sprintf("dry run = %v", dryRun), func(t *testing.T) {
					t.Parallel()
					cfg, runtime := newVerifySetupFixture(t, row.keyringPath, row.sources)
					cfg.DryRun = dryRun

					err := Start(context.Background(), cfg, runtime)
					if !errors.Is(err, row.wantErr) {
						t.Fatalf("Start() = %v, want errors.Is %v", err, row.wantErr)
					}
					if got := exitcode.FromError(err); got != exitcode.ExitUsage {
						t.Fatalf("exit code = %d, want %d (ExitUsage)", got, exitcode.ExitUsage)
					}
				})
			}
		})
	}

	t.Run("positive control: a good keyring with no signatures succeeds under both spellings", func(t *testing.T) {
		t.Parallel()
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("dry run = %v", dryRun), func(t *testing.T) {
				t.Parallel()
				cfg, runtime, _ := newVerifyCommandFixture(t, nil)
				cfg.DryRun = dryRun

				if err := Start(context.Background(), cfg, runtime); err != nil {
					t.Fatalf("Start() = %v, want nil", err)
				}
			})
		}
	})
}

// offlineWarnRow is declared sources plus --offline, keyring and
// --disable-gpg-verify settings, and whether warnOfflineSignatureSources fires.
type offlineWarnRow struct {
	name             string
	sources          []string
	offline          bool
	keyring          bool
	disableGPGVerify bool
	wantWarn         bool
}

// offlineWarnRows is TestOfflineWarnsAboutUnfetchableSignatureSources's table;
// its second row lists a file source ahead of an https one in the same entry.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var offlineWarnRows = []offlineWarnRow{
	{
		name: "offline with an https source warns", offline: true, keyring: true,
		sources: []string{"https://example.invalid/acme-app.asc"}, wantWarn: true,
	},
	{
		name: "offline with a file source listed first still reaches the https source after it", offline: true, keyring: true,
		sources: []string{"file:///nonexistent/sig.asc", "https://example.invalid/acme-app.asc"}, wantWarn: true,
	},
	{
		name: "online is silent", offline: false, keyring: true,
		sources: []string{"https://example.invalid/acme-app.asc"}, wantWarn: false,
	},
	{
		name: "offline with only a file source is silent", offline: true, keyring: true,
		sources: []string{"file:///nonexistent/sig.asc"}, wantWarn: false,
	},
	{
		name: "offline with no keyring is silent", offline: true, keyring: false,
		sources: []string{"https://example.invalid/acme-app.asc"}, wantWarn: false,
	},
	{
		name: "offline with --disable-gpg-verify is silent", offline: true, keyring: true, disableGPGVerify: true,
		sources: []string{"https://example.invalid/acme-app.asc"}, wantWarn: false,
	},
	{
		name: "offline with no declared sources at all is silent", offline: true, keyring: true,
		wantWarn: false,
	},
}

// offlineWarnCommand is one command TestOfflineWarnsAboutUnfetchableSignatureSources
// drives both offlineWarnRows and its own dry-run spellings through.
type offlineWarnCommand struct {
	run  func(context.Context, *config.Config, *infra.Infra) error
	name string
}

// offlineWarnCommands is install and warm, the commands reaching
// warnOfflineSignatureSources.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var offlineWarnCommands = []offlineWarnCommand{
	{name: "install", run: Start},
	{name: "warm", run: Warm},
}

// newVerifyTwoRootFixture builds requirements naming acme.app and acme.other,
// only the second carrying secondSources; acme.other is never served, since the
// offline warning fires from parsed roots before any server contact.
func newVerifyTwoRootFixture(t *testing.T, secondSources []string) (*config.Config, *infra.Infra) {
	t.Helper()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	root := t.TempDir()
	reqPath := filepath.Join(root, "requirements.yml")

	var body strings.Builder
	body.WriteString("collections:\n")
	body.WriteString("  - name: acme.app\n    version: \"*\"\n")
	body.WriteString("  - name: acme.other\n    version: \"*\"\n")
	if len(secondSources) > 0 {
		body.WriteString("    signatures:\n")
		for _, source := range secondSources {
			body.WriteString("      - " + source + "\n")
		}
	}
	mustWriteFile(t, reqPath, []byte(body.String()))

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		Workers:          2,
		DownloadWorkers:  2,
		Timeout:          verifyCommandTimeout,
		Signature: config.SignatureConfig{
			KeyringPath:   writeTestKeyring(t),
			RequiredCount: "1",
		},
	}

	return cfg, infra.New(&capturingPrinter{}, srv.Client())
}

// assertOuterLoopReachesSecondRoot asserts the offline warning fires when only
// the second of two roots declares a network signature source.
func assertOuterLoopReachesSecondRoot(t *testing.T, offlineWarnSubstr string) {
	t.Helper()
	cfg, runtime := newVerifyTwoRootFixture(t, []string{"https://example.invalid/acme-app.asc"})
	cfg.Offline = true

	_ = Start(context.Background(), cfg, runtime)
	printer := capturedOutput(t, runtime)
	if !printer.hasWarnContaining(offlineWarnSubstr) {
		t.Logf("warns = %v", printer.warns)
		t.Fatalf("the outer loop stopped at the first root and never reached the second one's own source")
	}
}

// TestOfflineWarnsAboutUnfetchableSignatureSources pins that --offline plus a
// network signature source on any root warns, under install and warm, dry run or
// not, only while verification is on; a real offline install then fails closed.
func TestOfflineWarnsAboutUnfetchableSignatureSources(t *testing.T) {
	t.Parallel()

	const offlineWarnSubstr = "declares a signature source that must be fetched over the network"

	for _, row := range offlineWarnRows {
		for _, command := range offlineWarnCommands {
			for _, dryRun := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/dry run = %v", row.name, command.name, dryRun), func(t *testing.T) {
					t.Parallel()
					cfg, runtime, _ := newVerifyCommandFixture(t, row.sources)
					cfg.Offline = row.offline
					cfg.DryRun = dryRun
					cfg.Signature.DisableGPGVerify = row.disableGPGVerify
					if !row.keyring {
						cfg.Signature.KeyringPath = ""
					}

					_ = command.run(context.Background(), cfg, runtime)
					printer := capturedOutput(t, runtime)
					got := printer.hasWarnContaining(offlineWarnSubstr)
					if got != row.wantWarn {
						t.Logf("warns = %v", printer.warns)
						t.Fatalf("offline-signature-source warning present = %v, want %v", got, row.wantWarn)
					}
				})
			}
		}
	}

	t.Run("the outer loop does not stop at the first root: only the second collection declares a source", func(t *testing.T) {
		t.Parallel()
		assertOuterLoopReachesSecondRoot(t, offlineWarnSubstr)
	})

	t.Run("the fact behind the warning: a real offline gather fails rather than installing unverified", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, []string{"https://example.invalid/acme-app.asc"})

		cfg.Signature.DisableGPGVerify = true
		if err := Warm(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("priming Warm() = %v, want nil", err)
		}

		cfg.Signature.DisableGPGVerify = false
		cfg.Offline = true
		err := Start(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureSourceUnavailable", err)
		}
		if !errors.Is(err, helpers.ErrOfflineMode) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrOfflineMode", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitInstall {
			t.Fatalf("exit code = %d, want %d (ExitInstall)", got, exitcode.ExitInstall)
		}
	})
}

// assertSameDryRunReport fails the test unless off and on agree byte for byte
// on the ok, error and persist tiers; the warn tier is left to the caller,
// which needs it to differ.
func assertSameDryRunReport(t *testing.T, off, on *capturingPrinter) {
	t.Helper()
	if !slices.Equal(off.oks, on.oks) {
		t.Fatalf("oks differ: off=%v on=%v", off.oks, on.oks)
	}
	if !slices.Equal(off.errs, on.errs) {
		t.Fatalf("errs differ: off=%v on=%v", off.errs, on.errs)
	}
	if !slices.Equal(off.persists, on.persists) {
		t.Fatalf("persists differ: off=%v on=%v", off.persists, on.persists)
	}
}

// TestDryRunReportIsUnchangedByVerification pins that a dry run's ok, error and
// persist tiers never depend on whether a keyring is configured; only the warn
// tier differs. An online-versus-offline pair proves the comparison can fail.
func TestDryRunReportIsUnchangedByVerification(t *testing.T) {
	t.Parallel()

	t.Run("verification off and on report identically apart from the warn tier", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)
		cfg.DryRun = true
		cfg.Signature.KeyringPath = ""

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() (verification off) = %v, want nil", err)
		}
		off := capturedOutput(t, runtime)

		// onErr is checked after the comparisons so a failing "on" run is
		// reported as the report difference it causes.
		runtime.Output = &capturingPrinter{}
		cfg.Signature.KeyringPath = writeTestKeyring(t)
		onErr := Start(context.Background(), cfg, runtime)
		on := capturedOutput(t, runtime)

		assertSameDryRunReport(t, off, on)
		if slices.Equal(off.warns, on.warns) {
			t.Fatal("expected the warn tier to differ between verification off and on, and it did not")
		}
		if onErr != nil {
			t.Fatalf("Start() (verification on) = %v, want nil", onErr)
		}
	})

	t.Run("positive control: differing --offline reports are caught as different", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, nil)
		cfg.DryRun = true

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() (online) = %v, want nil", err)
		}
		online := capturedOutput(t, runtime)

		runtime.Output = &capturingPrinter{}
		cfg.Offline = true
		if err := Start(context.Background(), cfg, runtime); err == nil {
			t.Fatal("Start() (offline, uncached) = nil, want a would-fail error")
		}
		offline := capturedOutput(t, runtime)

		if slices.Equal(online.oks, offline.oks) &&
			slices.Equal(online.errs, offline.errs) &&
			slices.Equal(online.persists, offline.persists) {
			t.Fatal("expected the online and offline dry-run reports to differ, and they did not")
		}
	})
}

// TestWarmVerifiesACollectionInstallWouldSkip pins that warm re-verifies an
// already-cached collection and fails, while install skips an already-installed
// one unverified and says so through reportSkippedUnverified.
func TestWarmVerifiesACollectionInstallWouldSkip(t *testing.T) {
	t.Parallel()

	t.Run("warm has no skip gate: it re-verifies a cached collection and fails", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, []string{writeUnverifiableSignature(t)})
		cfg.Signature.DisableGPGVerify = true

		if err := Warm(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("priming Warm() (verification off) = %v, want nil", err)
		}

		cfg.Signature.DisableGPGVerify = false
		err := Warm(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Warm() (verification on, cache primed) = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
	})

	t.Run("install's skip gate makes its verdict faithful: an already-installed collection is never re-verified", func(t *testing.T) {
		t.Parallel()
		cfg, runtime, _ := newVerifyCommandFixture(t, []string{writeUnverifiableSignature(t)})
		cfg.Signature.DisableGPGVerify = true

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("priming Start() (verification off) = %v, want nil", err)
		}

		cfg.Signature.DisableGPGVerify = false
		runtime.Output = &capturingPrinter{}
		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() (verification on, already installed) = %v, want nil", err)
		}
		printer := capturedOutput(t, runtime)
		if !printer.hasPersistentPrintContaining("already-installed collection(s) were skipped and therefore not verified") {
			t.Fatalf("persists = %v, want the skipped-unverified line", printer.persists)
		}
	})
}

// TestQueryBearingSignatureSourceIsFetchedWithItsQueryIntact pins that the live
// signature source keeps its query when fetched, since normalizeSignatures cuts
// the query only from the persisted spec.
func TestQueryBearingSignatureSourceIsFetchedWithItsQueryIntact(t *testing.T) {
	t.Parallel()
	const capabilityQuery = "X-Amz-Signature=deadbeefcapability&X-Amz-Expires=3600"

	var hits atomic.Int32
	queries := make(chan string, 4)
	sigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		queries <- r.URL.RawQuery
		_, _ = w.Write([]byte("these bytes are not an OpenPGP signature\n"))
	}))
	t.Cleanup(sigSrv.Close)

	cfg, runtime, _ := newVerifyCommandFixture(t, []string{sigSrv.URL + "/sig.asc?" + capabilityQuery})

	err := Start(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("signature source fetched %d times, want exactly 1", got)
	}

	got := <-queries
	if got != capabilityQuery {
		t.Fatalf("request query = %q, want %q (the live source keeps its query; only the persisted spec cuts it)", got, capabilityQuery)
	}
}
