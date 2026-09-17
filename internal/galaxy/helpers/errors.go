// Package helpers is this module's shared vocabulary: the sentinel errors
// every other package wraps with %w, the tuning constants and size caps they
// enforce, the server-scoped cache-key builders, and small pure predicates
// over untrusted values - IsPathElement, IsCollectionName, IsSHA256Hex,
// IsExactVersion. It exists so a value's shape is judged by one rule wherever
// it enters the program, and so an error class can be matched across layers
// without either layer importing the other.
//
// It sits at the bottom of the import graph and depends on nothing else in
// this module except internal/safeout, whose rune predicate IsPathElement
// consults. WriteFileAtomic is the only thing here that touches a filesystem;
// everything else is a value, a predicate, or a wrapper the caller drives -
// Retry and NewSizeLimitedReader among them.
package helpers

import "errors"

var (
	// ErrSymlinkTargetResolvesToSelf indicates a symlink resolves to itself.
	ErrSymlinkTargetResolvesToSelf = errors.New("symlink target resolves to self")
	// ErrSymlinkTargetEscapesDestination indicates a symlink escapes the target directory.
	ErrSymlinkTargetEscapesDestination = errors.New("symlink target escapes destination")
	// ErrSymlinkTarget indicates a symlink target is invalid.
	ErrSymlinkTarget = errors.New("symlink target is invalid")
	// ErrSymlinkTargetResolvesToRoot indicates a symlink resolves to the root directory.
	ErrSymlinkTargetResolvesToRoot = errors.New("symlink target resolves to root")
	// ErrSymlinkTargetIsAbsolute indicates a symlink target is an absolute path.
	ErrSymlinkTargetIsAbsolute = errors.New("symlink target is absolute")
	// ErrSymlinkTargetIsEmpty indicates a symlink target is empty.
	ErrSymlinkTargetIsEmpty = errors.New("symlink target is empty")

	// ErrArchivePathContainsSymlinkComponent indicates an archive path traverses a symlink.
	ErrArchivePathContainsSymlinkComponent = errors.New("archive path contains symlink component")
	// ErrArchiveExceedsMaxSize indicates an archive exceeds the maximum total size.
	ErrArchiveExceedsMaxSize = errors.New("archive exceeds maximum total size")
	// ErrArchiveDecompressedTooLarge indicates an archive made a reader of it
	// pull more raw bytes out of its decompressor than ArchiveMaxDecompressedSize
	// allows. It is a separate sentinel from ErrArchiveExceedsMaxSize on
	// purpose: that one fires on the sizes an archive's headers DECLARE, this
	// one on the bytes archive/tar actually reads, and the two disagree for
	// every shape whose declared sizes understate what the reader consumes - a
	// sparse entry, or a chain of PAX/GNU meta headers the extractor never sees
	// a header for at all. Pinning which of the two gates fired is what tells a
	// reader of a failing test whether the declared-size budget or the stream
	// cap is the rule doing the work.
	ErrArchiveDecompressedTooLarge = errors.New("archive decompressed stream exceeds maximum size")
	// ErrArchiveEntryHasNegativeSize indicates an archive entry has a negative size.
	ErrArchiveEntryHasNegativeSize = errors.New("archive entry has negative size")
	// ErrArchiveEntryIsTooLarge indicates an archive entry is too large.
	ErrArchiveEntryIsTooLarge = errors.New("archive entry is too large")
	// ErrArchiveEntryEscapesDestination indicates an archive entry escapes the destination.
	ErrArchiveEntryEscapesDestination = errors.New("archive entry escapes destination")
	// ErrArchiveEntryIsAbsolutePath indicates an archive entry uses an absolute path.
	ErrArchiveEntryIsAbsolutePath = errors.New("archive entry is absolute path")
	// ErrArchiveEntryHasEmptyName indicates an archive entry has an empty name.
	ErrArchiveEntryHasEmptyName = errors.New("archive entry has empty name")
	// ErrArchiveEntryNameTooLong indicates an archive entry declares a name, or
	// a link target, longer than ArchiveMaxEntryNameLen. It is raised by a
	// reader that walks an archive without unpacking it, never by the
	// extractor, whose own retention is bounded by the filesystem instead; see
	// ArchiveMaxEntryNameLen for what the cap bounds and why a reader that
	// holds every name until the stream ends needs it.
	ErrArchiveEntryNameTooLong = errors.New("archive entry name is too long")
	// ErrArchiveTooManyEntries indicates an archive contains more entries
	// than ArchiveMaxEntryCount allows.
	ErrArchiveTooManyEntries = errors.New("archive contains too many entries")
	// ErrArchiveDuplicateEntry indicates a tar archive contains a regular-file
	// entry whose on-disk path is already occupied by something the same
	// archive extracted earlier - most often a second regular-file entry at
	// that path, but equally a directory entry the file now collides with.
	// Extracted regular files are opened read-only (see
	// archive.extractRegularFile), so such an entry fails os.OpenFile with a
	// bare permission error against what is already there, which is
	// undebuggable on its own; this sentinel reclassifies that failure into
	// something that names the offending archive path. The classification is
	// existence-based rather than errno-based, so it deliberately covers both
	// collision shapes under one name - "two entries want the same path" is
	// the actionable fact either way. Refusing the archive outright is
	// deliberate: letting the later entry win is what would make a colliding
	// archive undetectable, since nothing on disk afterwards records that a
	// path was claimed twice.
	//
	// A reader that writes nothing raises it for the fact underneath rather
	// than for that on-disk consequence: manifest.claim refuses two entries
	// competing for one archive path, link entries included, so the pass that
	// verifies a manifest chain and the pass that read the manifest cannot
	// disagree about which entry a name meant.
	ErrArchiveDuplicateEntry = errors.New("archive contains a duplicate entry")

	// ErrEmptyGzipMember indicates a gzip stream presented a member that
	// produced no bytes at all. Every gzip stream this program reads is either
	// a collection artifact or its own persisted cache state, and a member
	// contributing nothing cannot be part of either: the smallest tar stream
	// there can be is the 1024-byte end marker its two zero blocks make, and
	// store.Store.MarshalSnapshot never produces zero bytes. That is why the
	// rule is a property of what such a member can be rather than a count of
	// how many are too many - internal/gzipstream, its one producer, refuses
	// the first one and never reaches the second.
	//
	// It is deliberately in no cmd/go-galaxy/exitcode predicate, and the
	// reason is that every reader of it already classifies without one. The
	// download path's shape probe wraps it in ErrArtifactNotTarGz, which
	// isArtifactShapeError already covers; the extractor and the manifest
	// readers reach ExitInstall through the per-collection aggregation behind
	// ErrInstallationFailed, exactly as every other cause a worker hits does;
	// and the S3 backend's state-object read is neither of those - adding this
	// sentinel to isArtifactShapeError would newly report a corrupt snapshot
	// object as an install failure, on a run that may have installed nothing
	// at all.
	//
	// That third reader is handled by reclassification at its own producer
	// rather than left unclassified: internal/cache/s3's readObject wraps this
	// sentinel into ErrCorruptStateObject, which classifies ExitCacheCorrupt
	// beside the object's other two unusable-state verdicts. The wrap keeps
	// this sentinel reachable through errors.Is, which is safe precisely
	// because it belongs to no predicate of its own and so can carry no second
	// exit class into the tree.
	ErrEmptyGzipMember = errors.New("gzip stream carries a member that produces no bytes")

	// ErrArtifactNotTarGz indicates downloaded bytes do not have the outer
	// shape of a collection artifact: they are not gzip, or nothing that looks
	// like a tar stream begins inside the gzip. It says nothing about the
	// archive's completeness or its contents - only that what arrived is not
	// the kind of thing worth putting into a shared cache slot.
	ErrArtifactNotTarGz = errors.New("downloaded artifact is not a gzip-compressed tar archive")

	// ErrArtifactTarHeaderNotFound indicates an artifact's decompressed tar
	// stream presented no header within ArchiveProbeMaxBytes of its start, so
	// the shape probe stopped rather than reading as far as the archive wanted
	// it to.
	//
	// It is deliberately NOT ErrArtifactNotTarGz. That one says the bytes are
	// not a gzip-compressed tar; here they are - valid gzip carrying valid tar
	// framing, in a chain of meta headers archive/tar consumes inside Next()
	// without ever returning one - so reporting it would be a false statement,
	// sending an operator to look for an error page inside an artifact that
	// does hold a tar stream.
	//
	// It is deliberately NOT ErrArchiveDecompressedTooLarge either, for the
	// reason that sentinel's own doc comment gives for standing apart from
	// ErrArchiveExceedsMaxSize: each names one cap, and pinning which gate
	// fired is what tells a reader of a failing test which rule did the work.
	// That one is the extractor's ceiling on a whole stream; this is the
	// probe's bound on a look at an archive's head.
	//
	// A well-formed empty tar is unaffected: its trailer sits inside the bound,
	// tar.Reader.Next reports io.EOF, and the probe accepts it.
	// ErrManifestNotFound is the semantic precedent - a document that was not
	// presented within a window, rather than an archive that is malformed.
	ErrArtifactTarHeaderNotFound = errors.New("artifact presents no tar header within the shape probe's scan bound")

	// ErrHardlinkTargetIsEmpty indicates a hardlink target is empty.
	ErrHardlinkTargetIsEmpty = errors.New("hardlink target is empty")
	// ErrFileIsEmpty indicates a file is empty.
	ErrFileIsEmpty = errors.New("file is empty")

	// ErrS3EmptyCreds indicates S3 cache credentials are required but missing.
	ErrS3EmptyCreds = errors.New("s3 cache requires access/secret keys when GO_GALAXY_S3_BUCKET is set")

	// ErrArtifactCacheNotConfigured indicates the artifact cache is unavailable.
	ErrArtifactCacheNotConfigured = errors.New("artifact cache is not configured")
	// ErrMetadataIsNil indicates metadata is nil when required.
	ErrMetadataIsNil = errors.New("metadata is nil")
	// ErrMissingDownloadURL indicates a collection download URL is missing.
	ErrMissingDownloadURL = errors.New("missing download url")
	// ErrUnsupportedDownloadURLScheme indicates a collection's download URL
	// names something other than the two schemes this tool fetches over. The
	// value is judged against an allow-list rather than a blocklist because it
	// is not this program's own: it arrives from a Galaxy server's version
	// metadata, or from a cached snapshot a bucket writer could poison. A
	// blocklist would have to name every scheme worth refusing and would admit
	// anything it forgot; the allow-list admits exactly what the pipeline
	// speaks and refuses the rest by default.
	ErrUnsupportedDownloadURLScheme = errors.New("collection download url scheme is not http or https")
	// ErrDownloadURLUserinfo indicates a collection's download URL embeds
	// userinfo, e.g. "https://user:pass@objects.example/a.tar.gz".
	//
	// The value decides what authenticates the request as well as where it
	// goes, and the first half of that is measured rather than recalled: an
	// *http.Client whose RoundTripper only records what it is handed, given
	// "https://user:s3cr3t@127.0.0.1/artifact.tar.gz", observed
	// `Authorization: Basic dXNlcjpzM2NyM3Q=` on the request, with the header
	// unset before the call - net/http composes Basic auth from a URL's
	// userinfo before any transport runs. The second half is what that costs
	// here: fetch.authTransport returns early for a request whose
	// Authorization header is already set, so a credential a Galaxy server (or
	// a snapshot replaying one) chose silently replaces the operator's own
	// configured token on the request that fetches the artifact.
	//
	// url.URL.String() renders that password back out in plain text at every
	// sink the value reaches, which is why the refusal must also keep it out of
	// its own message.
	//
	// It is deliberately NOT ErrGalaxyServerURLUserinfo, which is scoped to a
	// value that SELECTS a server and is raised while building the config out
	// of operator-authored input, before any request exists;
	// ErrSignatureSourceUserinfo above already draws that same distinction for
	// a value a repository authored, and states it once. And it is deliberately
	// NOT ErrUnsupportedDownloadURLScheme: that one says the value names
	// nothing this tool fetches, while this one names something perfectly
	// fetchable and carries a credential while doing it, so the remedy is
	// deleting the credential rather than rewriting the URL.
	ErrDownloadURLUserinfo = errors.New("collection download url must not contain userinfo")
	// ErrMetadataURLUserinfo indicates a Galaxy metadata URL embeds userinfo:
	// a server's own versions_url or highest_version.href, or either of them
	// replayed out of a poisonable cached snapshot, resolved against the base
	// that served it.
	//
	// The two effects are ErrDownloadURLUserinfo's above, unchanged - net/http
	// composes Basic auth from the userinfo before any transport runs, and
	// fetch.authTransport then declines to attach the operator's own token -
	// and that sentinel holds the measurement.
	//
	// It is a second sentinel rather than a reuse of that one because the two
	// name two different fields an operator inspects and edits nothing of:
	// an artifact's download_url, and the metadata reference the version walk
	// follows. This project already splits its userinfo sentinels by the
	// boundary a value enters through rather than by the effect they share -
	// ErrGalaxyServerURLUserinfo and ErrSignatureSourceUserinfo are the same
	// split one layer out. Both of these two classify identically all the same;
	// exitcode.isServerSuppliedURLPolicyError holds that argument.
	ErrMetadataURLUserinfo = errors.New("galaxy metadata url must not contain userinfo")
	// ErrMetadataRequestBuildFailed indicates net/http refused to build a
	// request from a Galaxy metadata URL - a server's own versions_url or
	// highest_version.href, either replayed out of a poisonable cached
	// snapshot, or a root-metadata candidate built from a configured server's
	// base - because url.Parse will not accept the value as a URL at all.
	//
	// It names no part of that value, deliberately, and that refusal to render
	// is the whole reason it exists. The error net/http returns from the
	// refused build is a *url.Error naming the raw string whole: measured,
	// http.NewRequest over "https://user:s3cr3t@h:notaport/x?sig=abc" returns
	// `parse "https://user:s3cr3t@h:notaport/x?sig=abc": invalid port
	// ":notaport" after host`, with the password in cleartext. net/http masks a
	// password only when its client composes a transport error, never when
	// NewRequest refuses to build one.
	//
	// Three reasons make this a replacement of that message rather than a cut
	// of it. The population reaching this sentinel is exactly the values
	// url.Parse refuses, which is the one place WithoutUserinfo's residual is
	// least bounded: that function's own doc comment bounds the residual by
	// arguing url.Parse never reads such a value as carrying userinfo, so the
	// credential authenticates nothing - a claim about the wire, and no comfort
	// here, where the refused set is the entire population and the cut would be
	// doing all the work. What leaks is not this program's rendering but
	// *url.Error's, so cutting it would mean errors.As-ing that error apart and
	// rebuilding its message, which is a replacement performed tacitly rather
	// than a cut. And nothing actionable is lost by dropping it: a refused
	// build means a server declared something that is not a URL, so there is
	// nothing for an operator to go edit and those bytes are not what they
	// need - the run names the collection and the failure instead.
	//
	// It is deliberately not an *HTTPStatusError (internal/galaxy/cache), which
	// leaves collections.tryServerRootMetadata's status routing untouched: no
	// server answered anything here, so there is no status to route the walk
	// by. It classifies ExitNetwork through exitcode.isMetadataFetchError, the
	// class ErrUnsupportedDownloadURLScheme carries there and for the same
	// reason - metadata that cannot name something this tool can fetch is one
	// defect class, whether the URL is absent, unusable, or unbuildable. A
	// sentinel of its own rather than a reuse of a member of that set is what
	// an operator gains here: ErrMetadataUnavailable, the only member whose
	// wording would fit, is load-bearing in collections.prepareInstall as the
	// one tolerated shape a cache hit may proceed on, so reusing it would turn
	// a metadata URL nobody can request into a silent install from cache; every
	// other member names a nil metadata value, a download URL, a versions
	// payload, a deadline, or the `outdated` command's own aggregation
	// headline.
	ErrMetadataRequestBuildFailed = errors.New("galaxy metadata url could not be built into a request")
	// ErrConfigIsNil indicates a nil config was provided.
	ErrConfigIsNil = errors.New("config is nil")
	// ErrSHA256Mismatch indicates a checksum mismatch.
	ErrSHA256Mismatch = errors.New("sha256 mismatch")
	// ErrMetadataUnavailable indicates metadata could not be loaded.
	ErrMetadataUnavailable = errors.New("metadata unavailable")
	// ErrUnsupportedRequirementsFormat indicates the requirements file format is unsupported.
	ErrUnsupportedRequirementsFormat = errors.New("unsupported requirements file format")

	// ErrCacheDirEmpty indicates the cache directory is empty.
	ErrCacheDirEmpty = errors.New("cache directory is empty")
	// ErrAnotherInstanceIsRunning indicates another instance is already running.
	ErrAnotherInstanceIsRunning = errors.New("another instance is running")
	// ErrCacheBusy indicates the cache is held by another holder and could
	// not be acquired within this backend's own ceiling - true of a local
	// Bolt open timeout and of the S3 distributed lock's wait-ceiling
	// timeout against an observed holder alike; neither names the other's
	// mechanism.
	ErrCacheBusy = errors.New("another process holds the cache")
	// ErrCacheLockLost indicates the cache lock was acquired by this run and
	// then taken away mid-run: a backend observed the lock object recording
	// some other holder's token while this run was still working under it.
	// That is a different condition from ErrCacheBusy, which says the lock
	// was never acquired in the first place, and the remedy differs with it -
	// a busy cache invites a retry once the other holder finishes, a lost one
	// says everything this run wrote after the takeover was written without
	// exclusivity and cannot be trusted.
	//
	// It carries none of the three cache-backend classes
	// (ErrCacheBackendUnavailable, ErrCacheBackendUnusable, ErrCacheBusy):
	// those describe a backend's ability to reach or serve its store, while
	// this describes ownership of a lock the backend served perfectly well.
	// It is a fourth condition with its own exit class, the same way
	// ErrCorruptSnapshotStore sits outside that partition.
	//
	// A carrier must never wrap a context sentinel behind it with %w. The
	// holder context a backend cancels on loss carries context.Canceled, and
	// cmd/go-galaxy/exitcode's FromError checks context.Canceled ahead of
	// every other class, so a %w would report a stolen lock as an operator's
	// own Ctrl-C. Render the cause with %v - see internal/galaxy/cache's
	// LockLostError, the single producer of a run's own lock-loss verdict.
	ErrCacheLockLost = errors.New("cache lock ownership was lost to another holder")
	// ErrCacheBackendUnavailable indicates a remote cache backend could not
	// be reached, or answered a request with a failure that is not this
	// program's own doing.
	ErrCacheBackendUnavailable = errors.New("cache backend unavailable")
	// ErrCacheBackendUnusable indicates the configured backend cannot
	// provide a guarantee this tool requires, or cannot be addressed at
	// all; no retry can change that and the only remedy is a configuration
	// change.
	ErrCacheBackendUnusable = errors.New("cache backend cannot be used as configured")
	// ErrNoSemverCandidates indicates no semver candidates are available.
	ErrNoSemverCandidates = errors.New("no semver candidates available")
	// ErrMissingResolvedParent indicates a resolved parent is missing.
	ErrMissingResolvedParent = errors.New("missing resolved parent")
	// ErrMissingResolvedDependency indicates a resolved dependency is missing.
	ErrMissingResolvedDependency = errors.New("missing resolved dependency")
	// ErrNoVersionSatisfiesConstraints indicates no version satisfies constraints.
	ErrNoVersionSatisfiesConstraints = errors.New("no version satisfies constraints")
	// ErrConflictingRootConstraints indicates root constraints conflict.
	ErrConflictingRootConstraints = errors.New("conflicting root constraints")
	// ErrConflictingExactVersions indicates exact version constraints conflict.
	ErrConflictingExactVersions = errors.New("conflicting exact versions")
	// ErrDependencyGraphHasACycle indicates the dependency graph has a cycle.
	ErrDependencyGraphHasACycle = errors.New("dependency graph has a cycle")
	// ErrVersionsPayloadEmpty indicates a versions payload is empty.
	ErrVersionsPayloadEmpty = errors.New("versions payload is empty")
	// ErrVersionsPayloadUnsupported indicates a versions payload is unsupported.
	ErrVersionsPayloadUnsupported = errors.New("unsupported versions payload")
	// ErrVersionsPagingExceeded indicates the versions list kept reporting
	// more pages than the page ceiling allows. This is treated as a hard
	// failure rather than a silent truncation, since a caller resolving
	// against a truncated list could pick a version that does not actually
	// satisfy the requested constraints.
	ErrVersionsPagingExceeded = errors.New("versions pagination exceeded the page ceiling")
	// ErrDownloadFailed indicates a download failed.
	ErrDownloadFailed = errors.New("download failed")
	// ErrMissingResolvedRoot indicates a resolved root is missing.
	ErrMissingResolvedRoot = errors.New("missing resolved root")
	// ErrInstallationFailed indicates installation failed.
	ErrInstallationFailed = errors.New("installation failed")
	// ErrInvalidCollectionsList indicates the collections list is invalid.
	ErrInvalidCollectionsList = errors.New("invalid collections list")
	// ErrMissingCollection indicates a collection is missing.
	ErrMissingCollection = errors.New("missing collection")
	// ErrInvalidCollectionEntry indicates a collection entry is invalid.
	ErrInvalidCollectionEntry = errors.New("invalid collection entry")
	// ErrEmptyCollectionName indicates a collection name is empty.
	ErrEmptyCollectionName = errors.New("empty collection name")
	// ErrUnsupportedCollectionSource indicates a collection source is unsupported.
	ErrUnsupportedCollectionSource = errors.New("unsupported collection source")
	// ErrUnsupportedCollectionType indicates a collection type is unsupported.
	ErrUnsupportedCollectionType = errors.New("unsupported collection type")
	// ErrUnsupportedCollectionFormat indicates a collection format is unsupported.
	ErrUnsupportedCollectionFormat = errors.New("unsupported collection format")
	// ErrInvalidCollectionName indicates a collection name is invalid.
	ErrInvalidCollectionName = errors.New("invalid collection name")
	// ErrInvalidDependencyKey indicates a dependency map key is not a valid
	// "namespace.name" FQDN.
	ErrInvalidDependencyKey = errors.New("invalid dependency key")
	// ErrConflictingNamespaceName indicates an explicit namespace was given
	// alongside a dotted collection name, which would otherwise silently
	// install a different collection than either field implies alone.
	ErrConflictingNamespaceName = errors.New("explicit namespace conflicts with dotted collection name")
	// ErrInvalidCollectionKey indicates a collection key is invalid.
	ErrInvalidCollectionKey = errors.New("invalid collection key")
	// ErrDuplicateCollectionRequirement indicates a duplicate collection requirement.
	ErrDuplicateCollectionRequirement = errors.New("duplicate collection requirement")
	// ErrLoadMetadataFailed indicates loading collection metadata failed.
	ErrLoadMetadataFailed = errors.New("failed to load collection metadata")
	// ErrDuplicateCollectionKey indicates a duplicate collection entry.
	ErrDuplicateCollectionKey = errors.New("duplicate collection entry")
	// ErrWarmCacheDisabled indicates the warm command was run with --no-cache.
	// Unlike install, whose --no-cache still yields a correct install, warm's
	// entire output IS cache state: with caching disabled, every artifact
	// would be downloaded and then discarded without ever being committed to
	// the cache or the extracted store, so the run would report success while
	// warming nothing. This is rejected before any backend lock is taken or
	// any network request is made.
	ErrWarmCacheDisabled = errors.New("warm requires a cache: --no-cache leaves nothing to warm")

	// ErrDbNil indicates a nil Bolt DB was provided.
	ErrDbNil = errors.New("bolt DB is nil")
	// ErrStoreNil indicates a nil store was provided.
	ErrStoreNil = errors.New("store is nil")
	// ErrUnsupportedSchemaVersion indicates the snapshot schema version is unsupported.
	ErrUnsupportedSchemaVersion = errors.New("unsupported snapshot schema version")
	// ErrOutdatedSchemaVersion indicates the snapshot schema version is older
	// than the current one and the snapshot should be dropped and rebuilt.
	ErrOutdatedSchemaVersion = errors.New("outdated snapshot schema version")
	// ErrCorruptProjectRegistry indicates the project registry file or
	// object could not be decoded. It must never be treated as an empty
	// registry: cleanup computes reachability from every recorded project,
	// so silently substituting an empty registry would make it believe
	// nothing is reachable and delete every installed collection.
	ErrCorruptProjectRegistry = errors.New("corrupt project registry")
	// ErrCorruptSnapshotStore indicates the local Bolt snapshot file failed
	// one of bbolt's own integrity checks on open (see openBolt in
	// internal/galaxy/store for the closed set): this binary cannot read
	// it, no retry changes that, and the remedy is discarding the file and
	// letting the cache rebuild cold.
	//
	// bolterrors.ErrInvalid, the broadest member of that set, does not mean
	// only "a meta page was read and then failed validation". Verified in
	// the vendored bbolt: db.getPageSize (by way of
	// getPageSizeFromFirstMeta/getPageSizeFromSecondMeta) returns this same
	// sentinel when NEITHER meta page could be read at all - a failing-media
	// ReadAt, or a file truncated below where the meta pages live - folding
	// that case into the identical "damaged bytes" label as a meta page that
	// was read successfully but failed its own checksum or version check.
	//
	// This is accepted rather than split into a fourth class: the remedy
	// this sentinel advises (discard the file, let the cache rebuild cold)
	// is correct for every one of these shapes, since a Bolt snapshot is a
	// reconstructible cache and never holds data this program cannot
	// regenerate - and the shape that actually occurs in practice, a
	// truncated or otherwise short file, genuinely is corruption in the
	// ordinary sense of the word.
	//
	// What this still does not reach, and must not be read as reaching: a
	// permissions failure. bolt.Open fails at its own os.OpenFile call
	// before the page-size probe above ever runs, so a permission-denied
	// path never reaches ErrInvalid, ErrVersionMismatch, or ErrChecksum at
	// all - it stays unclassified, exactly as openBolt's own doc comment
	// states for the closed set this sentinel maps from.
	//
	// One member of that set carries a residual worth naming rather than
	// glossing over: bbolt's ErrVersionMismatch names its own vendored
	// on-disk FORMAT version, not go-galaxy's snapshot schema
	// (helpers.StoreSnapshotSchemaVersion) - a binary built against a
	// different bbolt release genuinely could read a file that trips it
	// here. This program only ever writes with the one bbolt version it
	// vendors, so today a mismatch still means a foreign or damaged file
	// relative to what this binary itself could have written - but that is
	// a fact about this codebase's dependency, not a property this
	// sentinel's own name guarantees forever.
	//
	// This is distinct from ErrUnsupportedSchemaVersion, which is about a
	// different version entirely - go-galaxy's own snapshot schema, not
	// bbolt's on-disk format - and describes a snapshot a newer go-galaxy
	// binary wrote correctly and this one cannot yet read: there the bytes
	// are fine and the fix is an environment change (a newer binary, or
	// pointing at a different cache), never discarding data another
	// runner still depends on. ErrCorruptSnapshotStore carries none of the
	// three cache-backend classes (ErrCacheBackendUnavailable,
	// ErrCacheBackendUnusable, ErrCacheBusy): those describe a backend's
	// ability to reach or serve its store, not whether the bytes it did
	// reach are usable once read.
	ErrCorruptSnapshotStore = errors.New("corrupt snapshot store")
	// ErrStateObjectTooLarge indicates a persisted cache-state object (the S3
	// snapshot or project registry) could not be read within its declared
	// size ceiling - StateObjectMaxCompressedSize on the wire,
	// StateObjectMaxDecompressedSize once inflated - so a planted oversized
	// or high-ratio gzip object is never buffered whole into memory. Like
	// ErrCorruptProjectRegistry, the object this names cannot be trusted by
	// anyone and must be discarded (or replaced by clearing the cache)
	// before a run can proceed; it is never retried, since the same bytes
	// would overrun the same ceiling again.
	ErrStateObjectTooLarge = errors.New("cache state object exceeds the maximum allowed size")
	// ErrCorruptStateObject indicates a persisted cache-state object (the S3
	// snapshot or project registry) could not be read back as the gzipped or
	// plain JSON this program writes there. Like ErrStateObjectTooLarge, which
	// it sits beside for the same reason, the object it names cannot be used
	// by anyone and must be discarded - or replaced by clearing the cache -
	// before a run can proceed, and it is never retried, since the same bytes
	// decode the same way every time.
	//
	// It is the S3 backend's counterpart to ErrCorruptSnapshotStore rather
	// than a second spelling of it: that one names a local Bolt file failing
	// bbolt's own checks on open and originates nowhere else, while this one
	// names the object an S3 read got back. The remedy is the same, which is
	// why both classify alike; the producer and the evidence are not, which is
	// why they are two sentinels.
	//
	// It is deliberately broader than the one shape that raises it today, a
	// gzip member producing no bytes: what it asserts is that the object is
	// not something this program can read back, which is a property of the
	// bytes rather than of which check noticed.
	ErrCorruptStateObject = errors.New("corrupt cache state object")

	// ErrOfflineMode indicates a network operation was attempted in offline mode.
	ErrOfflineMode = errors.New("offline mode is enabled, network access is forbidden")
	// ErrReadStalled indicates a response body read made no progress within
	// the configured timeout window while the request's own context was
	// still live. A parent-context cancellation is reported as
	// context.Canceled instead, never as ErrReadStalled.
	//
	// It deliberately does NOT wrap its cause with %w, and this is a rule, not
	// a note about one call site: this sentinel names a condition a remote peer
	// drove rather than a caller's own cancellation, so it must not leave
	// context.Canceled reachable through errors.Is - see cmd/go-galaxy/exitcode's
	// isCanceled for the argument and for the exception it states. The watchdog
	// aborts a stall by canceling its own derived context to unblock the stuck
	// read, so the cause is context.Canceled; left wrapped with %w it would steal
	// this failure's exit-code classification, because exitcode.FromError checks
	// context.Canceled ahead of every other class and would report a hostile or
	// degraded server as a caught Ctrl-C. The cause is rendered with %v instead,
	// so it stays diagnosable without being matchable.
	ErrReadStalled = errors.New("network read stalled")
	// ErrArtifactDownloadDeadline indicates one artifact's acquisition exceeded
	// ArtifactDownloadDeadline: the whole-transfer ceiling the read-inactivity
	// watchdog cannot enforce, since a byte-drip makes real progress in every
	// idle window and so never trips it. It is deliberately distinct from
	// ErrReadStalled (no progress at all within one idle window) and from a
	// caller cancellation, which still surfaces as context.Canceled.
	//
	// It deliberately does NOT wrap its cause with %w. The cause is
	// context.DeadlineExceeded, or - when the watchdog's own derived-context
	// cancel raced the deadline - context.Canceled, and it is the second of
	// those a %w would let steal this failure's exit-code classification:
	// exitcode.FromError checks context.Canceled ahead of every other class and
	// would report a hostile server as ExitInterrupt, i.e. as a Ctrl-C. The
	// first would cost nothing, since isTransportError matches it to the class
	// this sentinel already carries - but one rendering rule for one cause
	// beats a rule that has to ask which cause it got. The cause is rendered
	// into the message with %v instead, so it stays diagnosable without being
	// matchable.
	//
	// It is never retried: the budget is spent, so every remaining attempt
	// would fail instantly against the same dead context.
	ErrArtifactDownloadDeadline = errors.New("artifact download deadline exceeded")
	// ErrMetadataFetchDeadline indicates one Galaxy metadata request exceeded
	// MetadataFetchDeadline: the whole-request ceiling that catches a
	// byte-drip response, which the read-inactivity watchdog cannot enforce
	// since it always makes real progress within every idle window.
	//
	// It deliberately does NOT wrap its cause with %w, the rule every
	// deadline sentinel here follows: this is a budget this program imposed
	// rather than a caller's own cancellation, so leaving context.Canceled
	// reachable through errors.Is would have exitcode.FromError report a
	// hostile or degraded Galaxy server as a caught Ctrl-C. The cause is
	// rendered into the message with %v instead, so it stays diagnosable
	// without being matchable. The rule is over context.Canceled alone;
	// cmd/go-galaxy/exitcode's own isCanceled holds the argument.
	//
	// It is never retried: the budget is spent, so every remaining attempt
	// would fail instantly against the same dead context.
	//
	// It is also never raised for an error that does not itself carry a
	// context signal (a %v-rendered context.DeadlineExceeded or
	// context.Canceled): a *cacheManager.HTTPStatusError - a 404, a 401/403,
	// or a retryable status - keeps its own identity even when this
	// request's budget expires in the same instant, so the root-metadata
	// server walk keeps routing on status instead of misreading a race
	// between "the response arrived" and "the budget expired" as a deadline.
	ErrMetadataFetchDeadline = errors.New("galaxy metadata fetch deadline exceeded")
	// ErrStateObjectDeadline indicates one persisted cache-state operation
	// (LoadStore, SaveStore, LoadProjectRegistry, or RecordProject) exceeded
	// StateObjectDeadline. Unlike ErrMetadataFetchDeadline, the operator
	// remediation this names is not "your Galaxy server is dripping" but
	// "your object store is dripping, and while it was, it held the
	// distributed lock and blocked every runner sharing this bucket" - hence
	// a separate sentinel rather than reusing ErrMetadataFetchDeadline for
	// both surfaces.
	//
	// It deliberately does NOT wrap its cause with %w, the same rule every
	// deadline sentinel here follows: this is a budget this program imposed
	// rather than a caller's own cancellation, so leaving context.Canceled
	// reachable through errors.Is would have exitcode.FromError report a
	// hostile or degraded object store as a caught Ctrl-C. The cause is
	// rendered into the message with %v instead, so it stays diagnosable
	// without being matchable. The rule is over context.Canceled alone;
	// cmd/go-galaxy/exitcode's own isCanceled holds the argument.
	//
	// It is never retried: the budget is spent, so every remaining attempt
	// would fail instantly against the same dead context.
	ErrStateObjectDeadline = errors.New("cache state object deadline exceeded")
	// ErrLockfileMismatch indicates the lockfile content does not match the resolution.
	ErrLockfileMismatch = errors.New("lockfile does not match resolved requirements")
	// ErrLockfileMissing indicates a lockfile a command required was not
	// found. The text names no flag: the same absence reaches this sentinel
	// from --frozen's three commands and from tree, explain and outdated,
	// which require the lockfile for a reason of their own, and a message
	// naming --frozen would be false for four of the six.
	ErrLockfileMissing = errors.New("lockfile not found")
	// ErrLockfileInvalid indicates the lockfile is malformed or unsupported.
	ErrLockfileInvalid = errors.New("lockfile is invalid")
	// ErrLockfileDrift indicates lock --frozen compared a fresh resolve
	// against the lockfile already on disk and found a difference: the file
	// that exists is not the file a real `lock` run would write right now.
	// This is distinct from ErrLockfileMismatch (a lockfile that does not
	// cover the requirements roots at all) the same way a malformed digest
	// is kept distinct from a mismatched one - both pairs describe a
	// different failure shape under the same exit class, not the same
	// failure under two names.
	ErrLockfileDrift = errors.New("lockfile is out of date")

	// ErrInvalidTimeout indicates the --timeout value is neither a positive
	// integer number of seconds nor a valid positive Go duration string.
	ErrInvalidTimeout = errors.New("invalid timeout")

	// ErrUnexpectedArguments indicates positional arguments a command does not
	// take. With install as the root's default command, a first word that
	// names no command arrives here too, as an argument to install - which is
	// how "go-galaxy collection install ns.name" is refused rather than
	// installing the requirements file with no word about ns.name.
	ErrUnexpectedArguments = errors.New("unexpected arguments")

	// ErrMissingArgument indicates a command that takes a positional argument
	// was run without it: explain with no collection or role to explain.
	ErrMissingArgument = errors.New("missing argument")

	// ErrAnsibleConfigNotFound indicates an explicitly requested ansible.cfg
	// path does not exist.
	ErrAnsibleConfigNotFound = errors.New("ansible config file not found")

	// ErrUnsafeCollectionIdentifier indicates a manifest field (namespace,
	// name, or version) cannot be safely used as a single filesystem path
	// element, e.g. it contains a path separator or is "..".
	ErrUnsafeCollectionIdentifier = errors.New("unsafe collection identifier")
	// ErrInvalidCollectionVersion indicates a resolved collection's version
	// is not IsExactVersion - a constraint string like "*" or ">=1.0.0", or
	// any other value that does not name one release - reaching a point in
	// the pipeline that requires an already-resolved, installable version.
	// This is deliberately distinct from ErrUnsafeCollectionIdentifier: an
	// unsafe identifier describes a path-traversal shape, while this
	// sentinel describes a version that is well-formed as a path element but
	// still not a version anything could install - the same distinction
	// ErrMalformedArtifactSHA256 draws against a syntactically fine but
	// wrong-shaped digest.
	ErrInvalidCollectionVersion = errors.New("collection version is not an exact version")
	// ErrUnsafeRemovalPath indicates a computed removal path failed a
	// containment check against its expected root directory.
	ErrUnsafeRemovalPath = errors.New("unsafe removal path")
	// ErrProjectRequirementsUnreadable indicates a recorded project's
	// requirements file failed to load for any reason other than a plain
	// fs.ErrNotExist - as opposed to a genuinely absent file, which is a
	// stale registry entry cleanup tolerates instead (a warning,
	// contributing no reachability roots). Cleanup must abort rather than
	// silently treat this as contributing zero reachability roots, since the
	// roots that file would have contributed cannot be determined and could
	// have been protecting any project's on-disk copies, not only this
	// one's.
	ErrProjectRequirementsUnreadable = errors.New("project requirements file is unreadable")
	// ErrCorruptManifest indicates a MANIFEST.json file exists but could
	// not be parsed as JSON. The install it describes cannot be
	// identified, so it is reported rather than silently discarded, but is
	// still treated as neither a reachability source nor a deletion
	// candidate.
	ErrCorruptManifest = errors.New("corrupt manifest")

	// ErrResponseTooLarge indicates a response body read through
	// NewSizeLimitedReader exceeded its configured ceiling before it finished
	// streaming - an artifact download over ArtifactMaxDownloadSize, a Galaxy
	// metadata document over MetadataMaxSize, or an S3 list or batch-delete
	// response over S3ListMaxSize. It is never retried: a server that streams
	// past the ceiling once will do so again, so retrying would only spend
	// the retry budget re-fetching a hostile or broken response.
	//
	// A persisted cache-state object is capped by the same reader but never
	// surfaces as this sentinel: s3.readObject reclassifies it into
	// ErrStateObjectTooLarge before it reaches a caller, since an unreadable
	// state object is a corrupt-cache condition rather than a fetch that
	// failed.
	ErrResponseTooLarge = errors.New("response body exceeds the maximum allowed size")

	// ErrUnsupportedGalaxyServerKey indicates a [galaxy_server.<id>] section
	// used a key this tool deliberately refuses to interpret: username,
	// password, auth_url, or client_id imply Basic auth or a Keycloak/SSO
	// token exchange, neither of which this tool implements. This fails
	// config loading outright, before any request is made, rather than
	// silently sending an unauthenticated request and getting a confusing
	// 401 later.
	ErrUnsupportedGalaxyServerKey = errors.New("unsupported galaxy_server key: configure a Galaxy API token instead")
	// ErrUnsupportedGalaxyServerAPIVersion indicates a [galaxy_server.<id>]
	// section set api_version to something other than the one value this
	// tool understands ("v3").
	ErrUnsupportedGalaxyServerAPIVersion = errors.New("unsupported galaxy_server api_version")
	// ErrMissingGalaxyServerURL indicates a server named in server_list has
	// no url configured, from either its [galaxy_server.<id>] section or
	// the matching ANSIBLE_GALAXY_SERVER_<ID>_URL env var.
	ErrMissingGalaxyServerURL = errors.New("galaxy server is missing its url")
	// ErrInvalidGalaxyServerURL indicates a configured Galaxy server URL
	// could not be parsed as an absolute URL with a scheme and host. The
	// raw value is deliberately never included in this error's context: it
	// may carry embedded userinfo, and echoing it back would defeat the
	// point of ErrGalaxyServerURLUserinfo below.
	ErrInvalidGalaxyServerURL = errors.New("invalid galaxy server url")
	// ErrInvalidGalaxyServerID indicates a server_list id contains a
	// character outside [A-Za-z0-9_-]. Ansible does not enforce this, but
	// this tool does: a "." would make the "[galaxy_server.<id>]" section
	// grammar ambiguous, and every other character is unsafe to fold into
	// an ANSIBLE_GALAXY_SERVER_<ID>_* environment variable name.
	ErrInvalidGalaxyServerID = errors.New("invalid galaxy server id")
	// ErrDuplicateGalaxyServerID indicates server_list repeats an id, or
	// lists two ids that differ only in case. The latter is rejected
	// because both would resolve to the same ANSIBLE_GALAXY_SERVER_<ID>_*
	// environment variable prefix, making per-server env overrides
	// ambiguous.
	ErrDuplicateGalaxyServerID = errors.New("duplicate galaxy server id")
	// ErrInvalidValidateCerts indicates a validate_certs value is not one
	// of ansible's recognized boolean spellings (true/false, yes/no,
	// on/off, 1/0, case-insensitive). This is always a hard error: an
	// unparseable value must never silently resolve to "false" (certs
	// unverified).
	ErrInvalidValidateCerts = errors.New("invalid validate_certs value")
	// ErrGalaxyServerURLUserinfo indicates a configured Galaxy server URL, or
	// a requirements.yml collection's "source:", embeds userinfo (e.g.
	// "https://user:pass@hub/"). url.URL.String() renders the password back
	// out in plain text, so such a URL would otherwise leak into debug
	// output, the lockfile, GALAXY.yml, and the persisted snapshot; rejecting
	// it at config/requirements load closes that off structurally instead of
	// relying on every downstream consumer to remember to redact it.
	ErrGalaxyServerURLUserinfo = errors.New("galaxy server url must not contain userinfo")
	// ErrInsecureTokenTransport indicates a token is configured for a
	// plaintext (http) origin that is not loopback (localhost,
	// 127.0.0.0/8, or ::1). Sending a token over such a connection lets any
	// on-path observer capture it, so this is rejected at config load
	// rather than warned about at request time.
	ErrInsecureTokenTransport = errors.New("galaxy server token configured for an insecure plaintext transport")
	// ErrConflictingServerTLSPolicy indicates two configured servers share
	// a normalized origin (scheme, host, and effective port) but disagree
	// on validate_certs. One origin must map to exactly one transport, so
	// this conflict is a config error rather than an arbitrary
	// last-one-wins resolution.
	ErrConflictingServerTLSPolicy = errors.New("conflicting validate_certs for the same galaxy server origin")
	// ErrConflictingServerToken indicates two configured servers share a
	// normalized origin but carry different tokens (including one set and
	// one unset). Like ErrConflictingServerTLSPolicy, this keeps "one
	// origin, one transport" a structural invariant instead of a silent
	// pick between two credentials for the same endpoint.
	ErrConflictingServerToken = errors.New("conflicting token for the same galaxy server origin")
	// ErrAmbiguousGalaxyToken indicates --token (or GO_GALAXY_TOKEN) was set
	// while a multi-entry server_list is in effect. The flag names no server,
	// so there is no answer to which one the credential belongs to, and
	// guessing could send a private hub's token to the public Galaxy.
	// Configure the token in that server's own [galaxy_server.<id>] section
	// or its ANSIBLE_GALAXY_SERVER_<ID>_TOKEN variable instead.
	ErrAmbiguousGalaxyToken = errors.New("--token is ambiguous with a multi-entry server_list")
	// ErrTokenDestinationFromAnsibleConfig indicates a Galaxy token is
	// paired with a server URL this run sourced from an ansible.cfg file
	// rather than from the operator, while the token itself did not come
	// from that same file's own [galaxy_server.<id>] section. This is a
	// hard refusal rather than a warning or a silent drop: a warning is
	// easy to miss in CI output, and a silent drop would still perform an
	// unauthenticated request against an address the operator never chose -
	// both leave the credential's destination up to whatever ansible.cfg a
	// checked-out repository happens to carry. config.checkTokenPairing
	// is the single gate that produces this and holds the full argument for
	// the pairing rule it enforces. It sits beside ErrAmbiguousGalaxyToken
	// as the answer to a different question, not as a narrower form of the
	// same one: that sentinel asks which server a --token/GO_GALAXY_TOKEN
	// credential is even for, and fires only where no single server is
	// addressable; this one asks who chose the address a credential is
	// already bound to, and fires for a per-server
	// ANSIBLE_GALAXY_SERVER_<ID>_TOKEN inside a multi-entry server_list too -
	// a shape the other sentinel cannot reach at all, since such a token
	// names its own server and nothing about it is ambiguous.
	ErrTokenDestinationFromAnsibleConfig = errors.New("galaxy server token destination came from ansible.cfg")
	// ErrTokenTLSPolicyFromAnsibleConfig indicates a Galaxy token is paired
	// with a server whose certificate verification an ansible.cfg file
	// disabled for it, while the token itself did not come from that same
	// file. It is the second value of the one pairing rule
	// config.checkTokenPairing enforces and holds the argument for -
	// ErrTokenDestinationFromAnsibleConfig covers where the credential is
	// sent, this covers whether that address's certificate is checked. The
	// two stay distinct sentinels because they name distinct remedies:
	// ANSIBLE_GALAXY_SERVER_<ID>_URL for the first,
	// ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS for the second.
	ErrTokenTLSPolicyFromAnsibleConfig = errors.New(
		"galaxy server certificate verification was disabled by ansible.cfg for a token it did not supply")

	// ErrGalaxyAuthFailed indicates a configured Galaxy server answered a
	// root-metadata request with 401 or 403. This is fail-closed: unlike a
	// 404 (which only means this server does not have the collection and
	// advances the server-list walk to the next candidate), a credential
	// failure aborts the whole run rather than silently falling through to
	// another server that might answer anonymously.
	ErrGalaxyAuthFailed = errors.New("galaxy server authentication failed")
	// ErrGalaxyServerUnavailable indicates a configured Galaxy server kept
	// answering a root-metadata request with a retryable status (429, 500,
	// 502, 503, or 504) until the retry budget was spent. Like
	// ErrGalaxyAuthFailed, this aborts the run instead of advancing to the
	// next server in the list: a transient outage on one server is not
	// evidence the collection is absent there, so silently falling through
	// would risk installing from the wrong server once the outage clears.
	ErrGalaxyServerUnavailable = errors.New("galaxy server unavailable")

	// ErrCollectionsPathEscape indicates an install-side write refused to
	// follow a path component under cfg.DownloadPath (most dangerously
	// ansible_collections itself, or a namespace/name component beneath it)
	// because it resolved to a symlink, or because a filesystem error other
	// than "does not exist" was found while diagnosing an os.Root refusal.
	// os.Root itself is what actually blocks the escape, atomically, inside
	// the kernel; this sentinel only names which component classifyCollectionsRootError
	// found responsible, for an operator-facing message - it is never the
	// mechanism that prevents the write.
	ErrCollectionsPathEscape = errors.New("path escapes the collections directory")

	// ErrMalformedArtifactSHA256 indicates a value that was supposed to be an
	// artifact's sha256 digest is not a 64-character lowercase hex string.
	// This is deliberately distinct from ErrSHA256Mismatch: a mismatch means
	// two valid digests disagree, while a malformed digest is not a digest
	// at all - most often a poisoned snapshot or a lying server.
	//
	// This exclusion is specific to one arm of prepareWithRecovery, not a
	// global claim about retrying: that function's prepareInstall-error arm
	// classifies on ErrSHA256Mismatch and this sentinel is deliberately not
	// in that class, since retrying cannot repair the metadata cache entry
	// that produced a malformed value in the first place. Its separate
	// action-error arm evicts on every artifact-side failure, unclassified
	// among those, per the tradeoff documented on prepareWithRecovery itself
	// - and this sentinel sits outside that class for the identical reason:
	// eviction cannot repair a value that was never a valid digest to begin
	// with. That action arm also carries one further, destination-side
	// exclusion of its own (isDestinationSideFailure) that has nothing to do
	// with this sentinel; see prepareWithRecovery's doc comment for that one.
	ErrMalformedArtifactSHA256 = errors.New("artifact sha256 is not a 64-character lowercase hex digest")

	// ErrSignatureVerificationFailed is one collection's aggregate signature
	// verdict: the signatures actually checked did not satisfy the policy in
	// force - fewer valid ones than the required count, at least one outright
	// failure under an "all" policy, or none valid under a "+N" policy. It
	// names the verdict for a collection, never one signature's own outcome, so
	// a collection carrying a bad signature alongside enough good ones does not
	// reach it.
	//
	// It is deliberately NOT ErrSHA256Mismatch and must never be folded into
	// it: a digest answers whether these are the bytes that were named, a
	// signature answers who published them. Bytes that hash exactly as expected
	// and carry no acceptable signature are intact and unattributed, which is a
	// different question with a different remedy - most often this run's own
	// keyring or signature policy rather than the artifact or its server.
	ErrSignatureVerificationFailed = errors.New("collection signature verification failed")
	// ErrSignatureAttributionMismatch indicates a signature verified, and the
	// document it vouches for names a different collection than the one being
	// installed: MANIFEST.json's collection_info declares a namespace, name or
	// version that is not the resolved collection's own.
	//
	// It is a signature verdict rather than a defect in the material, which is
	// what keeps it out of all three neighboring sentinels. NOT
	// ErrSignatureVerificationFailed: every signature checked verified perfectly,
	// and the policy in force was satisfied - what failed is who the signature
	// was made about. NOT ErrManifestChainMismatch: the chain from MANIFEST.json
	// to FILES.json to every file is intact, and it is intact for a real,
	// coherent collection - just not this one. NOT ErrSHA256Mismatch: the bytes
	// are exactly the bytes that were named, hashing as promised.
	//
	// Without it, a key this run trusts vouching for acme.other@0.0.1 installs
	// those bytes as acme.app@1.0.0 - a signed downgrade to a known-vulnerable
	// version, or a signed substitution of one collection for another, reaching
	// exit 0. The comparison is byte-for-byte on all three components and is
	// deliberately not normalized: the resolved version already satisfies
	// IsExactVersion, and a normalizing comparison is the one an attacker aims
	// at.
	ErrSignatureAttributionMismatch = errors.New("collection signature vouches for a different collection")
	// ErrTooManySignatureSources indicates one collection's requirements entry
	// declares more signature sources than MaxSignaturesPerCollection allows.
	//
	// It is refused where the file is read rather than left for the gather to
	// discover the cap later, because a declared list over the cap is the
	// operator's own mistake, and this is the one place it is still a
	// load-time usage error rather than something an install worker would
	// otherwise discover mid-run and only degrade into a warning. The message
	// itself names neither the file nor the offending collection, only the
	// count - "65 declared, at most 64 are gathered" for a requirements entry
	// naming one source too many, nothing more. A combined candidate set that
	// exceeds the cap for a reason this entry alone did not cause - a server
	// also offering signatures on top of an already-large declared list - is
	// a different case with a different remedy, and gatherLimit
	// (internal/galaxy/collections/verify.go) is where that one is decided
	// and reported instead.
	ErrTooManySignatureSources = errors.New("too many signature sources declared for one collection")
	// ErrSignatureSourceUnavailable indicates a signature this run was told to
	// check could not be obtained at all: a network failure fetching it, an
	// offline-mode refusal, a file:// source that could not be read, or version
	// metadata that would have carried a server's own signatures being
	// unreachable.
	//
	// It is deliberately NOT ErrSignatureVerificationFailed: nothing was
	// verified and nothing failed verification, so the two must stay
	// distinguishable - that one says a publisher could not be authenticated,
	// this one says the material to try was never in hand.
	ErrSignatureSourceUnavailable = errors.New("collection signature source unavailable")
	// ErrSignatureFetchDeadline indicates one collection's signature fetching
	// exceeded SignatureFetchDeadline: the whole-phase ceiling that catches a
	// byte-drip response, which the read-inactivity watchdog cannot enforce
	// since such a response makes genuine progress inside every idle window.
	//
	// A producer must NOT wrap its cause with %w, the rule every deadline
	// sentinel here follows: this is a budget this program imposed rather than
	// a caller's own cancellation, so leaving context.Canceled reachable
	// through errors.Is would have exitcode.FromError report a hostile or
	// degraded signature host as a caught Ctrl-C. Render the cause with %v
	// instead, so it stays diagnosable without being matchable. The rule is
	// over context.Canceled alone; cmd/go-galaxy/exitcode's own isCanceled
	// holds the argument, including why ErrSignatureSourceUnavailable above -
	// which passes a caller's cancellation through unchanged - sits outside it.
	//
	// It is never retried: the budget is spent, so every remaining attempt
	// would fail instantly against the same dead context.
	//
	// It is deliberately NOT ErrSignatureSourceUnavailable, even though a spent
	// budget also leaves the material out of hand: this names a link or a host
	// too slow to finish inside the ceiling, not one that refused or vanished,
	// and only one of the two is worth reporting as an endpoint that answered.
	ErrSignatureFetchDeadline = errors.New("collection signature fetch deadline exceeded")
	// ErrUnsupportedSignatureSource indicates a signature source does not name
	// something this tool fetches, and it is the whole class of ways a value can
	// fail to: one url.Parse itself refuses, one with no scheme at all, one
	// with a scheme outside file, http and https, an opaque URL -
	// "http:host/sig.asc", which names no authority and no absolute path and
	// which no request can be composed from - an http or https URL naming no
	// authority at all ("https:///sig.asc", "https://"), which no request can be
	// composed from either, a file URL whose authority names some other host,
	// and a file URL carrying a relative path rather than an absolute one. Like
	// ErrUnsupportedDownloadURLScheme, the value is judged against an allow-list
	// rather than a blocklist: a blocklist would have to name every scheme worth
	// refusing and would admit whatever it forgot.
	//
	// It is deliberately NOT ErrSignatureSourceUnavailable: no fetch was
	// attempted and none failed - the value never named something this tool
	// fetches - so the remedy is editing the source, never retrying it.
	ErrUnsupportedSignatureSource = errors.New("signature source is not a fetchable file, http, or https URL")
	// ErrSignatureSourceUserinfo indicates a signature source URL embeds
	// userinfo, e.g. "https://user:pass@hub/sig.asc".
	//
	// The value decides what authenticates the request as well as where it
	// goes: net/http sets Basic auth from a URL's userinfo before any transport
	// runs. A signature source is repository content, so without this refusal a
	// credential a repository chose would ride on a request an operator's run
	// makes - and url.URL.String() renders that password back out in plain text
	// at every sink the value reaches, which is why the refusal must also keep
	// it out of its own message.
	//
	// It is deliberately NOT ErrGalaxyServerURLUserinfo, which is scoped to a
	// value that SELECTS a server - a configured server URL, or a collection's
	// source: - and is raised while building the config rather than while
	// fetching. And it is deliberately NOT ErrUnsupportedSignatureSource: that
	// one says the value names nothing this tool fetches, while this one names
	// something perfectly fetchable and carries a credential while doing it, so
	// the remedy is deleting the credential rather than rewriting the source.
	ErrSignatureSourceUserinfo = errors.New("signature source url must not contain userinfo")
	// ErrKeyringUnreadable indicates the configured keyring could not be read
	// as one: it is absent, it cannot be opened, or its bytes do not parse as
	// the OpenPGP key material this tool reads.
	//
	// It is deliberately NOT a verification verdict: nothing was checked
	// against this keyring, so a run that hits it never learns whether an
	// artifact would have verified. ErrKeyringIsKeybox is the one unreadable
	// shape kept out of it, because that shape has a specific remedy to name.
	ErrKeyringUnreadable = errors.New("keyring could not be read")
	// ErrKeyringIsKeybox indicates the configured keyring is a GnuPG keybox -
	// the .kbx container a default GnuPG installation writes - which this tool
	// cannot read: it verifies in pure Go against OpenPGP key material and
	// keeps no gpg process to delegate a container format to. The message
	// carries the export command rather than leaving an operator to find it,
	// since being told only that the file is unreadable is a dead end for the
	// file GnuPG itself produced by default.
	//
	// It is deliberately NOT folded into ErrKeyringUnreadable: that one says
	// the bytes are not key material this tool understands, this one says they
	// are key material in a container it does not open, and only the second has
	// a one-command remedy to state.
	ErrKeyringIsKeybox = errors.New(
		"keyring is a GnuPG keybox (.kbx), which this tool cannot read; export an armored keyring instead: " +
			"gpg --no-default-keyring --keyring <kbx> --export --armor > keyring.asc")
	// ErrKeyringRequired indicates a requirements file declares signatures: for
	// a collection while no keyring is configured, so nothing exists to verify
	// those signatures against. This is a hard error rather than a
	// warn-and-continue, which is parity with ansible-galaxy: a requirements
	// file that asks for verification is refused rather than silently installed
	// unverified.
	//
	// It is deliberately NOT ErrKeyringUnreadable: no keyring was named at all,
	// so the remedy is to configure one (or to drop the signatures: block),
	// never to repair a file this run tried to read.
	ErrKeyringRequired = errors.New("requirements declare signatures but no keyring is configured")
	// ErrInvalidSignatureCount indicates the required-valid-signature-count
	// value is not a form this tool accepts: it names neither "all" nor a
	// positive count, in either the bare or the "+N" spelling.
	//
	// It is deliberately NOT ErrSignatureVerificationFailed: the policy itself
	// could not be read, so no count was ever compared against anything, and
	// the remedy is editing the value rather than looking at an artifact.
	ErrInvalidSignatureCount = errors.New("invalid required valid signature count")
	// ErrUnknownSignatureStatusCode indicates an ignore-signature-status-code
	// value naming a status code outside the set this tool recognizes in a
	// verification result.
	//
	// It is refused rather than ignored, which is the whole point of the
	// sentinel: a status code nothing recognizes would ignore nothing, so a
	// typo in the value would read as "this failure is being tolerated" while
	// the run kept failing on exactly that failure.
	ErrUnknownSignatureStatusCode = errors.New("unknown signature status code")
	// ErrInvalidDisableGPGVerify indicates an ANSIBLE_GALAXY_DISABLE_GPG_VERIFY
	// value that is not one of ansible's recognized boolean spellings
	// (true/false, yes/no, on/off, 1/0, case-insensitive). The variable is read
	// by the config layer rather than by the flag itself, because that
	// vocabulary is wider than the one Go's own bool parser accepts.
	//
	// It is a hard error rather than a warned-and-defaulted false, for the same
	// reason ErrInvalidValidateCerts is: a security-relevant boolean must never
	// be guessed, and a value ansible itself refuses must not silently work
	// here. Defaulting it either way is worse than refusing it - false would
	// leave an operator who meant to switch verification off believing they
	// had, and true would switch it off for a typo.
	//
	// It is a configuration failure, never a verdict: nothing was verified when
	// this fires, so it classifies with isSignatureConfigError rather than with
	// the verification sentinels above.
	ErrInvalidDisableGPGVerify = errors.New("invalid disable_gpg_verify value")
	// ErrEmptySignatureValue indicates a source explicitly supplied an empty
	// value for the keyring path or the required-valid-signature-count, both of
	// which name something rather than switch something.
	//
	// Omitting a setting is how an operator asks for its default; supplying it
	// empty is an expression that failed. The shape this exists for is a CI
	// block writing a secret into an environment variable, which evaluates to
	// nothing precisely on a fork's pull request, since that is the run secrets
	// are withheld from - and the run most in need of verification. Left
	// unrefused, the empty keyring reads as "verify nothing" and the empty
	// count silently replaces a configured "+all" with the default of 1.
	//
	// It is a refusal rather than a warning for the same reason
	// ErrInsecureTokenTransport is one: a security-relevant value is never
	// guessed, and a warning in a CI log is not a control. It covers those two
	// settings only - an empty disable-gpg-verify reads false and an empty
	// ignore list tolerates nothing, each a well-defined meaning in the safe
	// direction, so neither is a failed expression this can recognize.
	ErrEmptySignatureValue = errors.New("signature setting was supplied empty")
	// ErrManifestNotFound indicates a collection artifact names no
	// MANIFEST.json within ManifestScanMaxBytes of the start of its tar stream.
	// The bound is what makes this a verdict rather than an abandoned search: a
	// real collection carries its manifest at the front of the archive, so an
	// archive that has not named one by then is refused instead of read to its
	// end.
	//
	// It is deliberately NOT ErrCorruptManifest: that one names a manifest that
	// exists and does not parse, which cleanup tolerates as neither a
	// reachability root nor a deletion candidate, while this names an artifact
	// with no manifest to parse at all.
	ErrManifestNotFound = errors.New("collection artifact contains no MANIFEST.json")
	// ErrManifestChainMismatch indicates the chain from MANIFEST.json down to
	// an artifact's files broke: FILES.json did not match the digest
	// MANIFEST.json names for it, a listed file did not match the digest
	// FILES.json names for it, or the archive carried a file FILES.json does
	// not list at all.
	//
	// This one IS an integrity failure in the same sense as ErrSHA256Mismatch,
	// and classifies with it rather than with the signature sentinels above:
	// what failed is bytes against a digest. A signature is made over
	// MANIFEST.json alone, so it is worth exactly what that chain is worth - a
	// broken chain means the signed digests no longer describe the content, and
	// no keyring or policy change repairs that.
	ErrManifestChainMismatch = errors.New("collection manifest chain does not match")

	// ErrLatestVersionLookupFailed is the headline for an `outdated` run in
	// which at least one lockfile entry's latest-version lookup failed. Named
	// for the lookup rather than for the command so it does not read as a
	// sibling of ErrOutdatedSchemaVersion above, which uses "outdated" in the
	// unrelated sense of a stale snapshot schema.
	ErrLatestVersionLookupFailed = errors.New("latest version lookup failed")

	// The git-source sentinels below are grouped by the exit class each one
	// lands in (cmd/go-galaxy/exitcode), because the class is the contract a
	// caller relies on when it picks one: a configuration refusal must stay a
	// usage error even when it is discovered only after a network round trip,
	// and a remote that does not hold a ref must classify with the solver's
	// own "no such version" answers rather than with a transport failure.

	// ErrInvalidGitURL names a repository URL this tool will not use: an
	// unsupported scheme (file, git, anything but https, http, ssh and the
	// scp-like user@host:path form), an empty host or path, a query, a
	// fragment left after the #subdir split, a quote or control rune in the
	// path (go-git hands the path to the remote as `git-upload-pack '<path>'`
	// without escaping), or a leading "-" in the first path segment. Usage
	// class: the value came from requirements.yml or a credential binding.
	ErrInvalidGitURL = errors.New("invalid git url")
	// ErrGitURLUserinfo names a repository URL that carries a credential:
	// any userinfo on http(s), or a password on ssh. The URL is repository
	// content, so the credential would be too; a git credential comes from
	// the environment, bound to a host, and never from the URL.
	ErrGitURLUserinfo = errors.New("git url must not contain a credential")
	// ErrInvalidGitRef names a requirements version: for a git source that
	// is not a ref this tool accepts: an empty name, a name outside git's own
	// check-ref-format rules, or a refs/ prefix other than refs/heads/ and
	// refs/tags/.
	ErrInvalidGitRef = errors.New("invalid git ref")
	// ErrGitAbbreviatedCommit names a ref that reads as a shortened commit
	// hash (7 to 39 hex digits). It is refused rather than resolved because
	// a lockfile pins a full commit and a prefix may become ambiguous later;
	// a branch that happens to be spelled in hex is reachable through the
	// qualified refs/heads/<name> form.
	ErrGitAbbreviatedCommit = errors.New("abbreviated git commit is not accepted")
	// ErrInvalidGitSubdir names a #subdir fragment whose elements are not
	// all safe path elements (see IsPathElement), or that names a .git
	// directory.
	ErrInvalidGitSubdir = errors.New("invalid git subdir")
	// ErrInvalidGitLocator names a persisted git source locator
	// (git+<url>#<subdir>@<commit>) that does not parse back into its parts.
	// It is reachable only through a hand-edited cache or record, never
	// through a value this tool wrote itself.
	ErrInvalidGitLocator = errors.New("invalid git source locator")
	// ErrGitNameMismatch reports that a requirements entry named a
	// collection (name: namespace.name with a git source:) the repository at
	// the resolved commit does not carry.
	ErrGitNameMismatch = errors.New("explicit collection name does not match the git repository")
	// ErrGitCollectionNotFound reports that the repository, at the resolved
	// commit and under the requested subdir, holds no collection: the subdir
	// does not exist, or neither it nor any of its immediate children carries
	// a galaxy.yml or a MANIFEST.json.
	ErrGitCollectionNotFound = errors.New("no collection found in git repository")
	// ErrGitDuplicateCollection reports that two directories of one
	// repository declare the same namespace.name.
	ErrGitDuplicateCollection = errors.New("git repository declares the same collection twice")
	// ErrGalaxyYMLInvalid names a galaxy.yml (or a MANIFEST.json standing in
	// for one) this tool cannot build from: not a mapping, larger than the
	// cap, a key of the wrong type, a missing mandatory key, a manifest:
	// directive block (not supported; use build_ignore), or a directory
	// carrying both galaxy.yml and MANIFEST.json. The message names the
	// specific defect.
	ErrGalaxyYMLInvalid = errors.New("invalid galaxy.yml")
	// ErrGitCollectionVersionNotExact reports a galaxy.yml whose version is
	// missing or not an exact semver version. ansible-galaxy installs such a
	// collection as version "*"; here the install path, the lockfile and the
	// cache key all need an exact version, so the run is refused naming the
	// repository and the remedy.
	ErrGitCollectionVersionNotExact = errors.New("git collection galaxy.yml version is not an exact version")
	// ErrGitCredentialInvalid names a GO_GALAXY_GIT_<ID>_* binding this tool
	// refuses: an id outside the server-id alphabet or listed twice, a
	// missing or malformed _URL, a _PASSWORD without a _USERNAME, both
	// _SSH_KEY and _SSH_KEY_FILE, a credential kind that does not match the
	// URL's scheme, an unreadable or unparseable key, or two ids bound to one
	// URL. The message names the variable, never its value.
	ErrGitCredentialInvalid = errors.New("invalid git credential configuration")
	// ErrGitSSHNoCredential reports an ssh repository URL for which no key is
	// bound and no ssh-agent is reachable through SSH_AUTH_SOCK. Usage class:
	// the remedy is a binding or an agent, not a retry.
	ErrGitSSHNoCredential = errors.New("no ssh credential for git repository")

	// ErrGitTransportFailed wraps a go-git transport failure: a refused
	// connection, a TLS or protocol error, a pkt-line ERR from the remote, a
	// redirect that would change the origin, or a host-key database this
	// tool could not load. Network class.
	ErrGitTransportFailed = errors.New("git transport failure")
	// ErrGitAuthFailed reports that the remote refused the credential this
	// run presented (401, 403, a rejected key) or that its host key was not
	// the one known_hosts vouches for. Network class, like a Galaxy 401:
	// a runtime condition to investigate, not a configuration shape to fix.
	ErrGitAuthFailed = errors.New("git authentication failed")

	// ErrGitRefNotFound reports that the remote advertises no such branch or
	// tag, and no HEAD when HEAD was asked for. Resolution class: the remote
	// has no candidate for what requirements.yml asked for.
	ErrGitRefNotFound = errors.New("git ref not found on the remote")
	// ErrGitCommitNotFound reports that a commit named by its hash is not
	// reachable on the remote, or that the remote did not ship it.
	ErrGitCommitNotFound = errors.New("git commit not found on the remote")

	// ErrGitCommitMismatch reports that the remote advertised one commit for
	// the requested ref and then shipped a pack in which that commit does
	// not exist, or that a requested commit came back under a different
	// hash. Integrity class: bytes did not match the identity they were
	// promised under.
	ErrGitCommitMismatch = errors.New("fetched git commit does not match the requested commit")
	// ErrGitArtifactIdentityMismatch reports that rebuilding a pinned git
	// collection from its commit produced a different namespace, name or
	// version than the pin records.
	ErrGitArtifactIdentityMismatch = errors.New("rebuilt git artifact is not the pinned collection")

	// ErrGitTreeEntryInvalid names a tree entry this tool refuses to
	// materialize: an empty name, ".", "..", a slash or backslash, a NUL or
	// control rune, a .git or git~1 name (case-insensitive), or a malformed
	// mode. Install class: the archive that would be built from it would be
	// refused by the extractor anyway.
	ErrGitTreeEntryInvalid = errors.New("git tree entry is invalid")
	// ErrGitTreeDuplicateEntry names two entries of one tree whose names are
	// equal, or equal once case is folded - the install destination may be a
	// case-insensitive filesystem, where the second would overwrite the first.
	ErrGitTreeDuplicateEntry = errors.New("git tree carries duplicate entries")
	// ErrGitTreeTooDeep reports a tree nested deeper than GitTreeMaxDepth.
	ErrGitTreeTooDeep = errors.New("git tree is nested too deep")
	// ErrGitSymlinkUnresolvable names a symlink whose target cannot be
	// resolved inside the collection: a dangling link, or a chain longer than
	// the manifest chain check follows.
	ErrGitSymlinkUnresolvable = errors.New("git symlink target cannot be resolved")
	// ErrGitArtifactSelfCheck reports that an artifact this tool built from a
	// git tree did not pass its own manifest chain check. That is a defect in
	// the builder, not in the repository, which is why it carries the cause as
	// text rather than wrapping it: a wrapped ErrManifestChainMismatch would
	// classify as an integrity failure of the remote's bytes.
	ErrGitArtifactSelfCheck = errors.New("built git artifact failed its self-check")
)

