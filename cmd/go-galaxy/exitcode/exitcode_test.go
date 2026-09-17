package exitcode

import (
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
// TestSaveFailureDoesNotMaskIntegrity. Declared as a static package-level
// sentinel, rather than an inline errors.New call, purely to satisfy err113 -
// production code never compares against it.
var errTestSaveFailure = errors.New("simulated save failure")

// errTestUnreadableCause stands in for a project requirements file's
// non-fs.ErrNotExist read/parse failure (a malformed YAML document, not a
// missing file) in fromErrorCases's "project requirements unreadable,
// non-fs.ErrNotExist cause" row. Declared as a static package-level
// sentinel, rather than an inline errors.New call, purely to satisfy err113 -
// production code never compares against it.
var errTestUnreadableCause = errors.New("yaml: unexpected end of file")

// exitCase is one FromError classification expectation.
type exitCase struct {
	err      error
	name     string
	wantCode int
}

// fromErrorCases is TestFromError's table, hoisted to package level so the
// test function itself stays within the complexity budget as classes are
// added. It checks one representative wrapped error per exit class, plus
// the nil/context/fs.ErrNotExist/unclassified edge cases; wrapping with
// fmt.Errorf("%w: ctx", sentinel) verifies FromError matches through
// errors.Is rather than requiring exact identity.
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
		// ErrCollectionsPathEscape is grouped with the archive/symlink sentinels
		// in isSymlinkError, since a symlinked ansible_collections (or a
		// namespace/name component beneath it) is the same class of unsafe
		// filesystem write as an unsafe symlink found inside an extracted
		// archive - both must exit ExitInstall, not the generic fallback.
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
		// validateDownloadInputs refuses a download_url whose scheme is not
		// http or https. It classifies with the rest of isMetadataFetchError
		// rather than as an integrity failure: nothing was fetched, hashed, or
		// compared - the metadata simply cannot name a fetchable artifact.
		name:     "unsupported download url scheme",
		err:      fmt.Errorf("%w: %q", helpers.ErrUnsupportedDownloadURLScheme, "file:///etc/passwd"),
		wantCode: ExitNetwork,
	},
	{
		// The discriminator against the row directly above, which keeps
		// ExitNetwork: that one's metadata could not name a fetchable artifact
		// at all, while this one names a perfectly fetchable artifact and is
		// refused for the credential that would ride along to it.
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
		// buildCollectionsMap raises this over a malformed resolved identity
		// (ns/name/version) before any install work starts, the same
		// plan-build-time class as ErrDuplicateCollectionKey right below it in
		// isCollectionListUsageError - not an install-time or a network failure.
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
		// buildCollectionsMap raises this over a resolved version that is not
		// helpers.IsExactVersion (a constraint string like "*"), before any
		// install work starts - the identical plan-build-time reasoning the
		// row above already states for ErrUnsafeCollectionIdentifier, kept as
		// its own sentinel rather than folded into that one.
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
		// The real shape downloadCollectionToCache/fetchArtifact produce:
		// helpers.ErrArtifactDownloadDeadline deliberately does not wrap its
		// context.DeadlineExceeded cause with %w (see the sentinel's own doc
		// comment), so this must classify as ExitNetwork through the
		// sentinel match alone, never as ExitInterrupt via a reachable
		// context.Canceled/context.DeadlineExceeded.
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
		// A shape-fidelity row, and deliberately not a guard on the
		// rendering: the literal below hard-codes what
		// cacheManager.LockLostError has ALREADY produced - the sentinel
		// with its cause flattened in by %v - so no edit to that rendering
		// can change what this row hands FromError, and a %v-to-%w edit
		// there cannot fail it. What it does pin is the exit code a CI
		// script actually reads for the commonest lock-loss shape: the run's
		// own error carrying context.Canceled, since canceling the holder
		// context is how a backend signals the loss. The rendering is
		// guarded where it lives, by TestLockLostError's own mustNotMatch
		// rows in internal/galaxy/cache.
		name:     "cache lock lost carrying a flattened cancellation cause",
		err:      fmt.Errorf("%w: %v", helpers.ErrCacheLockLost, context.Canceled), //nolint:errorlint
		wantCode: ExitCacheBusy,
	},
	{
		// The same shape-fidelity row for the supersession case, and it
		// cannot pin supersession either: helpers.ErrSHA256Mismatch is text
		// in this literal rather than a matchable sentinel, so the integrity
		// class never competes for FromError's ordering here at all.
		// Supersession is a property of the rendering and is pinned by
		// TestLockLostError's "supersedes the run's own integrity failure"
		// row; what this row adds is that the string that rendering produces
		// still reaches a CI script as 8 rather than 7.
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
		// The production shape internal/cache/s3's readObject builds, cause
		// and all, so the classification is asserted against the tree a run
		// produces rather than the headline alone. The wrap decides the code
		// - without it the tree falls through every class to ExitError - and
		// the cause beside it is not decoration: it pins that
		// helpers.ErrEmptyGzipMember joins no predicate checked above this
		// class, since one that ever claimed it would flip this row's code.
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
		// The real production shape wraps a second cause with %w
		// (fmt.Errorf("%w: %s: %w", ...)); a non-fs.ErrNotExist cause is the
		// row that actually exercises isConfigUsageError's own arm rather
		// than the fs.ErrNotExist row already covered elsewhere in this
		// table - this is what proves the same-condition/two-codes split
		// documented on isConfigUsageError is gone.
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
		// Raised by the download path's shape probe, not by the extractor,
		// before the bytes are ever committed to the artifact cache. It
		// classifies with the install-time sentinels rather than as a transport
		// failure, since the transfer itself succeeded and no retry turns the
		// delivered bytes into an archive.
		name:     "artifact is not a tar.gz",
		err:      fmt.Errorf("%w: /tmp/a: gzip: invalid header", helpers.ErrArtifactNotTarGz),
		wantCode: ExitInstall,
	},
	{
		// Raised by the same shape probe, on bytes that ARE a gzipped tar and
		// spend the probe's whole scan bound on meta headers without ever
		// presenting an entry. It classifies with the row above rather
		// than as a transport failure for the same reason: the transfer
		// succeeded, and the same URL delivers the same prologue however many
		// times it is asked.
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
		// The retention sibling of the row above, and the one archive sentinel
		// no extraction can raise: it bounds what a reader holds on to or
		// renders rather than what an archive writes or decompresses to, so it
		// arrives from a pass that walks an archive without unpacking it.
		name:     "archive entry name too long",
		err:      fmt.Errorf("%w: ctx", helpers.ErrArchiveEntryNameTooLong),
		wantCode: ExitInstall,
	},
	{
		// The byte-budget sibling of the row above: the per-entry cap is
		// charged against the declared size of every header the extractor is
		// handed, whatever its typeflag, not only against the regular files
		// extraction actually writes to disk.
		name:     "archive entry too large",
		err:      fmt.Errorf("%w: ctx", helpers.ErrArchiveEntryIsTooLarge),
		wantCode: ExitInstall,
	},
	{
		// The declared-size budget's counterpart on the bytes actually read: an
		// archive whose headers understate what archive/tar consumes for them is
		// refused by the decompressed-stream cap rather than by either row above,
		// and only once it pulls a byte past that ceiling - below it, the
		// understatement is accepted. It classifies with the archive family, not
		// as a transport failure, which is why it is not ErrResponseTooLarge.
		name:     "archive decompressed stream too large",
		err:      fmt.Errorf("%w: ctx", helpers.ErrArchiveDecompressedTooLarge),
		wantCode: ExitInstall,
	},
	{
		// Paired with "response too large, aggregated" below: this row is the
		// bare/unaggregated shape, reached wherever a capped body overruns
		// its ceiling outside any per-collection worker - an oversized Galaxy
		// metadata document at resolve time, or an oversized S3 listing or
		// batch-delete response during an init-time ClearFiles - so no
		// helpers.ErrInstallationFailed headline exists to fold it behind. It
		// classifies ExitNetwork: a size ceiling, not a digest mismatch, so
		// ExitIntegrity is deliberately not the answer.
		name:     "response too large, bare",
		err:      fmt.Errorf("%w: ctx", helpers.ErrResponseTooLarge),
		wantCode: ExitNetwork,
	},
	{
		// Paired with "response too large, bare" above: the identical
		// sentinel, joined behind collections.Start's own
		// helpers.ErrInstallationFailed headline the way a per-collection
		// worker's failure actually reaches FromError, classifies
		// ExitInstall instead - proving the two rows exercise different
		// classifiers (isTransportError bare vs. isFileIntegrityError's
		// headline match once aggregated) rather than the same one twice.
		name: "response too large, aggregated behind installation failure",
		err: errors.Join(
			fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed),
			helpers.ErrResponseTooLarge,
		),
		wantCode: ExitInstall,
	},
	{
		// outdated's own aggregation headline, bare: every lookup that failed
		// was a metadata-fetch failure (a 404, a 5xx, a timeout), none of them
		// a lockfile-shape problem, so this classifies through
		// isMetadataFetchError like every other member of that set.
		//
		// Mutation: removing helpers.ErrLatestVersionLookupFailed from
		// isMetadataFetchError makes this row fail with
		// "FromError(latest version lookup failed for 1 collections) = 1,
		// want 4" - run and confirmed. The paired "joined with an invalid
		// lockfile entry name" row below is unaffected by that same mutation,
		// since isLockError claims that tree first regardless.
		name:     "latest version lookup failed, bare",
		err:      fmt.Errorf("%w for 1 collections", helpers.ErrLatestVersionLookupFailed),
		wantCode: ExitNetwork,
	},
	{
		// Paired with the bare row above: the identical headline, this time
		// joined with a cause that must classify differently - a lockfile
		// entry whose name is not a "namespace.name" FQDN. isLockError is
		// checked ahead of isNetworkError in the exitClasses table, so it
		// claims the whole joined tree even though the headline alone would
		// have classified ExitNetwork. This is the predicate isMetadataFetchError's
		// own doc comment describes: which cause is joined determines the
		// class, not which command produced the headline.
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

// galaxyServerConfigSentinels is every Galaxy server configuration sentinel
// this package must classify as ExitUsage. It is listed exhaustively rather
// than sampled: each one is raised only while building the config, and a
// missed entry would silently exit 1 - indistinguishable to a CI pipeline
// from a genuine runtime failure it should retry.
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

// TestGalaxyServerConfigErrorsMapToUsage pins every Galaxy server
// configuration sentinel to ExitUsage. These are raised before any request
// is made, so a pipeline that branches on the exit code must be able to
// tell "your ansible.cfg is wrong, editing it is the only fix" apart from
// a transient failure worth retrying.
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

// integritySentinels is every artifact-digest-authentication sentinel this
// package must classify as ExitIntegrity. Listed exhaustively, not sampled,
// mirroring galaxyServerConfigSentinels's own convention: a missed entry
// would silently fall back to a different exit class, indistinguishable to a
// CI pipeline from a class where retrying the same run might actually help.
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

// TestIntegrityOutranksInstallFailureHeadline proves that once
// collections.Start starts joining a per-collection integrity cause behind
// the helpers.ErrInstallationFailed headline, FromError still classifies the
// combined tree as ExitIntegrity rather than stopping at the headline it
// finds first in the tree. The positive control in the same test - the
// identical headline joined with helpers.ErrDownloadFailed instead - proves
// this is not just "any joined error becomes ExitIntegrity": only the
// presence of an actual integrity sentinel does. The killing mutation is
// moving the isIntegrityError entry of exitClasses below the isLockError
// and isInstallError entries, which makes isInstallError claim the headline
// first; verified, that mutation makes this test fail with:
// "exitcode_test.go:569: FromError(integrity join) = 5, want 7".
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

// TestIntegrityPrecedenceIndependentOfCauseOrder proves the ExitIntegrity
// classification does not depend on where, in the joined tree, the integrity
// sentinel sits - both orderings of the same three causes, and a nested join
// of them, all classify identically.
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

// TestSaveFailureDoesNotMaskIntegrity proves the annotateSaveFailure-shaped
// wrap collections.Start builds ("%w; snapshot save failed: %w") still
// classifies as ExitIntegrity when its primary side already carries an
// integrity cause, matching the real shape a frozen install with a corrupted
// pin and a failing snapshot save would produce.
func TestSaveFailureDoesNotMaskIntegrity(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joinedInstall := errors.Join(headline, helpers.ErrSHA256Mismatch)

	err := fmt.Errorf("%w; snapshot save failed: %w", joinedInstall, errTestSaveFailure)
	if got := FromError(err); got != ExitIntegrity {
		t.Errorf("FromError(err) = %d, want %d", got, ExitIntegrity)
	}
}

// TestLockDriftOutranksSaveFailure proves the annotateSaveFailure-shaped wrap
// lockFrozen builds ("%w; snapshot save failed: %w") still classifies as
// ExitLock when its primary side already carries helpers.ErrLockfileDrift,
// matching the real shape a frozen lock run that found drift and then also
// failed its tail snapshot save would produce - the drift verdict, not the
// save failure's own network-class sentinel, is what the operator must act
// on.
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

// TestSolverConflictMapsToResolution pins that a *solver.ConflictError, the
// error type the version solver returns for an unsatisfiable requirement
// set, classifies as ExitResolution - both bare and wrapped, matching
// through errors.Is via ConflictError's own Is method.
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

// TestArtifactDownloadDeadlineClassification pins helpers.ErrArtifactDownloadDeadline's
// full classification story beyond the single representative case already
// covered in fromErrorCases: unaggregated it is ExitNetwork, joined behind
// collections.Start's helpers.ErrInstallationFailed headline it is ExitInstall
// (identical to every other per-collection failure, helpers.ErrDownloadFailed
// included - this is not a behavior change), and it is never ExitInterrupt in
// either shape, which is exactly what the sentinel's deliberate %v-not-%w
// cause rendering buys: leaving context.DeadlineExceeded or context.Canceled
// reachable via errors.Is would let FromError's cancellation check (checked
// first, ahead of every other class) misclassify a hostile or slow server as
// a caught Ctrl-C.
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

// TestReadStalledClassification pins helpers.ErrReadStalled's full
// classification story beyond the single representative case already
// covered in fromErrorCases, mirroring TestArtifactDownloadDeadlineClassification:
// (a) the bare sentinel is ExitNetwork; (b) the real production shape
// watchdogBody.Read builds - the cause rendered with %v, not wrapped with %w
// - is also ExitNetwork; (c) joined behind collections.Start's
// helpers.ErrInstallationFailed headline it is ExitInstall, identical to
// every other per-collection failure. This test does NOT prove the watchdog
// fix itself: it builds its own error shape rather than calling into
// package fetch, so reverting internal/galaxy/fetch/watchdog.go's %v back to
// %w does not make this test fail - only TestMixedDripAndStallDoesNotClassifyAsInterrupt
// in package collections exercises the real producer end to end.
//
// The got != ExitInterrupt checks below are documentary, not independent
// pins: they are implied by the preceding got == ExitNetwork/ExitInstall
// checks on the same value, the same redundancy TestArtifactDownloadDeadlineClassification
// already carries. They are kept anyway because they name the security
// property this test exists to cover.
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

// TestMixedDeadlineAndStallShapeIsNotInterrupt is a deliberately synthetic
// shape contract: it joins one collection's real deadline-cause rendering
// with a second collection's real stall-cause rendering behind a single
// installation headline - the shape a mixed byte-dripped/stalled run
// produces - and asserts the combination still classifies as ExitInstall,
// never ExitInterrupt, even though both causes carry a rendered
// context.Canceled/context.DeadlineExceeded that is unreachable through
// errors.Is.
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

// TestFromErrorInterruptSurvivesStallSentinel is what makes the rejected
// alternative fix (checking helpers.ErrReadStalled ahead of the
// isCanceled entry of exitClasses) enforceable: joining the production
// stall shape with a genuine, separate context.Canceled - the shape a real
// Ctrl-C produces alongside an in-flight stall - must still classify as
// ExitInterrupt. Any stall class placed above the isCanceled entry in
// exitClasses would make this test fail.
func TestFromErrorInterruptSurvivesStallSentinel(t *testing.T) {
	t.Parallel()
	//nolint:errorlint // pinning the real, deliberately non-wrapping shape watchdogBody.Read builds.
	stallCause := fmt.Errorf("%w: no data for %s: %v", helpers.ErrReadStalled, 30*time.Second, context.Canceled)

	joined := errors.Join(stallCause, context.Canceled)
	if got := FromError(joined); got != ExitInterrupt {
		t.Errorf("FromError(joined) = %d, want ExitInterrupt (%d)", got, ExitInterrupt)
	}
}

// TestMetadataFetchDeadlineClassification pins helpers.ErrMetadataFetchDeadline's
// full classification story, mirroring TestArtifactDownloadDeadlineClassification:
// the bare sentinel is ExitNetwork; joined behind collections.Start's
// helpers.ErrInstallationFailed headline it is ExitInstall, identical to
// every other per-collection failure; and the real rendered shape (%v, not
// %w) is ExitNetwork, never ExitInterrupt. Killing mutation: removing the
// errors.Is(err, helpers.ErrMetadataFetchDeadline) check from
// isMetadataFetchError makes the bare-sentinel assertion fail with
// "FromError(bare sentinel) = 1, want ExitNetwork (4)".
//
// FALSIFIABILITY CONTROL: the "not ExitInterrupt" assertion on the
// %v-rendered shape is unfalsifiable on its own - a %v-rendered cause can
// never match context.Canceled through errors.Is, so that check would pass
// even against a classifier that always returns something other than
// ExitInterrupt. TestReadStalledClassification and
// TestArtifactDownloadDeadlineClassification carry the identical trap; this
// sibling row closes it the same way they do: the identical message shape,
// but %w-wrapping context.Canceled instead of %v-rendering it, DOES
// classify as ExitInterrupt, proving the %v row's assertion actually
// discriminates rather than passing vacuously.
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

// TestStateObjectDeadlineClassification pins helpers.ErrStateObjectDeadline's
// full classification story, mirroring
// TestMetadataFetchDeadlineClassification: the bare sentinel is ExitNetwork;
// the real rendered shape (%v, not %w) is ExitNetwork, never ExitInterrupt,
// with the identical %v/%w falsifiability control; and it CAN be aggregated,
// since WithStateDeadline bounds SaveStore as well as the init-time reads,
// and SaveStore is called well past init too: finalizeInstall and
// warmWithState fold a save failure in through annotateSaveFailure
// ("%w; snapshot save failed: %w"), so a tail save failure joined behind
// helpers.ErrInstallationFailed classifies ExitInstall, identical to every
// other per-collection failure and to how helpers.ErrMetadataFetchDeadline
// classifies once joined the same way.
//
// Killing mutations, both verified: removing the
// errors.Is(err, helpers.ErrStateObjectDeadline) check from isTransportError
// makes the bare-sentinel assertion fail with "FromError(bare sentinel) = 1,
// want ExitNetwork (4)"; removing isFileIntegrityError from isInstallError's
// checks (the sub-check that matches helpers.ErrInstallationFailed itself)
// makes the tail-save-failure assertion fail with
// "FromError(tail save failure) = 4, want ExitInstall (5)" - the joined tree
// falls through to isNetworkError instead, since nothing left in
// isInstallError still matches the headline.
func TestStateObjectDeadlineClassification(t *testing.T) {
	t.Parallel()
	bare := helpers.ErrStateObjectDeadline
	if got := FromError(bare); got != ExitNetwork {
		t.Errorf("FromError(bare sentinel) = %d, want ExitNetwork (%d)", got, ExitNetwork)
	}

	// This bare shape is not only "the sentinel hit at init": it is the exact
	// tree finalizeInstall/warmWithState return for a byte-dripped SaveStore
	// on a run that recorded zero collection failures (summary.count == 0,
	// so annotateSaveFailure is never reached) - the common case, since most
	// runs have no collection failures. No separate row is needed for that
	// case; this one already covers it.
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

	// A tail SaveStore failure (finalizeInstall/warmWithState, well past
	// init) joins the same way a per-collection failure does:
	// annotateSaveFailure's exact wrap shape, "%w; snapshot save failed: %w",
	// around a headline that already carries helpers.ErrInstallationFailed.
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	tailSaveFailure := fmt.Errorf("%w; snapshot save failed: %w", headline, renderedCause)
	if got := FromError(tailSaveFailure); got != ExitInstall {
		t.Errorf("FromError(tail save failure) = %d, want ExitInstall (%d)", got, ExitInstall)
	}
}

// TestCacheBusyFoldedBehindInstallFailureClassifiesAsInstall pins that a
// contention failure joined behind helpers.ErrInstallationFailed classifies
// ExitInstall, the same as every other per-collection cause - identical to
// how helpers.ErrStateObjectDeadline already classifies once joined the same
// way (TestStateObjectDeadlineClassification's tail-save-failure assertion).
//
// This shape is SYNTHETIC: no production path in this repository aggregates
// a helpers.ErrCacheBusy failure behind helpers.ErrInstallationFailed today
// - the distributed lock is acquired once at init, before any per-collection
// work starts, so a contention failure there is always returned bare, never
// joined. This test pins the invariant the classifier must keep if that
// ever changes: the per-collection aggregation rule outranks the cache-busy
// class, exactly as it already outranks every other per-collection cause.
//
// KILLING MUTATION, run and reverted: moving the isCacheBusyError entry of
// exitClasses above its isInstallError entry makes this test fail with:
//
//	exitcode_test.go:928: FromError(joined) = 8, want 5
func TestCacheBusyFoldedBehindInstallFailureClassifiesAsInstall(t *testing.T) {
	t.Parallel()
	headline := fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed)
	joined := errors.Join(headline, helpers.ErrCacheBusy)
	if got := FromError(joined); got != ExitInstall {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitInstall)
	}
}

