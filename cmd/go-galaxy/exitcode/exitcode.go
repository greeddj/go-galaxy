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
	//
	// It shares its number with the one exit this package does not choose: the
	// Go runtime exits a fatal error - a stack overflow, an out-of-memory
	// kill, a runtime throw - with status 2 as well, so a process that died
	// that way is indistinguishable from a misconfigured run by exit code
	// alone. What separates them is the output, not the code: a fatal error
	// prints a line beginning "fatal error:" to stderr, followed by a
	// goroutine dump, and runs no deferred function on the way out, so a run
	// that ends this way also leaves whatever it held unreleased. A pipeline
	// branching on 2 that needs to tell the two apart greps for that prefix.
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
	// the resolved requirements. It names the requirements lockfile only,
	// never the distributed cache lock: a cache-lock contention failure
	// classifies as ExitCacheBusy instead.
	ExitLock = 6
	// ExitIntegrity indicates artifact content failed to authenticate against
	// the sha256 that named it - a lockfile pin, a Galaxy server's declared
	// digest, a cache sidecar, or the extracted store's content-address key -
	// or that such a digest was structurally malformed. This is a stop-and-alert
	// class, deliberately separate from ExitInstall and ExitNetwork: retrying
	// the same run cannot repair it, because the bytes or the digest are wrong
	// at the source.
	ExitIntegrity = 7
	// ExitCacheBusy indicates this run does not have the cache to itself.
	// Three of its producers say the cache could not be acquired in the first
	// place, because something else already holds it, across three shapes: a
	// local process's flock(2) refusing immediately with no wait at all
	// (helpers.ErrAnotherInstanceIsRunning - EWOULDBLOCK from a non-blocking
	// LOCK_EX|LOCK_NB, nothing "reached and answered" since there is no
	// remote party involved), a local Bolt file open timing out against
	// another process's held lock, or an S3 distributed-lock acquisition
	// exhausting its own wait ceiling against a live foreign holder. This is
	// a contention class, distinct from ExitNetwork: the operator's
	// actionable remedy is to retry, possibly after the other holder
	// finishes, rather than to treat it as a dead or misconfigured backend.
	// On the S3 backend an acquisition reaches this class only on positive
	// evidence: the run observed another acquirer holding the lock at least
	// once before its wait ceiling elapsed. A wait that never obtained that
	// evidence classifies as ExitNetwork instead, so an endpoint that
	// answered nothing is never reported as a busy one - see
	// internal/cache/s3/variables.go's partition doc and lock.go's
	// waitCeilingErr for the mechanism and its one disclosed residual.
	//
	// A fourth producer reaches this class from the other side of the same
	// question: helpers.ErrCacheLockLost, a lock this run DID acquire and then
	// had taken away by another holder. It shares the class because it shares
	// the remedy - rerun once nothing else holds the cache - and it
	// SUPERSEDES every other class rather than merely joining them: a run that
	// both lost the lock and failed an integrity check exits 8, not 7. That is
	// not a taxonomy bug. Once another holder is writing the same cache, this
	// run's own verdicts stop being trustworthy on their own terms - the
	// checksum mismatch it reports may be the other holder rewriting an
	// artifact underneath it - so the exclusivity failure is the actionable
	// fact and everything else is evidence for it. The supersession is
	// mechanical, not positional: cacheManager.LockLostError renders the run's
	// own error with %v, flattening its tree so no other class can match it
	// through errors.Is. This class's own position in the exitClasses table
	// carries none of that weight: the reasons it sits where it sits are
	// stated at that table's isCacheBusyError entry, and every one of them is
	// about the acquisition producers.
	ExitCacheBusy = 8
	// ExitCacheCorrupt indicates the persisted cache state itself - a project
	// registry that exists but fails to decode, a state object that could not
	// be read within its declared size ceiling or could not be read back as
	// what this program writes there at all, or (local backend only) a Bolt
	// snapshot file whose bytes fail one of bbolt's own corruption checks -
	// cannot be used by anyone and must be discarded before the run
	// can proceed. The remedy is mechanical and safe to automate: delete the
	// offending object (or the whole cache directory / bucket prefix) or
	// rerun with --clear-cache, then rerun the command.
	//
	// This is deliberately distinct from ExitUsage: helpers.ErrUnsupportedSchemaVersion
	// - a snapshot a newer binary wrote in a shape this one cannot safely
	// interpret - classifies ExitUsage instead of this class, because the
	// snapshot itself is not damaged, only unreadable by this particular
	// reader. Discarding it would destroy a shared cache the newer binary's
	// other runners still depend on, and the actual remedy is an environment
	// change (a newer binary, or pointing at a different cache), which is
	// what ExitUsage means. See isCacheCorruptError and
	// isRecordedStateUsageError below for the two classifiers this split
	// lives in.
	ExitCacheCorrupt = 9
	// ExitSignature indicates a collection's signatures did not satisfy the
	// policy in force: the artifact's bytes may authenticate perfectly against
	// the digest that named them, and this run still could not attribute them
	// to a publisher it was configured to accept.
	//
	// It is deliberately not ExitIntegrity, because the two authenticate
	// different actors: exit 7 says these are not the bytes that were named,
	// exit 10 says nobody this run trusts vouched for them. The remedies part
	// company with the actors - an integrity failure is a stop-and-alert about
	// the artifact or the server that served it, while a signature failure is
	// most often about this run's own keyring or its required-count policy,
	// which is a different CI branch and usually a different team. Folding both
	// into 7 would make that branch impossible to write.
	//
	// helpers.ErrManifestChainMismatch is the deliberate exception and
	// classifies ExitIntegrity rather than here, even though the chain it names
	// is exactly what a collection signature covers: what fails there is a file
	// against the digest MANIFEST.json or FILES.json names for it, which is
	// bytes against a digest - exit 7's own predicate - and no keyring or
	// policy change repairs it.
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

