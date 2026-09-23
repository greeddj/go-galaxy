// Package exitcode maps sentinel errors and OS signals to process exit codes,
// giving go-galaxy a stable, documented exit-code taxonomy that scripts and CI
// pipelines can branch on instead of treating every failure as a flat 1.
package exitcode

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// ExitOK indicates the command completed successfully.
	ExitOK = 0
	// ExitError is the generic fallback for errors that do not map to a
	// more specific class below.
	ExitError = 1
	// ExitUsage indicates invalid configuration, flags, or requirements input.
	// The Go runtime also exits a fatal error with 2, running no deferred
	// function; its "fatal error:" stderr prefix, not the code, tells them apart.
	ExitUsage = 2
	// ExitResolution indicates dependency resolution failed (conflicts,
	// missing candidates, or a cycle in the dependency graph).
	ExitResolution = 3
	// ExitNetwork indicates a network or Galaxy API failure, including
	// timeouts and offline-mode violations.
	ExitNetwork = 4
	// ExitInstall indicates an install-time failure (unsafe archive/symlink
	// content, empty file). A checksum mismatch or malformed digest is
	// ExitIntegrity below instead.
	ExitInstall = 5
	// ExitLock indicates a lockfile is missing, invalid, or does not match
	// the resolved requirements; cache-lock contention is ExitCacheBusy.
	ExitLock = 6
	// ExitIntegrity indicates artifact content failed to authenticate against
	// the sha256 that named it, or that digest was malformed. A stop-and-alert
	// class kept apart from ExitInstall and ExitNetwork: a retry cannot fix it.
	ExitIntegrity = 7
	// ExitCacheBusy indicates this run does not have the cache to itself: the
	// lock was refused or timed out against another holder, or was lost mid-run.
	// Lock loss supersedes every other class via cache.LockLostError's %v.
	ExitCacheBusy = 8
	// ExitCacheCorrupt indicates persisted cache state that nobody can use and
	// must be discarded. An unsupported schema version is ExitUsage instead: the
	// snapshot is sound, and discarding it would break newer runners sharing it.
	ExitCacheCorrupt = 9
	// ExitSignature indicates a collection's signatures did not satisfy the
	// policy in force. Kept apart from ExitIntegrity: that one says the bytes are
	// wrong, this one that nobody trusted vouched for them, a different remedy.
	ExitSignature = 10
	// ExitInterrupt indicates the run was canceled, either by a caught
	// signal falling back to this default or by context cancellation.
	ExitInterrupt = 130
)

// signalExitBase is added to the numeric signal value to build a
// shell-convention exit code (128 + signal number), per FromSignal.
const signalExitBase = 128

// exitClass pairs one classification predicate with the exit code it yields.
type exitClass struct {
	match func(error) bool
	code  int
}

// exitClasses is FromError's precedence, first match wins; the order decides
// the exit code a CI branches on and is pinned by TestExitClassOrderIsPinned.
//
//nolint:gochecknoglobals // a fixed, immutable ordered table, not mutable shared state.
var exitClasses = []exitClass{
	{match: isCanceled, code: ExitInterrupt},
	// Above isInstallError: per-collection causes are joined behind
	// helpers.ErrInstallationFailed, which isInstallError would claim first.
	{match: isIntegrityError, code: ExitIntegrity},
	// Below isIntegrityError, since wrong bytes outrank who vouched for them;
	// above isInstallError for the same aggregation reason as isIntegrityError.
	{match: isSignatureError, code: ExitSignature},
	{match: isLockError, code: ExitLock},
	// Above isInstallError and isNetworkError so a userinfo refusal exits 5
	// whether bare or joined behind helpers.ErrInstallationFailed or
	// helpers.ErrLatestVersionLookupFailed.
	{match: isServerSuppliedURLPolicyError, code: ExitInstall},
	{match: isInstallError, code: ExitInstall},
	{match: isNetworkError, code: ExitNetwork},
	// Below isInstallError so contention joined behind the install headline
	// exits 5, below isNetworkError so a joined wire failure stays the cause,
	// above isUsageError whose fs.ErrNotExist arm matches any not-exist cause.
	{match: isCacheBusyError, code: ExitCacheBusy},
	// Below isInstallError for the same aggregation rule, above
	// isResolutionError and isUsageError so the fs.ErrNotExist arm cannot claim
	// a state-object read that wraps an os-level cause.
	{match: isCacheCorruptError, code: ExitCacheCorrupt},
	{match: isResolutionError, code: ExitResolution},
	{match: isUsageError, code: ExitUsage},
}

