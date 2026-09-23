package exitcode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/solver"
)

// errTestGeneric is an unrecognized sentinel used to exercise the default
// ExitError fallback in TestFromError.
var errTestGeneric = errors.New("some unclassified error")

// errTestSaveFailure stands in for a snapshot-save failure in
// TestSaveFailureDoesNotMaskIntegrity, declared at package level for err113.
var errTestSaveFailure = errors.New("simulated save failure")

// errTestUnreadableCause stands in for a requirements file that exists but
// fails to parse, a cause other than fs.ErrNotExist, in fromErrorCases.
var errTestUnreadableCause = errors.New("yaml: unexpected end of file")

// exitCase is one FromError classification expectation.
type exitCase struct {
	err      error
	name     string
	wantCode int
}

// fromErrorCases is TestFromError's table: one wrapped error per exit class
// plus the nil, context, fs.ErrNotExist and unclassified edge cases.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var fromErrorCases = []exitCase{
	{name: "nil error", err: nil, wantCode: ExitOK},
	{
		name:     "context canceled",
		err:      fmt.Errorf("%w: ctx", context.Canceled),
		wantCode: ExitInterrupt,
	},
	{
		name:     "lockfile mismatch",
		err:      fmt.Errorf("%w: ctx", helpers.ErrLockfileMismatch),
		wantCode: ExitLock,
	},
	{
		// lock --frozen's own drift-gate verdict: a fresh resolve disagrees
		// with the lockfile already on disk. Exit 6 is the same class as
		// every other lockfile sentinel, per ExitLock's own doc comment.
		name:     "lockfile drift",
		err:      fmt.Errorf("%w: ctx", helpers.ErrLockfileDrift),
		wantCode: ExitLock,
	},
	{
		name:     "installation failed",
		err:      fmt.Errorf("%w: ctx", helpers.ErrInstallationFailed),
		wantCode: ExitInstall,
	},
	{
		name:     "artifact sha256 mismatch",
		err:      fmt.Errorf("%w: ctx", helpers.ErrSHA256Mismatch),
		wantCode: ExitIntegrity,
	},
	{
		// A symlinked ansible_collections, or a component beneath it, is the
		// same unsafe-write class as an unsafe symlink inside an archive.
		name:     "collections path escape",
		err:      fmt.Errorf("%w: ctx", helpers.ErrCollectionsPathEscape),
		wantCode: ExitInstall,
	},
	{
		name:     "context deadline exceeded",
		err:      fmt.Errorf("%w: ctx", context.DeadlineExceeded),
		wantCode: ExitNetwork,
	},
	{
		name:     "download failed",
		err:      fmt.Errorf("%w: ctx", helpers.ErrDownloadFailed),
		wantCode: ExitNetwork,
	},
	{
		// A download_url with a non-http(s) scheme is a metadata fault, not an
		// integrity one: nothing was fetched or hashed.
		name:     "unsupported download url scheme",
		err:      fmt.Errorf("%w: %q", helpers.ErrUnsupportedDownloadURLScheme, "file:///etc/passwd"),
		wantCode: ExitNetwork,
	},
	{
		// Unlike the row above, the artifact is fetchable and is refused for
		// the credential that would ride along to it.
		name:     "download url with userinfo",
		err:      fmt.Errorf("%w: %q", helpers.ErrDownloadURLUserinfo, "https://h/a.tar.gz"),
		wantCode: ExitInstall,
	},
	{
		name:     "metadata url with userinfo",
		err:      fmt.Errorf("%w: %q", helpers.ErrMetadataURLUserinfo, "https://h/api/v3/versions/"),
		wantCode: ExitInstall,
	},
	{
		name:     "galaxy server auth failed",
		err:      fmt.Errorf("%w: server a: ctx", helpers.ErrGalaxyAuthFailed),
		wantCode: ExitNetwork,
	},
	{
		name:     "galaxy server unavailable",
		err:      fmt.Errorf("%w: server a: ctx", helpers.ErrGalaxyServerUnavailable),
		wantCode: ExitNetwork,
	},
	{
		name:     "dependency graph has a cycle",
		err:      fmt.Errorf("%w: ctx", helpers.ErrDependencyGraphHasACycle),
		wantCode: ExitResolution,
	},
	{
		name:     "missing resolved root",
		err:      fmt.Errorf("%w: ctx", helpers.ErrMissingResolvedRoot),
		wantCode: ExitResolution,
	},
	{
		name:     "fs.ErrNotExist",
		err:      fmt.Errorf("%w: ctx", fs.ErrNotExist),
		wantCode: ExitUsage,
	},
	{
		name:     "invalid collection key",
		err:      fmt.Errorf("%w: ctx", helpers.ErrInvalidCollectionKey),
		wantCode: ExitUsage,
	},
	{
		// buildCollectionsMap raises this for a malformed resolved identity
		// before any install work starts: a plan-build usage error.
		name:     "unsafe collection identifier",
		err:      fmt.Errorf("%w: ctx", helpers.ErrUnsafeCollectionIdentifier),
		wantCode: ExitUsage,
	},
	{
		name:     "duplicate collection key",
		err:      fmt.Errorf("%w: ctx", helpers.ErrDuplicateCollectionKey),
		wantCode: ExitUsage,
	},
	{
		// buildCollectionsMap raises this for a resolved version that is not
		// exact (a constraint like "*"), at plan-build time like the row above.
		name:     "invalid collection version",
		err:      fmt.Errorf("%w: ctx", helpers.ErrInvalidCollectionVersion),
		wantCode: ExitUsage,
	},
	{
		name:     "invalid timeout",
		err:      fmt.Errorf("%w: ctx", helpers.ErrInvalidTimeout),
		wantCode: ExitUsage,
	},
	{
		name:     "ansible config not found",
		err:      fmt.Errorf("%w: ctx", helpers.ErrAnsibleConfigNotFound),
		wantCode: ExitUsage,
	},
	{
		// The production shape: a line past the scanner's limit, the one way
		// an ansible.cfg that opened can still fail to load.
		name:     "ansible config unreadable",
		err:      fmt.Errorf("%w: ansible.cfg: %w", helpers.ErrAnsibleConfigUnreadable, bufio.ErrTooLong),
		wantCode: ExitUsage,
	},
	{
		// A requirements file that exists but cannot be read; the os-level
		// cause alone classifies nowhere and would exit 1.
		name: "requirements unreadable",
		err: fmt.Errorf("failed to load requirements file: %w: %w", helpers.ErrRequirementsUnreadable,
			&fs.PathError{Op: "open", Path: "requirements.yml", Err: fs.ErrPermission}),
		wantCode: ExitUsage,
	},
	{
		name:     "requirements not valid YAML",
		err:      fmt.Errorf("load requirements requirements.yml: %w: %w", helpers.ErrInvalidRequirementsYAML, errTestUnreadableCause),
		wantCode: ExitUsage,
	},
	{
		name:     "warm cache disabled",
		err:      fmt.Errorf("%w: ctx", helpers.ErrWarmCacheDisabled),
		wantCode: ExitUsage,
	},
	{
		name:     "unclassified error falls back to ExitError",
		err:      errTestGeneric,
		wantCode: ExitError,
	},
	{
		name:     "bare artifact download deadline",
		err:      helpers.ErrArtifactDownloadDeadline,
		wantCode: ExitNetwork,
	},
	{
		// The producer renders the context cause with %v, so this must be
		// ExitNetwork through the sentinel alone, never ExitInterrupt.
		name: "artifact download deadline wraps its cause with %v, not %w",
		// Pinning the real, deliberately non-wrapping shape; see
		// helpers.ErrArtifactDownloadDeadline's own doc comment for why.
		err:      fmt.Errorf("%w after 15m0s: %v", helpers.ErrArtifactDownloadDeadline, context.DeadlineExceeded), //nolint:errorlint
		wantCode: ExitNetwork,
	},
	{
		name:     "read stalled",
		err:      fmt.Errorf("%w: ctx", helpers.ErrReadStalled),
		wantCode: ExitNetwork,
	},
	{
		name:     "cache backend unavailable",
		err:      fmt.Errorf("%w: ctx", helpers.ErrCacheBackendUnavailable),
		wantCode: ExitNetwork,
	},
	{
		name:     "cache backend unusable",
		err:      fmt.Errorf("%w: ctx", helpers.ErrCacheBackendUnusable),
		wantCode: ExitUsage,
	},
	{
		name:     "cache busy",
		err:      fmt.Errorf("%w: ctx", helpers.ErrCacheBusy),
		wantCode: ExitCacheBusy,
	},
	{
		name:     "another instance is running",
		err:      fmt.Errorf("%w: ctx", helpers.ErrAnotherInstanceIsRunning),
		wantCode: ExitCacheBusy,
	},
	{
		// A lock this run DID acquire and then had taken away mid-run: the
		// same exit class as one it never got at all, because the remedy is
		// the same - rerun once nothing else holds the cache.
		name:     "cache lock lost",
		err:      helpers.ErrCacheLockLost,
		wantCode: ExitCacheBusy,
	},
	{
		// The exit code of cacheManager.LockLostError's commonest output, a
		// canceled run flattened by %v; the rendering itself is pinned by
		// TestLockLostError in internal/galaxy/cache.
		name:     "cache lock lost carrying a flattened cancellation cause",
		err:      fmt.Errorf("%w: %v", helpers.ErrCacheLockLost, context.Canceled), //nolint:errorlint
		wantCode: ExitCacheBusy,
	},
	{
		// The flattened integrity cause is text, not a sentinel, so this pins
		// only that the rendering exits 8 rather than 7; supersession itself
		// is pinned by TestLockLostError.
		name:     "cache lock lost carrying a flattened integrity cause",
		err:      fmt.Errorf("%w: %v", helpers.ErrCacheLockLost, helpers.ErrSHA256Mismatch), //nolint:errorlint
		wantCode: ExitCacheBusy,
	},
	{
		name:     "corrupt project registry",
		err:      fmt.Errorf("%w: ctx", helpers.ErrCorruptProjectRegistry),
		wantCode: ExitCacheCorrupt,
	},
	{
		name:     "state object too large",
		err:      fmt.Errorf("%w: ctx", helpers.ErrStateObjectTooLarge),
		wantCode: ExitCacheCorrupt,
	},
	{
		// The shape internal/cache/s3's readObject builds; the
		// ErrEmptyGzipMember cause pins that no higher class claims it.
		name: "state object that will not inflate",
		err: fmt.Errorf("%w: state object %s: %w",
			helpers.ErrCorruptStateObject, "state/store.json.gz", helpers.ErrEmptyGzipMember),
		wantCode: ExitCacheCorrupt,
	},
	{
		// A newer-than-supported schema version describes this reader, not
		// damaged bytes: see ExitCacheCorrupt's own doc comment for why this
		// is ExitUsage rather than ExitCacheCorrupt.
		name:     "unsupported schema version",
		err:      fmt.Errorf("%w: ctx", helpers.ErrUnsupportedSchemaVersion),
		wantCode: ExitUsage,
	},
	{
		// The production shape wraps a second, non-fs.ErrNotExist cause with
		// %w, so this row reaches isConfigUsageError's own arm.
		name:     "project requirements unreadable, non-fs.ErrNotExist cause",
		err:      fmt.Errorf("%w: requirements.yml: %w", helpers.ErrProjectRequirementsUnreadable, errTestUnreadableCause),
		wantCode: ExitUsage,
	},
	{
		name:     "conflicting namespace name",
		err:      fmt.Errorf("%w: ctx", helpers.ErrConflictingNamespaceName),
		wantCode: ExitUsage,
	},
	{
		name:     "versions paging exceeded",
		err:      fmt.Errorf("%w: ctx", helpers.ErrVersionsPagingExceeded),
		wantCode: ExitNetwork,
	},
	{
		name:     "invalid dependency key",
		err:      fmt.Errorf("%w: ctx", helpers.ErrInvalidDependencyKey),
		wantCode: ExitResolution,
	},
	{
		name:     "unsafe removal path",
		err:      fmt.Errorf("%w: ctx", helpers.ErrUnsafeRemovalPath),
		wantCode: ExitInstall,
	},
	{
		// Raised by the download shape probe after a successful transfer: no
		// retry turns the delivered bytes into an archive, so not network.
		name:     "artifact is not a tar.gz",
		err:      fmt.Errorf("%w: /tmp/a: gzip: invalid header", helpers.ErrArtifactNotTarGz),
		wantCode: ExitInstall,
	},
	{
		// A gzipped tar whose meta headers exhaust the probe's scan bound:
		// install class like the row above, as a retry gets the same prologue.
		name:     "artifact presents no tar header inside the probe's bound",
		err:      fmt.Errorf("%w: /tmp/a", helpers.ErrArtifactTarHeaderNotFound),
		wantCode: ExitInstall,
	},
	{
		name:     "archive duplicate entry",
		err:      fmt.Errorf("%w: ctx", helpers.ErrArchiveDuplicateEntry),
		wantCode: ExitInstall,
	},
	{
		name:     "archive too many entries",
		err:      fmt.Errorf("%w: ctx", helpers.ErrArchiveTooManyEntries),
		wantCode: ExitInstall,
	},
	{
		// Raised by the manifest chain walk and the tree archive writer rather
		// than by extraction; it still classifies with the archive family.
		name:     "archive entry name too long",
		err:      fmt.Errorf("%w: ctx", helpers.ErrArchiveEntryNameTooLong),
		wantCode: ExitInstall,
	},
	{
		// The per-entry cap is charged against every header's declared size,
		// whatever its typeflag, not only against regular files.
		name:     "archive entry too large",
		err:      fmt.Errorf("%w: ctx", helpers.ErrArchiveEntryIsTooLarge),
		wantCode: ExitInstall,
	},
	{
		// Headers that understate what archive/tar consumes hit the
		// decompressed-stream cap: the archive family, not ErrResponseTooLarge.
		name:     "archive decompressed stream too large",
		err:      fmt.Errorf("%w: ctx", helpers.ErrArchiveDecompressedTooLarge),
		wantCode: ExitInstall,
	},
	{
		// Bare, as an overrun outside any install worker arrives: a size
		// ceiling is network class, not a digest mismatch.
		name:     "response too large, bare",
		err:      fmt.Errorf("%w: ctx", helpers.ErrResponseTooLarge),
		wantCode: ExitNetwork,
	},
	{
		// The same sentinel joined behind the install headline, as a worker
		// failure arrives, exits ExitInstall instead.
		name: "response too large, aggregated behind installation failure",
		err: errors.Join(
			fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed),
			helpers.ErrResponseTooLarge,
		),
		wantCode: ExitInstall,
	},
	{
		// outdated's aggregation headline alone: every failed lookup was a
		// metadata-fetch failure, so it classifies through isMetadataFetchError.
		name:     "latest version lookup failed, bare",
		err:      fmt.Errorf("%w for 1 collections", helpers.ErrLatestVersionLookupFailed),
		wantCode: ExitNetwork,
	},
	{
		// The same headline joined with an invalid lockfile entry: isLockError
		// precedes isNetworkError, so the joined cause decides the class.
		name: "latest version lookup failed, joined with an invalid lockfile entry name",
		err: errors.Join(
			fmt.Errorf("%w for 2 collections", helpers.ErrLatestVersionLookupFailed),
			fmt.Errorf("%w: invalid name %q", helpers.ErrLockfileInvalid, "nodothere"),
		),
		wantCode: ExitLock,
	},
}