// TestNetworkOutranksCacheBusyWithoutAnInstallHeadline pins one adjacent pair
// of exitClasses: isNetworkError is checked before isCacheBusyError, so a
// tree carrying both a network-class and a cache-busy sentinel - with no
// helpers.ErrInstallationFailed headline to route it to an earlier entry
// instead - classifies as ExitNetwork, not ExitCacheBusy.
func TestNetworkOutranksCacheBusyWithoutAnInstallHeadline(t *testing.T) {
	t.Parallel()
	joined := errors.Join(helpers.ErrCacheBusy, helpers.ErrCacheBackendUnavailable)
	if got := FromError(joined); got != ExitNetwork {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitNetwork)
	}
}

// TestCacheBusyOutranksUsage pins that isCacheBusyError is checked before
// isUsageError in exitClasses: a helpers.ErrCacheBusy wrapped around
// fs.ErrNotExist (isUsageError's own broad fs.ErrNotExist arm) still
// classifies as ExitCacheBusy, not ExitUsage.
func TestCacheBusyOutranksUsage(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w: %w", helpers.ErrCacheBusy, fs.ErrNotExist)
	if got := FromError(err); got != ExitCacheBusy {
		t.Errorf("FromError(err) = %d, want %d", got, ExitCacheBusy)
	}
}