// exitClasses is the classification precedence FromError walks, top to
// bottom, returning the code of the first entry whose predicate matches it.
// The order is observable behavior rather than an implementation detail - it
// decides the exit code a CI branches on, and a single error tree routinely
// satisfies several of these predicates at once - so it is stated as data in
// one place, and each entry whose position is load-bearing carries the
// argument for that position in place.
//
//nolint:gochecknoglobals // a fixed, immutable ordered table, not mutable shared state.
var exitClasses = []exitClass{
	{match: isCanceled, code: ExitInterrupt},
	// isIntegrityError must be checked before isInstallError: after
	// collections.Start started joining per-collection causes behind
	// helpers.ErrInstallationFailed, every integrity failure's error tree
	// also contains that sentinel, so placing this entry any lower would make
	// it unreachable - isInstallError would already have claimed the error.
	// Cancellation still outranks it.
	{match: isIntegrityError, code: ExitIntegrity},
	// isSignatureError sits below isIntegrityError and above isInstallError,
	// and both bounds are load-bearing. Below isIntegrityError: an error tree
	// carrying both a digest failure and a signature verdict classifies
	// ExitIntegrity, because "these are not the bytes that were named" is the
	// more fundamental fact - who published an artifact is a question that only
	// arises once the artifact is the one it claims to be. Above isInstallError:
	// a verdict raised inside a per-collection worker reaches FromError joined
	// behind helpers.ErrInstallationFailed like every other per-collection
	// cause, so any position below that entry would make this class unreachable
	// for the shape aggregation produces - the identical trap isIntegrityError's
	// own entry above describes for itself. Its position relative to the
	// isLockError entry it now precedes carries no argument of its own: that
	// follows from the two bounds, and a tree carrying both a signature verdict
	// and a lockfile verdict resolves here.
	{match: isSignatureError, code: ExitSignature},
	{match: isLockError, code: ExitLock},
	// isServerSuppliedURLPolicyError sits above isInstallError even though it
	// yields that entry's own code, and what the entry buys is uniformity
	// rather than a code: measured with this entry removed, a tree carrying
	// one of these sentinels with no aggregation headline matches no predicate
	// at all and falls back to ExitError, while the same sentinel joined
	// behind helpers.ErrLatestVersionLookupFailed is claimed by isNetworkError
	// and one joined behind helpers.ErrInstallationFailed by isInstallError -
	// one condition with one remedy reporting three different codes decided
	// only by what it was joined behind. Its position is the table's own
	// convention, not a new rule: below isCanceled, isIntegrityError,
	// isSignatureError and isLockError, since a Ctrl-C, a digest failure, a
	// signature verdict and a lockfile verdict each outrank it for the reasons
	// stated on those entries; and above isInstallError and isNetworkError,
	// the two that were measured claiming these shapes and the reason this
	// class is reachable at all. isResolutionError sits below it positionally
	// rather than as a third hazard: the only member of that predicate a
	// userinfo sentinel could plausibly travel behind is
	// helpers.ErrLoadMetadataFailed, which loadRootMetadataCached raises bare
	// as its own exhausted-walk verdict and joins with nothing.
	{match: isServerSuppliedURLPolicyError, code: ExitInstall},
	{match: isInstallError, code: ExitInstall},
	{match: isNetworkError, code: ExitNetwork},
	// isCacheBusyError sits in exactly this one position, for four
	// independent reasons. Not above isCanceled: a Ctrl-C that races a
	// contention failure must still report as ExitInterrupt, not as
	// contention. Not above isIntegrityError/isLockError/isInstallError: if a
	// contention failure is ever folded behind helpers.ErrInstallationFailed
	// by annotateSaveFailure, it must classify ExitInstall like every other
	// per-collection cause, exactly as helpers.ErrStateObjectDeadline already
	// does - placing this entry any higher would invert that rule for
	// contention alone. Not above isNetworkError: a contention verdict
	// arriving joined with a genuine transport failure must keep the wire
	// failure as the actionable cause. Not below isUsageError: isUsageError's
	// fs.ErrNotExist arm is broad enough that any error tree carrying an os
	// path error would satisfy it, which would swallow a contention failure
	// that happens to wrap one.
	{match: isCacheBusyError, code: ExitCacheBusy},
	// isCacheCorruptError sits directly below isCacheBusyError and above
	// isResolutionError. It must stay below the isLockError and
	// isInstallError entries above, for the identical reason
	// isCacheBusyError's own comment gives: if either of this class's
	// sentinels is ever joined behind helpers.ErrInstallationFailed,
	// isInstallError must claim the tree first and classify it ExitInstall,
	// the same per-collection aggregation rule every class in this table
	// already follows. It sits above isResolutionError and isUsageError so
	// that isUsageError's broad fs.ErrNotExist arm can never claim a
	// state-object read that happens to wrap an os-level cause ahead of this
	// more specific verdict - the same defensive posture isCacheBusyError's
	// own position takes against that identical arm.
	{match: isCacheCorruptError, code: ExitCacheCorrupt},
	{match: isResolutionError, code: ExitResolution},
	{match: isUsageError, code: ExitUsage},
}

// FromError classifies err into an exit code by matching it against the known
// sentinel errors declared in internal/galaxy/helpers. A nil error is ExitOK;
// otherwise exitClasses is walked top to bottom and the first class whose
// predicate matches wins, with an error no class recognizes falling back to
// ExitError. The priority order is that table's own order - cancellation,
// integrity, signature, lock, install, network, cache contention, cache
// corruption, resolution, then usage/config - and the reason a class sits
// where it sits is stated on its entry there.
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

