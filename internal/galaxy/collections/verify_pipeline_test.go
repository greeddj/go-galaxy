package collections

// This file pins how verification composes with the install pipeline: which
// failures evict a cached artifact, what a cache hit costs, where warm verifies
// and how a verdict reaches an exit code. It reuses verify_test.go's fixtures.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
	"github.com/psvmcc/hub/pkg/types"
)

// signedCacheFixture is a cache already holding one signed artifact, wired so
// that a forced refetch serves the identical bytes: it is what an install worker
// sees on a cache hit, with every Delete counted.
type signedCacheFixture struct {
	deps         installDeps
	artifacts    *deleteCountingArtifacts
	meta         *types.GalaxyCollectionVersionInfo
	artifactPath string
	manifestJSON []byte
}

// newSignedCacheFixture seeds an artifact and its sha sidecar into a real local
// artifact store and points its metadata at a server serving the same bytes, so
// a refetch succeeds and a download error cannot hide whether eviction ran.
func newSignedCacheFixture(t *testing.T, breakChain bool, sources []string) *signedCacheFixture {
	t.Helper()
	tarPath, manifestJSON := buildSignedArtifact(t, breakChain)
	content := mustReadFile(t, tarPath)
	sha := sha256Hex(content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	mustMkdirAll(t, cacheDir)

	col := testSignedCollection
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, content)
	mustWriteFile(t, artifactPath+helpers.ArtifactSHASidecarSuffix, []byte(sha))

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Signature: config.SignatureConfig{
			KeyringPath:   writeTestKeyring(t),
			RequiredCount: "1",
		},
	}
	runtime := infra.New(&capturingPrinter{}, http.DefaultClient)
	signedRoot := col
	signedRoot.Signatures = sources
	verify, err := newVerifyContext(cfg, runtime, []collection{signedRoot})
	if err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}

	artifacts := &deleteCountingArtifacts{Artifacts: local.NewArtifacts(cacheDir)}
	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = sha

	return &signedCacheFixture{
		deps: installDeps{
			collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
			artifacts:      artifacts,
			root:           newTestCollectionsRoot(t, downloadPath),
			verify:         verify,
		},
		artifacts:    artifacts,
		meta:         meta,
		artifactPath: artifactPath,
		manifestJSON: manifestJSON,
	}
}

// TestSignatureSourceFailureDoesNotEvictTheArtifact pins isSignatureSourceFailure:
// an unreadable signature source is not the artifact's fault and evicts nothing,
// while a chain mismatch on the same harness evicts once (the positive control).
func TestSignatureSourceFailureDoesNotEvictTheArtifact(t *testing.T) {
	t.Parallel()

	t.Run("an unreadable signature source evicts nothing", func(t *testing.T) {
		t.Parallel()
		absent := "file://" + filepath.Join(t.TempDir(), "absent.asc")
		fx := newSignedCacheFixture(t, false, []string{absent})

		err := installCollection(context.Background(), testSignedCollection, fx.deps, nil, fx.meta, downloadResult{})
		if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
			t.Fatalf("installCollection() = %v, want errors.Is helpers.ErrSignatureSourceUnavailable", err)
		}
		if got := fx.artifacts.deleteCalls.Load(); got != 0 {
			t.Fatalf("Delete calls = %d, want 0: a signature source that could not be read is not the artifact's fault", got)
		}
		assertExists(t, fx.artifactPath)
	})

	t.Run("a chain mismatch on the same harness evicts once", func(t *testing.T) {
		t.Parallel()
		fx := newSignedCacheFixture(t, true, nil)
		// Written after construction, which verifyContext forbids elsewhere: the
		// signature must cover this fixture's manifest, and no worker exists yet.
		fx.deps.verify.sources[requirementKey(testSignedCollection)] =
			[]string{writeTestSignature(t, fx.manifestJSON)}

		err := installCollection(context.Background(), testSignedCollection, fx.deps, nil, fx.meta, downloadResult{})
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("installCollection() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
		}
		if got := fx.artifacts.deleteCalls.Load(); got != 1 {
			t.Fatalf("Delete calls = %d, want exactly 1 (one bounded evict-and-refetch)", got)
		}
	})
}

// TestCacheHitForcesMetadataWhenVerifying pins that a verifying run fetches
// version metadata on a cache hit, since a server's signatures ride on it; with
// verification off the same hit costs no request (the control).
func TestCacheHitForcesMetadataWhenVerifying(t *testing.T) {
	t.Parallel()

	t.Run("verifying forces the metadata fetch", func(t *testing.T) {
		t.Parallel()
		srv, deps, col := newCacheHitMetadataFixture(t, true)
		if _, _, err := prepareInstall(
			context.Background(), deps, col, nil, downloadResult{}, "acme-app-1.0.0.tar.gz", false); err != nil {
			t.Fatalf("prepareInstall() error = %v, want nil", err)
		}
		if got := srv.Total(); got == 0 {
			t.Fatalf("fake server request count = %d, want at least one version-metadata request while verifying", got)
		}
	})

	t.Run("not verifying serves the same hit with no request", func(t *testing.T) {
		t.Parallel()
		srv, deps, col := newCacheHitMetadataFixture(t, false)
		if _, _, err := prepareInstall(
			context.Background(), deps, col, nil, downloadResult{}, "acme-app-1.0.0.tar.gz", false); err != nil {
			t.Fatalf("prepareInstall() error = %v, want nil", err)
		}
		if got := srv.Total(); got != 0 {
			t.Fatalf("fake server request count = %d, want 0 (the fast path serves a cache hit unasked)", got)
		}
	})
}