// TestFromError walks fromErrorCases, checking one representative error per
// exit class.
func TestFromError(t *testing.T) {
	t.Parallel()
	for _, tt := range fromErrorCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FromError(tt.err); got != tt.wantCode {
				t.Errorf("FromError(%v) = %d, want %d", tt.err, got, tt.wantCode)
			}
		})
	}
}

// galaxyServerConfigSentinels lists every Galaxy server configuration
// sentinel exhaustively: a missed one would exit 1 instead of ExitUsage.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var galaxyServerConfigSentinels = []struct {
	err  error
	name string
}{
	{name: "unsupported galaxy_server key", err: helpers.ErrUnsupportedGalaxyServerKey},
	{name: "unsupported api_version", err: helpers.ErrUnsupportedGalaxyServerAPIVersion},
	{name: "missing server url", err: helpers.ErrMissingGalaxyServerURL},
	{name: "invalid server url", err: helpers.ErrInvalidGalaxyServerURL},
	{name: "invalid server id", err: helpers.ErrInvalidGalaxyServerID},
	{name: "duplicate server id", err: helpers.ErrDuplicateGalaxyServerID},
	{name: "invalid validate_certs", err: helpers.ErrInvalidValidateCerts},
	{name: "server url with userinfo", err: helpers.ErrGalaxyServerURLUserinfo},
	{name: "token over insecure transport", err: helpers.ErrInsecureTokenTransport},
	{name: "conflicting tls policy", err: helpers.ErrConflictingServerTLSPolicy},
	{name: "conflicting token", err: helpers.ErrConflictingServerToken},
	{name: "ambiguous --token", err: helpers.ErrAmbiguousGalaxyToken},
	{name: "token destination from ansible.cfg", err: helpers.ErrTokenDestinationFromAnsibleConfig},
	{name: "token tls policy from ansible.cfg", err: helpers.ErrTokenTLSPolicyFromAnsibleConfig},
}