// FromError classifies err into an exit code: ExitOK for nil, else the code of
// the first exitClasses entry whose predicate matches, else ExitError.
func FromError(err error) int {
	if err == nil {
		return ExitOK
	}
	for _, class := range exitClasses {
		if class.match(err) {
			return class.code
		}
	}
	return ExitError
}

// isCanceled reports whether err carries a caller's own cancellation. It is
// checked first, so a deadline or stall sentinel must render its context cause
// with %v; ErrSignatureSourceUnavailable alone passes a real Ctrl-C through.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// isCacheCorruptError reports whether the persisted cache state itself is
// unusable and must be discarded. helpers.ErrUnsupportedSchemaVersion is not a
// member: a newer binary's sound snapshot is isRecordedStateUsageError's.
func isCacheCorruptError(err error) bool {
	return errors.Is(err, helpers.ErrCorruptProjectRegistry) ||
		errors.Is(err, helpers.ErrStateObjectTooLarge) ||
		errors.Is(err, helpers.ErrCorruptStateObject) ||
		errors.Is(err, helpers.ErrCorruptSnapshotStore)
}

// isIntegrityError reports whether err is content that did not match the
// digest, commit or pinned identity that named it, or a digest that was
// malformed.
func isIntegrityError(err error) bool {
	return errors.Is(err, helpers.ErrSHA256Mismatch) ||
		errors.Is(err, helpers.ErrMalformedArtifactSHA256) ||
		errors.Is(err, helpers.ErrManifestChainMismatch) ||
		errors.Is(err, helpers.ErrGitCommitMismatch) ||
		errors.Is(err, helpers.ErrGitArtifactIdentityMismatch) ||
		errors.Is(err, helpers.ErrRoleArtifactIdentityMismatch) ||
		// The url siblings of the two above: a refetched url collection
		// whose manifest names a different identity than the pin, and a url
		// role's origin serving bytes with a different sha256 than the pin.
		errors.Is(err, helpers.ErrURLArtifactIdentityMismatch) ||
		errors.Is(err, helpers.ErrURLArtifactSHA256Mismatch)
}

// isSignatureError reports whether err is a signature verdict: the policy was
// not satisfied, or the signatures vouched for another collection. Signature
// fetch, config and manifest-chain failures classify by what failed instead.
func isSignatureError(err error) bool {
	return errors.Is(err, helpers.ErrSignatureVerificationFailed) ||
		errors.Is(err, helpers.ErrSignatureAttributionMismatch)
}

// isLockError reports whether err is a lockfile sentinel: missing, invalid,
// not covering the requirements roots, or lock --frozen's drift verdict.
func isLockError(err error) bool {
	return errors.Is(err, helpers.ErrLockfileMismatch) ||
		errors.Is(err, helpers.ErrLockfileMissing) ||
		errors.Is(err, helpers.ErrLockfileInvalid) ||
		errors.Is(err, helpers.ErrLockfileDrift)
}

// isCacheBusyError reports whether this run does not have the cache to itself:
// the lock was refused by another holder, or granted and then lost mid-run.
// Both share one remedy, rerunning once nothing else holds the cache.
func isCacheBusyError(err error) bool {
	return errors.Is(err, helpers.ErrCacheBusy) ||
		errors.Is(err, helpers.ErrAnotherInstanceIsRunning) ||
		errors.Is(err, helpers.ErrCacheLockLost)
}

// isServerSuppliedURLPolicyError reports whether a Galaxy server supplied a
// download or metadata URL embedding userinfo. It is ExitInstall, not
// ExitNetwork: the refusal is deterministic, so a CI retry would only loop.
func isServerSuppliedURLPolicyError(err error) bool {
	return errors.Is(err, helpers.ErrDownloadURLUserinfo) ||
		errors.Is(err, helpers.ErrMetadataURLUserinfo)
}

