package collections

// This file drives real Start and Warm runs against fakegalaxy with signatures
// that verify through the whole manifest chain, plus refusal rows on the same
// fixtures, so a refusal proven here is of what was actually checked.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// writeSignedRequirements writes a requirements.yml under dir naming exactly
// one collection, acme.app@*, with sources appended under its own
// signatures: block when non-empty, and returns its path.
func writeSignedRequirements(t *testing.T, dir string, sources []string) string {
	t.Helper()
	path := filepath.Join(dir, "requirements.yml")
	var body strings.Builder
	body.WriteString("collections:\n  - name: acme.app\n    version: \"*\"\n")
	if len(sources) > 0 {
		body.WriteString("    signatures:\n")
		for _, source := range sources {
			body.WriteString("      - " + source + "\n")
		}
	}
	mustWriteFile(t, path, []byte(body.String()))
	return path
}

// signedRequiredCount is the strict spelling of the default count every row
// here uses: a vacuous pass fails closed instead of warning, so a gather that
// silently lost a signature is an observable failure.
const signedRequiredCount = "+1"

// newSignedFixture builds a cfg and runtime wired to srv through a fresh
// requirements.yml with sources under acme.app's signatures: block, under
// signedRequiredCount; it returns cfg, runtime and cfg.DownloadPath.
func newSignedFixture(
	t *testing.T, srv *fakegalaxy.Server, sources []string, workers int,
) (*config.Config, *infra.Infra, string) {
	t.Helper()
	root := t.TempDir()
	reqPath := writeSignedRequirements(t, root, sources)
	downloadPath := filepath.Join(root, "install")

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         filepath.Join(root, "cache"),
		DownloadPath:     downloadPath,
		RequirementsFile: reqPath,
		Workers:          workers,
		DownloadWorkers:  workers,
		Timeout:          verifyCommandTimeout,
		Signature: config.SignatureConfig{
			KeyringPath:   writeTestKeyring(t),
			RequiredCount: signedRequiredCount,
		},
	}
	return cfg, infra.New(&capturingPrinter{}, srv.Client()), downloadPath
}

// TestInstallVerifiesAServerSignedCollectionEndToEnd pins that Start installs a
// collection signed only in its server's metadata, walking the chain through
// FILES.json to the file it lists; a signature over other bytes fails closed.
func TestInstallVerifiesAServerSignedCollectionEndToEnd(t *testing.T) {
	t.Parallel()

	t.Run("a server-carried signature that verifies installs cleanly", func(t *testing.T) {
		t.Parallel()
		srv := fakegalaxy.New(t)
		srv.AddVersion("acme", "app", "1.0.0", nil)
		srv.SignVersion(t, "acme", "app", "1.0.0", signTestBytes(t, srv.ManifestJSON("acme", "app", "1.0.0")))

		cfg, runtime, downloadPath := newSignedFixture(t, srv, nil, 2)

		if err := Start(context.Background(), cfg, runtime); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		installDir := filepath.Join(downloadPath, "ansible_collections", "acme", "app")
		assertExists(t, filepath.Join(installDir, "MANIFEST.json"))
		assertExists(t, filepath.Join(installDir, "FILES.json"))

		printer := capturedOutput(t, runtime)
		if printer.hasWarnContaining("Nothing verified") {
			t.Fatalf("a verified collection must not warn about a vacuous pass: %v", printer.warns)
		}
	})

	t.Run("control: a signature over bytes the artifact does not carry fails closed", func(t *testing.T) {
		t.Parallel()
		srv := fakegalaxy.New(t)
		srv.AddVersion("acme", "app", "1.0.0", nil)
		srv.SignVersion(t, "acme", "app", "1.0.0", signTestBytes(t, []byte("a document this artifact does not carry")))

		cfg, runtime, downloadPath := newSignedFixture(t, srv, nil, 2)

		err := Start(context.Background(), cfg, runtime)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
		}
		if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "acme", "app")); statErr == nil {
			t.Fatal("the refused collection's install directory exists")
		}
	})
}

// TestWarmVerifiesAServerSignedCollectionAcrossCachedRuns pins that a second
// Warm, loading a fresh backend from disk, still verifies the server's
// signature from the persisted API cache without a version-detail request.
func TestWarmVerifiesAServerSignedCollectionAcrossCachedRuns(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	srv.SignVersion(t, "acme", "app", "1.0.0", signTestBytes(t, srv.ManifestJSON("acme", "app", "1.0.0")))

	cfg, runtime, _ := newSignedFixture(t, srv, nil, 2)

	if err := Warm(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Warm() (first run) = %v, want nil", err)
	}

	srv.ResetCounts()
	runtime.Output = &capturingPrinter{}
	if err := Warm(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Warm() (second run) = %v, want nil", err)
	}
	if got := srv.Count(fakegalaxy.EndpointVersionDetail); got != 0 {
		t.Fatalf("second Warm() reached the version-detail endpoint %d times, want 0: the server's own "+
			"signature must be served from the persisted cache, not fetched again", got)
	}
	printer := capturedOutput(t, runtime)
	if printer.hasWarnContaining("Nothing verified") {
		t.Fatalf("a verified collection must not warn about a vacuous pass: %v", printer.warns)
	}
}