// TestGalaxyServerConfigErrorsMapToUsage pins every Galaxy server config
// sentinel to ExitUsage: raised before any request, so a pipeline can tell
// "fix ansible.cfg" apart from a transient failure worth retrying.
func TestGalaxyServerConfigErrorsMapToUsage(t *testing.T) {
	t.Parallel()
	for _, tt := range galaxyServerConfigSentinels {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("%w: ctx", tt.err)
			if got := FromError(wrapped); got != ExitUsage {
				t.Errorf("FromError(%v) = %d, want %d", wrapped, got, ExitUsage)
			}
		})
	}
}

// integritySentinels lists every artifact-digest sentinel exhaustively, as
// galaxyServerConfigSentinels does, since each must exit ExitIntegrity.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var integritySentinels = []struct {
	err  error
	name string
}{
	{name: "sha256 mismatch", err: helpers.ErrSHA256Mismatch},
	{name: "malformed artifact sha256", err: helpers.ErrMalformedArtifactSHA256},
}

// TestIntegritySentinelsMapToExitIntegrity pins every artifact-digest
// sentinel to ExitIntegrity, wrapped so the check goes through errors.Is
// rather than requiring exact identity.
func TestIntegritySentinelsMapToExitIntegrity(t *testing.T) {
	t.Parallel()
	for _, tt := range integritySentinels {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("%w: ctx", tt.err)
			if got := FromError(wrapped); got != ExitIntegrity {
				t.Errorf("FromError(%v) = %d, want %d", wrapped, got, ExitIntegrity)
			}
		})
	}
}

// TestIntegrityOutranksInstallFailureHeadline pins that an integrity cause
// joined behind helpers.ErrInstallationFailed exits ExitIntegrity, while the
// same headline joined with helpers.ErrDownloadFailed stays ExitInstall.
func TestIntegrityOutranksInstallFailureHeadline(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)

	integrity := errors.Join(headline, fmt.Errorf("%w: acme.app@1.0.0", helpers.ErrSHA256Mismatch))
	if got := FromError(integrity); got != ExitIntegrity {
		t.Errorf("FromError(integrity join) = %d, want %d", got, ExitIntegrity)
	}

	notIntegrity := errors.Join(headline, helpers.ErrDownloadFailed)
	if got := FromError(notIntegrity); got != ExitInstall {
		t.Errorf("FromError(non-integrity join) = %d, want %d", got, ExitInstall)
	}
}