// isInstallError reports whether err is an install-time sentinel: unsafe
// archive or symlink content, a non-archive, an empty file, a failed git
// build, a foreign role directory, or a missing artifact cache.
func isInstallError(err error) bool {
	return isFileIntegrityError(err) || isArchiveError(err) ||
		isSymlinkError(err) || isArtifactShapeError(err) || isGitBuildError(err) ||
		errors.Is(err, helpers.ErrRoleDirectoryForeign)
}

// isGitBuildError reports whether a git tree that arrived intact still cannot
// become a collection artifact; the git build's size budgets raise the archive
// budget sentinels and classify through isArchiveBudgetError.
func isGitBuildError(err error) bool {
	return errors.Is(err, helpers.ErrGitTreeEntryInvalid) ||
		errors.Is(err, helpers.ErrGitTreeDuplicateEntry) ||
		errors.Is(err, helpers.ErrGitTreeTooDeep) ||
		errors.Is(err, helpers.ErrGitSymlinkUnresolvable) ||
		errors.Is(err, helpers.ErrGitArtifactSelfCheck)
}

// isArtifactShapeError reports whether what arrived is not a collection
// artifact at all: not a tar.gz, no tar header within the probe's bound, or no
// MANIFEST.json. The transfer succeeded, so it is never a transport failure.
func isArtifactShapeError(err error) bool {
	return errors.Is(err, helpers.ErrArtifactNotTarGz) ||
		errors.Is(err, helpers.ErrArtifactTarHeaderNotFound) ||
		errors.Is(err, helpers.ErrManifestNotFound)
}

// isFileIntegrityError reports whether err is the install aggregation headline,
// an empty-file or a missing-artifact-cache sentinel; helpers.ErrSHA256Mismatch
// belongs to isIntegrityError alone.
func isFileIntegrityError(err error) bool {
	return errors.Is(err, helpers.ErrInstallationFailed) ||
		errors.Is(err, helpers.ErrFileIsEmpty) ||
		errors.Is(err, helpers.ErrHardlinkTargetIsEmpty) ||
		errors.Is(err, helpers.ErrArtifactCacheNotConfigured)
}

// isArchiveError reports whether a tar stream broke a rule about its contents:
// an entry path, name length, declared size or count, or a decompressed size
// past its ceiling.
func isArchiveError(err error) bool {
	return isArchiveEntryPathError(err) || isArchiveBudgetError(err)
}

// isArchiveEntryPathError reports whether err is about what an entry names: a
// path that escapes, is absolute, is empty, traverses a symlink, or collides
// with one already seen.
func isArchiveEntryPathError(err error) bool {
	return errors.Is(err, helpers.ErrArchivePathContainsSymlinkComponent) ||
		errors.Is(err, helpers.ErrArchiveEntryEscapesDestination) ||
		errors.Is(err, helpers.ErrArchiveEntryIsAbsolutePath) ||
		errors.Is(err, helpers.ErrArchiveEntryHasEmptyName) ||
		errors.Is(err, helpers.ErrArchiveDuplicateEntry)
}

// isArchiveBudgetError reports whether err is about how much an archive costs:
// a declared size that is negative or past its ceiling, or a decompressed
// stream, a name length or an entry count past one of the ceilings in helpers.
func isArchiveBudgetError(err error) bool {
	return errors.Is(err, helpers.ErrArchiveExceedsMaxSize) ||
		errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) ||
		errors.Is(err, helpers.ErrArchiveEntryHasNegativeSize) ||
		errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) ||
		errors.Is(err, helpers.ErrArchiveEntryNameTooLong) ||
		errors.Is(err, helpers.ErrArchiveTooManyEntries)
}

