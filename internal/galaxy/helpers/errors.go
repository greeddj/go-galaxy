// Package helpers is the module's shared vocabulary: sentinel errors matched
// with errors.Is across layers, size caps, cache-key builders and predicates
// over untrusted values. It imports nothing in this module but internal/safeout.
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
	// ErrArchiveDecompressedTooLarge indicates a reader pulled more raw bytes out
	// of an archive's decompressor than ArchiveMaxDecompressedSize allows; unlike
	// ErrArchiveExceedsMaxSize, which judges declared header sizes, it counts reads.
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
	// ErrArchiveEntryNameTooLong indicates an entry name or link target longer than
	// ArchiveMaxEntryNameLen. Code that holds every name in memory raises it; the
	// extractor never does, since the filesystem bounds what it retains.
	ErrArchiveEntryNameTooLong = errors.New("archive entry name is too long")
	// ErrArchiveTooManyEntries indicates an archive contains more entries
	// than ArchiveMaxEntryCount allows.
	ErrArchiveTooManyEntries = errors.New("archive contains too many entries")
	// ErrArchiveDuplicateEntry indicates two archive entries claim one path, found
	// on disk by the extractor or by name in manifest's archiveScan.claim. Refused,
	// never last-wins, which would leave the collision undetectable afterwards.
	ErrArchiveDuplicateEntry = errors.New("archive contains a duplicate entry")

	// ErrEmptyGzipMember indicates a gzip member that produced no bytes, which no
	// artifact or cache state can contain. It is deliberately in no exitcode
	// predicate; each reader's own wrap classifies it (s3's: ErrCorruptStateObject).
	ErrEmptyGzipMember = errors.New("gzip stream carries a member that produces no bytes")

	// ErrArtifactNotTarGz indicates downloaded bytes are not gzip, or the gzip
	// carries no tar stream: the wrong kind of thing for a shared cache slot. It
	// says nothing about the archive's completeness or contents.
	ErrArtifactNotTarGz = errors.New("downloaded artifact is not a gzip-compressed tar archive")

	// ErrArtifactTarHeaderNotFound indicates the shape probe read ArchiveProbeMaxBytes
	// without reaching a tar header, as a long chain of meta headers does. It is not
	// ErrArtifactNotTarGz: the bytes are valid gzip carrying tar framing.
	ErrArtifactTarHeaderNotFound = errors.New("artifact presents no tar header within the shape probe's scan bound")

	// ErrHardlinkTargetIsEmpty indicates a hardlink target is empty.
	ErrHardlinkTargetIsEmpty = errors.New("hardlink target is empty")
	// ErrFileIsEmpty indicates a file is empty.
	ErrFileIsEmpty = errors.New("file is empty")

	// ErrS3EmptyCreds indicates an S3 bucket configured, by flag, variable or
	// galaxy.toml, without both keys from any of those sources.
	ErrS3EmptyCreds = errors.New("s3 cache requires access and secret keys when an S3 bucket is configured")
	// ErrS3CacheOffline indicates --offline together with an S3 cache, whose
	// bucket is reached only over the network, so the backend could never open.
	// It is refused while the config is built, before any backend exists.
	ErrS3CacheOffline = errors.New("--offline cannot be combined with an S3 cache bucket: the S3 cache is reached over the network")

	// ErrArtifactCacheNotConfigured indicates the artifact cache is unavailable.
	ErrArtifactCacheNotConfigured = errors.New("artifact cache is not configured")
	// ErrMetadataIsNil indicates metadata is nil when required.
	ErrMetadataIsNil = errors.New("metadata is nil")
	// ErrMissingDownloadURL indicates a collection download URL is missing.
	ErrMissingDownloadURL = errors.New("missing download url")
	// ErrUnsupportedDownloadURLScheme indicates a download URL whose scheme is not
	// http or https. It comes from server metadata or a poisonable snapshot, so it
	// is judged against an allow-list: anything not named is refused.
	ErrUnsupportedDownloadURLScheme = errors.New("collection download url scheme is not http or https")
	// ErrDownloadURLUserinfo indicates a collection's download URL embeds userinfo.
	// net/http would turn it into Basic auth that fetch.authTransport then leaves in
	// place of the operator's token; the refusal never renders the password.
	ErrDownloadURLUserinfo = errors.New("collection download url must not contain userinfo")
	// ErrDownloadURLQuery indicates `lock` was handed a download URL carrying a
	// query string: typically a presigned capability that expires, and a
	// lockfile is committed, so the URL is refused rather than written.
	ErrDownloadURLQuery = errors.New("collection download url must not carry a query string")
	// ErrDownloadURLNotServerArtifact indicates a lockfile download URL off its
	// server's origin or not ending in the artifact's file name, which could fill
	// that server's cache slot with another host's bytes.
	ErrDownloadURLNotServerArtifact = errors.New("collection download url is not its server's own artifact url")
	// ErrMetadataURLUserinfo indicates a Galaxy metadata URL (versions_url or
	// highest_version.href, fresh or replayed from a snapshot) embeds userinfo, with
	// ErrDownloadURLUserinfo's effect. It classifies alike but names another field.
	ErrMetadataURLUserinfo = errors.New("galaxy metadata url must not contain userinfo")
	// ErrMetadataRequestBuildFailed indicates net/http refused to build a request
	// from a Galaxy metadata URL. Its text omits the value (*url.Error echoes any
	// password); it is not ErrMetadataUnavailable, which prepareInstall tolerates.
	ErrMetadataRequestBuildFailed = errors.New("galaxy metadata url could not be built into a request")
	// ErrConfigIsNil indicates a nil config was provided.
	ErrConfigIsNil = errors.New("config is nil")
	// ErrSHA256Mismatch indicates a checksum mismatch.
	ErrSHA256Mismatch = errors.New("sha256 mismatch")
	// ErrMetadataUnavailable indicates metadata could not be loaded.
	ErrMetadataUnavailable = errors.New("metadata unavailable")
	// ErrUnsupportedRequirementsFormat indicates the requirements file format is unsupported.
	ErrUnsupportedRequirementsFormat = errors.New("unsupported requirements file format")
	// ErrRequirementsUnreadable indicates a requirements file that exists but
	// cannot be read: permission denied, a directory. Absence stays a bare
	// fs.ErrNotExist, which cleanup tells apart from this.
	ErrRequirementsUnreadable = errors.New("requirements file is unreadable")
	// ErrInvalidRequirementsYAML indicates a requirements file whose bytes do not
	// parse as YAML; a document that parses into the wrong shape is
	// ErrUnsupportedRequirementsFormat or an entry sentinel instead.
	ErrInvalidRequirementsYAML = errors.New("requirements file is not valid YAML")
	// ErrInvalidRequirementsTOML indicates a galaxy.toml whose bytes do not
	// parse as TOML; a document that parses into the wrong shape is
	// ErrUnsupportedRequirementsFormat or an entry sentinel instead.
	ErrInvalidRequirementsTOML = errors.New("requirements file is not valid TOML")
	// ErrProjectFileEnvUnset indicates a ${VAR} under [tool.go-galaxy] naming a
	// variable the environment lacks; every such name is reported in one error,
	// sorted, and no value is ever rendered. projectfile.LoadSettings raises it.
	ErrProjectFileEnvUnset = errors.New("project file references unset environment variables")

	// ErrCacheDirEmpty indicates the cache directory is empty.
	ErrCacheDirEmpty = errors.New("cache directory is empty")
	// ErrAnotherInstanceIsRunning indicates another instance is already running.
	ErrAnotherInstanceIsRunning = errors.New("another instance is running")
	// ErrCacheBusy indicates another holder kept the cache past this backend's own
	// ceiling: a local Bolt open timeout, or the S3 lock's wait ceiling against an
	// observed holder.
	ErrCacheBusy = errors.New("another process holds the cache")
	// ErrCacheLockLost indicates another holder took the cache lock mid-run, so work
	// after the takeover was not exclusive. A carrier renders its context cause with
	// %v, never %w, or exitcode.FromError would report the loss as a Ctrl-C.
	ErrCacheLockLost = errors.New("cache lock ownership was lost to another holder")
	// ErrCacheBackendUnavailable indicates a remote cache backend could not
	// be reached, or answered a request with a failure that is not this
	// program's own doing.
	ErrCacheBackendUnavailable = errors.New("cache backend unavailable")
	// ErrCacheBackendUnusable indicates the configured backend cannot provide a
	// guarantee this tool requires, or cannot be addressed at all; only a
	// configuration change helps.
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
	// ErrVersionsPagingExceeded indicates a versions list reported more pages than
	// the ceiling allows. It fails hard: a truncated list could resolve a version
	// that does not satisfy the constraints.
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
	// ErrWarmCacheDisabled indicates warm was run with --no-cache, which would
	// download everything and commit nothing. It is refused before any lock is taken
	// or request made.
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
	// ErrCorruptProjectRegistry indicates the project registry could not be decoded.
	// Never treat it as empty: cleanup would find nothing reachable and delete every
	// installed collection.
	ErrCorruptProjectRegistry = errors.New("corrupt project registry")
	// ErrCorruptSnapshotStore indicates the local Bolt snapshot failed one of
	// bbolt's integrity checks on open (store's openBolt); discard it. Unlike
	// ErrUnsupportedSchemaVersion, the bytes themselves are damaged.
	ErrCorruptSnapshotStore = errors.New("corrupt snapshot store")
	// ErrStateObjectTooLarge indicates an S3 snapshot or registry object overran
	// StateObjectMaxCompressedSize or StateObjectMaxDecompressedSize. It is never
	// retried, and the object must be discarded before a run can proceed.
	ErrStateObjectTooLarge = errors.New("cache state object exceeds the maximum allowed size")
	// ErrCorruptStateObject indicates an S3 snapshot or registry object is not the
	// gzipped or plain JSON this program writes there. It is never retried; it is
	// the S3 counterpart of ErrCorruptSnapshotStore, with the same remedy.
	ErrCorruptStateObject = errors.New("corrupt cache state object")

	// ErrOfflineMode indicates a network operation was attempted in offline mode.
	ErrOfflineMode = errors.New("offline mode is enabled, network access is forbidden")
	// ErrReadStalled indicates a body read made no progress within the timeout
	// while the request context was live. Its cause, the watchdog's own cancel,
	// renders with %v, never %w, or exitcode.FromError would report it as a Ctrl-C.
	ErrReadStalled = errors.New("network read stalled")
	// ErrArtifactDownloadDeadline indicates one artifact acquisition exceeded
	// ArtifactDownloadDeadline, which catches a byte-drip. Never retried; the cause
	// renders with %v, never %w, so context.Canceled cannot steal its exit class.
	ErrArtifactDownloadDeadline = errors.New("artifact download deadline exceeded")
	// ErrMetadataFetchDeadline indicates one Galaxy metadata request exceeded
	// MetadataFetchDeadline. Never retried; the cause renders with %v, and an error
	// with no context signal (an HTTPStatusError) keeps its identity instead.
	ErrMetadataFetchDeadline = errors.New("galaxy metadata fetch deadline exceeded")
	// ErrStateObjectDeadline indicates one persisted cache-state operation exceeded
	// StateObjectDeadline, a slow object store blocking every runner on the bucket.
	// Never retried; the cause renders with %v, never %w, like every deadline here.
	ErrStateObjectDeadline = errors.New("cache state object deadline exceeded")
	// ErrLockfileMismatch indicates the lockfile content does not match the resolution.
	ErrLockfileMismatch = errors.New("lockfile does not match resolved requirements")
	// ErrLockfileMissing indicates a lockfile a command required was not found. Its
	// text names no flag: tree, explain and outdated require the file without
	// --frozen.
	ErrLockfileMissing = errors.New("lockfile not found")
	// ErrLockfileInvalid indicates the lockfile is malformed or unsupported.
	ErrLockfileInvalid = errors.New("lockfile is invalid")
	// ErrLockfileDrift indicates lock --frozen found the lockfile on disk differs
	// from what a fresh lock would write. Unlike ErrLockfileMismatch, the file covers
	// the roots but is out of date.
	ErrLockfileDrift = errors.New("lockfile is out of date")

	// ErrInvalidTimeout indicates the --timeout value is neither a positive
	// integer number of seconds nor a valid positive Go duration string.
	ErrInvalidTimeout = errors.New("invalid timeout")

	// ErrUnexpectedArguments indicates positional arguments a command does not take.
	// A word naming no command reaches install as one, so "go-galaxy collection
	// install ns.name" is refused.
	ErrUnexpectedArguments = errors.New("unexpected arguments")

	// ErrMissingArgument indicates a command that takes a positional argument
	// was run without it: explain with no collection or role to explain.
	ErrMissingArgument = errors.New("missing argument")

	// ErrAnsibleConfigNotFound indicates an explicitly requested ansible.cfg
	// path does not exist.
	ErrAnsibleConfigNotFound = errors.New("ansible config file not found")
	// ErrAnsibleConfigUnreadable indicates an ansible.cfg, named or discovered,
	// that exists but cannot be read to the end: permission denied, a
	// directory, or a line longer than the parser's 64 KiB limit.
	ErrAnsibleConfigUnreadable = errors.New("ansible config file is unreadable")

	// ErrUnsafeCollectionIdentifier indicates a manifest field (namespace,
	// name, or version) cannot be safely used as a single filesystem path
	// element, e.g. it contains a path separator or is "..".
	ErrUnsafeCollectionIdentifier = errors.New("unsafe collection identifier")
	// ErrInvalidCollectionVersion indicates a resolved collection's version is not
	// IsExactVersion ("*", ">=1.0.0") where an installable version is required.
	// Unlike ErrUnsafeCollectionIdentifier, it is path-safe but names no release.
	ErrInvalidCollectionVersion = errors.New("collection version is not an exact version")
	// ErrInvalidCollectionConstraint indicates a galaxy.toml version constraint
	// semver refuses at load; requirements.yml leaves that check to the solver,
	// so only the TOML path raises it.
	ErrInvalidCollectionConstraint = errors.New("invalid collection version constraint")
	// ErrUnsafeRemovalPath indicates a computed removal path failed a
	// containment check against its expected root directory.
	ErrUnsafeRemovalPath = errors.New("unsafe removal path")
	// ErrProjectRequirementsUnreadable indicates a recorded project's requirements
	// failed to load for a reason other than fs.ErrNotExist. Cleanup aborts: the
	// roots it would contribute are unknown and could protect any project's copies.
	ErrProjectRequirementsUnreadable = errors.New("project requirements file is unreadable")
	// ErrCorruptManifest indicates a MANIFEST.json exists but is not valid JSON. It
	// is reported, and the install counts as neither a reachability source nor a
	// deletion candidate.
	ErrCorruptManifest = errors.New("corrupt manifest")

	// ErrResponseTooLarge indicates a body read through NewSizeLimitedReader passed
	// its ceiling. It is never retried; the s3 backend's readObject reclassifies a
	// state object's overrun as ErrStateObjectTooLarge.
	ErrResponseTooLarge = errors.New("response body exceeds the maximum allowed size")

	// ErrUnsupportedGalaxyServerKey indicates a [galaxy_server.<id>] key implying
	// Basic auth or a Keycloak/SSO exchange (username, password, auth_url,
	// client_id). Config load fails rather than sending a request that will 401.
	ErrUnsupportedGalaxyServerKey = errors.New("unsupported galaxy_server key: configure a Galaxy API token instead")
	// ErrUnsupportedGalaxyServerAPIVersion indicates a [galaxy_server.<id>]
	// section set api_version to something other than the one value this
	// tool understands ("v3").
	ErrUnsupportedGalaxyServerAPIVersion = errors.New("unsupported galaxy_server api_version")
	// ErrMissingGalaxyServerURL indicates a server named in server_list has
	// no url configured, from either its [galaxy_server.<id>] section or
	// the matching ANSIBLE_GALAXY_SERVER_<ID>_URL env var.
	ErrMissingGalaxyServerURL = errors.New("galaxy server is missing its url")
	// ErrInvalidGalaxyServerURL indicates a configured Galaxy server URL is not an
	// absolute URL with a scheme and host. The raw value is never included: it may
	// carry userinfo.
	ErrInvalidGalaxyServerURL = errors.New("invalid galaxy server url")
	// ErrInvalidGalaxyServerID indicates a server_list id outside [A-Za-z0-9_-]: a
	// "." would make [galaxy_server.<id>] ambiguous, and other runes cannot fold
	// into an ANSIBLE_GALAXY_SERVER_<ID>_* name.
	ErrInvalidGalaxyServerID = errors.New("invalid galaxy server id")
	// ErrDuplicateGalaxyServerID indicates server_list repeats an id, or lists two
	// differing only in case, which share one ANSIBLE_GALAXY_SERVER_<ID>_* prefix.
	ErrDuplicateGalaxyServerID = errors.New("duplicate galaxy server id")
	// ErrInvalidValidateCerts indicates a validate_certs value outside ansible's
	// boolean spellings. Always a hard error: an unparseable value must never
	// resolve to "false" (certs unverified).
	ErrInvalidValidateCerts = errors.New("invalid validate_certs value")
	// ErrGalaxyServerURLUserinfo indicates a configured server URL or a collection
	// source: embeds userinfo, which url.URL.String() would leak into logs, the
	// lockfile, GALAXY.yml and the snapshot. It is refused at load.
	ErrGalaxyServerURLUserinfo = errors.New("galaxy server url must not contain userinfo")
	// ErrInsecureTokenTransport indicates a token configured for a plaintext http
	// origin that is not loopback, where any on-path observer could capture it. It
	// is refused at config load.
	ErrInsecureTokenTransport = errors.New("galaxy server token configured for an insecure plaintext transport")
	// ErrConflictingServerTLSPolicy indicates two servers share a normalized origin
	// but disagree on validate_certs: one origin must map to exactly one transport.
	ErrConflictingServerTLSPolicy = errors.New("conflicting validate_certs for the same galaxy server origin")
	// ErrConflictingServerToken indicates two servers share a normalized origin but
	// carry different tokens (one unset included): one origin, one credential.
	ErrConflictingServerToken = errors.New("conflicting token for the same galaxy server origin")
	// ErrAmbiguousGalaxyToken indicates --token (or GO_GALAXY_TOKEN) with a
	// multi-entry server_list: it names no server, and guessing could send a
	// private hub's token to the public Galaxy.
	ErrAmbiguousGalaxyToken = errors.New("--token is ambiguous with a multi-entry server_list")
	// ErrTokenDestinationFromFile indicates a token paired with a server URL an
	// ansible.cfg or galaxy.toml chose, while the token came from elsewhere. It
	// is refused, never warned; config.checkTokenPairing is its single producer.
	ErrTokenDestinationFromFile = errors.New("galaxy server token destination came from a configuration file")
	// ErrTokenTLSPolicyFromFile indicates a token paired with a server whose
	// certificate checks an ansible.cfg or galaxy.toml disabled, while the token
	// came from elsewhere: the second half of config.checkTokenPairing's rule.
	ErrTokenTLSPolicyFromFile = errors.New(
		"galaxy server certificate verification was disabled by a configuration file for a token it did not supply")

	// ErrGalaxyAuthFailed indicates a server answered root metadata with 401 or 403.
	// Fail-closed: unlike a 404 it aborts the run rather than falling through to a
	// server that might answer anonymously.
	ErrGalaxyAuthFailed = errors.New("galaxy server authentication failed")
	// ErrGalaxyServerUnavailable indicates a server kept answering root metadata
	// with a retryable status until the retry budget was spent. It aborts the run:
	// an outage is no evidence the collection is absent there.
	ErrGalaxyServerUnavailable = errors.New("galaxy server unavailable")

	// ErrCollectionsPathEscape names the component under cfg.DownloadPath that made
	// os.Root refuse an install write: a symlink, or an unexpected error found while
	// diagnosing. os.Root is what blocks the escape; this only names the component.
	ErrCollectionsPathEscape = errors.New("path escapes the collections directory")

	// ErrMalformedArtifactSHA256 indicates a value meant as an artifact's sha256 is
	// not 64 lowercase hex characters. Unlike ErrSHA256Mismatch it is no digest at
	// all: most often a poisoned snapshot or a lying server.
	ErrMalformedArtifactSHA256 = errors.New("artifact sha256 is not a 64-character lowercase hex digest")

	// ErrSignatureVerificationFailed is one collection's aggregate verdict: the
	// signatures checked did not satisfy the policy in force. It is never
	// ErrSHA256Mismatch: a digest names the bytes, a signature names who published.
	ErrSignatureVerificationFailed = errors.New("collection signature verification failed")
	// ErrSignatureAttributionMismatch indicates a verified signature vouches for a
	// MANIFEST.json naming another namespace, name or version: a signed downgrade
	// or substitution. The comparison is exact on purpose; never normalize it.
	ErrSignatureAttributionMismatch = errors.New("collection signature vouches for a different collection")
	// ErrTooManySignatureSources indicates one requirements entry declares more
	// signature sources than MaxSignaturesPerCollection. It is a load-time usage
	// error; a combined set over the cap is gatherLimit's to report instead.
	ErrTooManySignatureSources = errors.New("too many signature sources declared for one collection")
	// ErrSignatureSourceUnavailable indicates a signature to check could not be
	// obtained (network, offline mode, unreadable file://, unreachable metadata).
	// Unlike ErrSignatureVerificationFailed, nothing was verified.
	ErrSignatureSourceUnavailable = errors.New("collection signature source unavailable")
	// ErrSignatureFetchDeadline indicates one collection's signature fetching
	// exceeded SignatureFetchDeadline. Never retried; the cause renders with %v,
	// never %w, a rule ErrSignatureSourceUnavailable alone is exempt from.
	ErrSignatureFetchDeadline = errors.New("collection signature fetch deadline exceeded")
	// ErrUnsupportedSignatureSource indicates a signature source names nothing this
	// tool fetches, judged by an allow-list of file, http and https URLs with a
	// usable authority and absolute path. Nothing was fetched; edit the source.
	ErrUnsupportedSignatureSource = errors.New("signature source is not a fetchable file, http, or https URL")
	// ErrSignatureSourceUserinfo indicates a signature source URL embeds userinfo.
	// A source is repository content, so net/http would send its credential as
	// Basic auth on this run's request; the message never renders the password.
	ErrSignatureSourceUserinfo = errors.New("signature source url must not contain userinfo")
	// ErrKeyringUnreadable indicates the configured keyring is absent, cannot be
	// opened, or does not parse as OpenPGP key material. It is no verification
	// verdict; ErrKeyringIsKeybox is the one unreadable shape kept out of it.
	ErrKeyringUnreadable = errors.New("keyring could not be read")
	// ErrKeyringIsKeybox indicates the keyring is a GnuPG keybox (.kbx), which a
	// pure-Go verifier running no gpg process cannot read. It is kept apart from
	// ErrKeyringUnreadable for its one-command remedy, named in the text.
	ErrKeyringIsKeybox = errors.New(
		"keyring is a GnuPG keybox (.kbx), which this tool cannot read; export an armored keyring instead: " +
			"gpg --no-default-keyring --keyring <kbx> --export --armor > keyring.asc")
	// ErrKeyringRequired indicates a requirements entry declares signatures: while
	// no keyring is configured. It is refused, as ansible-galaxy does, rather than
	// installed unverified; unlike ErrKeyringUnreadable, no keyring was named.
	ErrKeyringRequired = errors.New("requirements declare signatures but no keyring is configured")
	// ErrInvalidSignatureCount indicates a required-valid-signature-count that is
	// neither "all" nor a positive count, bare or "+N". No count was compared, so
	// it is a config error, not ErrSignatureVerificationFailed.
	ErrInvalidSignatureCount = errors.New("invalid required valid signature count")
	// ErrUnknownSignatureStatusCode indicates an ignore-signature-status-code value
	// naming no known status code. It is refused: a typo would ignore nothing while
	// reading as a tolerated failure.
	ErrUnknownSignatureStatusCode = errors.New("unknown signature status code")
	// ErrInvalidDisableGPGVerify indicates ANSIBLE_GALAXY_DISABLE_GPG_VERIFY is not
	// one of ansible's boolean spellings. A security boolean is never guessed: false
	// would ignore the intent, true would turn verification off for a typo.
	ErrInvalidDisableGPGVerify = errors.New("invalid disable_gpg_verify value")
	// ErrEmptySignatureValue indicates the keyring or required count was supplied
	// empty, typically a CI secret withheld from a fork's pull request. Refused, or
	// it would verify nothing or replace a configured count with the default.
	ErrEmptySignatureValue = errors.New("signature setting was supplied empty")
	// ErrManifestNotFound indicates an artifact names no MANIFEST.json within
	// ManifestScanMaxBytes of its tar stream's start; a real collection carries it
	// up front. Unlike ErrCorruptManifest, there is no manifest to parse.
	ErrManifestNotFound = errors.New("collection artifact contains no MANIFEST.json")
	// ErrManifestChainMismatch indicates the digest chain from MANIFEST.json through
	// FILES.json to each file broke, or a file is unlisted. It classifies as
	// integrity: a signature covers MANIFEST.json alone, so it rests on this chain.
	ErrManifestChainMismatch = errors.New("collection manifest chain does not match")

	// ErrLatestVersionLookupFailed is the headline of an outdated run in which a
	// latest-version lookup failed. Named for the lookup so it is not read as a
	// sibling of ErrOutdatedSchemaVersion.
	ErrLatestVersionLookupFailed = errors.New("latest version lookup failed")

	// The git-source sentinels below are grouped by exit class, the contract a
	// caller relies on: a config refusal stays usage even when found after a round
	// trip, and a missing ref classifies as resolution, not transport.

	// ErrInvalidGitURL names a repository URL this tool refuses: a bad scheme, an
	// empty host or path, a query or fragment, a quote or control rune (go-git
	// quotes the path without escaping it), or a leading "-". Usage class.
	ErrInvalidGitURL = errors.New("invalid git url")
	// ErrGitURLUserinfo names a repository URL carrying a credential (userinfo on
	// http(s), a password on ssh). The URL is repository content; git credentials
	// come from the environment, bound to a host.
	ErrGitURLUserinfo = errors.New("git url must not contain a credential")
	// ErrInvalidGitRef names a git source's version that is no acceptable ref: an
	// empty name, one git's check-ref-format refuses, or a refs/ prefix other than
	// refs/heads/ and refs/tags/.
	ErrInvalidGitRef = errors.New("invalid git ref")
	// ErrGitAbbreviatedCommit names a ref reading as a shortened commit hash (7 to
	// 39 hex digits): a lockfile pins full commits and a prefix may turn ambiguous.
	// A hex-spelled branch is reachable as refs/heads/<name>.
	ErrGitAbbreviatedCommit = errors.New("abbreviated git commit is not accepted")
	// ErrInvalidGitSubdir names a #subdir fragment whose elements are not
	// all safe path elements (see IsPathElement), or that names a .git
	// directory.
	ErrInvalidGitSubdir = errors.New("invalid git subdir")
	// ErrInvalidGitLocator names a persisted locator (git+<url>#<subdir>@<commit>)
	// that does not parse back into its parts; only a hand-edited cache or record
	// reaches it.
	ErrInvalidGitLocator = errors.New("invalid git source locator")
	// ErrGitNameMismatch reports that a requirements entry named a
	// collection (name: namespace.name with a git source:) the repository at
	// the resolved commit does not carry.
	ErrGitNameMismatch = errors.New("explicit collection name does not match the git repository")
	// ErrGitCollectionNotFound reports that the requested subdir at the resolved
	// commit is missing, or neither it nor an immediate child carries a galaxy.yml
	// or MANIFEST.json.
	ErrGitCollectionNotFound = errors.New("no collection found in git repository")
	// ErrGitDuplicateCollection reports that two directories of one
	// repository declare the same namespace.name.
	ErrGitDuplicateCollection = errors.New("git repository declares the same collection twice")
	// ErrGalaxyYMLInvalid names a galaxy.yml (or a MANIFEST.json standing in) this
	// tool cannot build from, including a manifest: directive block and a directory
	// carrying both files. The message names the specific defect.
	ErrGalaxyYMLInvalid = errors.New("invalid galaxy.yml")
	// ErrGitCollectionVersionNotExact reports a galaxy.yml version missing or not
	// exact. ansible-galaxy installs it as "*"; the install path, lockfile and cache
	// key here all need an exact version.
	ErrGitCollectionVersionNotExact = errors.New("git collection galaxy.yml version is not an exact version")
	// ErrGitCredentialInvalid names a GO_GALAXY_GIT_<ID>_* binding this tool
	// refuses: a bad or repeated id, a malformed _URL, or credential parts that do
	// not fit together. The message names the variable, never its value.
	ErrGitCredentialInvalid = errors.New("invalid git credential configuration")
	// ErrGitSSHNoCredential reports an ssh repository URL for which no key is
	// bound and no ssh-agent is reachable through SSH_AUTH_SOCK. Usage class:
	// the remedy is a binding or an agent, not a retry.
	ErrGitSSHNoCredential = errors.New("no ssh credential for git repository")

	// ErrGitTransportFailed wraps a go-git transport failure: connection, TLS or
	// protocol errors, a remote ERR, an origin-changing redirect, or an unloadable
	// host-key database. Network class.
	ErrGitTransportFailed = errors.New("git transport failure")
	// ErrGitAuthFailed reports the remote refused this run's credential, or its
	// host key is not the one known_hosts vouches for. Network class, like a
	// Galaxy 401.
	ErrGitAuthFailed = errors.New("git authentication failed")

	// ErrGitRefNotFound reports that the remote advertises no such branch or
	// tag, and no HEAD when HEAD was asked for. Resolution class: the remote
	// has no candidate for what requirements.yml asked for.
	ErrGitRefNotFound = errors.New("git ref not found on the remote")
	// ErrGitCommitNotFound reports that a commit named by its hash is not
	// reachable on the remote, or that the remote did not ship it.
	ErrGitCommitNotFound = errors.New("git commit not found on the remote")

	// ErrGitCommitMismatch reports the remote shipped a pack without the commit it
	// advertised for the ref, or a requested commit under another hash. Integrity
	// class.
	ErrGitCommitMismatch = errors.New("fetched git commit does not match the requested commit")
	// ErrGitArtifactIdentityMismatch reports that rebuilding a pinned git
	// collection from its commit produced a different namespace, name or
	// version than the pin records.
	ErrGitArtifactIdentityMismatch = errors.New("rebuilt git artifact is not the pinned collection")

	// ErrGitTreeEntryInvalid names a tree entry this tool will not materialize: an
	// unsafe name (".", "..", separators, control runes, .git or git~1) or a
	// malformed mode. Install class.
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
	// ErrGitArtifactSelfCheck reports a built artifact failing its own manifest
	// chain check, a builder defect. The cause is text, not wrapped, or
	// ErrManifestChainMismatch would blame the remote's bytes as integrity.
	ErrGitArtifactSelfCheck = errors.New("built git artifact failed its self-check")
)