// TestIntegrityOutranksNetworkCause proves the same precedence holds when the
// joined tree also contains a network-class sentinel: the integrity cause
// still wins.
func TestIntegrityOutranksNetworkCause(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joined := errors.Join(headline, helpers.ErrDownloadFailed, helpers.ErrSHA256Mismatch)
	if got := FromError(joined); got != ExitIntegrity {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitIntegrity)
	}
}

// TestIntegrityPrecedenceIndependentOfCauseOrder pins that the integrity
// sentinel wins wherever it sits in the joined tree, nested joins included.
func TestIntegrityPrecedenceIndependentOfCauseOrder(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)

	forward := errors.Join(headline, helpers.ErrDownloadFailed, helpers.ErrSHA256Mismatch)
	if got := FromError(forward); got != ExitIntegrity {
		t.Errorf("FromError(forward order) = %d, want %d", got, ExitIntegrity)
	}

	reversed := errors.Join(headline, helpers.ErrSHA256Mismatch, helpers.ErrDownloadFailed)
	if got := FromError(reversed); got != ExitIntegrity {
		t.Errorf("FromError(reversed order) = %d, want %d", got, ExitIntegrity)
	}

	nested := errors.Join(headline, errors.Join(helpers.ErrDownloadFailed, helpers.ErrSHA256Mismatch))
	if got := FromError(nested); got != ExitIntegrity {
		t.Errorf("FromError(nested) = %d, want %d", got, ExitIntegrity)
	}
}

// TestSaveFailureDoesNotMaskIntegrity pins that annotateSaveFailure's
// "%w; snapshot save failed: %w" wrap keeps an integrity cause ExitIntegrity.
func TestSaveFailureDoesNotMaskIntegrity(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joinedInstall := errors.Join(headline, helpers.ErrSHA256Mismatch)

	err := fmt.Errorf("%w; snapshot save failed: %w", joinedInstall, errTestSaveFailure)
	if got := FromError(err); got != ExitIntegrity {
		t.Errorf("FromError(err) = %d, want %d", got, ExitIntegrity)
	}
}

// TestLockDriftOutranksSaveFailure pins that lock --frozen's drift verdict
// wrapped by annotateSaveFailure still exits ExitLock: the drift, not the
// save failure's network-class cause, is what the operator must act on.
func TestLockDriftOutranksSaveFailure(t *testing.T) {
	t.Parallel()
	drift := fmt.Errorf("%w: galaxy.lock: run `go-galaxy lock` to update it", helpers.ErrLockfileDrift)

	err := fmt.Errorf("%w; snapshot save failed: %w", drift, helpers.ErrStateObjectDeadline)
	if got := FromError(err); got != ExitLock {
		t.Errorf("FromError(err) = %d, want %d", got, ExitLock)
	}
}

// TestCanceledOutranksIntegrity proves context.Canceled still outranks
// ExitIntegrity, matching FromError's documented priority order: cancellation
// first, integrity second.
func TestCanceledOutranksIntegrity(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	integrity := errors.Join(headline, helpers.ErrSHA256Mismatch)

	err := errors.Join(integrity, context.Canceled)
	if got := FromError(err); got != ExitInterrupt {
		t.Errorf("FromError(err) = %d, want %d", got, ExitInterrupt)
	}
}

// TestSolverConflictMapsToResolution pins *solver.ConflictError to
// ExitResolution, bare and wrapped, through ConflictError's own Is method.
func TestSolverConflictMapsToResolution(t *testing.T) {
	t.Parallel()
	err := error(&solver.ConflictError{})
	if got := FromError(err); got != ExitResolution {
		t.Fatalf("FromError(*solver.ConflictError) = %d, want ExitResolution (%d)", got, ExitResolution)
	}
	if got := FromError(fmt.Errorf("resolve: %w", err)); got != ExitResolution {
		t.Fatalf("FromError(wrapped) = %d, want ExitResolution (%d)", got, ExitResolution)
	}
}

// TestArtifactDownloadDeadlineClassification pins the deadline sentinel as
// ExitNetwork bare and ExitInstall behind the install headline, never
// ExitInterrupt: its %v-rendered cause keeps a slow server from reading as one.
func TestArtifactDownloadDeadlineClassification(t *testing.T) {
	t.Parallel()
	bare := helpers.ErrArtifactDownloadDeadline
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}
	if got := FromError(bare); got == ExitInterrupt {
		t.Errorf("FromError(bare sentinel) = ExitInterrupt, want anything else")
	}

	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape; see ErrArtifactDownloadDeadline's doc comment.
	deadlineCause := fmt.Errorf("%w after 15m0s: %v", helpers.ErrArtifactDownloadDeadline, context.DeadlineExceeded)
	joined := errors.Join(headline, deadlineCause)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
	if got := FromError(joined); got == ExitInterrupt {
		t.Errorf("FromError(joined) = ExitInterrupt, want anything else")
	}
}

// TestReadStalledClassification pins ErrReadStalled bare and in watchdogBody's
// %v shape as ExitNetwork and joined as ExitInstall; the real producer is
// covered only by TestMixedDripAndStallDoesNotClassifyAsInterrupt.
func TestReadStalledClassification(t *testing.T) {
	t.Parallel()
	bare := helpers.ErrReadStalled
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}
	if got := FromError(bare); got == ExitInterrupt {
		t.Errorf("FromError(bare sentinel) = ExitInterrupt, want anything else")
	}

	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	production := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, 30*time.Second, context.Canceled)
	if got := FromError(production); got != ExitNetwork {
		t.Errorf("FromError(production shape) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}
	if got := FromError(production); got == ExitInterrupt {
		t.Errorf("FromError(production shape) = ExitInterrupt, want anything else")
	}

	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joined := errors.Join(headline, production)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
	if got := FromError(joined); got == ExitInterrupt {
		t.Errorf("FromError(joined) = ExitInterrupt, want anything else")
	}
}

// TestMixedDeadlineAndStallShapeIsNotInterrupt pins that a deadline cause and
// a stall cause joined behind one install headline exit ExitInstall, never
// ExitInterrupt, though both render a context error with %v.
func TestMixedDeadlineAndStallShapeIsNotInterrupt(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 2 collections", helpers.ErrInstallationFailed)
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape; see ErrArtifactDownloadDeadline's doc comment.
	deadlineCause := fmt.Errorf("%w after 3s: %v", helpers.ErrArtifactDownloadDeadline, context.DeadlineExceeded)
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stallCause := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, 30*time.Second, context.Canceled)

	joined := errors.Join(headline, deadlineCause, stallCause)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
	if got := FromError(joined); got == ExitInterrupt {
		t.Errorf("FromError(joined) = ExitInterrupt, want anything else")
	}
}

