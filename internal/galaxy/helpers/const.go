package helpers

import (
	"io/fs"
	"net/http"
	"time"
)

const (
	// DirMod is the default permission for created directories.
	DirMod = 0o755
	// FileMod is the default permission for created files.
	FileMod = 0o644
	// WritePermBits is the owner, group and other write mask ReadOnlyPerm clears,
	// applied only as archive.extractRegularFile creates a file: the inode is then
	// hard-linked into every install, so a later chmod would alias into all of them.
	WritePermBits = 0o222

	// ExtractMarkerPrefix starts the marker name, suffixed with the artifact
	// sha256, that a finished extraction writes in a collection's version .info
	// directory or a role's directory; verifyExtractMarker also checks a tally.
	ExtractMarkerPrefix = ".extract-done."

	// CollectionNameParts is the expected number of parts in a collection name like "namespace.collection".
	CollectionNameParts = 2

	// CacheLatestMetadataTTL is the TTL for cached metadata before revalidation.
	CacheLatestMetadataTTL = 10 * time.Minute

	// ArchiveMaxEntrySize caps the size one tar header DECLARES, charged for every
	// header before its typeflag is dispatched. A hostile header can understate
	// what archive/tar reads; ArchiveMaxDecompressedSize bounds the bytes.
	ArchiveMaxEntrySize = int64(512 << 20) // 512 MiB per file
	// ArchiveMaxTotalSize caps the declared header sizes summed over one archive:
	// a cheap refusal before a body byte is read, not a bound on what the tar
	// reader consumes.
	ArchiveMaxTotalSize = int64(4 << 30) // 4 GiB per archive
	// ArchiveMaxDecompressedSize caps every byte pulled out of an archive's gzip
	// reader, tar framing included: the real decompression-bomb bound. It grants
	// no framing headroom, which would only fund uncounted meta-header chains.
	ArchiveMaxDecompressedSize = ArchiveMaxTotalSize
	// ArchiveMaxEntryCount caps the headers tar.Reader.Next may return for one
	// archive, any typeflag: it refuses a tarbomb of zero-byte directories or
	// hardlinks that no size cap trips, and sits far above any real collection.
	ArchiveMaxEntryCount = int64(100_000)
	// ArchiveMaxEntryNameLen caps one entry's name and link target in bytes. It
	// bounds what the manifest chain scan RETAINS until the stream ends, which
	// archive/tar alone would let reach gigabytes; 1024 is darwin's PATH_MAX.
	ArchiveMaxEntryNameLen = 1024
	// ArchiveProbeMaxBytes caps the decompressed bytes archive.ProbeTarGz reads
	// before ErrArtifactTarHeaderNotFound. It must clear 4,196,352, what archive/tar
	// may read before its first header; re-derive it whenever archive/tar changes.
	ArchiveProbeMaxBytes = int64(5 << 20) // 5 MiB

	// ArtifactMaxDownloadSize caps the compressed on-the-wire bytes of one
	// artifact download, bounding the temp file a server-chosen URL can fill even
	// when extraction is skipped; a real artifact is far smaller.
	ArtifactMaxDownloadSize = ArchiveMaxTotalSize

	// GitPackMaxSize caps the compressed bytes a git fetch may write to its
	// object store. It is far below ArtifactMaxDownloadSize because go-git
	// inflates every object into memory while indexing, before any cap of ours.
	GitPackMaxSize = int64(512 << 20) // 512 MiB

	// GitErrorBodyMaxSize caps a non-2xx body on the git HTTP client: go-git
	// reads it whole to compose its error and the pack cap never sees it, so an
	// endless error body would otherwise grow the process until the deadline.
	GitErrorBodyMaxSize = int64(64 << 10) // 64 KiB

	// GitTreeMaxDepth caps how deep a git tree may nest before it is refused, so
	// the walk's recursion is bounded by this tool rather than by the remote.
	GitTreeMaxDepth = 64

	// BuildMetadataMaxBytes caps a metadata file a builder reads from a source
	// tree (galaxy.yml, MANIFEST.json, meta/main.yml, meta/requirements.yml)
	// before it is decoded.
	BuildMetadataMaxBytes = 1 << 20

	// RoleVersionsMaxPages caps the Galaxy v1 version pages (50 tags each) walked
	// for one role; past it the walk is refused rather than silently cut short.
	RoleVersionsMaxPages = 20

	// RoleGraphMaxRoles caps the roles one run may discover through
	// requirements.yml and dependencies, bounding a chain of repositories.
	RoleGraphMaxRoles = 1000

	// ArtifactDownloadDeadline bounds one artifact acquisition end to end,
	// retries and backoff included, so a byte-drip that never trips --timeout
	// still ends. Never configurable: Infra's field exists only for tests.
	ArtifactDownloadDeadline = 15 * time.Minute

	// StateObjectMaxCompressedSize caps the on-the-wire bytes read for a cache
	// state object (the S3 snapshot and the project registry), bounding what a
	// malicious or corrupt object can buffer into memory.
	StateObjectMaxCompressedSize = int64(256 << 20) // 256 MiB
	// StateObjectMaxDecompressedSize caps the inflated size of a gzip-encoded
	// cache-state object, so a high-ratio gzip bomb that stays under the
	// compressed ceiling cannot expand without bound.
	StateObjectMaxDecompressedSize = int64(1 << 30) // 1 GiB

	// StateObjectDeadline bounds one Backend state operation end to end, S3
	// retries included. It runs under the lock, so it must stay inside the S3
	// lock timings TestStateObjectDeadlineFitsInsideTheLockTimings pins.
	StateObjectDeadline = 1 * time.Minute

	// MetadataMaxSize caps the bytes read for one Galaxy API metadata response.
	// Real documents are KB to low MB (a 10,000-version list is about 1.5 MB),
	// so this refuses a hostile body without refusing a legitimate one.
	MetadataMaxSize = int64(16 << 20) // 16 MiB

	// MetadataFetchDeadline bounds one Galaxy metadata request end to end,
	// retries included, against a byte-drip; the versions-list page loop shares
	// one budget. Never configurable: Infra's field exists only for tests.
	MetadataFetchDeadline = 2 * time.Minute

	// SignatureFetchDeadline bounds one collection's whole signature phase, all
	// sources together. Its gatherer must bound each source itself, or the first
	// source in a server- or file-chosen list starves the rest.
	SignatureFetchDeadline = 1 * time.Minute

	// SignatureMaxSize caps the bytes read for one signature blob, never the set:
	// a verifier must release each blob before pulling the next, or
	// MaxSignaturesPerCollection blobs per worker stay resident.
	SignatureMaxSize = int64(1 << 20) // 1 MiB

	// MaxSignaturesPerCollection caps the signature blobs gathered for one
	// collection, server-offered and configured together: the server's list
	// crosses the snapshot trust boundary and would otherwise be unbounded.
	MaxSignaturesPerCollection = 64

	// DefaultRequiredValidSignatureCount is ansible's default count spec, used as
	// the flag default and as the config fallback for a command that never
	// registers the flag, whose empty spec ParseCountSpec would refuse.
	DefaultRequiredValidSignatureCount = "1"

	// ManifestFileName is the archive-relative name of a collection's manifest,
	// the one document a collection signature is made over.
	ManifestFileName = "MANIFEST.json"
	// FilesManifestFileName is the per-file digest list MANIFEST.json names, the
	// second link of the chain a collection signature covers.
	FilesManifestFileName = "FILES.json"
	// FilesManifestMaxBytes caps the size FILES.json's tar header may declare,
	// since that entry alone is buffered whole: it bounds the allocation in
	// manifest.recordFilesManifest. A policy value, not headroom over a maximum.
	FilesManifestMaxBytes = int64(32 << 20) // 32 MiB
	// ManifestScanMaxBytes caps the decompressed bytes a scan for
	// ManifestFileName reads before ErrManifestNotFound; it also bounds the
	// scan's entries, since each tar header is 512 bytes.
	ManifestScanMaxBytes = int64(64 << 20) // 64 MiB

	// S3ListMaxSize caps the bytes read for one ListObjectsV2 page, about 16x a
	// 1000-key page, so an oversized page is refused while a large bucket is
	// still read page by page.
	S3ListMaxSize = int64(16 << 20) // 16 MiB

	// ArtifactSHASidecarSuffix names the sha256 sidecar beside a locally cached
	// tarball. A non-pinned cache hit may trust it; a frozen (pinned) hit never
	// does and always hashes the bytes.
	ArtifactSHASidecarSuffix = ".sha256"

	// ArtifactDownloadTempPrefix prefixes the local store's in-flight temp files,
	// a download or a sha256 sidecar; one outliving its run is a dead-run orphan,
	// and this is the exact string both the dead-run and --clear-cache sweeps match.
	ArtifactDownloadTempPrefix = ".download-"

	// FetchDefaultTimeout is --timeout's default: a no-progress budget on the
	// response headers and on each body read, never a cap on a whole transfer,
	// so a large artifact streams for as long as it makes progress.
	FetchDefaultTimeout = 30 * time.Second
	// FetchDialContextTimeout is the dial timeout for outbound connections.
	FetchDialContextTimeout = 10 * time.Second
	// FetchDialContextKeepAlive is the TCP keep-alive for dials.
	FetchDialContextKeepAlive = 30 * time.Second
	// FetchForceAttemptHTTP2 enables HTTP/2 attempts when possible.
	FetchForceAttemptHTTP2 = true
	// FetchMaxIdleConns is the maximum number of idle connections.
	FetchMaxIdleConns = 256
	// FetchMaxIdleConnsPerHost limits idle connections per host.
	// Single Galaxy server is the common case, so keep it generous to match
	// worker concurrency and avoid TCP churn under high parallelism.
	FetchMaxIdleConnsPerHost = 64
	// FetchIdleConnTimeout is the idle connection timeout.
	FetchIdleConnTimeout = 30 * time.Second
	// FetchTLSHandshakeTimeout is the TLS handshake timeout.
	FetchTLSHandshakeTimeout = 3 * time.Second
	// FetchExpectContinueTimeout is the expect-continue timeout.
	FetchExpectContinueTimeout = 1 * time.Second
	// FetchMaxRedirects bounds the redirect hops of one request. It equals
	// net/http's own default, so fetch.checkRedirect imposing it by hand changes
	// nothing about how far a server can bounce a request.
	FetchMaxRedirects = 10

	// FetchRetryMaxAttempts bounds how many times a Galaxy API GET or an
	// artifact download is attempted before its last failure is returned as
	// final.
	FetchRetryMaxAttempts = 4
	// FetchRetryBackoffBase bounds the initial full-jitter backoff between
	// retried attempts of a Galaxy API GET or artifact download.
	FetchRetryBackoffBase = 200 * time.Millisecond
	// FetchRetryBackoffCap bounds the full-jitter backoff ceiling between
	// retried attempts of a Galaxy API GET or artifact download.
	FetchRetryBackoffCap = 5 * time.Second

	// MinDefaultInstallWorkers floors DefaultInstallWorkers so one stalled
	// acquisition cannot serialize the run, and floors MaxAcceptedInstallWorkers
	// with it so the two never disagree on a single permitted CPU.
	MinDefaultInstallWorkers = 2
	// MaxDefaultInstallWorkers caps DefaultInstallWorkers by memory: a worker
	// holds one pgzip reader (4 x 1 MiB blocks) at a time, so 16 fill a 64 MiB
	// budget, and ext4 was still gaining at 12 workers.
	MaxDefaultInstallWorkers = 16

	// DownloadWorkersPerCPU multiplies the permitted CPU count in
	// DefaultDownloadWorkers: a download or presence probe mostly waits on the
	// network, unlike an install worker, which extracts a tree.
	DownloadWorkersPerCPU = 4
	// MinDefaultDownloadWorkers floors DefaultDownloadWorkers' result so a
	// single- or dual-core CI runner still gets meaningful download
	// concurrency instead of one artifact acquisition at a time.
	MinDefaultDownloadWorkers = 8
	// MaxDefaultDownloadWorkers caps DefaultDownloadWorkers at half of
	// FetchMaxIdleConnsPerHost, so the idle pool keeps every worker's connection
	// between requests. It bounds the default only, not --download-workers.
	MaxDefaultDownloadWorkers = 32

	// StoreSnapshotSchemaVersion is the snapshot schema version. Bump it for any
	// change, additive included: an older binary sharing a cache must fail with
	// ErrUnsupportedSchemaVersion rather than drop buckets it does not know.
	StoreSnapshotSchemaVersion = 9

	// CacheEntryMaxAge is the retention window for persisted cache entries; one
	// last written (or, for API entries, revalidated) longer ago is pruned at
	// persist time, bounding the Bolt file and the S3 object in size and age.
	CacheEntryMaxAge = 30 * 24 * time.Hour

	// WarmedEntryMaxAge is how long cleanup protects a warmed extracted tree
	// after its last warm. It is separate from CacheEntryMaxAge: a warmed entry
	// has no project behind it, so a recent warm is its only reachability proof.
	WarmedEntryMaxAge = 30 * 24 * time.Hour

	// StoreDBLock is the cache lock file name.
	StoreDBLock = ".go-galaxy.lock"

	// BoltOpenTimeout bounds how long opening a Bolt file waits for its flock, so
	// CI fails fast instead of hanging on a lock another process holds.
	BoltOpenTimeout = 5 * time.Second

	// StoreDBProjects is the project registry filename.
	StoreDBProjects = "projects.json"

	// StoreDBLocal is the local cache database filename.
	StoreDBLocal = "go-galaxy.db"

	// StoreSnapshotMeta is the snapshot DB filename for metadata.
	StoreSnapshotMeta = "go-galaxy-meta.db"
	// StoreSnapshotAPICache is the snapshot DB filename for API cache entries.
	StoreSnapshotAPICache = "go-galaxy-api-cache.db"
	// StoreSnapshotDepsCache is the snapshot DB filename for dependency cache.
	StoreSnapshotDepsCache = "go-galaxy-deps-cache.db"
	// StoreSnapshotInstalled is the snapshot DB filename for installed collections.
	StoreSnapshotInstalled = "go-galaxy-installed.db"
	// StoreSnapshotGraph is the snapshot DB filename for dependency graph.
	StoreSnapshotGraph = "go-galaxy-graph.db"
	// StoreSnapshotRequirements is the snapshot DB filename for requirements.
	StoreSnapshotRequirements = "go-galaxy-requirements.db"
	// StoreSnapshotRoots is the snapshot DB filename for root collections.
	StoreSnapshotRoots = "go-galaxy-roots.db"
	// StoreSnapshotResolved is the snapshot DB filename for resolved collections.
	StoreSnapshotResolved = "go-galaxy-resolved.db"
	// StoreSnapshotVersions is the snapshot DB filename for versions cache.
	StoreSnapshotVersions = "go-galaxy-versions.db"

	// StoreBucketMeta is the bucket name for snapshot metadata.
	StoreBucketMeta = "meta"
	// StoreBucketAPICache is the bucket name for API cache entries.
	StoreBucketAPICache = "api_cache"
	// StoreBucketDepsCache is the bucket name for dependency cache.
	StoreBucketDepsCache = "deps_cache"
	// StoreBucketInstalled is the bucket name for installed collections.
	StoreBucketInstalled = "installed"
	// StoreBucketGraph is the bucket name for dependency graph.
	StoreBucketGraph = "graph"
	// StoreBucketRequirements is the bucket name for requirements.
	StoreBucketRequirements = "requirements"
	// StoreBucketResolved is the bucket name for resolved collections.
	StoreBucketResolved = "resolved"
	// StoreBucketVersions is the bucket name for versions cache.
	StoreBucketVersions = "versions_cache"
	// StoreBucketWarmed is the bucket name for warmed extracted entries.
	StoreBucketWarmed = "warmed"
	// StoreBucketGitPins is the bucket name for git source pins: the commit a
	// (url, ref, subdir) requirement resolved to and the collections it held.
	StoreBucketGitPins = "git_pins"
	// StoreBucketInstalledRoles is the bucket name for installed roles, keyed
	// by install name: where each was materialized and from which artifact.
	StoreBucketInstalledRoles = "installed_roles"
	// StoreBucketRolePins is the bucket name for role pins: the repository,
	// commit and version a role requirement line resolved to.
	StoreBucketRolePins = "role_pins"
	// StoreBucketURLPins is the bucket name for url source pins: the sha256
	// and collection identity a url requirement's tarball resolved to.
	StoreBucketURLPins = "url_pins"

	// StoreMetaSchemaVersion is the metadata key for the snapshot schema version.
	StoreMetaSchemaVersion = "schema_version"
	// StoreMetaLastSnapshot is the metadata key for the last snapshot time.
	StoreMetaLastSnapshot = "last_snapshot"
	// StoreMetaContentRecorded is the metadata key for the last save that
	// recorded on-disk content; see store.Store.HasRecordedContent.
	StoreMetaContentRecorded = "content_recorded"
	// StoreMetaRequirementsHash is the metadata key for the requirements hash.
	StoreMetaRequirementsHash = "requirements_hash"
	// StoreMetaServer is the metadata key for the Galaxy server.
	StoreMetaServer = "server"
)