// Role requirement errors, raised where a roles: entry enters (the requirements
// parser or root preparation) before any request. All classify as usage.
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
	// ErrUnsupportedRoleSource reports a src: this tool does not install from: a
	// URL neither git nor http(s) .tar.gz, a local path, a non-http tarball, or a
	// git pointer carrying a #subdir fragment.
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
	// ErrRoleMetaNotFound reports a repository root with no meta/main.yml (nor
	// meta/main.yaml), so not a role. Usage class: point the entry at the role's
	// own repository.
	ErrRoleMetaNotFound = errors.New("no role found in git repository")
	// ErrRoleMetaInvalid names a meta/main.yml or meta/requirements.yml this tool
	// cannot read dependencies from (not a mapping, over the cap, both .yml and
	// .yaml, a dependency ansible would refuse). The message names the defect.
	ErrRoleMetaInvalid = errors.New("invalid role meta")
	// ErrRoleDirectoryForeign reports an existing role directory with neither this
	// tool's extract marker nor meta/.galaxy_install_info. It may be hand-written,
	// so the install stops, as ansible-galaxy's does.
	ErrRoleDirectoryForeign = errors.New("role directory exists and was not installed by a Galaxy client")
	// ErrRoleArtifactIdentityMismatch reports that rebuilding a pinned role
	// from its commit produced a different commit than the pin records.
	ErrRoleArtifactIdentityMismatch = errors.New("rebuilt role artifact is not the pinned role")
)