// isCanceled reports whether err carries a caller's own cancellation.
//
// Classifying it ahead of every other class is correct only under a rule every
// sentinel this program raises to describe why work ended is bound by: such a
// sentinel may leave context.Canceled reachable through errors.Is ONLY when
// that value is the caller's own cancellation passed through unchanged. This
// class is checked first and cannot tell "the sentinel's cause happens to be a
// cancellation" from "the caller genuinely canceled", so a sentinel that broke
// the rule would silently steal it.
//
// The rule is over context.Canceled and nothing else, because that is the only
// value this predicate matches. context.DeadlineExceeded is deliberately not
// its business: isTransportError matches that one itself, so a remote peer
// driving a timeout into an error tree is the classification working rather
// than the rule being broken - which is exactly what
// helpers.ErrSignatureSourceUnavailable wrapping a black-holed host's dial
// timeout is, since net's own timeout error answers errors.Is for that value.
//
// The deadline sentinels are the never half, and each states the rule on
// itself - see helpers.ErrReadStalled's and helpers.ErrArtifactDownloadDeadline's
// doc comments - binding whatever raises one to render the context cause with
// %v, so a hostile or degraded peer's stall cannot arrive here as a
// cancellation. That is a constraint on a producer rather than a survey of
// them: it binds one written tomorrow exactly as it binds the ones written
// already. helpers.ErrSignatureSourceUnavailable is the ONLY-when half:
// signature.transportCause unwraps the *url.Error http.Client produced and
// hands back its cause for the caller to wrap with %w, so a real Ctrl-C during
// a signature fetch reaches this class and reports as an interrupt, which is
// the correct answer for a signal the operator sent.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// isCacheCorruptError reports whether err says the persisted cache state
// itself - not this reader's ability to interpret it, and not a backend's
// ability to reach it - cannot be used by anyone and must be discarded
// before the run can proceed: a project registry that exists but fails to
// decode (helpers.ErrCorruptProjectRegistry), a state object that could not
// be read within its declared size ceiling (helpers.ErrStateObjectTooLarge)
// or could not be read back as what this program writes there at all
// (helpers.ErrCorruptStateObject), or the local Bolt snapshot file itself
// failing one of bbolt's own corruption checks (helpers.ErrCorruptSnapshotStore
// - see openBolt in internal/galaxy/store for the closed set of bbolt
// sentinels this maps from). helpers.ErrUnsupportedSchemaVersion is
// deliberately not a member: a schema version newer than this binary
// understands means the snapshot was written correctly by a newer binary, not
// that its bytes are damaged, so
// isRecordedStateUsageError classifies it instead - see that function's own
// doc comment for the flip side of this distinction. This class is scoped to
// the local backend only for helpers.ErrCorruptSnapshotStore specifically:
// the S3 backend's state object is gzipped JSON with its own sentinels
// (helpers.ErrCorruptProjectRegistry, helpers.ErrStateObjectTooLarge and
// helpers.ErrCorruptStateObject), so it reaches this class through those
// three instead, never through
// helpers.ErrCorruptSnapshotStore, which only ever originates from opening a
// local Bolt file. helpers.ErrCorruptStateObject is that backend's own third
// member and is produced at internal/cache/s3's readObject, which is what
// keeps a sentinel carrying no exit class of its own - a gzip member producing
// no bytes - from reaching this table unclassified.
//
// No error tree produced by this program carries both this class and the
// network class today: a size-ceiling failure and a state object that will
// not inflate are both only ever detected after
// their GET has already answered with a 200 (Client.getObject returns a
// response to readObject only on that status; every other status or
// transport failure returns an error before readAllCapped ever runs), and
// neither a decode failure nor a local Bolt file open ever touches the
// network at all. No member of this class carries a
// context.DeadlineExceeded/context.Canceled cause either, so
// cache.deadlineError (the StateObjectDeadline decorator's normalizer) never
// relabels one into helpers.ErrStateObjectDeadline - its own precondition
// requires the error being normalized to already carry one of those two
// signals, which none of these ever does. That holds for the inflate failure
// as well as for the three older members: a cancellation observed mid-stream
// is reported by internal/gzipstream as the context error itself, never as a
// member that produced nothing, since a member reaches that verdict only by
// ending cleanly.
func isCacheCorruptError(err error) bool {
	return errors.Is(err, helpers.ErrCorruptProjectRegistry) ||
		errors.Is(err, helpers.ErrStateObjectTooLarge) ||
		errors.Is(err, helpers.ErrCorruptStateObject) ||
		errors.Is(err, helpers.ErrCorruptSnapshotStore)
}

// isIntegrityError reports whether err is an artifact-digest authentication
// failure: content that did not hash to the digest that named it, or a value
// that was supposed to be such a digest and was not.
//
// helpers.ErrManifestChainMismatch is a member on that predicate rather than
// by association with the check it belongs to. It names content that did not
// match a digest naming it - FILES.json against MANIFEST.json's pointer, or a
// listed file against FILES.json's - which is this class's own question,
// whatever a signature check wrapped around it concluded; isSignatureError's
// one member asks who vouched for the bytes instead.
//
// The two git sentinels are members on the same predicate. A remote that
// advertised one commit and shipped a pack without it, or that answered a
// request for a commit with a different one, delivered bytes that do not
// match the identity they were promised under (helpers.ErrGitCommitMismatch);
// and a pinned git collection whose rebuild from its commit names a different
// namespace, name or version than the pin is the lockfile's own digest
// question asked of a commit (helpers.ErrGitArtifactIdentityMismatch).
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

// isSignatureError reports whether err carries one collection's signature
// verdict: this run could not attribute the artifact to a publisher it was
// configured to accept. Two sentinels answer that question and the pair is the
// whole class rather than a sample of it - helpers.ErrSignatureVerificationFailed
// when the signatures in hand did not satisfy the policy in force, and
// helpers.ErrSignatureAttributionMismatch when they did and vouched for a
// different collection than the one being installed. Both leave an operator in
// the same position, holding bytes nobody they trust vouched for, which is what
// makes them one exit class.
//
// Every other signature-related sentinel classifies elsewhere, on its own
// predicate rather than by association. A source that could not be fetched, or
// a fetch budget that expired, is a wire failure (isSignatureTransportError); a
// keyring or a policy value this tool refuses is a configuration failure
// (isSignatureConfigError), and so is a requirements entry declaring more
// sources than the cap allows; an artifact naming no manifest is an
// artifact-shape failure (isArtifactShapeError); and a broken manifest chain
// is bytes against a digest (isIntegrityError).
func isSignatureError(err error) bool {
	return errors.Is(err, helpers.ErrSignatureVerificationFailed) ||
		errors.Is(err, helpers.ErrSignatureAttributionMismatch)
}

// isLockError reports whether err is a lockfile-related sentinel:
// helpers.ErrLockfileMismatch (a lockfile that does not cover the
// requirements roots), helpers.ErrLockfileMissing, helpers.ErrLockfileInvalid,
// or helpers.ErrLockfileDrift - lock --frozen's own verdict that a fresh
// resolve disagrees with the lockfile already on disk. The last one is kept
// distinct from ErrLockfileMismatch even though both name a lockfile that
// disagrees with reality, because they describe different disagreements
// (missing coverage vs. stale content) - matching the precedent that keeps
// ErrMalformedArtifactSHA256 and ErrSHA256Mismatch apart under one exit
// class rather than folding them into a single sentinel.
func isLockError(err error) bool {
	return errors.Is(err, helpers.ErrLockfileMismatch) ||
		errors.Is(err, helpers.ErrLockfileMissing) ||
		errors.Is(err, helpers.ErrLockfileInvalid) ||
		errors.Is(err, helpers.ErrLockfileDrift)
}