// TestFromErrorInterruptSurvivesStallSentinel pins that a stall joined with a
// genuine context.Canceled still exits ExitInterrupt, so no stall class may
// sit above isCanceled in exitClasses.
func TestFromErrorInterruptSurvivesStallSentinel(t *testing.T) {
	t.Parallel()
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stallCause := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, 30*time.Second, context.Canceled)

	joined := errors.Join(stallCause, context.Canceled)
	if got := FromError(joined); got != ExitInterrupt {
		t.Errorf("FromError(joined) = %d, want ExitInterrupt (%d)", got, ExitInterrupt)
	}
}

// TestMetadataFetchDeadlineClassification pins ErrMetadataFetchDeadline as
// ExitNetwork bare and %v-rendered and ExitInstall joined; the %w control
// shows the "not ExitInterrupt" assertion is not vacuous.
func TestMetadataFetchDeadlineClassification(t *testing.T) {
	t.Parallel()
	bare := helpers.ErrMetadataFetchDeadline
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}

	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape; see ErrMetadataFetchDeadline's own doc comment.
	renderedCause := fmt.Errorf("%w after 2m0s: %v", helpers.ErrMetadataFetchDeadline, context.DeadlineExceeded)
	joined := errors.Join(headline, renderedCause)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want ExitInstall (%d)", got, ExitInstall)
	}

	if got := FromError(renderedCause); got != ExitNetwork {
		t.Errorf("FromError(rendered cause) = %d, want ExitNetwork (%d), not ExitInterrupt", got, ExitNetwork)
	}

	// Control: the identical shape, %w-wrapping context.Canceled instead of
	// %v-rendering context.DeadlineExceeded, DOES classify as ExitInterrupt.
	wrappedCause := fmt.Errorf("%w after 2m0s: %w", helpers.ErrMetadataFetchDeadline, context.Canceled)
	if got := FromError(wrappedCause); got != ExitInterrupt {
		t.Errorf("control: FromError(%%w-wrapped cause) = %d, want ExitInterrupt (%d)", got, ExitInterrupt)
	}
}

// TestStateObjectDeadlineClassification pins ErrStateObjectDeadline like its
// metadata sibling, plus a tail SaveStore failure that annotateSaveFailure
// joins behind the install headline, which exits ExitInstall.
func TestStateObjectDeadlineClassification(t *testing.T) {
	t.Parallel()
	bare := helpers.ErrStateObjectDeadline
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}

	// Also the tree a byte-dripped tail SaveStore returns on a run with no
	// collection failures, where annotateSaveFailure is not reached.
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape; see ErrStateObjectDeadline's own doc comment.
	renderedCause := fmt.Errorf("%w after 1m0s: %v", helpers.ErrStateObjectDeadline, context.DeadlineExceeded)
	if got := FromError(renderedCause); got != ExitNetwork {
		t.Errorf("FromError(rendered cause) = %d, want ExitNetwork (%d), not ExitInterrupt", got, ExitNetwork)
	}

	// Control: the identical shape, %w-wrapping context.Canceled instead of
	// %v-rendering context.DeadlineExceeded, DOES classify as ExitInterrupt.
	wrappedCause := fmt.Errorf("%w after 1m0s: %w", helpers.ErrStateObjectDeadline, context.Canceled)
	if got := FromError(wrappedCause); got != ExitInterrupt {
		t.Errorf("control: FromError(%%w-wrapped cause) = %d, want ExitInterrupt (%d)", got, ExitInterrupt)
	}

	// A tail SaveStore failure in annotateSaveFailure's exact wrap, around a
	// headline that already carries helpers.ErrInstallationFailed.
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	tailSaveFailure := fmt.Errorf("%w; snapshot save failed: %w", headline, renderedCause)
	if got := FromError(tailSaveFailure); got != ExitInstall {
		t.Errorf("FromError(tail save failure) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
}

// TestCacheBusyFoldedBehindInstallFailureClassifiesAsInstall pins that a
// contention failure joined behind the install headline exits ExitInstall; the
// shape is synthetic, as the lock is taken once at init and fails bare.
func TestCacheBusyFoldedBehindInstallFailureClassifiesAsInstall(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joined := errors.Join(headline, helpers.ErrCacheBusy)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitInstall)
	}
}

// TestNetworkOutranksCacheBusyWithoutAnInstallHeadline pins that a tree with a
// network-class and a cache-busy sentinel and no install headline exits
// ExitNetwork, since isNetworkError precedes isCacheBusyError.
func TestNetworkOutranksCacheBusyWithoutAnInstallHeadline(t *testing.T) {
	t.Parallel()
	joined := errors.Join(helpers.ErrCacheBusy, helpers.ErrCacheBackendUnavailable)
	if got := FromError(joined); got != ExitNetwork {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitNetwork)
	}
}

// TestCacheBusyOutranksUsage pins that ErrCacheBusy wrapping fs.ErrNotExist
// exits ExitCacheBusy, not ExitUsage through isUsageError's broad arm.
func TestCacheBusyOutranksUsage(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w: %w", helpers.ErrCacheBusy, fs.ErrNotExist)
	if got := FromError(err); got != ExitCacheBusy {
		t.Errorf("FromError(err) = %d, want %d", got, ExitCacheBusy)
	}
}

// TestCanceledOutranksCacheBusy pins that context.Canceled joined with
// helpers.ErrCacheBusy exits ExitInterrupt.
func TestCanceledOutranksCacheBusy(t *testing.T) {
	t.Parallel()
	joined := errors.Join(context.Canceled, helpers.ErrCacheBusy)
	if got := FromError(joined); got != ExitInterrupt {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitInterrupt)
	}
}

// TestCacheCorruptOutranksUsage pins that ErrCorruptProjectRegistry wrapping
// fs.ErrNotExist exits ExitCacheCorrupt, not ExitUsage.
func TestCacheCorruptOutranksUsage(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w: %w", helpers.ErrCorruptProjectRegistry, fs.ErrNotExist)
	if got := FromError(err); got != ExitCacheCorrupt {
		t.Errorf("FromError(err) = %d, want %d", got, ExitCacheCorrupt)
	}
}

// TestCanceledOutranksCacheCorrupt pins that context.Canceled joined with
// helpers.ErrCorruptProjectRegistry exits ExitInterrupt.
func TestCanceledOutranksCacheCorrupt(t *testing.T) {
	t.Parallel()
	joined := errors.Join(context.Canceled, helpers.ErrCorruptProjectRegistry)
	if got := FromError(joined); got != ExitInterrupt {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitInterrupt)
	}
}