// isSymlinkError reports whether err is an unsafe-symlink sentinel, in an
// archive or in the destination tree, or helpers.ErrUnsafeRemovalPath, so an
// install-side and a cleanup-side escape classify identically.
func isSymlinkError(err error) bool {
	return errors.Is(err, helpers.ErrSymlinkTargetResolvesToSelf) ||
		errors.Is(err, helpers.ErrSymlinkTargetEscapesDestination) ||
		errors.Is(err, helpers.ErrSymlinkTarget) ||
		errors.Is(err, helpers.ErrSymlinkTargetResolvesToRoot) ||
		errors.Is(err, helpers.ErrSymlinkTargetIsAbsolute) ||
		errors.Is(err, helpers.ErrSymlinkTargetIsEmpty) ||
		errors.Is(err, helpers.ErrCollectionsPathEscape) ||
		errors.Is(err, helpers.ErrUnsafeRemovalPath)
}

// isNetworkError reports whether err is a transport, Galaxy metadata or
// signature-fetch failure. Its deadline sentinels exit 4 bare, 5 once joined
// behind helpers.ErrInstallationFailed, and never 130 (their causes use %v).
func isNetworkError(err error) bool {
	return isTransportError(err) || isMetadataFetchError(err) || isSignatureTransportError(err)
}

// isSignatureTransportError reports whether a signature source could not be
// fetched or the signature phase overran helpers.SignatureFetchDeadline. Nothing
// was verified, so these are not isSignatureError's verdicts.
func isSignatureTransportError(err error) bool {
	return errors.Is(err, helpers.ErrSignatureSourceUnavailable) ||
		errors.Is(err, helpers.ErrSignatureFetchDeadline)
}

// isTransportError reports whether err is a request-level network failure: a
// deadline, stall, offline refusal, failed download, unreachable cache backend,
// remote refusal, or capped body overrun (a state object's is ExitCacheCorrupt).
func isTransportError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, helpers.ErrArtifactDownloadDeadline) ||
		errors.Is(err, helpers.ErrStateObjectDeadline) ||
		errors.Is(err, helpers.ErrReadStalled) ||
		errors.Is(err, helpers.ErrOfflineMode) ||
		errors.Is(err, helpers.ErrDownloadFailed) ||
		errors.Is(err, helpers.ErrCacheBackendUnavailable) ||
		errors.Is(err, helpers.ErrResponseTooLarge) ||
		isRemoteRefusalError(err)
}

// isRemoteRefusalError reports whether a Galaxy server-list walk or a git
// remote refused credentials or could not answer: runtime conditions to
// investigate, not configuration to fix.
func isRemoteRefusalError(err error) bool {
	return errors.Is(err, helpers.ErrGalaxyAuthFailed) ||
		errors.Is(err, helpers.ErrGalaxyServerUnavailable) ||
		errors.Is(err, helpers.ErrGitTransportFailed) ||
		errors.Is(err, helpers.ErrGitAuthFailed)
}

// isMetadataFetchError reports whether Galaxy metadata could not be fetched,
// parsed or turned into a fetchable URL. ErrLatestVersionLookupFailed is also
// an aggregation headline, so causes joined behind it may classify higher.
func isMetadataFetchError(err error) bool {
	return errors.Is(err, helpers.ErrMetadataUnavailable) ||
		errors.Is(err, helpers.ErrMetadataIsNil) ||
		errors.Is(err, helpers.ErrMissingDownloadURL) ||
		errors.Is(err, helpers.ErrUnsupportedDownloadURLScheme) ||
		errors.Is(err, helpers.ErrMetadataRequestBuildFailed) ||
		errors.Is(err, helpers.ErrVersionsPayloadEmpty) ||
		errors.Is(err, helpers.ErrVersionsPayloadUnsupported) ||
		errors.Is(err, helpers.ErrVersionsPagingExceeded) ||
		errors.Is(err, helpers.ErrMetadataFetchDeadline) ||
		errors.Is(err, helpers.ErrLatestVersionLookupFailed)
}