// newCacheHitMetadataFixture seeds a cache hit of the fake server's own bytes
// and returns the server with deps that verify or not, so the verifying row's
// metadata describes the cached artifact.
func newCacheHitMetadataFixture(t *testing.T, verifying bool) (*fakegalaxy.Server, installDeps, collection) {
	t.Helper()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", "1.0.0", nil)

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	mustMkdirAll(t, cacheDir)
	col := testSignedCollection
	col.Source = srv.URL()

	tarPath, _ := buildSignedArtifact(t, false)
	mustWriteFile(t, filepath.Join(cacheDir, artifactKey(col)), mustReadFile(t, tarPath))

	cfg := &config.Config{
		Server:       srv.URL(),
		Servers:      []config.Server{{URL: srv.URL()}},
		CacheDir:     cacheDir,
		DownloadPath: filepath.Join(root, "install"),
		Workers:      1,
		NoDeps:       true,
	}
	runtime := infra.New(&capturingPrinter{}, srv.Client())

	var verify *verifyContext
	if verifying {
		cfg.Signature = config.SignatureConfig{KeyringPath: writeTestKeyring(t), RequiredCount: "1"}
		built, err := newVerifyContext(cfg, runtime, []collection{col})
		if err != nil {
			t.Fatalf("newVerifyContext() error = %v, want nil", err)
		}
		verify = built
	}

	return srv, installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, store.New()),
		artifacts:      local.NewArtifacts(cacheDir),
		verify:         verify,
	}, col
}

// TestWarmVerifiesSignatures pins that warmVerifyAndEnsure refuses a signature
// that does not verify, accepts one that does on the same artifact, and checks
// the lockfile pin before the signature.
func TestWarmVerifiesSignatures(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, false)
	sha := sha256Hex(mustReadFile(t, tarPath))
	good := serverSignatureMeta(signTestBytes(t, manifestJSON))
	bad := serverSignatureMeta(signTestBytes(t, []byte("a document this artifact does not carry")))

	t.Run("a signature that does not verify refuses the warm", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		payload := installPayload{meta: bad, artifact: artifactData{Path: tarPath}, artifactSHA: sha}
		err := warmVerifyAndEnsure(context.Background(), fx.deps, testSignedCollection, payload)
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("warmVerifyAndEnsure() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
	})

	t.Run("a signature that verifies accepts the same artifact", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		payload := installPayload{meta: good, artifact: artifactData{Path: tarPath}, artifactSHA: sha}
		if err := warmVerifyAndEnsure(context.Background(), fx.deps, testSignedCollection, payload); err != nil {
			t.Fatalf("warmVerifyAndEnsure() = %v, want nil", err)
		}
	})

	t.Run("the pin is checked before the signature", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		pinned := testSignedCollection
		pinned.SHA256 = testOtherDigest
		payload := installPayload{meta: bad, artifact: artifactData{Path: tarPath}, artifactSHA: sha}
		err := warmVerifyAndEnsure(context.Background(), fx.deps, pinned, payload)
		if !errors.Is(err, helpers.ErrSHA256Mismatch) {
			t.Fatalf("warmVerifyAndEnsure() = %v, want errors.Is helpers.ErrSHA256Mismatch", err)
		}
	})
}

// TestSignatureVerdictAggregatesToTheSignatureExitClass carries real verdicts
// from verifyCollectionSignatures through failureRecorder, so a wrap here that
// hid a sentinel from errors.Is fails where exitcode's synthetic rows cannot.
func TestSignatureVerdictAggregatesToTheSignatureExitClass(t *testing.T) {
	t.Parallel()

	t.Run("a failed signature verdict aggregates to the signature exit class", func(t *testing.T) {
		t.Parallel()
		tarPath, _ := buildSignedArtifact(t, false)
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

		meta := serverSignatureMeta(signTestBytes(t, []byte("a document this artifact does not carry")))
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
		if err == nil {
			t.Fatal("verifyCollectionSignatures() = nil, want a verification verdict")
		}

		var failures failureRecorder
		failures.record(err)
		if got := exitcode.FromError(failures.summary().installError()); got != exitcode.ExitSignature {
			t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
		}
	})

	// A chain mismatch arrives wrapped twice, by verifyCollectionSignatures over
	// manifest.VerifyChain, and stays ExitIntegrity bare and aggregated, since
	// isIntegrityError precedes isSignatureError and isInstallError in exitcode.
	t.Run("a chain-mismatch verdict aggregates to the integrity exit class", func(t *testing.T) {
		t.Parallel()
		tarPath, manifestJSON := buildSignedArtifact(t, true)
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

		meta := serverSignatureMeta(signTestBytes(t, manifestJSON))
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitIntegrity {
			t.Fatalf("bare exit code = %d, want %d", got, exitcode.ExitIntegrity)
		}

		var failures failureRecorder
		failures.record(err)
		if got := exitcode.FromError(failures.summary().installError()); got != exitcode.ExitIntegrity {
			t.Fatalf("aggregated exit code = %d, want %d", got, exitcode.ExitIntegrity)
		}
	})
}