// genericSentinels lists the helpers sentinels deliberately left at ExitError,
// each absorbed by its producer or an internal nil guard.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var genericSentinels = []struct {
	err  error
	name string
}{
	{
		// Absorbed at the producer: the s3 backend's LoadStore and store.Load
		// turn it into a drop-and-rebuild before any caller sees it.
		name: "outdated schema version",
		err:  helpers.ErrOutdatedSchemaVersion,
	},
	{
		// Absorbed at the producer: the cleanup scan warns and skips a
		// MANIFEST.json that fails to parse rather than returning it.
		name: "corrupt manifest",
		err:  helpers.ErrCorruptManifest,
	},
	{
		// An internal nil guard in store.Save with no operator meaning: a nil
		// *bbolt.DB reaching Save is this program's own bookkeeping bug, not
		// a condition a CI pipeline should branch on.
		name: "bolt db is nil",
		err:  helpers.ErrDbNil,
	},
	{
		// An internal nil guard in store.Save with no operator meaning,
		// identical reasoning to "bolt db is nil" above.
		name: "store is nil",
		err:  helpers.ErrStoreNil,
	},
}

// TestGenericSentinelsMapToExitError pins every deliberately-unclassified
// sentinel to ExitError, wrapped so the check goes through errors.Is rather
// than requiring exact identity.
func TestGenericSentinelsMapToExitError(t *testing.T) {
	t.Parallel()
	for _, tt := range genericSentinels {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("%w: ctx", tt.err)
			if got := FromError(wrapped); got != ExitError {
				t.Errorf("FromError(%v) = %d, want %d", wrapped, got, ExitError)
			}
		})
	}
}

// fakeSignal is a non-syscall.Signal os.Signal implementation used to verify
// FromSignal's fallback path.
type fakeSignal struct{}

func (fakeSignal) String() string { return "fake" }
func (fakeSignal) Signal()        {}

// TestFromSignal pins the shell-convention 128+signal mapping, SIGQUIT
// included though main does not subscribe to it, and the non-syscall fallback.
func TestFromSignal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sig      os.Signal
		name     string
		wantCode int
	}{
		{name: "SIGINT", sig: syscall.SIGINT, wantCode: 130},
		{name: "SIGTERM", sig: syscall.SIGTERM, wantCode: 143},
		{name: "SIGHUP", sig: syscall.SIGHUP, wantCode: 129},
		{name: "SIGQUIT", sig: syscall.SIGQUIT, wantCode: 131},
		{name: "non-syscall.Signal falls back to ExitError", sig: fakeSignal{}, wantCode: ExitError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FromSignal(tt.sig); got != tt.wantCode {
				t.Errorf("FromSignal(%v) = %d, want %d", tt.sig, got, tt.wantCode)
			}
		})
	}
}

// TestLockfileUserinfoClassifiesAsLock pins that a lockfile source carrying
// userinfo, wrapped as both ErrLockfileInvalid and ErrGalaxyServerURLUserinfo,
// exits ExitLock, while the userinfo sentinel alone exits ExitUsage.
func TestLockfileUserinfoClassifiesAsLock(t *testing.T) {
	t.Parallel()
	joined := fmt.Errorf("%w: acme.widgets: %w", helpers.ErrLockfileInvalid, helpers.ErrGalaxyServerURLUserinfo)

	if got := FromError(joined); got != ExitLock {
		t.Errorf("FromError(lockfile userinfo) = %d, want ExitLock (%d)", got, ExitLock)
	}
	if got := FromError(helpers.ErrGalaxyServerURLUserinfo); got != ExitUsage {
		t.Errorf("FromError(bare userinfo sentinel) = %d, want ExitUsage (%d)", got, ExitUsage)
	}
}

// serverSuppliedURLPolicyCase is one row of
// TestServerSuppliedURLPolicyClassifiesUniformly: a server-supplied-URL
// sentinel under one aggregation shape.
type serverSuppliedURLPolicyCase struct {
	err  error
	name string
}

// serverSuppliedURLPolicyCases is the full cross-product of both sentinels and
// three shapes (bare, install headline, outdated headline), reachable today or
// not, since the class must not depend on what a sentinel is joined behind.
func serverSuppliedURLPolicyCases() []serverSuppliedURLPolicyCase {
	installHeadline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	outdatedHeadline := fmt.Errorf("%w for 1 collections", helpers.ErrLatestVersionLookupFailed)
	download := fmt.Errorf("%w: %q", helpers.ErrDownloadURLUserinfo, "https://h/a.tar.gz")
	metadata := fmt.Errorf("%w: %q", helpers.ErrMetadataURLUserinfo, "https://h/api/v3/versions/")

	return []serverSuppliedURLPolicyCase{
		{name: "download url, bare", err: download},
		{name: "metadata url, bare", err: metadata},
		{name: "download url, behind the install-failure headline", err: errors.Join(installHeadline, download)},
		{name: "metadata url, behind the install-failure headline", err: errors.Join(installHeadline, metadata)},
		{name: "download url, behind the latest-version-lookup headline", err: errors.Join(outdatedHeadline, download)},
		{name: "metadata url, behind the latest-version-lookup headline", err: errors.Join(outdatedHeadline, metadata)},
	}
}

// TestServerSuppliedURLPolicyClassifiesUniformly pins ExitInstall for a URL a
// Galaxy server, or a snapshot replaying one, supplies with a credential in
// it, whatever tree it arrives in.
func TestServerSuppliedURLPolicyClassifiesUniformly(t *testing.T) {
	t.Parallel()

	for _, tt := range serverSuppliedURLPolicyCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FromError(tt.err); got != ExitInstall {
				t.Errorf("FromError(%v) = %d, want ExitInstall (%d)", tt.err, got, ExitInstall)
			}
		})
	}
}

// wantExitClassOrder is exitClasses's precedence as codes, a separate literal
// so a reordering cannot agree by construction; ExitInstall appears twice.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var wantExitClassOrder = []int{
	ExitInterrupt,
	ExitIntegrity,
	ExitSignature,
	ExitLock,
	ExitInstall,
	ExitInstall,
	ExitNetwork,
	ExitCacheBusy,
	ExitCacheCorrupt,
	ExitResolution,
	ExitUsage,
}