// isCacheBusyError reports whether err says this run does not have the cache
// to itself, in either of the two ways that can be true: the lock was refused
// because another holder had it (helpers.ErrCacheBusy past a backend's own
// wait ceiling, or helpers.ErrAnotherInstanceIsRunning with no wait at all),
// or the lock was granted and then taken away mid-run
// (helpers.ErrCacheLockLost). Both share one remedy - rerun once nothing else
// holds the cache - which is what makes them one exit class rather than two.
func isCacheBusyError(err error) bool {
	return errors.Is(err, helpers.ErrCacheBusy) ||
		errors.Is(err, helpers.ErrAnotherInstanceIsRunning) ||
		errors.Is(err, helpers.ErrCacheLockLost)
}

// isServerSuppliedURLPolicyError reports whether err says a Galaxy server -
// or a cached snapshot replaying one - supplied a URL this run refuses to use,
// because it embeds a credential in its userinfo: an artifact's download URL
// (helpers.ErrDownloadURLUserinfo) or a metadata reference derived from a
// server's own versions_url or highest_version.href
// (helpers.ErrMetadataURLUserinfo). Neither is repaired by a retry, by a
// different server, or by an operator editing this run's configuration: the
// offending value is content a server chose, so the remedy is on the server
// that published it.
//
// The class is ExitInstall rather than a code of its own because there is no
// third answer a pipeline would take: the run refused to fetch what it was
// pointed at, exactly as it does for the install-time refusals that already
// carry that code, and a new code buys a branch nobody writes.
//
// ExitNetwork is the alternative that has a real claim, and it is not
// dismissed for lack of one. An entry sitting in this exact position and
// yielding that code instead would deliver the identical uniformity, since the
// uniformity comes from the position rather than from the code;
// helpers.ErrUnsupportedDownloadURLScheme is a refusal of the very same field
// already sitting there; and helpers.ErrLatestVersionLookupFailed, the
// headline an outdated run joins this sentinel behind, sits there too. Two
// things decide it the other way. The discriminator against
// ErrUnsupportedDownloadURLScheme is whether the metadata could name a
// fetchable value at all - isArtifactShapeError's own doc comment gives that
// same reasoning for its own split - and a URL refused for its userinfo names
// one perfectly well. And a network code is the one a CI reflexively runs
// again, while this refusal is deterministic: the same server answers with the
// same URL, so a retry can only spend the budget. What ExitInstall costs is
// stated rather than denied - a lock or an outdated run installs nothing and
// can still report an install-time failure, which the exit-code table
// discloses - and a code an operator reads once is cheaper than a loop a CI
// runs forever.
//
// And it is not ExitUsage, where helpers.ErrGalaxyServerURLUserinfo sits, for
// the plainest reason available: nothing an operator wrote produced this
// value, so there is nothing in flags, environment or requirements.yml to fix.
func isServerSuppliedURLPolicyError(err error) bool {
	return errors.Is(err, helpers.ErrDownloadURLUserinfo) ||
		errors.Is(err, helpers.ErrMetadataURLUserinfo)
}

// isInstallError reports whether err is an install-time sentinel (unsafe
// archive/symlink content, bytes that are not an archive at all, an empty
// file, or a missing artifact cache). Split into sub-checks purely to stay
// under the cyclomatic-complexity budget; together they still cover the exact
// same sentinel set.
func isInstallError(err error) bool {
	return isFileIntegrityError(err) || isArchiveError(err) ||
		isSymlinkError(err) || isArtifactShapeError(err) || isGitBuildError(err) ||
		errors.Is(err, helpers.ErrRoleDirectoryForeign)
}

// isGitBuildError reports whether err says a git tree could not be turned
// into a collection artifact: an entry name this tool refuses to materialize,
// two entries that would land on one name, nesting beyond the depth cap, a
// symlink that resolves nowhere inside the collection, or an artifact the
// builder produced and then failed its own manifest chain check on. Every
// member is about content that arrived intact and still cannot be installed,
// which is isArchiveError's own question asked of a tree instead of a tar,
// so the class is the same. The per-blob, entry-count and total-size budgets
// a git build enforces raise the archive budget sentinels themselves and
// classify through isArchiveBudgetError without an entry here.
func isGitBuildError(err error) bool {
	return errors.Is(err, helpers.ErrGitTreeEntryInvalid) ||
		errors.Is(err, helpers.ErrGitTreeDuplicateEntry) ||
		errors.Is(err, helpers.ErrGitTreeTooDeep) ||
		errors.Is(err, helpers.ErrGitSymlinkUnresolvable) ||
		errors.Is(err, helpers.ErrGitArtifactSelfCheck)
}

// isArtifactShapeError reports whether err says what arrived does not have the
// outer shape of a collection artifact: it is not a gzip-compressed tar at all
// (helpers.ErrArtifactNotTarGz, from the download path's shape probe), it is
// one whose tar stream presented no header at all inside that same probe's
// scan bound (helpers.ErrArtifactTarHeaderNotFound, an archive that spends its
// whole prologue on meta headers rather than reaching an entry), or it is one
// that names no MANIFEST.json within its own scan bound
// (helpers.ErrManifestNotFound). It is its own predicate rather than a member
// of isArchiveError because it answers a different question: every sentinel
// there is about what an archive holds, while these three are about whether
// there is a collection artifact to speak of at all. They classify alongside
// them, and never as a transport failure - the transfer succeeded, and no
// retry turns an error page into an archive, shortens a prologue, or puts a
// manifest into an archive that has none.
func isArtifactShapeError(err error) bool {
	return errors.Is(err, helpers.ErrArtifactNotTarGz) ||
		errors.Is(err, helpers.ErrArtifactTarHeaderNotFound) ||
		errors.Is(err, helpers.ErrManifestNotFound)
}