// Role requirement errors. Every one of them is raised at the boundary a
// roles: entry enters through - the requirements parser or the root
// preparation that follows it - before any request is made, and every one
// classifies as usage: the remedy is an edit to the file.
var (
	// ErrInvalidRolesList reports a roles: value that is neither a list nor
	// absent.
	ErrInvalidRolesList = errors.New("roles must be a list")
	// ErrInvalidRoleEntry reports a roles: item of a shape ansible would not
	// accept either, or one carrying a key a role cannot take (source:,
	// signatures:, type:); the message names the defect.
	ErrInvalidRoleEntry = errors.New("invalid role entry")
	// ErrInvalidRoleName reports a Galaxy role name outside IsRoleName:
	// anything but owner.role with both halves in the role alphabet.
	ErrInvalidRoleName = errors.New("invalid role name")
	// ErrInvalidRoleInstallName reports a name: (or a name derived from a
	// repository URL) that IsRoleInstallName refuses as a directory under
	// roles_path.
	ErrInvalidRoleInstallName = errors.New("invalid role install name")
	// ErrInvalidRoleVersion reports a version: that neither the ref grammar
	// nor IsRoleVersion accepts.
	ErrInvalidRoleVersion = errors.New("invalid role version")
	// ErrUnsupportedRoleSource reports a src: this tool does not install
	// from: a tarball or any other non-git URL, a local path, or a git
	// pointer carrying a #subdir fragment. Only a Galaxy role name and a git
	// repository are supported.
	ErrUnsupportedRoleSource = errors.New("unsupported role source")
	// ErrUnsupportedRoleScm reports an scm: other than git; ansible shells
	// out to hg for the other value and this tool executes no process.
	ErrUnsupportedRoleScm = errors.New("unsupported role scm")
	// ErrUnsupportedRoleInclude reports an include: entry, which ansible
	// reads another requirements file through; list the included roles
	// inline instead.
	ErrUnsupportedRoleInclude = errors.New("role include is not supported")
	// ErrDuplicateRoleRequirement reports two roles: entries that would
	// install into one directory under roles_path.
	ErrDuplicateRoleRequirement = errors.New("duplicate role requirement")
)