// Galaxy role API errors.
var (
	// ErrGalaxyRoleAPIUnavailable reports no configured server serves the v1 role
	// API (galaxy.ansible.com or standalone Galaxy NG; Automation Hub has none).
	// Usage class: the remedy is configuration.
	ErrGalaxyRoleAPIUnavailable = errors.New("no configured Galaxy server serves the v1 role API")
	// ErrRoleNotFound reports that every configured server with a v1 role API
	// answered, and none knows the role. Resolution class, like a collection
	// no server has.
	ErrRoleNotFound = errors.New("role not found on any configured Galaxy server")
	// ErrRoleVersionNotFound reports that the version asked for is not among
	// the versions the Galaxy server lists for the role.
	ErrRoleVersionNotFound = errors.New("role version not found")
	// ErrRoleVersionsIncomparable reports role version names that cannot be ordered
	// (a numeric component against a textual one); ansible fails the same way, and
	// the remedy is an explicit version.
	ErrRoleVersionsIncomparable = errors.New("role versions cannot be compared")
	// ErrGalaxyRoleInvalid names a v1 role record this tool will not build a URL
	// from: a GitHub user or repository outside the alphabet, or a branch that is
	// not a ref name. Usage class.
	ErrGalaxyRoleInvalid = errors.New("invalid role record from the Galaxy server")
)