// isResolutionError reports whether err is a dependency-resolution failure:
// conflicting constraints, no candidate, a cycle, or malformed graph input
// such as an invalid dependency key, none of which a retry repairs.
func isResolutionError(err error) bool {
	return errors.Is(err, helpers.ErrNoVersionSatisfiesConstraints) ||
		errors.Is(err, helpers.ErrConflictingRootConstraints) ||
		errors.Is(err, helpers.ErrConflictingExactVersions) ||
		errors.Is(err, helpers.ErrDependencyGraphHasACycle) ||
		errors.Is(err, helpers.ErrMissingResolvedParent) ||
		errors.Is(err, helpers.ErrMissingResolvedDependency) ||
		errors.Is(err, helpers.ErrMissingResolvedRoot) ||
		errors.Is(err, helpers.ErrLoadMetadataFailed) ||
		errors.Is(err, helpers.ErrInvalidDependencyKey) ||
		isNoCandidateError(err)
}

// isNoCandidateError reports whether nothing exists for what requirements.yml
// asked for: no semver release, an unknown git ref or commit, an unknown role,
// role versions that are unknown or cannot be ordered, or a url version mismatch.
func isNoCandidateError(err error) bool {
	return errors.Is(err, helpers.ErrNoSemverCandidates) ||
		errors.Is(err, helpers.ErrGitRefNotFound) ||
		errors.Is(err, helpers.ErrGitCommitNotFound) ||
		errors.Is(err, helpers.ErrRoleNotFound) ||
		errors.Is(err, helpers.ErrRoleVersionNotFound) ||
		errors.Is(err, helpers.ErrRoleVersionsIncomparable) ||
		// A url source's one candidate is not the version the entry asserted.
		errors.Is(err, helpers.ErrURLCollectionVersionMismatch)
}

// isUsageError reports whether err is a configuration or CLI-input sentinel
// (invalid flags, an unreadable or malformed requirements file or ansible.cfg,
// or a missing path).
func isUsageError(err error) bool {
	return isCommandLineUsageError(err) ||
		isConfigUsageError(err) ||
		isGalaxyServerConfigError(err) ||
		isCollectionNameUsageError(err) ||
		isCollectionListUsageError(err) ||
		isSignatureConfigError(err) ||
		isGitUsageError(err) ||
		isRoleUsageError(err) ||
		isURLUsageError(err)
}

// isCommandLineUsageError reports whether go-galaxy, not urfave, refused the
// positional arguments; a flag urfave cannot parse never reaches here, since
// handleResult exits ExitUsage for that shape directly.
func isCommandLineUsageError(err error) bool {
	return errors.Is(err, helpers.ErrUnexpectedArguments) ||
		errors.Is(err, helpers.ErrMissingArgument)
}

// isRoleUsageError reports whether a roles: entry cannot be used as written,
// or its source answered what no retry could change (no v1 API, an unusable
// v1 record, a repository that is not a role); the remedy is an edit.
func isRoleUsageError(err error) bool {
	return isRoleEntryUsageError(err) || isRoleSourceUsageError(err)
}

// isRoleEntryUsageError is the half of isRoleUsageError about the shape of
// a roles: entry as written.
func isRoleEntryUsageError(err error) bool {
	return errors.Is(err, helpers.ErrInvalidRolesList) ||
		errors.Is(err, helpers.ErrInvalidRoleEntry) ||
		errors.Is(err, helpers.ErrInvalidRoleName) ||
		errors.Is(err, helpers.ErrInvalidRoleInstallName) ||
		errors.Is(err, helpers.ErrInvalidRoleVersion) ||
		errors.Is(err, helpers.ErrDuplicateRoleRequirement)
}

// isRoleSourceUsageError is the half of isRoleUsageError about where a role
// would come from: an unsupported source, a server with no v1 role API, an
// unusable v1 record, or a repository that is not a role.
func isRoleSourceUsageError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedRoleSource) ||
		errors.Is(err, helpers.ErrUnsupportedRoleScm) ||
		errors.Is(err, helpers.ErrUnsupportedRoleInclude) ||
		errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) ||
		errors.Is(err, helpers.ErrGalaxyRoleInvalid) ||
		errors.Is(err, helpers.ErrRoleMetaNotFound) ||
		errors.Is(err, helpers.ErrRoleMetaInvalid)
}