// Role build and install errors.
var (
	// ErrRoleMetaNotFound reports that the repository root carries no
	// meta/main.yml (nor meta/main.yaml), so it is not an Ansible role.
	// Usage class, like ErrGitCollectionNotFound: the remedy is to point the
	// entry at the role's own repository.
	ErrRoleMetaNotFound = errors.New("no role found in git repository")
	// ErrRoleMetaInvalid names a meta/main.yml or meta/requirements.yml this
	// tool cannot read dependencies from: not a mapping, larger than the cap,
	// both .yml and .yaml present, or a dependency of a shape ansible would
	// not accept either. The message names the specific defect.
	ErrRoleMetaInvalid = errors.New("invalid role meta")
	// ErrRoleDirectoryForeign reports that the directory a role would install
	// into already exists and was installed neither by this tool (no extract
	// marker) nor by ansible-galaxy (no meta/.galaxy_install_info): replacing
	// it could destroy a role somebody wrote by hand, so the install stops,
	// as ansible-galaxy's does.
	ErrRoleDirectoryForeign = errors.New("role directory exists and was not installed by a Galaxy client")
	// ErrRoleArtifactIdentityMismatch reports that rebuilding a pinned role
	// from its commit produced a different commit than the pin records.
	ErrRoleArtifactIdentityMismatch = errors.New("rebuilt role artifact is not the pinned role")
)