// isFileIntegrityError reports whether err is an empty-file/missing-cache
// sentinel. helpers.ErrSHA256Mismatch is deliberately not here: it is
// isIntegrityError's alone, checked ahead of this function in FromError, so
// leaving it in both classes would be shadowed dead code.
func isFileIntegrityError(err error) bool {
	return errors.Is(err, helpers.ErrInstallationFailed) ||
		errors.Is(err, helpers.ErrFileIsEmpty) ||
		errors.Is(err, helpers.ErrHardlinkTargetIsEmpty) ||
		errors.Is(err, helpers.ErrArtifactCacheNotConfigured)
}

// isArchiveError reports whether err says a collection artifact's tar stream
// broke a rule about its own contents: an entry whose path, name length,
// declared size or count this project refuses, or a decompressed stream past
// the ceiling. The question is what the archive holds, which is why bytes that
// are not an archive at all answer isArtifactShapeError instead.
//
// Whether a member of this set decides the exit code is a property of the error
// tree rather than of which sentinel it is. Joined behind
// helpers.ErrInstallationFailed - what a per-collection install or warm worker
// produces - the headline already classifies ExitInstall through
// isFileIntegrityError, and short-circuit evaluation means this function is
// never consulted for it. Reached bare, this function answers, and it answers
// with the same class either way. So the set is written for completeness rather
// than sized to whichever sentinels happen to be reachable unaggregated.
//
// Split into two sub-checks purely to stay under the cyclomatic-complexity
// budget, the same way isInstallError above is; together they still cover the
// exact same sentinel set.
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

// isSymlinkError reports whether err is an unsafe-symlink sentinel. This
// group covers both an unsafe symlink found inside an extracted archive
// (the original set below) and an unsafe symlink found in the destination
// tree itself - a component of cfg.DownloadPath, most dangerously
// ansible_collections or a namespace/name directory beneath it, that
// resolves outside the collections root os.Root enforces.
// helpers.ErrUnsafeRemovalPath sits here too: cleanup's removeInstalled
// raises it for the identical meaning on the delete side - a computed
// removal path that failed a containment check against its expected root -
// so an install-side escape and a cleanup-side one classify identically.
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

// isNetworkError reports whether err is a network or Galaxy API sentinel,
// including request timeouts, offline-mode violations, and a server-list
// walk aborting on a credential failure or an exhausted retry budget.
// helpers.ErrArtifactDownloadDeadline classifies here, unaggregated, as
// ExitNetwork; once collections.Start joins it behind
// helpers.ErrInstallationFailed the isInstallError entry above claims it
// first, as ExitInstall - identical to every other per-collection failure,
// helpers.ErrDownloadFailed included. It
// never classifies as ExitInterrupt: the sentinel deliberately does not wrap
// its context.DeadlineExceeded/context.Canceled cause with %w (see its own
// doc comment), so that raw signal never reaches errors.Is(err,
// context.Canceled) above.
//
// helpers.ErrReadStalled follows the identical shape: unaggregated, it
// classifies here as ExitNetwork; once collections.Start joins it behind
// helpers.ErrInstallationFailed for a per-collection stall, isInstallError
// claims it first, as ExitInstall, the same as every other per-collection
// failure. It never classifies as ExitInterrupt either: the watchdog aborts a
// stall by canceling its own derived context, so the cause is
// context.Canceled, but the producer renders that cause with %v rather than
// wrapping it with %w (see helpers.ErrReadStalled's own doc comment), so it
// never reaches errors.Is(err, context.Canceled) above.
//
// helpers.ErrMetadataFetchDeadline follows the same shape as
// helpers.ErrArtifactDownloadDeadline: unaggregated (hit resolving, before
// any collection-level work starts) it classifies here as ExitNetwork - which
// also outranks isResolutionError below, and is the right answer, since the
// cause is the wire, not an unsatisfiable constraint; hit inside an install
// worker (metadata re-resolution during install.go's per-collection path) it
// is instead joined behind helpers.ErrInstallationFailed and isInstallError
// claims it first, as ExitInstall, identical to every other per-collection
// failure. It never classifies as ExitInterrupt, for the identical %v-not-%w
// reason.
//
// helpers.ErrStateObjectDeadline follows the identical shape too, and CAN be
// aggregated - but the rule is WHEN, not WHERE it was hit: it classifies
// ExitInstall only when it reaches FromError already joined behind
// helpers.ErrInstallationFailed, and it is joined there in exactly one
// circumstance - finalizeInstall/warmWithState fold a SaveStore failure in
// through annotateSaveFailure ("%w; snapshot save failed: %w") only when that
// run also recorded at least one per-collection failure (summary.count > 0);
// isInstallError then claims the joined tree, as ExitInstall, the same as
// every other per-collection failure. Every other path returns it bare, and
// therefore unaggregated, classifying ExitNetwork here: every init-time
// operation (LoadStore, LoadProjectRegistry, RecordProject), lockWithState
// and saveDryRunSnapshotIfPersisted (neither of which ever joins a SaveStore
// failure behind anything), and - the case an enumeration of "where" would
// miss - a tail SaveStore failure from finalizeInstall/warmWithState on a run
// that recorded zero collection failures, which is the common case: most
// runs have none. It never classifies as ExitInterrupt, for the identical
// %v-not-%w reason.
//
// Split into three sub-checks purely to stay under the cyclomatic-complexity
// budget; the three together still cover the exact same sentinel set.
func isNetworkError(err error) bool {
	return isTransportError(err) || isMetadataFetchError(err) || isSignatureTransportError(err)
}

// isSignatureTransportError reports whether err says a signature could not be
// obtained over the wire: the source was unreachable, unreadable, or refused
// by offline mode (helpers.ErrSignatureSourceUnavailable), or the collection's
// whole signature phase overran helpers.SignatureFetchDeadline. Both classify
// exactly like their siblings in isTransportError - ExitNetwork unaggregated,
// ExitInstall once joined behind helpers.ErrInstallationFailed by a
// per-collection worker.
//
// The two part company on the interrupt class above, and the rule deciding
// which way each one goes lives on isCanceled.
//
// helpers.ErrSignatureFetchDeadline is a budget this program imposed, so
// whatever raises it must render the context cause with %v and it must never
// classify ExitInterrupt. That binds any future producer as well as the one
// that exists: collections.signatureDeadlineError, which normalizes a spent
// signature phase into this sentinel and is the third place enforcing that
// rule, after fetch.watchdogBody.Read with collections.artifactDeadlineError
// and internal/galaxy/cache's own deadlineError.
//
// helpers.ErrSignatureSourceUnavailable carries whatever failed the transfer,
// and signature.transportCause deliberately keeps a genuine context.Canceled
// reachable, so an operator's Ctrl-C during a signature fetch classifies as the
// interrupt it was rather than as a network failure - the shape that rule's
// exception exists for.
//
// It is a sibling of isTransportError rather than two more lines inside it
// purely to stay under the cyclomatic-complexity budget, the same reason
// isNetworkError was split in the first place. Neither of these two is a
// signature VERDICT: nothing was verified, so isSignatureError deliberately
// does not match them and this class is where they belong.
func isSignatureTransportError(err error) bool {
	return errors.Is(err, helpers.ErrSignatureSourceUnavailable) ||
		errors.Is(err, helpers.ErrSignatureFetchDeadline)
}