// TestInstallVerifiesATransitiveServerSignedDependency pins that Start verifies
// the server signature of a dependency never named in requirements.yml, carried
// on the metadata its prefetched payload keeps.
func TestInstallVerifiesATransitiveServerSignedDependency(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "lib", "1.0.0", nil)
	srv.SignVersion(t, "acme", "lib", "1.0.0", signTestBytes(t, srv.ManifestJSON("acme", "lib", "1.0.0")))
	srv.AddVersion("acme", "app", "1.0.0", map[string]string{"acme.lib": "*"})
	srv.SignVersion(t, "acme", "app", "1.0.0", signTestBytes(t, srv.ManifestJSON("acme", "app", "1.0.0")))

	cfg, runtime, downloadPath := newSignedFixture(t, srv, nil, 4)

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	assertExists(t, filepath.Join(downloadPath, "ansible_collections", "acme", "app", "MANIFEST.json"))
	assertExists(t, filepath.Join(downloadPath, "ansible_collections", "acme", "lib", "MANIFEST.json"))
}

// TestInstallVerifiesAFileSignatureSourceOffline pins that a file:// source
// verifies under cfg.Offline over a primed cache; the fixture injects
// srv.Client(), so it does not prove the network transport was blocked.
func TestInstallVerifiesAFileSignatureSourceOffline(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)
	sigPath := filepath.Join(t.TempDir(), "collection.asc")
	mustWriteFile(t, sigPath, signTestBytes(t, srv.ManifestJSON("acme", "app", "1.0.0")))

	cfg, runtime, downloadPath := newSignedFixture(t, srv, []string{"file://" + sigPath}, 2)

	// Prime online with verification off: under cfg.Offline fetchArtifact refuses
	// a cache miss before verification is reached.
	cfg.Signature.DisableGPGVerify = true
	if err := Warm(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("priming Warm() = %v, want nil", err)
	}
	cfg.Signature.DisableGPGVerify = false
	cfg.Offline = true

	if err := Start(context.Background(), cfg, runtime); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	assertExists(t, filepath.Join(downloadPath, "ansible_collections", "acme", "app", "MANIFEST.json"))
}

// TestInstallRefusesWithNoSignatureAnywhere pins that Start, under the strict
// count, refuses a collection offered no signature by requirements or server
// and installs nothing.
func TestInstallRefusesWithNoSignatureAnywhere(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	cfg, runtime, downloadPath := newSignedFixture(t, srv, nil, 2)

	err := Start(context.Background(), cfg, runtime)
	if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("Start() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitSignature {
		t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
	}
	if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "acme", "app")); statErr == nil {
		t.Fatal("the refused collection's install directory exists")
	}
}

// TestWarmRefusedSignatureAndTheExtractedStore pins where a refused warm leaves
// the extracted tree: a prefetched artifact is extracted only after the verdict,
// while the direct-download fallback promotes the tree before it.
func TestWarmRefusedSignatureAndTheExtractedStore(t *testing.T) {
	t.Parallel()

	t.Run("a prefetched refusal leaves no extracted tree", func(t *testing.T) {
		t.Parallel()
		srv := fakegalaxy.New(t)
		v := srv.AddVersion("acme", "app", "1.0.0", nil)

		cfg, runtime, _ := newSignedFixture(t, srv, []string{writeUnverifiableSignature(t)}, 2)

		err := Warm(context.Background(), cfg, runtime)
		if got := exitcode.FromError(err); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d (err = %v)", got, exitcode.ExitSignature, err)
		}

		extractedStore := extracted.NewStore(cfg.CacheDir)
		if extractedStore.Ready(v.SHA256) {
			t.Fatalf("extracted tree for sha %s is present, want none: a prefetched handoff extracts only after "+
				"the signature verdict", v.SHA256)
		}
	})

	t.Run("a direct-download refusal still leaves the tree behind", func(t *testing.T) {
		t.Parallel()
		srv := fakegalaxy.New(t)
		v := srv.AddVersion("acme", "app", "1.0.0", nil)
		// Sized to the prefetch worker's whole retry budget, so warmOne's fallback
		// download runs streamDownloadAndExtract inside the warm worker itself.
		srv.Fail(fakegalaxy.EndpointArtifact, "acme", "app", fakegalaxy.Fault{
			Status: http.StatusServiceUnavailable,
			Count:  helpers.FetchRetryMaxAttempts,
		})

		cfg, runtime, _ := newSignedFixture(t, srv, []string{writeUnverifiableSignature(t)}, 2)

		err := Warm(context.Background(), cfg, runtime)
		if got := exitcode.FromError(err); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d (err = %v)", got, exitcode.ExitSignature, err)
		}

		extractedStore := extracted.NewStore(cfg.CacheDir)
		if !extractedStore.Ready(v.SHA256) {
			t.Fatalf("extracted tree for sha %s is not present, want it left behind by the fallback download that "+
				"ran ahead of the refused verification", v.SHA256)
		}
	})
}
