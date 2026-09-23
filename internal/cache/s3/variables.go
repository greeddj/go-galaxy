package s3

import (
	"errors"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Each sentinel below carries at most one helpers cache-backend class, chosen
// by where the failure was discovered: Unavailable (the remote answered or went
// silent), Unusable (a guarantee is missing), Busy (a holder was observed).
var (
	errS3LockLost                 = fmt.Errorf("%w: s3 lock ownership was lost to another holder", helpers.ErrCacheLockLost)
	errS3LockWaitTimeout          = fmt.Errorf("%w: s3 lock wait ceiling exceeded", helpers.ErrCacheBusy)
	errS3LockWaitNoHolderObserved = fmt.Errorf(
		"%w: s3 lock wait ceiling elapsed without ever observing a lock holder",
		helpers.ErrCacheBackendUnavailable,
	)
	errS3TokenGeneration           = errors.New("s3 lock token generation failed")
	errS3NotFound                  = errors.New("s3 object not found")
	errS3BucketNotFound            = fmt.Errorf("%w: s3 bucket not found", helpers.ErrCacheBackendUnavailable)
	errS3BucketEmpty               = errors.New("s3 bucket is empty")
	errS3BucketHeadFailed          = fmt.Errorf("%w: s3 bucket head is failed", helpers.ErrCacheBackendUnavailable)
	errS3CreateBucketFailed        = fmt.Errorf("%w: s3 create bucket failed", helpers.ErrCacheBackendUnavailable)
	errS3BucketRequestFailed       = fmt.Errorf("%w: s3 bucket request failed", helpers.ErrCacheBackendUnavailable)
	errS3PreconditionFailed        = errors.New("s3 precondition failed")
	errS3HTTPClientNil             = errors.New("s3 http client is nil")
	errS3InvalidEndpoint           = fmt.Errorf("%w: s3 invalid endpoint", helpers.ErrCacheBackendUnusable)
	errS3TransportFailed           = fmt.Errorf("%w: s3 request failed", helpers.ErrCacheBackendUnavailable)
	errS3GetFailed                 = fmt.Errorf("%w: s3 get object failed", helpers.ErrCacheBackendUnavailable)
	errS3HeadFailed                = fmt.Errorf("%w: s3 head object failed", helpers.ErrCacheBackendUnavailable)
	errS3PutFailed                 = fmt.Errorf("%w: s3 put object failed", helpers.ErrCacheBackendUnavailable)
	errS3DeleteFailed              = fmt.Errorf("%w: s3 delete object failed", helpers.ErrCacheBackendUnavailable)
	errS3ClientNil                 = errors.New("s3 client is nil")
	errArtifactSHA256Mismatch      = errors.New("s3 artifact sha256 mismatch")
	errS3ConditionalPutUnsupported = fmt.Errorf(
		"%w: s3 backend does not enforce conditional PUT (If-None-Match); distributed locking cannot guarantee mutual exclusion",
		helpers.ErrCacheBackendUnusable,
	)
	errS3ConditionalConflict = fmt.Errorf(
		"%w: s3 conditional write conflicted with a concurrent request",
		helpers.ErrCacheBackendUnavailable,
	)
	errS3CompareAndSwapUnsupported = fmt.Errorf(
		"%w: s3 backend does not support compare-and-swap PUT (If-Match against an ETag); "+
			"an expired lock holder cannot be reclaimed safely",
		helpers.ErrCacheBackendUnusable,
	)
	errS3RedirectRefused = fmt.Errorf("%w: s3 endpoint answered with a redirect", helpers.ErrCacheBackendUnusable)
)

const (
	emptySHA256     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	statePrefix     = "state"
	artifactsPrefix = "artifacts"
	locksPrefix     = "locks"
	storeObject     = "store.json.gz"
	projectsObject  = "projects.json"
	lockObject      = "cache.lock"
	peekBytes       = 2
	headerLength    = 2

	// s3ErrorBodyLimit bounds how much of a non-2xx response body is read
	// when looking for an S3 XML <Error> document, so a misbehaving or
	// unexpectedly large error body cannot force an unbounded read.
	s3ErrorBodyLimit = 8 << 10

	// conditionalProbeObject is the base name of Open's conditional-write probe
	// under the locks prefix; a per-process random suffix keeps it clear of a
	// stale probe a crashed run left behind.
	conditionalProbeObject = ".conditional-probe"

	// staleProbeETag is an entity tag no backend can have minted, so Open's
	// compare-and-swap probe can expect a refusal without first producing a
	// genuinely superseded ETag.
	staleProbeETag = `"go-galaxy-stale-probe-etag"`

	// lockTokenBytes is the number of random bytes read from crypto/rand to
	// build a lock token; hex-encoded, this yields a 32-character token.
	lockTokenBytes = 16

	// lockTTL is how long a holder is granted before another acquirer may
	// reclaim it. A holder killed by a fatal error keeps the bucket locked this
	// long; shortening it would let a live but slow holder be reclaimed.
	lockTTL = 10 * time.Minute
	// heartbeatInterval is how often a live holder refreshes the lock
	// object's deadline in the background.
	heartbeatInterval = 3 * time.Minute
	// heartbeatOpTimeout bounds each individual heartbeat HEAD/PUT pair so a
	// stalled S3 call cannot delay the next tick indefinitely.
	heartbeatOpTimeout = 30 * time.Second
	// lockReleaseTimeout bounds the release path's own S3 calls on a fresh
	// context.
	lockReleaseTimeout = 30 * time.Second
	// lockWaitCeiling bounds acquireLock's whole contention; waitCeilingErr
	// decides which sentinel reports its expiry.
	lockWaitCeiling = 5 * time.Minute
	// lockBackoffBase and lockBackoffCap bound the full-jitter exponential
	// backoff between failed acquisition attempts.
	lockBackoffBase = 250 * time.Millisecond
	lockBackoffCap  = 5 * time.Second

	// maxImmediateLockRetries bounds consecutive no-backoff retryNow handoffs,
	// so a backend answering 412 while HEAD reports the object missing cannot
	// drive a PUT+HEAD spin up to the wait ceiling.
	maxImmediateLockRetries = 8

	// s3RetryMaxAttempts bounds attempts of an idempotent S3 verb. A verb with
	// no budget of its own (headBucket, Artifacts.Has, ClearFiles) can wait this
	// many attempt costs plus backoff; a budgeted one is cut short by its ctx.
	s3RetryMaxAttempts = 4
	// s3RetryBackoffBase and s3RetryBackoffCap bound the full-jitter
	// exponential backoff between retried attempts of an idempotent S3 verb.
	s3RetryBackoffBase = 200 * time.Millisecond
	s3RetryBackoffCap  = 5 * time.Second
)