// The url-source sentinels below are grouped by exit class the way the
// git-source sentinels are: a shape refusal stays a usage error, a pin the
// remote no longer honors is an integrity failure.
var (
	// ErrInvalidURLRequirement names a tarball URL this tool refuses: a bad scheme
	// or host, any "#", a rune outside its alphabet, a dot or stray empty segment
	// (a path-bound credential could be spent elsewhere), or no path. Usage class.
	ErrInvalidURLRequirement = errors.New("invalid url source")
	// ErrURLRequirementUserinfo names a tarball URL carrying a credential. The URL
	// is repository content; url credentials come from the environment, bound to
	// an origin and path prefix.
	ErrURLRequirementUserinfo = errors.New("url source must not contain a credential")
	// ErrInvalidURLLocator names a persisted locator (url+<url>#sha256:<hex>) that
	// does not parse back into its parts; only a hand-edited cache or record
	// reaches it.
	ErrInvalidURLLocator = errors.New("invalid url source locator")
	// ErrURLCredentialInvalid names a GO_GALAXY_URL_<ID>_* binding this tool
	// refuses: a bad or repeated id, a malformed _URL, a missing _TOKEN, or two ids
	// on one URL. The message names the variable, never its value.
	ErrURLCredentialInvalid = errors.New("invalid url credential configuration")
	// ErrRoleTarballLayout reports a role tarball that does not resolve to exactly
	// one role: no meta/main.yml at the root or under a single top-level
	// directory, or one under several. Usage class.
	ErrRoleTarballLayout = errors.New("role tarball does not contain exactly one role")
	// ErrRoleTarballEntryInvalid names a tarball entry with a control rune or a
	// backslash. The extractor tolerates it, but the repacked role artifact would
	// fail this tool's own name rules, so it is refused at the read.
	ErrRoleTarballEntryInvalid = errors.New("role tarball entry is invalid")

	// ErrURLCollectionVersionMismatch reports a url artifact built as one version
	// while the entry asserts another. Resolution class: the URL's one candidate is
	// not the version asked for.
	ErrURLCollectionVersionMismatch = errors.New("url collection version does not match the requested version")

	// ErrURLArtifactIdentityMismatch reports a refetched pinned url collection
	// naming a different namespace, name or version than the pin. Integrity class.
	ErrURLArtifactIdentityMismatch = errors.New("refetched url artifact is not the pinned collection")
	// ErrURLArtifactSHA256Mismatch reports a pinned role's URL now serving bytes
	// with another sha256. A role artifact is repacked before storage, so this
	// names the origin bytes; collections use ErrSHA256Mismatch.
	ErrURLArtifactSHA256Mismatch = errors.New("url artifact does not match its pinned sha256")
)