// isGitUsageError reports whether a git source cannot be used as written or
// bound; even a defect found only after a fetch is a shape defect an operator
// must fix, never a transport one a retry repairs.
func isGitUsageError(err error) bool {
	return errors.Is(err, helpers.ErrInvalidGitURL) ||
		errors.Is(err, helpers.ErrGitURLUserinfo) ||
		errors.Is(err, helpers.ErrInvalidGitRef) ||
		errors.Is(err, helpers.ErrGitAbbreviatedCommit) ||
		errors.Is(err, helpers.ErrInvalidGitSubdir) ||
		errors.Is(err, helpers.ErrInvalidGitLocator) ||
		errors.Is(err, helpers.ErrGitCredentialInvalid) ||
		errors.Is(err, helpers.ErrGitSSHNoCredential) ||
		isGitContentUsageError(err)
}

// isGitContentUsageError reports whether the repository's content cannot be
// used as given: a missing or duplicated collection, a name mismatch, or a
// galaxy.yml that cannot be built from or has an inexact version.
func isGitContentUsageError(err error) bool {
	return errors.Is(err, helpers.ErrGitNameMismatch) ||
		errors.Is(err, helpers.ErrGitCollectionNotFound) ||
		errors.Is(err, helpers.ErrGitDuplicateCollection) ||
		errors.Is(err, helpers.ErrGalaxyYMLInvalid) ||
		errors.Is(err, helpers.ErrGitCollectionVersionNotExact)
}

// isURLUsageError reports whether a url source cannot be used as written or
// bound, including a role tarball whose layout cannot become one role; like
// isGitUsageError, a defect found after download is still a shape defect.
func isURLUsageError(err error) bool {
	return errors.Is(err, helpers.ErrInvalidURLRequirement) ||
		errors.Is(err, helpers.ErrURLRequirementUserinfo) ||
		errors.Is(err, helpers.ErrInvalidURLLocator) ||
		errors.Is(err, helpers.ErrURLCredentialInvalid) ||
		errors.Is(err, helpers.ErrRoleTarballLayout) ||
		errors.Is(err, helpers.ErrRoleTarballEntryInvalid)
}

// isSignatureConfigError reports whether the signature configuration (flags,
// environment or requirements, never ansible.cfg) cannot be used as given. No
// signature was checked, so it must not report as isSignatureError's verdict.
func isSignatureConfigError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedSignatureSource) ||
		errors.Is(err, helpers.ErrSignatureSourceUserinfo) ||
		errors.Is(err, helpers.ErrTooManySignatureSources) ||
		errors.Is(err, helpers.ErrKeyringUnreadable) ||
		errors.Is(err, helpers.ErrKeyringIsKeybox) ||
		errors.Is(err, helpers.ErrKeyringRequired) ||
		errors.Is(err, helpers.ErrInvalidSignatureCount) ||
		errors.Is(err, helpers.ErrUnknownSignatureStatusCode) ||
		errors.Is(err, helpers.ErrInvalidDisableGPGVerify) ||
		errors.Is(err, helpers.ErrEmptySignatureValue)
}

// isConfigUsageError reports whether err is a config/environment-level usage
// sentinel or a recorded-state usage sentinel.
func isConfigUsageError(err error) bool {
	return isEnvironmentUsageError(err) || isRecordedStateUsageError(err)
}

// isEnvironmentUsageError reports whether err is a config/environment-level
// usage sentinel, including a cache backend that cannot provide a guarantee
// this tool requires, which no retry can change.
func isEnvironmentUsageError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, helpers.ErrConfigIsNil) ||
		errors.Is(err, helpers.ErrS3EmptyCreds) ||
		errors.Is(err, helpers.ErrCacheDirEmpty) ||
		errors.Is(err, helpers.ErrInvalidTimeout) ||
		errors.Is(err, helpers.ErrWarmCacheDisabled) ||
		errors.Is(err, helpers.ErrCacheBackendUnusable) ||
		isInputFileUsageError(err)
}