// isTransportError reports whether err is a request-level network sentinel:
// a timeout, an offline-mode violation, this acquisition's own artifact
// download deadline, a persisted cache-state operation's own deadline, a bare
// download failure, a cache backend that could not be reached or answered
// with a failure that is not this program's own doing
// (helpers.ErrCacheBackendUnavailable), a server-list walk aborting on a
// credential failure or an exhausted retry budget, or any capped response
// body that overran its ceiling before it finished streaming
// (helpers.ErrResponseTooLarge - helpers.NewSizeLimitedReader raises it for
// an artifact download, a Galaxy metadata document over MetadataMaxSize, and
// an S3 list or batch-delete response over S3ListMaxSize alike). The one
// capped body that does not land here is a persisted cache-state object,
// which carries helpers.ErrStateObjectTooLarge instead, since a state object
// this program cannot read is a corrupt-cache condition rather than a
// network one. The size-ceiling class is deliberately not
// ExitIntegrity: it is a size ceiling, not a digest that failed to match -
// the same shape as the deadline sentinels above it, unaggregated here and
// ExitInstall once joined behind helpers.ErrInstallationFailed by
// isFileIntegrityError's match on that headline, identical to every other
// per-collection cause.
//
// The server-side refusals - a Galaxy server list walk aborting and a git
// source's wire failures - are matched by isRemoteRefusalError; the split is
// purely to stay under the cyclomatic-complexity budget and the two together
// still cover one class.
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

// isRemoteRefusalError reports whether err is a remote that refused or could
// not answer: a Galaxy server-list walk aborting on a credential failure or
// an exhausted retry budget (helpers.ErrGalaxyAuthFailed,
// helpers.ErrGalaxyServerUnavailable), and the git shapes of the same two - a
// transport, protocol or host-key-database failure while talking to a
// repository (helpers.ErrGitTransportFailed) and a credential or host key the
// remote did not accept (helpers.ErrGitAuthFailed). All four are runtime
// conditions to investigate rather than configuration shapes to fix.
func isRemoteRefusalError(err error) bool {
	return errors.Is(err, helpers.ErrGalaxyAuthFailed) ||
		errors.Is(err, helpers.ErrGalaxyServerUnavailable) ||
		errors.Is(err, helpers.ErrGitTransportFailed) ||
		errors.Is(err, helpers.ErrGitAuthFailed)
}

// isMetadataFetchError reports whether err is a Galaxy metadata-response
// sentinel: metadata that could not be fetched (including one whose fetch
// exceeded its own deadline, or whose versions list kept reporting more
// pages than the page ceiling allows) or parsed into the shape this tool
// expects. helpers.ErrLatestVersionLookupFailed belongs here on the same
// basis as every other member: `outdated`'s own per-entry lookup is itself a
// root-metadata fetch, so the headline's bare, unaggregated shape - no cause
// joined behind it - names a metadata-fetch failure.
//
// helpers.ErrUnsupportedDownloadURLScheme belongs here for the same reason
// helpers.ErrMissingDownloadURL does: metadata that parsed into the expected
// shape and still cannot yield a fetchable artifact is the same defect as
// metadata carrying no download URL at all, so it classifies alike rather
// than earning an exit class of its own. helpers.ErrMetadataRequestBuildFailed
// is that same defect one step earlier - a metadata URL net/http will not
// build a request from names nothing fetchable either - and it is deliberately
// not routed to the userinfo class below: net/http refusing a value is not a
// verdict about a credential, and the value it refused is never inspected.
//
// helpers.ErrLatestVersionLookupFailed is also an aggregation headline whose
// per-entry causes are joined behind it via errors.Join, exactly like
// helpers.ErrInstallationFailed elsewhere in this package - so this function
// matching the bare headline is not the last word once a cause is joined
// in. Whenever a joined cause must classify differently, the rule is a
// predicate on the error tree, not an enumeration of which command produced
// it: a lockfile entry whose name is not a "namespace.name" FQDN carries
// helpers.ErrLockfileInvalid instead, and isLockError is checked ahead of
// isNetworkError in the exitClasses table, so errors.Is walking the joined
// tree lets isLockError claim it first regardless of what else is joined
// alongside it. A metadata URL refused for embedded userinfo is the second
// example of that same rule: it answers isServerSuppliedURLPolicyError, whose
// entry sits ahead of this one's for the identical reason.
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

// isResolutionError reports whether err is a dependency-resolution sentinel
// (conflicting constraints, missing candidates, a cycle in the graph, or a
// dependency map key that is not a valid "namespace.name" FQDN). The last
// one, helpers.ErrInvalidDependencyKey, is malformed graph input discovered
// while walking a collection's declared dependencies - no retry repairs it,
// the same reasoning that makes every other member of this class a
// resolution failure rather than a network or install one. The members that
// say no candidate exists at all are matched by isNoCandidateError; the split
// is purely to stay under the cyclomatic-complexity budget and the two
// together still cover one class.
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