// FetchRetryPolicy is the fixed retry policy shared by every Galaxy API GET
// and artifact download: bounded attempts with full-jitter exponential
// backoff, not configurable per call site.
func FetchRetryPolicy() RetryPolicy {
	return RetryPolicy{Base: FetchRetryBackoffBase, Cap: FetchRetryBackoffCap, MaxAttempts: FetchRetryMaxAttempts}
}

// DefaultInstallWorkers derives the install and warm pool's default from
// procs, clamped to [MinDefaultInstallWorkers, MaxDefaultInstallWorkers].
// procs is the permitted CPU, runtime.GOMAXPROCS(0), never runtime.NumCPU().
func DefaultInstallWorkers(procs int) int {
	return min(max(procs, MinDefaultInstallWorkers), MaxDefaultInstallWorkers)
}

// MaxAcceptedInstallWorkers is the largest --workers value accepted when procs
// CPUs are permitted, floored at MinDefaultInstallWorkers, since past the
// permitted CPU extracting workers contend. It must not bound cfg.DownloadWorkers.
func MaxAcceptedInstallWorkers(procs int) int {
	return max(procs, MinDefaultInstallWorkers)
}

// DefaultDownloadWorkers derives the download and presence-probe pool's
// default: procs, as in DefaultInstallWorkers, times DownloadWorkersPerCPU,
// clamped to [MinDefaultDownloadWorkers, MaxDefaultDownloadWorkers].
func DefaultDownloadWorkers(procs int) int {
	return min(max(procs*DownloadWorkersPerCPU, MinDefaultDownloadWorkers), MaxDefaultDownloadWorkers)
}

// ReadOnlyPerm strips every write bit from perm, for regular files only: a
// read-only directory blocks entry creation and os.RemoveAll, and os.Chmod on
// a symlink follows it and changes the target instead.
func ReadOnlyPerm(perm fs.FileMode) fs.FileMode {
	return perm &^ fs.FileMode(WritePermBits)
}

// IsRetryableHTTPStatus reports whether status is a transient failure safe to
// retry on an idempotent request. It is the program's single definition, so no
// subsystem may keep a private copy that could drift.
func IsRetryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