// Galaxy role API errors.
var (
	// ErrGalaxyRoleAPIUnavailable reports that no configured Galaxy server
	// serves the v1 role API: a Galaxy role needs galaxy.ansible.com or a
	// standalone Galaxy NG, and an Automation Hub has no v1. Usage class: no
	// retry helps, the remedy is configuration.
	ErrGalaxyRoleAPIUnavailable = errors.New("no configured Galaxy server serves the v1 role API")
	// ErrRoleNotFound reports that every configured server with a v1 role API
	// answered, and none knows the role. Resolution class, like a collection
	// no server has.
	ErrRoleNotFound = errors.New("role not found on any configured Galaxy server")
	// ErrRoleVersionNotFound reports that the version asked for is not among
	// the versions the Galaxy server lists for the role.
	ErrRoleVersionNotFound = errors.New("role version not found")
	// ErrRoleVersionsIncomparable reports that the role's version names
	// cannot be ordered (a numeric component against a textual one), so no
	// highest version can be chosen; ansible fails the same way and gives
	// the same remedy, an explicit version.
	ErrRoleVersionsIncomparable = errors.New("role versions cannot be compared")
	// ErrGalaxyRoleInvalid names a v1 role record this tool refuses to build
	// a repository URL from: a GitHub user or repository outside the alphabet,
	// or a branch that is not a ref name. Usage class: the server's record is
	// not one this tool can act on.
	ErrGalaxyRoleInvalid = errors.New("invalid role record from the Galaxy server")
)