// isNoCandidateError reports whether err says nothing exists for what
// requirements.yml asked for: a Galaxy collection with no semver release
// (helpers.ErrNoSemverCandidates), and the same answer from a repository - a
// git ref the remote does not advertise, or a commit it does not hold
// (helpers.ErrGitRefNotFound, helpers.ErrGitCommitNotFound).
func isNoCandidateError(err error) bool {
	return errors.Is(err, helpers.ErrNoSemverCandidates) ||
		errors.Is(err, helpers.ErrGitRefNotFound) ||
		errors.Is(err, helpers.ErrGitCommitNotFound) ||
		errors.Is(err, helpers.ErrRoleNotFound) ||
		errors.Is(err, helpers.ErrRoleVersionNotFound) ||
		errors.Is(err, helpers.ErrRoleVersionsIncomparable) ||
		// A url source's one candidate - the artifact its MANIFEST.json
		// declares - is not the version the requirements entry asserted:
		// the same "nothing exists for what was asked" answer a ref the
		// remote does not advertise gives.
		errors.Is(err, helpers.ErrURLCollectionVersionMismatch)
}

// isUsageError reports whether err is a configuration or CLI-input sentinel
// (invalid flags, malformed requirements, or a missing path). Split into
// sub-checks purely to stay under the cyclomatic-complexity budget; together
// they still cover the exact same sentinel set.
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

// isCommandLineUsageError reports whether err says the command line cannot be
// run as written because of something go-galaxy judged rather than urfave:
// positional arguments a command does not take
// (helpers.ErrUnexpectedArguments), or the one a command requires and was not
// given (helpers.ErrMissingArgument). A flag urfave cannot parse never reaches
// this class, since urfave returns it from Run without an error value to
// classify and handleResult exits ExitUsage for that shape directly.
func isCommandLineUsageError(err error) bool {
	return errors.Is(err, helpers.ErrUnexpectedArguments) ||
		errors.Is(err, helpers.ErrMissingArgument)
}

// isRoleUsageError reports whether err says a roles: entry, as written in
// requirements.yml, cannot be used as given: a value that is not a list, an
// entry of a shape ansible would not take either or carrying a collection
// key, a name, install name or version outside the role alphabets, a source
// this tool does not install from (a tarball, a local path, an scm other
// than git, an include: of a second file), or two entries sharing one
// install directory; and, past load, the answers no retry could change: no
// configured server serves the v1 role API, a v1 record this tool cannot
// build a repository URL from, and a repository that is not a role or whose
// meta this tool cannot read. The remedy is an edit to the file or the
// configuration.
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
// would come from: a source this tool does not install from, a server with
// no v1 role API, a v1 record it cannot act on, a repository that is not a
// role.
func isRoleSourceUsageError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedRoleSource) ||
		errors.Is(err, helpers.ErrUnsupportedRoleScm) ||
		errors.Is(err, helpers.ErrUnsupportedRoleInclude) ||
		errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable) ||
		errors.Is(err, helpers.ErrGalaxyRoleInvalid) ||
		errors.Is(err, helpers.ErrRoleMetaNotFound) ||
		errors.Is(err, helpers.ErrRoleMetaInvalid)
}

// isGitUsageError reports whether err says a git source, as written in
// requirements.yml or bound through the environment, cannot be used as given:
// a URL, ref, subdir or locator this tool refuses; an explicit collection name
// the repository does not carry; a repository (or subdir) holding no
// collection, or holding one twice; a galaxy.yml that cannot be built from, or
// whose version is not exact; a credential binding that does not parse; or an
// ssh source with neither a bound key nor an agent. The predicate every member
// shares with the rest of this class is that an operator has to change
// something - the requirements file, a galaxy.yml in the repository, or an
// environment variable - and no retry repairs it. That holds even for the
// members discovered only after a network round trip (a missing galaxy.yml is
// learned from the fetched tree): what the round trip found is a shape
// defect, not a transport one. Split into the locator half here and the
// repository-content half in isGitContentUsageError purely to stay under the
// cyclomatic-complexity budget; the two together still cover one class.
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

// isGitContentUsageError reports whether err says what the repository holds
// cannot be used as given: an explicit collection name it does not carry, no
// collection (or one twice) under the subdir, or a galaxy.yml that cannot be
// built from or whose version is not exact.
func isGitContentUsageError(err error) bool {
	return errors.Is(err, helpers.ErrGitNameMismatch) ||
		errors.Is(err, helpers.ErrGitCollectionNotFound) ||
		errors.Is(err, helpers.ErrGitDuplicateCollection) ||
		errors.Is(err, helpers.ErrGalaxyYMLInvalid) ||
		errors.Is(err, helpers.ErrGitCollectionVersionNotExact)
}

// isURLUsageError reports whether err says a url source, as written in
// requirements.yml or bound through the environment, cannot be used as
// given: a tarball URL or persisted locator this tool refuses, a URL
// carrying a credential, a credential binding that does not parse, or a role
// tarball whose layout or entries cannot become one role. The predicate
// every member shares with the rest of this class is the one
// isGitUsageError states: an operator has to change something, and no retry
// repairs it - even for the members discovered only after a download, since
// what the download found is a shape defect, not a transport one.
func isURLUsageError(err error) bool {
	return errors.Is(err, helpers.ErrInvalidURLRequirement) ||
		errors.Is(err, helpers.ErrURLRequirementUserinfo) ||
		errors.Is(err, helpers.ErrInvalidURLLocator) ||
		errors.Is(err, helpers.ErrURLCredentialInvalid) ||
		errors.Is(err, helpers.ErrRoleTarballLayout) ||
		errors.Is(err, helpers.ErrRoleTarballEntryInvalid)
}

// isSignatureConfigError reports whether err says this run's signature
// configuration cannot be used as given: a source that does not name something
// this tool fetches, a source URL embedding a credential in its userinfo, a
// keyring it cannot read or whose container format it does not open, a
// requirements file declaring signatures with no keyring configured, or a
// required-count, ignored-status-code, or disable-verification value it does
// not accept, one of the two settings that name something supplied as an empty
// value, or a requirements entry declaring more signature sources than
// helpers.MaxSignaturesPerCollection allows. The predicate every member shares is the one every other usage
// sentinel shares: an operator has to change something - and no retry repairs
// it. Which places that means is narrower here than for a usage sentinel in
// general: a flag, an environment value, or the requirements file, never
// ansible.cfg, since this whole surface is configured from flags and their
// environment variables alone (see cliflags.SignatureFlags for why).
//
// helpers.ErrSignatureSourceUserinfo belongs here on that predicate rather than
// by association with the fetch that raised it: the value is refused before a
// request is composed, and the remedy is deleting the credential from the
// requirements file. It is deliberately not isSignatureTransportError's, since
// nothing was fetched and no endpoint answered.
//
// None of them is a signature verdict, which is what keeps them out of
// isSignatureError and its exit class: a run that hits one of these never
// checked a signature at all, so reporting it as a verification failure would
// tell a pipeline the artifact was rejected when the configuration was.
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
// sentinel or a recorded-state usage sentinel. Split into two sub-checks
// purely to stay under the cyclomatic-complexity budget; the two together
// still cover the exact same sentinel set.
func isConfigUsageError(err error) bool {
	return isEnvironmentUsageError(err) || isRecordedStateUsageError(err)
}