// TestExitClassOrderIsPinned is a deliberate change detector on exitClasses's
// order: swapping two entries compiles yet changes the code a CI reads.
func TestExitClassOrderIsPinned(t *testing.T) {
	t.Parallel()
	if len(exitClasses) != len(wantExitClassOrder) {
		t.Fatalf("len(exitClasses) = %d, want %d", len(exitClasses), len(wantExitClassOrder))
	}
	for i, want := range wantExitClassOrder {
		if got := exitClasses[i].code; got != want {
			t.Errorf("exitClasses[%d].code = %d, want %d", i, got, want)
		}
	}
}

// exitPrecedenceCase is one adjacent pair of exitClasses: the code the join of
// higher and lower must yield, and the code each yields alone.
type exitPrecedenceCase struct {
	higher     error
	lower      error
	name       string
	wantJoined int
	wantHigher int
	wantLower  int
}

// adjacentExitPrecedenceCases has one row per adjacent pair of exitClasses,
// except the server-supplied URL policy and install pair, which share a code.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var adjacentExitPrecedenceCases = []exitPrecedenceCase{
	{
		name:       "interrupt over integrity",
		higher:     context.Canceled,
		lower:      helpers.ErrSHA256Mismatch,
		wantJoined: ExitInterrupt,
		wantHigher: ExitInterrupt,
		wantLower:  ExitIntegrity,
	},
	{
		name:       "integrity over signature",
		higher:     helpers.ErrSHA256Mismatch,
		lower:      helpers.ErrSignatureVerificationFailed,
		wantJoined: ExitIntegrity,
		wantHigher: ExitIntegrity,
		wantLower:  ExitSignature,
	},
	{
		name:       "signature over lock",
		higher:     helpers.ErrSignatureVerificationFailed,
		lower:      helpers.ErrLockfileDrift,
		wantJoined: ExitSignature,
		wantHigher: ExitSignature,
		wantLower:  ExitLock,
	},
	{
		name:       "lock over server-supplied url policy",
		higher:     helpers.ErrLockfileDrift,
		lower:      helpers.ErrDownloadURLUserinfo,
		wantJoined: ExitLock,
		wantHigher: ExitLock,
		wantLower:  ExitInstall,
	},
	{
		name:       "lock over install",
		higher:     helpers.ErrLockfileDrift,
		lower:      helpers.ErrInstallationFailed,
		wantJoined: ExitLock,
		wantHigher: ExitLock,
		wantLower:  ExitInstall,
	},
	{
		name:       "install over network",
		higher:     helpers.ErrInstallationFailed,
		lower:      helpers.ErrDownloadFailed,
		wantJoined: ExitInstall,
		wantHigher: ExitInstall,
		wantLower:  ExitNetwork,
	},
	{
		name:       "network over cache busy",
		higher:     helpers.ErrCacheBackendUnavailable,
		lower:      helpers.ErrCacheBusy,
		wantJoined: ExitNetwork,
		wantHigher: ExitNetwork,
		wantLower:  ExitCacheBusy,
	},
	{
		name:       "cache busy over cache corrupt",
		higher:     helpers.ErrCacheBusy,
		lower:      helpers.ErrCorruptProjectRegistry,
		wantJoined: ExitCacheBusy,
		wantHigher: ExitCacheBusy,
		wantLower:  ExitCacheCorrupt,
	},
	{
		name:       "cache corrupt over resolution",
		higher:     helpers.ErrStateObjectTooLarge,
		lower:      helpers.ErrDependencyGraphHasACycle,
		wantJoined: ExitCacheCorrupt,
		wantHigher: ExitCacheCorrupt,
		wantLower:  ExitResolution,
	},
	{
		name:       "resolution over usage",
		higher:     helpers.ErrDependencyGraphHasACycle,
		lower:      fs.ErrNotExist,
		wantJoined: ExitResolution,
		wantHigher: ExitResolution,
		wantLower:  ExitUsage,
	},
}

// TestAdjacentExitClassPrecedence pins that each adjacent pair's join yields
// the higher code; the solo assertions prove the lower sentinel is recognized.
func TestAdjacentExitClassPrecedence(t *testing.T) {
	t.Parallel()
	for _, tt := range adjacentExitPrecedenceCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			joined := errors.Join(tt.higher, tt.lower)
			if got := FromError(joined); got != tt.wantJoined {
				t.Errorf("FromError(joined) = %d, want %d", got, tt.wantJoined)
			}
			if got := FromError(tt.higher); got != tt.wantHigher {
				t.Errorf("FromError(higher alone) = %d, want %d", got, tt.wantHigher)
			}
			if got := FromError(tt.lower); got != tt.wantLower {
				t.Errorf("FromError(lower alone) = %d, want %d", got, tt.wantLower)
			}
		})
	}
}

// aggregatedBehindInstallFailure joins cause behind the
// helpers.ErrInstallationFailed headline, as a per-collection failure arrives.
func aggregatedBehindInstallFailure(cause error) error {
	return errors.Join(fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed), cause)
}