// TestCanceledOutranksCacheBusy pins the top of exitClasses: a
// context.Canceled sentinel still outranks a joined helpers.ErrCacheBusy,
// since the isCanceled entry is checked before the isCacheBusyError entry is
// ever reached.
func TestCanceledOutranksCacheBusy(t *testing.T) {
	t.Parallel()
	joined := errors.Join(context.Canceled, helpers.ErrCacheBusy)
	if got := FromError(joined); got != ExitInterrupt {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitInterrupt)
	}
}

// TestCacheCorruptOutranksUsage pins that isCacheCorruptError is checked
// before isUsageError in exitClasses, mirroring TestCacheBusyOutranksUsage:
// a helpers.ErrCorruptProjectRegistry wrapped around fs.ErrNotExist
// (isUsageError's own broad fs.ErrNotExist arm) still classifies as
// ExitCacheCorrupt, not ExitUsage.
func TestCacheCorruptOutranksUsage(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w: %w", helpers.ErrCorruptProjectRegistry, fs.ErrNotExist)
	if got := FromError(err); got != ExitCacheCorrupt {
		t.Errorf("FromError(err) = %d, want %d", got, ExitCacheCorrupt)
	}
}

// TestCanceledOutranksCacheCorrupt pins the top of exitClasses: a
// context.Canceled sentinel still outranks a joined
// helpers.ErrCorruptProjectRegistry, since the isCanceled entry is checked
// before the isCacheCorruptError entry is ever reached.
func TestCanceledOutranksCacheCorrupt(t *testing.T) {
	t.Parallel()
	joined := errors.Join(context.Canceled, helpers.ErrCorruptProjectRegistry)
	if got := FromError(joined); got != ExitInterrupt {
		t.Errorf("FromError(joined) = %d, want %d", got, ExitInterrupt)
	}
}