// TestSignatureFetchDeadlineFiresOnAStalledSource pins that a source that never
// answers is ended by the collection's own signature budget and reported as
// ErrSignatureFetchDeadline (exit network), never as a caller's cancellation.
func TestSignatureFetchDeadlineFiresOnAStalledSource(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)

	stalled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer stalled.Close()

	fx := newVerifyFixture(t, writeTestKeyring(t), "1", []string{stalled.URL + "/acme-app.asc"})
	fx.deps.runtime.SignatureFetchDeadline = 50 * time.Millisecond

	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath))
	if !errors.Is(err, helpers.ErrSignatureFetchDeadline) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureFetchDeadline", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("verifyCollectionSignatures() = %v, must not leave a context sentinel reachable", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
		t.Fatalf("exit code = %d, want %d (a spent budget is a wire failure, never an interrupt)", got, exitcode.ExitNetwork)
	}
}

// TestVerifyContextIsSafeForConcurrentUse shares one verifyContext (keyring,
// policy, fetcher) across workers verifying through both gather paths; under
// -race, any write to that shared state fails here.
func TestVerifyContextIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	const workers = 32

	tarPath, manifestJSON := buildSignedArtifact(t, false)
	source := writeTestSignature(t, manifestJSON)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", []string{source})
	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			payload := verifyPayload(nil, tarPath)
			if i%2 == 1 {
				payload = verifyPayload(meta, tarPath)
			}
			errs <- verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, payload)
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil from every worker", err)
		}
	}
}

// TestSignatureVerdictDoesNotEvictTheArtifact pins isBlobSetVerdict: a retry
// re-reads the same cached metadata, so a failed verdict must not buy a hostile
// server a cache delete per run; a chain mismatch still evicts once (control).
func TestSignatureVerdictDoesNotEvictTheArtifact(t *testing.T) {
	t.Parallel()

	t.Run("a failing signature evicts nothing", func(t *testing.T) {
		t.Parallel()
		fx := newSignedCacheFixture(t, false, nil)
		fx.meta.Signatures = []any{map[string]any{"signature": "these bytes are not an OpenPGP signature\n"}}

		err := installCollection(context.Background(), testSignedCollection, fx.deps, nil, fx.meta, downloadResult{})
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("installCollection() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
		if got := fx.artifacts.deleteCalls.Load(); got != 0 {
			t.Fatalf("Delete calls = %d, want 0: a verdict over the gathered blobs is not the artifact's fault", got)
		}
		assertExists(t, fx.artifactPath)
	})

	t.Run("a chain mismatch on the same harness still evicts once", func(t *testing.T) {
		t.Parallel()
		fx := newSignedCacheFixture(t, true, nil)
		fx.deps.verify.sources[requirementKey(testSignedCollection)] =
			[]string{writeTestSignature(t, fx.manifestJSON)}

		err := installCollection(context.Background(), testSignedCollection, fx.deps, nil, fx.meta, downloadResult{})
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("installCollection() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
		}
		if got := fx.artifacts.deleteCalls.Load(); got != 1 {
			t.Fatalf("Delete calls = %d, want exactly 1", got)
		}
	})
}

// TestSkippedCollectionsAreReportedWhenVerifying pins the result-tier line that
// counts already-installed collections a verifying run skipped unverified; with
// verification off the line must not appear on every ordinary re-run.
func TestSkippedCollectionsAreReportedWhenVerifying(t *testing.T) {
	t.Parallel()

	t.Run("verifying reports the skipped collections", func(t *testing.T) {
		t.Parallel()
		printer := &capturingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		fx.deps.verify.recordSkippedUnverified()
		fx.deps.verify.recordSkippedUnverified()

		fx.deps.verify.reportSkippedUnverified(runtime)
		if !printer.hasPersistentPrintContaining("not verified on this run") {
			t.Fatalf("no result-tier line about skipped collections; persists=%v", printer.persists)
		}
		if !printer.hasPersistentPrintContaining("2 ") {
			t.Fatalf("the line does not carry the count; persists=%v", printer.persists)
		}
	})

	t.Run("not verifying reports nothing", func(t *testing.T) {
		t.Parallel()
		printer := &capturingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)
		var off *verifyContext

		off.recordSkippedUnverified()
		off.reportSkippedUnverified(runtime)
		if len(printer.persists) != 0 {
			t.Fatalf("persists = %v, want nothing when verification is off", printer.persists)
		}
	})
}