// isInputFileUsageError reports whether the requirements file or ansible.cfg
// cannot be used: an explicit ansible.cfg that is missing, either file
// unreadable, or requirements that are not YAML or not a supported shape.
func isInputFileUsageError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) ||
		errors.Is(err, helpers.ErrRequirementsUnreadable) ||
		errors.Is(err, helpers.ErrInvalidRequirementsYAML) ||
		errors.Is(err, helpers.ErrAnsibleConfigNotFound) ||
		errors.Is(err, helpers.ErrAnsibleConfigUnreadable)
}

// isRecordedStateUsageError reports whether something this program recorded
// cannot be used as recorded: a newer binary's snapshot schema, or a recorded
// project's requirements file that fails to load for a reason other than absence.
func isRecordedStateUsageError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedSchemaVersion) ||
		errors.Is(err, helpers.ErrProjectRequirementsUnreadable)
}

// isGalaxyServerConfigError reports whether a Galaxy server's configuration
// is malformed or refused; all are raised while building the config, before
// any request, so the remedy is an edit, not a retry.
func isGalaxyServerConfigError(err error) bool {
	return isGalaxyServerSectionError(err) || isGalaxyServerPolicyError(err)
}

// isGalaxyServerSectionError reports whether err says the configuration
// itself is malformed: an unsupported or unparseable key, a missing or
// syntactically invalid url, or an id that cannot be represented.
func isGalaxyServerSectionError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedGalaxyServerKey) ||
		errors.Is(err, helpers.ErrUnsupportedGalaxyServerAPIVersion) ||
		errors.Is(err, helpers.ErrMissingGalaxyServerURL) ||
		errors.Is(err, helpers.ErrInvalidGalaxyServerURL) ||
		errors.Is(err, helpers.ErrInvalidGalaxyServerID) ||
		errors.Is(err, helpers.ErrDuplicateGalaxyServerID) ||
		errors.Is(err, helpers.ErrInvalidValidateCerts)
}

// isGalaxyServerPolicyError reports whether the configuration parses but would
// leak a credential, give one origin two credential or TLS policies, or pair a
// token with an address or TLS policy the operator did not supply.
func isGalaxyServerPolicyError(err error) bool {
	return errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) ||
		errors.Is(err, helpers.ErrInsecureTokenTransport) ||
		errors.Is(err, helpers.ErrConflictingServerTLSPolicy) ||
		errors.Is(err, helpers.ErrConflictingServerToken) ||
		errors.Is(err, helpers.ErrAmbiguousGalaxyToken) ||
		errors.Is(err, helpers.ErrTokenDestinationFromAnsibleConfig) ||
		errors.Is(err, helpers.ErrTokenTLSPolicyFromAnsibleConfig)
}

// isCollectionNameUsageError reports whether a requirements entry's collection
// name, key, source, type or format is invalid, including an explicit
// namespace conflicting with a dotted name.
func isCollectionNameUsageError(err error) bool {
	return errors.Is(err, helpers.ErrEmptyCollectionName) ||
		errors.Is(err, helpers.ErrInvalidCollectionName) ||
		errors.Is(err, helpers.ErrInvalidCollectionKey) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionSource) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionType) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionFormat) ||
		errors.Is(err, helpers.ErrConflictingNamespaceName)
}

// isCollectionListUsageError reports whether the collections list or the
// resolved set is malformed; buildCollectionsMap and buildLockfile raise these
// before per-collection work starts, so they stay out of ErrInstallationFailed.
func isCollectionListUsageError(err error) bool {
	return errors.Is(err, helpers.ErrInvalidCollectionsList) ||
		errors.Is(err, helpers.ErrInvalidCollectionEntry) ||
		errors.Is(err, helpers.ErrMissingCollection) ||
		errors.Is(err, helpers.ErrDuplicateCollectionRequirement) ||
		errors.Is(err, helpers.ErrDuplicateCollectionKey) ||
		errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) ||
		errors.Is(err, helpers.ErrInvalidCollectionVersion)
}

// FromSignal converts an OS signal into a shell-convention exit code
// (128 + signal number). Signals that are not syscall.Signal (unusual on
// supported platforms) fall back to ExitError.
func FromSignal(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return signalExitBase + int(s)
	}
	return ExitError
}