// genericSentinels is every sentinel this package deliberately leaves
// unclassified - FromError falls back to ExitError for each - rather than by
// omission. A fixed table naming both the sentinel and the reason it stays
// generic: an error absorbed at its own producer before ever reaching
// FromError, or an internal nil guard with no operator-actionable meaning.
// The rest of this file is this table's own positive control: the same
// FromError demonstrably returns every other exit code for every other
// sentinel it recognizes, so ExitError here is a verdict, not the absence of
// a check.
//
// This table covers internal/galaxy/helpers only. The exported sentinels
// declared outside it - internal/galaxy/extracted's ErrStoreNotConfigured,
// ErrSHAEmpty, and ErrSHAUnsafe - are deliberately not matched by name
// anywhere in this package, on a predicate rather than a list: every path
// that raises one runs inside a per-collection install or warm worker, so it
// arrives joined behind helpers.ErrInstallationFailed and isFileIntegrityError
// already classifies the tree ExitInstall. Matching them by name would mean
// importing internal/galaxy/extracted into the CLI's classifier for no
// behavioral change. If one of them is ever surfaced outside such a worker it
// silently becomes ExitError, which is the case this note exists to make
// visible.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var genericSentinels = []struct {
	err  error
	name string
}{
	{
		// Consumed at the producer: internal/cache/s3/backend.go's LoadStore
		// and internal/galaxy/store/snapshot.go's Load both resolve this
		// sentinel into a drop-and-rebuild (a fresh empty store, nil error)
		// before it can ever propagate to a caller, let alone FromError.
		name: "outdated schema version",
		err:  helpers.ErrOutdatedSchemaVersion,
	},
	{
		// Consumed at the producer: internal/galaxy/cleanup/scan.go warns
		// and continues past a MANIFEST.json that fails to parse, treating it
		// as neither a reachability root nor a deletion candidate rather than
		// returning it from Start.
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

// TestFromSignal checks the shell-convention 128+signal mapping for the
// signals go-galaxy handles (plus SIGQUIT, still a valid direct FromSignal
// input even though main.go does not subscribe to it) and the
// non-syscall.Signal fallback.
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

// TestLockfileUserinfoClassifiesAsLock pins which class wins when a lockfile
// entry's source embeds URL userinfo. lockfile.File.validate wraps both
// helpers.ErrLockfileInvalid and helpers.ErrGalaxyServerURLUserinfo into one
// error, and the two belong to different classes on their own - the lockfile
// class, and the Galaxy-server configuration class that lands in ExitUsage.
// exitClasses puts isLockError ahead of the entry that would claim the
// second, so the run exits ExitLock, which is the right answer: the file
// that failed to load is the lockfile, not an operator's server config.
//
// The negative half is the point of the test - without it, "it exits 6" would
// not distinguish this ordering from one where the usage class had simply
// never been reachable for this shape at all.
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
// TestServerSuppliedURLPolicyClassifiesUniformly: an error tree carrying one
// of the two server-supplied-URL sentinels under one of the three aggregation
// shapes this project's headlines put an error into.
type serverSuppliedURLPolicyCase struct {
	err  error
	name string
}

// serverSuppliedURLPolicyCases is a deliberate cross-product, not a catalog
// of shapes production produces: both sentinels times all three aggregation
// shapes, including two cells nothing produces today. A bare download-URL
// refusal is one - checkDownloadURL is reached only from
// validateDownloadInputs inside an install worker, so it always arrives behind
// the install headline - and a download-URL refusal behind the
// latest-version-lookup headline is the other, since outdated resolves root
// metadata and never validates a download URL at all. Both are still rows,
// because what the entry under test promises is a property of the error tree
// rather than of the command that built it: a sentinel classifies the same
// wherever it is joined, and a table cut down to today's reachable cells would
// go quiet the moment a command grew a path into one of the others.
//
// The three shapes, named as properties of the tree rather than as a list of
// the commands that build them: the sentinel alone, wrapped by prefixes that
// add no sentinel of their own; the sentinel joined behind the per-collection
// install-failure headline; and the sentinel joined behind the per-entry
// latest-version-lookup headline. Both headlines are spelled exactly as
// failureSummary renders them, "%w for %d collections", so a cell that IS a
// production shape pins what a run actually produces rather than something
// written for this test.
//
// Every row must classify identically. That is the whole point of the
// isServerSuppliedURLPolicyError entry: without it the three shapes above
// answer three different predicates, so one condition with one remedy would
// report a different exit code depending only on what it was joined behind.
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

// TestServerSuppliedURLPolicyClassifiesUniformly pins the class this run
// reports when a Galaxy server, or a snapshot replaying one, supplies a URL
// carrying a credential: ExitInstall, whatever the tree it arrives in.
//
// KILLING MUTATION, run: removing the isServerSuppliedURLPolicyError entry
// from exitClasses fails four of the six rows - the two bare ones as ExitError
// and the two behind the latest-version-lookup headline as ExitNetwork, while
// the two behind the install-failure headline stay green because isInstallError
// claims that headline anyway:
//
//	exitcode_test.go:1199: FromError(collection download url must not contain
//	userinfo: "https://h/a.tar.gz") = 1, want ExitInstall (5)
//
// and, on the outdated-shaped rows, the same assertion reporting = 4.
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

// wantExitClassOrder is the precedence exitClasses must express, written out
// as exit codes so the expectation is readable as the contract a CI branches
// on rather than as a list of predicate names. Kept as a separate literal
// from the table it checks: a copy of exitClasses's own order would agree
// with any reordering by construction.
//
// Two adjacent entries carry the same code, which is a property of the table
// rather than a typo here: isServerSuppliedURLPolicyError yields ExitInstall
// and sits immediately above isInstallError, since what its entry buys is a
// uniform class across every aggregation shape, not a code of its own.
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

// TestExitClassOrderIsPinned pins exitClasses's order entry by entry. It is a
// change detector on purpose: swapping two entries of that table is a
// one-line, compiling edit that silently changes which exit code a CI reads
// for an error tree carrying both classes, so the order is a specification
// and a silent reordering is exactly the failure this must catch. The length
// check comes first so that an added or removed class is reported as such
// rather than as a cascade of mismatched codes.
//
// KILLING MUTATION, run and reverted: swapping the isCacheBusyError and
// isCacheCorruptError entries of exitClasses makes this test fail with:
//
//	exitcode_test.go:1251: exitClasses[7].code = 9, want 8
//	exitcode_test.go:1251: exitClasses[8].code = 8, want 9
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

// exitPrecedenceCase is one adjacent-pair precedence expectation: higher and
// lower are sentinels whose classes sit next to each other in exitClasses,
// wantJoined is the code their join must produce, and wantHigher/wantLower
// are the codes each must produce on its own.
type exitPrecedenceCase struct {
	higher     error
	lower      error
	name       string
	wantJoined int
	wantHigher int
	wantLower  int
}

// adjacentExitPrecedenceCases holds one row per adjacent pair of exitClasses,
// each naming a sentinel that reaches only the higher class and one that
// reaches only the lower. Adjacent pairs are what a reordering actually
// disturbs first, and covering every pair means no swap anywhere in the table
// can leave this table silent.
//
// One adjacent pair has no row and cannot have one:
// isServerSuppliedURLPolicyError sits directly above isInstallError and yields
// that entry's own code, so no error tree can distinguish which of the two
// claimed it and a row would assert nothing. The "lock over install" row is
// the other consequence of that entry - the pair it names is no longer
// adjacent - and it is kept anyway, since a lockfile verdict outranking the
// install class is worth pinning whether or not another entry sits between
// them.
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

// TestAdjacentExitClassPrecedence proves each adjacent pair of exitClasses
// resolves the way the table says: an error tree carrying both sentinels
// classifies as the higher entry's code. The two solo assertions are the
// positive control, and they are what makes the joined one mean anything -
// without them, "the lower class did not win" would be indistinguishable from
// "the lower sentinel is not recognized at all", which is the state a deleted
// predicate leaves behind.
//
// KILLING MUTATION, run and reverted: swapping the isCacheBusyError and
// isCacheCorruptError entries of exitClasses makes the "cache busy over cache
// corrupt" row fail with:
//
//	exitcode_test.go:1388: FromError(joined) = 9, want 8
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

// aggregatedBehindInstallFailure builds the shape a per-collection failure
// actually reaches FromError in: the cause joined behind the
// helpers.ErrInstallationFailed headline finalizeInstall and warmWithState
// render for a run that recorded at least one failed collection.
func aggregatedBehindInstallFailure(cause error) error {
	return errors.Join(fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed), cause)
}

// signatureExitCases is TestSignatureExitClassification's table: every
// signature-family sentinel in both shapes it can reach FromError in - bare,
// and joined behind the installation-failure headline a per-collection worker
// aggregates it behind - plus the two shapes that bound the class from above.
//
// Read as a specification, the table says one thing: the signature class is
// helpers.ErrSignatureVerificationFailed and
// helpers.ErrSignatureAttributionMismatch, the two ways a run ends up unable to
// attribute an artifact to a publisher it accepts. Every other
// sentinel here classifies by what actually failed - a wire failure, a
// configuration value this run cannot use, an artifact's shape, or bytes
// against a digest - rather than by having been raised while checking a
// signature. The bare/aggregated pair is what makes that visible per sentinel:
// for all but the verdict itself and the manifest-chain mismatch, the
// aggregated shape classifies ExitInstall like every other per-collection
// cause, and those two exceptions are exactly the classes that sit above
// isInstallError in exitClasses.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var signatureExitCases = []exitCase{
	{
		name:     "signature verification failed, bare",
		err:      fmt.Errorf("%w: acme.app@1.0.0", helpers.ErrSignatureVerificationFailed),
		wantCode: ExitSignature,
	},
	{
		// The row the isSignatureError entry's position exists for: a
		// per-collection verdict arrives joined behind the installation
		// headline, so a class placed below isInstallError would never be
		// reached for the only shape that actually occurs.
		name:     "signature verification failed, aggregated",
		err:      aggregatedBehindInstallFailure(helpers.ErrSignatureVerificationFailed),
		wantCode: ExitSignature,
	},
	{
		// The second member of the signature class: the signatures verified and
		// vouched for another collection, so the artifact is unattributed for a
		// different reason than a failed policy - and lands in the same class,
		// since an operator holds bytes nobody they trust vouched for either way.
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
		// The message deliberately names a credential-free rendering of the
		// offending value, which is what the producer builds; the row is here
		// for the class, and the hygiene of that rendering is pinned in the
		// producer's own package.
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
		// The one pair that does not discriminate, and it is written down
		// rather than left to be rediscovered: both shapes reach ExitInstall
		// through isInstallError, the bare one via isArtifactShapeError and the
		// aggregated one via the headline isFileIntegrityError matches. The row
		// documents the aggregated shape rather than pinning a precedence.
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
		// Aggregation does not move it either, and here that IS the claim: the
		// isIntegrityError entry sits above isInstallError, so a chain mismatch
		// found inside a per-collection worker still reports as an integrity
		// failure rather than as a generic install failure.
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

// TestSignatureExitClassification walks signatureExitCases. The failure
// message names the row instead of rendering the error, because an aggregated
// row's errors.Join renders across several lines and the citation below has to
// name a single one.
//
// KILLING MUTATION, run and reverted: moving the isSignatureError entry of
// exitClasses below its isInstallError entry. Both aggregated rows fail:
//
//	exitcode_test.go:1642: FromError(signature verification failed, aggregated) = 5, want 10
//	exitcode_test.go:1642: FromError(signature attribution mismatch, aggregated) = 5, want 10
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

// TestMetadataRequestBuildFailedClassifiesNetwork pins the classification
// helpers.ErrMetadataRequestBuildFailed's own doc comment claims for itself,
// on both shapes it reaches FromError in: bare, as internal/galaxy/cache
// raises it, and wrapped, as collections.loadCollectionMetadata carries it up.
// Both are asserted because they arrive by different routes and
// isMetadataFetchError has to claim each one.
//
// It is a test of its own rather than another row in the tables above, and the
// reason has nothing to do with this sentinel: each of those tables is
// followed by a comment citing a line of this file by number, so inserting a
// row silently invalidates every citation below it.
//
// KILLING MUTATION, run: deleting the helpers.ErrMetadataRequestBuildFailed
// line from isMetadataFetchError. Both assertions fail:
//
//	exitcode_test.go:1669: FromError(bare) = 1, want 4
//	exitcode_test.go:1673: FromError(wrapped) = 1, want 4
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

// TestCommandLineUsageErrorsMapToExitUsage pins isCommandLineUsageError's
// sentinel, helpers.ErrUnexpectedArguments, to ExitUsage, bare and wrapped,
// since the argument validators in cmd/go-galaxy/commands raise it both ways.
// A test of its own for the reason
// TestMetadataRequestBuildFailedClassifiesNetwork gives: a new row in a table
// above would move every line citation below it.
//
// KILLING MUTATION, run and reverted, in isCommandLineUsageError
// (exitcode.go) - return false. Both shapes fail:
//
//	exitcode_test.go:1694: FromError(unexpected arguments) = 1, want 2
//	exitcode_test.go:1694: FromError(unexpected arguments: explain takes one) = 1, want 2
func TestCommandLineUsageErrorsMapToExitUsage(t *testing.T) {
	t.Parallel()
	sentinel := helpers.ErrUnexpectedArguments
	for _, err := range []error{sentinel, fmt.Errorf("%w: explain takes one", sentinel)} {
		if got := FromError(err); got != ExitUsage {
			t.Errorf("FromError(%v) = %d, want %d", err, got, ExitUsage)
		}
	}
}