// isEnvironmentUsageError reports whether err is a config/environment-level
// usage sentinel. helpers.ErrCacheBackendUnusable belongs here on the same
// predicate as every other member: the remedy is a configuration change and
// no retry can succeed, since the configured backend cannot provide a
// guarantee this tool requires (or cannot be addressed at all) regardless of
// how many times the same request is retried.
func isEnvironmentUsageError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, helpers.ErrConfigIsNil) ||
		errors.Is(err, helpers.ErrS3EmptyCreds) ||
		errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) ||
		errors.Is(err, helpers.ErrCacheDirEmpty) ||
		errors.Is(err, helpers.ErrInvalidTimeout) ||
		errors.Is(err, helpers.ErrAnsibleConfigNotFound) ||
		errors.Is(err, helpers.ErrWarmCacheDisabled) ||
		errors.Is(err, helpers.ErrCacheBackendUnusable)
}

// isRecordedStateUsageError reports whether err is a usage sentinel about
// something this program persisted or tracks on a caller's behalf and cannot
// use as recorded, where the fix is an operator action, never a retry.
//
// helpers.ErrUnsupportedSchemaVersion belongs here rather than in
// isCacheCorruptError for the identical reason that function's own doc
// comment states from the other side: a schema version newer than this
// binary understands describes this reader, not the snapshot - the object
// was written correctly by a newer binary this one cannot safely interpret,
// so the remedy is an environment change (a newer binary, or a different
// cache), not discarding data a newer runner still depends on.
//
// helpers.ErrProjectRequirementsUnreadable belongs here on the same
// predicate: a recorded project's requirements file this program cannot
// load, whatever the underlying cause, is something an operator must fix by
// hand - editing or restoring the file - not something retrying the same
// run can repair. A load failure matching errors.Is(err, fs.ErrNotExist) is
// not this sentinel's concern at all: cleanup treats a recorded
// requirements file that fails to load that way as a stale registry entry -
// a single warning, contributing no reachability roots - rather than a load
// failure, so that case never reaches FromError as an error in the first
// place. This sentinel, and therefore this exit code, is reserved for any
// other load failure.
func isRecordedStateUsageError(err error) bool {
	return errors.Is(err, helpers.ErrUnsupportedSchemaVersion) ||
		errors.Is(err, helpers.ErrProjectRequirementsUnreadable)
}

// isGalaxyServerConfigError reports whether err is a Galaxy server
// configuration sentinel: a bad [galaxy_server.<id>] section, a bad
// server_list id, or a server whose URL/credential/TLS combination this
// tool refuses. Every one of these is raised while building the config,
// before a single request is made, which is what makes them usage errors
// rather than network ones - the operator has to edit ansible.cfg or an
// environment variable, not retry. Split into two sub-checks purely to
// stay under the cyclomatic-complexity budget; the two together still
// cover the exact same sentinel set.
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

// isGalaxyServerPolicyError reports whether err says the configuration
// parses but describes something this tool refuses to do: leak a
// credential through a URL or a plaintext transport, give one origin two
// different credential/TLS policies, or pair a credential with a server
// address or TLS policy this run did not source from the operator -
// config.checkTokenPairing's two sentinels, which share this class because
// they share one remedy: an operator export naming the address, or the TLS
// policy, on the operator's own channel.
func isGalaxyServerPolicyError(err error) bool {
	return errors.Is(err, helpers.ErrGalaxyServerURLUserinfo) ||
		errors.Is(err, helpers.ErrInsecureTokenTransport) ||
		errors.Is(err, helpers.ErrConflictingServerTLSPolicy) ||
		errors.Is(err, helpers.ErrConflictingServerToken) ||
		errors.Is(err, helpers.ErrAmbiguousGalaxyToken) ||
		errors.Is(err, helpers.ErrTokenDestinationFromAnsibleConfig) ||
		errors.Is(err, helpers.ErrTokenTLSPolicyFromAnsibleConfig)
}

// isCollectionNameUsageError reports whether err is an invalid-collection-name/format
// sentinel. helpers.ErrConflictingNamespaceName joins this class for the
// same reason: an explicit namespace given alongside a dotted collection
// name is a malformed requirements.yml entry, raised while parsing
// requirements before any resolution or network work starts, exactly like
// every other member here.
func isCollectionNameUsageError(err error) bool {
	return errors.Is(err, helpers.ErrEmptyCollectionName) ||
		errors.Is(err, helpers.ErrInvalidCollectionName) ||
		errors.Is(err, helpers.ErrInvalidCollectionKey) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionSource) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionType) ||
		errors.Is(err, helpers.ErrUnsupportedCollectionFormat) ||
		errors.Is(err, helpers.ErrConflictingNamespaceName)
}

// isCollectionListUsageError reports whether err is an invalid-collections-list
// sentinel. helpers.ErrUnsafeCollectionIdentifier and
// helpers.ErrInvalidCollectionVersion sit here alongside
// ErrDuplicateCollectionKey: buildCollectionsMap raises all three while
// folding a resolved set into a key-addressed identity map, before any
// install or warm work starts, over malformed resolution input rather than
// anything the network or the install pipeline did.
// helpers.ErrInvalidCollectionVersion has a second producer with the
// identical shape but a different caller: buildLockfile
// (internal/galaxy/collections/lock.go) raises it while assembling a
// lockfile from an already-resolved map, checked before that entry's own
// metadata fetch runs. Both producers run before any of their command's own
// per-collection work begins - a worker in install/warm's case, buildLockfile's
// own iteration in lock's case, which has no worker pool at all - which is
// what keeps the sentinel out of helpers.ErrInstallationFailed's aggregation
// and therefore out of isInstallError above, checked earlier in the
// exitClasses table.
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