// signatureExitCases pins that only a failed verdict and an attribution
// mismatch exit ExitSignature; the rest classify by what actually failed.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var signatureExitCases = []exitCase{
	{
		name:     "signature verification failed, bare",
		err:      fmt.Errorf("%w: acme.app@1.0.0", helpers.ErrSignatureVerificationFailed),
		wantCode: ExitSignature,
	},
	{
		// A per-collection verdict arrives behind the install headline, so
		// the signature class must sit above isInstallError to be reached.
		name:     "signature verification failed, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrSignatureVerificationFailed),
		wantCode: ExitSignature,
	},
	{
		// Signatures that verified but vouched for another collection share
		// the class: either way nobody the operator trusts vouched for them.
		name:     "signature attribution mismatch, bare",
		err:      fmt.Errorf("%w: acme.app@1.0.0", helpers.ErrSignatureAttributionMismatch),
		wantCode: ExitSignature,
	},
	{
		// Aggregated, for the reason the verdict's own aggregated row states:
		// this is raised inside a per-collection worker, so the joined shape is
		// the only one that actually occurs.
		name:     "signature attribution mismatch, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrSignatureAttributionMismatch),
		wantCode: ExitSignature,
	},
	{
		// Not a verdict: a requirements file declaring more signature sources
		// than are ever gathered is a configuration the operator edits, refused
		// before any signature was looked at.
		name:     "too many signature sources, bare",
		err:      fmt.Errorf("%w: 65 distinct sources declared", helpers.ErrTooManySignatureSources),
		wantCode: ExitUsage,
	},
	{
		name:     "signature source unavailable, bare",
		err:      fmt.Errorf("%w: ctx", helpers.ErrSignatureSourceUnavailable),
		wantCode: ExitNetwork,
	},
	{
		name:     "signature source unavailable, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrSignatureSourceUnavailable),
		wantCode: ExitInstall,
	},
	{
		name:     "signature fetch deadline, bare",
		err:      helpers.ErrSignatureFetchDeadline,
		wantCode: ExitNetwork,
	},
	{
		name:     "signature fetch deadline, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrSignatureFetchDeadline),
		wantCode: ExitInstall,
	},
	{
		name:     "unsupported signature source, bare",
		err:      fmt.Errorf("%w: %q", helpers.ErrUnsupportedSignatureSource, "ftp://keys/sig.asc"),
		wantCode: ExitUsage,
	},
	{
		name:     "unsupported signature source, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrUnsupportedSignatureSource),
		wantCode: ExitInstall,
	},
	{
		// The producer renders a credential-free value; this row pins only
		// the class.
		name:     "signature source userinfo, bare",
		err:      fmt.Errorf("%w: %q", helpers.ErrSignatureSourceUserinfo, "https://hub.example/sig.asc"),
		wantCode: ExitUsage,
	},
	{
		name:     "signature source userinfo, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrSignatureSourceUserinfo),
		wantCode: ExitInstall,
	},
	{
		name:     "keyring unreadable, bare",
		err:      fmt.Errorf("%w: ctx", helpers.ErrKeyringUnreadable),
		wantCode: ExitUsage,
	},
	{
		name:     "keyring unreadable, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrKeyringUnreadable),
		wantCode: ExitInstall,
	},
	{
		name:     "keyring is a keybox, bare",
		err:      fmt.Errorf("%w: ctx", helpers.ErrKeyringIsKeybox),
		wantCode: ExitUsage,
	},
	{
		name:     "keyring is a keybox, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrKeyringIsKeybox),
		wantCode: ExitInstall,
	},
	{
		name:     "keyring required, bare",
		err:      fmt.Errorf("%w: ctx", helpers.ErrKeyringRequired),
		wantCode: ExitUsage,
	},
	{
		name:     "keyring required, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrKeyringRequired),
		wantCode: ExitInstall,
	},
	{
		name:     "invalid signature count, bare",
		err:      fmt.Errorf("%w: %q", helpers.ErrInvalidSignatureCount, "some"),
		wantCode: ExitUsage,
	},
	{
		name:     "invalid signature count, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrInvalidSignatureCount),
		wantCode: ExitInstall,
	},
	{
		name:     "unknown signature status code, bare",
		err:      fmt.Errorf("%w: %q", helpers.ErrUnknownSignatureStatusCode, "NOT_A_CODE"),
		wantCode: ExitUsage,
	},
	{
		name:     "unknown signature status code, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrUnknownSignatureStatusCode),
		wantCode: ExitInstall,
	},
	{
		name:     "invalid disable_gpg_verify, bare",
		err:      fmt.Errorf("%w: %q", helpers.ErrInvalidDisableGPGVerify, "maybe"),
		wantCode: ExitUsage,
	},
	{
		name:     "invalid disable_gpg_verify, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrInvalidDisableGPGVerify),
		wantCode: ExitInstall,
	},
	{
		name:     "empty signature value, bare",
		err:      fmt.Errorf("%w: --keyring is empty", helpers.ErrEmptySignatureValue),
		wantCode: ExitUsage,
	},
	{
		name:     "empty signature value, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrEmptySignatureValue),
		wantCode: ExitInstall,
	},
	{
		name:     "manifest not found, bare",
		err:      fmt.Errorf("%w: ctx", helpers.ErrManifestNotFound),
		wantCode: ExitInstall,
	},
	{
		// The one non-discriminating pair: both shapes reach ExitInstall
		// through isInstallError, so this row only documents the shape.
		name:     "manifest not found, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrManifestNotFound),
		wantCode: ExitInstall,
	},
	{
		name:     "manifest chain mismatch, bare",
		err:      fmt.Errorf("%w: FILES.json", helpers.ErrManifestChainMismatch),
		wantCode: ExitIntegrity,
	},
	{
		// isIntegrityError sits above isInstallError, so a chain mismatch
		// inside a worker still reports as an integrity failure.
		name:     "manifest chain mismatch, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrManifestChainMismatch),
		wantCode: ExitIntegrity,
	},
	{
		// Both classes in one tree: integrity wins, because "these are not the
		// bytes that were named" outranks "nobody vouched for them".
		name:     "integrity cause alongside a signature verdict",
		err:      errors.Join(helpers.ErrSHA256Mismatch, helpers.ErrSignatureVerificationFailed),
		wantCode: ExitIntegrity,
	},
	{
		// The top of exitClasses is unchanged by the new entry: a genuine
		// caller cancellation racing a signature verdict is still an interrupt.
		name:     "cancellation alongside a signature verdict",
		err:      errors.Join(context.Canceled, helpers.ErrSignatureVerificationFailed),
		wantCode: ExitInterrupt,
	},
}

// TestSignatureExitClassification walks signatureExitCases, naming the row in
// failures because an aggregated errors.Join renders across several lines.
func TestSignatureExitClassification(t *testing.T) {
	t.Parallel()
	for _, tt := range signatureExitCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FromError(tt.err); got != tt.wantCode {
				t.Errorf("FromError(%s) = %d, want %d", tt.name, got, tt.wantCode)
			}
		})
	}
}

// TestMetadataRequestBuildFailedClassifiesNetwork pins
// helpers.ErrMetadataRequestBuildFailed to ExitNetwork, bare as
// internal/galaxy/cache raises it and wrapped by loadCollectionMetadata.
func TestMetadataRequestBuildFailedClassifiesNetwork(t *testing.T) {
	t.Parallel()

	if got := FromError(helpers.ErrMetadataRequestBuildFailed); got != ExitNetwork {
		t.Errorf("FromError(bare) = %d, want %d", got, ExitNetwork)
	}
	wrapped := fmt.Errorf("failed to load root metadata: %w", helpers.ErrMetadataRequestBuildFailed)
	if got := FromError(wrapped); got != ExitNetwork {
		t.Errorf("FromError(wrapped) = %d, want %d", got, ExitNetwork)
	}
}

// TestCommandLineUsageErrorsMapToExitUsage pins ErrUnexpectedArguments and
// ErrMissingArgument to ExitUsage, bare and wrapped, as the argument
// validators in cmd/go-galaxy/commands raise both shapes.
func TestCommandLineUsageErrorsMapToExitUsage(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []error{helpers.ErrUnexpectedArguments, helpers.ErrMissingArgument} {
		for _, err := range []error{sentinel, fmt.Errorf("%w: explain takes one", sentinel)} {
			if got := FromError(err); got != ExitUsage {
				t.Errorf("FromError(%v) = %d, want %d", err, got, ExitUsage)
			}
		}
	}
}