// The url-source sentinels below are grouped by exit class the way the
// git-source sentinels are: a shape refusal stays a usage error, a pin the
// remote no longer honors is an integrity failure.
var (
	// ErrInvalidURLRequirement names a tarball URL this tool will not use:
	// a scheme other than https or http, a missing or invalid host, a "#"
	// anywhere in it (a fragment is never sent to a server, and the locator
	// grammar relies on the URL carrying none), a path or query rune outside
	// the conservative alphabet, a dot or empty path segment (a server
	// resolves those, so they would let a requirements file spend a
	// credential bound to one path prefix elsewhere on the host), or a URL
	// naming nothing beyond its origin. Usage class: the value came from
	// requirements.yml or a credential binding.
	ErrInvalidURLRequirement = errors.New("invalid url source")
	// ErrURLRequirementUserinfo names a tarball URL that carries a
	// credential. The URL is repository content, so the credential would be
	// too; a url credential comes from the environment, bound to an origin
	// and path prefix, and never from the URL.
	ErrURLRequirementUserinfo = errors.New("url source must not contain a credential")
	// ErrInvalidURLLocator names a persisted url source locator
	// (url+<url>#sha256:<hex>) that does not parse back into its parts. It
	// is reachable only through a hand-edited cache or record, never through
	// a value this tool wrote itself.
	ErrInvalidURLLocator = errors.New("invalid url source locator")
	// ErrURLCredentialInvalid names a GO_GALAXY_URL_<ID>_* binding this tool
	// refuses: an id outside the server-id alphabet or listed twice, a
	// missing or malformed _URL, a missing _TOKEN, or two ids bound to one
	// URL. The message names the variable, never its value.
	ErrURLCredentialInvalid = errors.New("invalid url credential configuration")
	// ErrRoleTarballLayout reports a role tarball whose layout this tool
	// cannot resolve to exactly one role: no meta/main.yml at the archive
	// root or under a single top-level directory, or a meta/main.yml under
	// more than one. Usage class, like ErrRoleMetaNotFound: the remedy is a
	// different artifact or entry.
	ErrRoleTarballLayout = errors.New("role tarball does not contain exactly one role")
	// ErrRoleTarballEntryInvalid names a tarball entry this tool refuses to
	// repack into a role artifact: a name carrying a control rune or a
	// backslash. The extractor tolerates such a name on disk; the archive
	// this tool would build from it would not survive its own extractor's
	// name rules, so it is refused at the read instead.
	ErrRoleTarballEntryInvalid = errors.New("role tarball entry is invalid")

	// ErrURLCollectionVersionMismatch reports that the artifact a url source
	// serves is built as one version while the requirements entry asserts
	// another. Resolution class, like a constraint no candidate satisfies:
	// the URL's one candidate is not the version the file asked for.
	ErrURLCollectionVersionMismatch = errors.New("url collection version does not match the requested version")

	// ErrURLArtifactIdentityMismatch reports that refetching a pinned url
	// collection produced a different namespace, name or version than the
	// pin records. Integrity class: bytes did not match the identity they
	// were promised under.
	ErrURLArtifactIdentityMismatch = errors.New("refetched url artifact is not the pinned collection")
	// ErrURLArtifactSHA256Mismatch reports that the URL a pinned role was
	// fetched from now serves bytes with a different sha256 than the pin
	// records. Collections report the same condition through
	// ErrSHA256Mismatch; a role artifact is repacked before it is stored, so
	// its refusal names the origin bytes explicitly.
	ErrURLArtifactSHA256Mismatch = errors.New("url artifact does not match its pinned sha256")
)
