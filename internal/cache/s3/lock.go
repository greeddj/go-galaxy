package s3

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// lockTiming holds the distributed lock's timing parameters. It lives on
// Backend so tests can shrink every interval; New fills it from the lock*
// constants.
type lockTiming struct {
	// ttl is the lifetime a lock holder is granted before another acquirer
	// may consider it dead and reclaim it.
	ttl time.Duration
	// heartbeatInterval is how often a live holder refreshes the lock
	// object's deadline in the background.
	heartbeatInterval time.Duration
	// heartbeatOpTimeout bounds each individual heartbeat HEAD/PUT pair.
	heartbeatOpTimeout time.Duration
	// releaseTimeout bounds the release path's own S3 calls, which run on a
	// fresh context independent of the (possibly already-canceled) context
	// the caller originally acquired the lock with.
	releaseTimeout time.Duration
	// waitCeiling bounds the total time acquireLock contends before giving
	// up with errS3LockWaitTimeout or errS3LockWaitNoHolderObserved (see
	// waitCeilingErr).
	waitCeiling time.Duration
	// backoffBase and backoffCap bound the full-jitter exponential backoff
	// between failed acquisition attempts.
	backoffBase time.Duration
	backoffCap  time.Duration
}

// lockRecord mirrors the lock object's X-Amz-Meta-* headers in its body for a
// human reading the object. The protocol never parses it back: only the
// metadata headers, read via HEAD, are authoritative.
type lockRecord struct {
	Token    string `json:"token"`
	Deadline string `json:"deadline"`
	Owner    string `json:"owner"`
	Updated  string `json:"updated"`
}

// acquireLock returns a holder context, canceled with errS3LockLost when the
// heartbeat sees a foreign token, and its release. The context derives from
// ctx, not the wait ceiling, which would kill any run holding the lock past it.
func (b *Backend) acquireLock(ctx context.Context, key string) (context.Context, func() error, error) {
	holderCtx, holderCancel := context.WithCancelCause(ctx)
	release, err := b.acquireLockLoop(ctx, key, holderCancel)
	if err != nil {
		holderCancel(err)
		return nil, nil, err
	}
	return holderCtx, release, nil
}

// acquireLockLoop creates the lock object with If-None-Match: * or reclaims
// one whose recorded deadline has passed, retrying until the wait ceiling.
// holderCancel is only passed through to the heartbeat that claim starts.
func (b *Backend) acquireLockLoop(ctx context.Context, key string, holderCancel context.CancelCauseFunc) (func() error, error) {
	waitCtx, cancel := context.WithTimeout(ctx, b.lock.waitCeiling)
	defer cancel()

	// One token identifies this entire acquisition attempt (including any
	// retries and the eventual reclaim), so verifyOwner and the heartbeat
	// consistently recognize writes made by this call.
	token, err := generateLockToken()
	if err != nil {
		return nil, err
	}

	// consecutiveRetryNow bounds immediate retries, so a backend answering the
	// create with 412 while HEAD reports no object cannot spin PUT+HEAD with no
	// backoff for the whole wait.
	var consecutiveRetryNow int

	// observedHolder is set once any attempt saw another acquirer holding the
	// lock and is never reset: it makes an eventual ceiling contention rather
	// than an unreachable backend (see waitCeilingErr).
	var observedHolder bool

	for attempt := 0; ; attempt++ {
		res, err := b.tryAcquireOnce(waitCtx, key, token, holderCancel)
		observedHolder = observedHolder || res.observed
		switch {
		case err != nil:
			return nil, acquireLockAttemptErr(ctx, waitCtx, observedHolder, err)
		case res.release != nil:
			return res.release, nil
		case res.retryNow:
			consecutiveRetryNow++
			if consecutiveRetryNow <= maxImmediateLockRetries {
				continue
			}
			// The budget is spent, which means a misbehaving backend: fall
			// through to the ordinary backoff sleep instead of spinning.
		default:
			consecutiveRetryNow = 0
		}
		if err := b.lockBackoff(ctx, waitCtx, attempt, observedHolder); err != nil {
			return nil, err
		}
	}
}

// acquireLockAttemptErr classifies an attempt's error: once waitCtx is done it
// goes through waitCeilingErr, since the ceiling can fire inside an in-flight
// call; while waitCtx is live the error is the attempt's own and returned as is.
func acquireLockAttemptErr(ctx, waitCtx context.Context, observed bool, err error) error {
	if waitCtx.Err() != nil {
		return waitCeilingErr(ctx, observed, err)
	}
	return err
}

// lockAttempt is what one create-or-reclaim step learned; the zero value
// proves nothing. release and a non-nil error must never both be set: the loop
// checks err first and would leak the heartbeat and the lock object.
type lockAttempt struct {
	release  func() error // non-nil once this call holds the lock
	retryNow bool         // the object vanished between our PUT and the HEAD; retry with no backoff
	observed bool         // the backend answered that another acquirer has it
}

// tryAcquireOnce makes one create-if-absent attempt and reclaims on 412. A
// failed claim or a PUT error other than 412 or 409 abandons the object the PUT
// may have written; a 412 or 409 proves it wrote nothing.
func (b *Backend) tryAcquireOnce(
	opCtx context.Context, key, token string, holderCancel context.CancelCauseFunc,
) (lockAttempt, error) {
	deadline := time.Now().UTC().Add(b.lock.ttl)
	putErr := b.putLock(opCtx, key, token, deadline, putCondition{ifNoneMatch: true})
	if putErr == nil {
		// claim itself decides whether this counts as an observation of
		// another acquirer (a foreign token raced in ahead of us) or a
		// successful claim; either way it is this step's whole answer.
		attempt, err := b.claim(opCtx, key, token, holderCancel)
		if err != nil {
			//nolint:contextcheck // abandonLockObject builds its own fresh context instead of
			// taking opCtx, and must: opCtx dying is one of the ways the claim above fails, so a
			// cleanup running on opCtx would fail for the very reason it was needed.
			b.abandonLockObject(key, token)
		}
		return attempt, err
	}
	if errors.Is(putErr, errS3ConditionalConflict) {
		// 409: a concurrent request beat this create, and S3 documents the write
		// as not applied and safe to retry, so back off. Not observed: the
		// conflict names a request, not necessarily another acquirer.
		return lockAttempt{}, nil
	}
	if !errors.Is(putErr, errS3PreconditionFailed) {
		// A PUT failing without a 412 may still have landed, so it is abandoned;
		// no test reaches it, since the fake cannot lose an applied PUT's reply.
		//nolint:contextcheck // abandonLockObject builds its own fresh context instead of
		// taking opCtx for the same reason as the arm above.
		b.abandonLockObject(key, token)
		return lockAttempt{}, putErr
	}
	return b.reclaimIfExpired(opCtx, key, token, holderCancel)
}

// reclaimIfExpired HEADs the existing object and, once its deadline passed,
// takes it over with If-Match on that HEAD's ETag so only one reclaimer wins.
// An expired holder is not an observation; a live holder or a lost swap is.
func (b *Backend) reclaimIfExpired(
	opCtx context.Context, key, token string, holderCancel context.CancelCauseFunc,
) (lockAttempt, error) {
	headers, headErr := b.client.headObject(opCtx, key)
	switch {
	case errors.Is(headErr, errS3NotFound):
		// The holder released (or its create raced with a delete) right
		// after our precondition failure: retry the create immediately.
		return lockAttempt{retryNow: true}, nil
	case headErr != nil:
		return lockAttempt{}, headErr
	}

	if !lockExpired(headers, b.lock.ttl) {
		// The holder is still live: back off and retry later. This is the
		// primary contention signal.
		return lockAttempt{observed: true}, nil
	}

	etag := strings.TrimSpace(headers.Get("ETag"))
	if etag == "" {
		// No ETag leaves nothing to condition the swap on: refuse rather than
		// overwrite unconditionally, which would take the lock unarbitrated.
		return lockAttempt{}, errS3CompareAndSwapUnsupported
	}

	reclaimDeadline := time.Now().UTC().Add(b.lock.ttl)
	putErr := b.putLock(opCtx, key, token, reclaimDeadline, putCondition{ifMatch: etag})
	if putErr == nil {
		attempt, err := b.claim(opCtx, key, token, holderCancel)
		if err != nil {
			//nolint:contextcheck // abandonLockObject builds its own fresh context instead of
			// taking opCtx, and must: opCtx dying is one of the ways the claim above fails, so a
			// cleanup running on opCtx would fail for the very reason it was needed.
			b.abandonLockObject(key, token)
		}
		return attempt, err
	}
	if errors.Is(putErr, errS3ConditionalConflict) {
		// A concurrent request beat this swap to the object; see
		// tryAcquireOnce's own arm for why that is a lost race to back off
		// from rather than a failure, and why it is not an observation.
		return lockAttempt{}, nil
	}
	if errors.Is(putErr, errS3NotFound) {
		// 404: a releasing holder deleted the object after our HEAD, so retry
		// the create at once. The remote answered, so nothing is abandoned.
		return lockAttempt{retryNow: true}, nil
	}
	if !errors.Is(putErr, errS3PreconditionFailed) {
		// A PUT failing without a 412 may still have landed, so it is abandoned;
		// no test reaches it, since the fake cannot lose an applied PUT's reply.
		//nolint:contextcheck // abandonLockObject builds its own fresh context instead of
		// taking opCtx for the same reason as the arm above.
		b.abandonLockObject(key, token)
		return lockAttempt{}, putErr
	}
	// 412: another writer changed the object between our HEAD and swap, so it
	// is an observed acquirer; the refusal proves we wrote nothing to abandon.
	return lockAttempt{observed: true}, nil
}

// claim confirms the object just written still carries token and, if so,
// starts the heartbeat, handing it holderCancel. A foreign token counts as
// observing another acquirer.
func (b *Backend) claim(
	opCtx context.Context, key, token string, holderCancel context.CancelCauseFunc,
) (lockAttempt, error) {
	ours, err := b.verifyOwner(opCtx, key, token)
	if err != nil {
		return lockAttempt{}, err
	}
	if !ours {
		return lockAttempt{observed: true}, nil
	}
	//nolint:contextcheck // startHeartbeat's release closure deliberately builds its own fresh
	// context instead of reusing opCtx: opCtx may already be canceled by the time release runs.
	return lockAttempt{release: b.startHeartbeat(key, token, holderCancel)}, nil
}

// verifyOwner reports whether the lock object records token. An absent token
// header is a mismatch, never a transient failure: treating it as transient
// would let a run keep writing under a lock it may no longer hold.
func (b *Backend) verifyOwner(ctx context.Context, key, token string) (bool, error) {
	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		return false, err
	}
	return headers.Get("X-Amz-Meta-Token") == token, nil
}

// putLock writes the lock's four metadata headers plus a JSON mirror body.
// cond picks create-if-absent (acquire), If-Match on a HEAD's ETag (reclaim)
// or an unconditional overwrite (the heartbeat's refresh under our token).
func (b *Backend) putLock(ctx context.Context, key, token string, deadline time.Time, cond putCondition) error {
	now := time.Now().UTC().Format(time.RFC3339)
	owner := lockOwner()
	deadlineStr := deadline.Format(time.RFC3339)

	meta := map[string]string{
		"token":    token,
		"deadline": deadlineStr,
		"owner":    owner,
		"updated":  now,
	}
	body, err := json.Marshal(lockRecord{
		Token:    token,
		Deadline: deadlineStr,
		Owner:    owner,
		Updated:  now,
	})
	if err != nil {
		return err // unreachable for this fixed four-string struct; kept as a defensive guard.
	}
	reader := bytes.NewReader(body)
	return b.client.putObject(ctx, key, reader, int64(len(body)),
		putObjectAttrs{contentType: "application/json", meta: meta}, cond)
}

// lockOwner identifies this process for the lock object's diagnostic Owner
// field; it is never parsed or compared, only ever displayed.
func lockOwner() string {
	host, _ := os.Hostname()
	return host + "/" + strconv.Itoa(os.Getpid())
}

// generateLockToken returns a fresh 32-hex-character token for one
// acquisition. A crypto/rand failure is errS3TokenGeneration, never a fallback
// to a weaker source.
func generateLockToken() (string, error) {
	buf := make([]byte, lockTokenBytes)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", fmt.Errorf("%w: %w", errS3TokenGeneration, err)
	}
	return hex.EncodeToString(buf), nil
}

// waitCeilingErr classifies an ended wait: a parent error passes unchanged,
// then observed beats inFlight, since the ceiling hits a request as often as a
// sleep. inFlight is %v, never %w, so exitcode cannot read it as an interrupt.
func waitCeilingErr(parent context.Context, observed bool, inFlight error) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if observed {
		return errS3LockWaitTimeout
	}
	if inFlight != nil {
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: %v", errS3LockWaitNoHolderObserved, inFlight)
	}
	return errS3LockWaitNoHolderObserved
}

// lockBackoff sleeps one full-jitter backoff interval, or returns
// waitCeilingErr with no in-flight error once waitCtx expires.
func (b *Backend) lockBackoff(parent, waitCtx context.Context, attempt int, observed bool) error {
	timer := time.NewTimer(helpers.BackoffDelay(b.lock.backoffBase, b.lock.backoffCap, attempt))
	defer timer.Stop()
	select {
	case <-waitCtx.Done():
		return waitCeilingErr(parent, observed, nil)
	case <-timer.C:
		return nil
	}
}

// startHeartbeat refreshes the lock's deadline on its own background context
// until release; on a foreign token it marks the lock lost and cancels the
// holder context. The returned release runs on a fresh timeout context.
func (b *Backend) startHeartbeat(key, token string, holderCancel context.CancelCauseFunc) func() error {
	hbCtx, hbCancel := context.WithCancel(context.Background())
	var lost atomic.Bool
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(b.lock.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if !b.heartbeatTick(hbCtx, key, token, &lost, holderCancel) {
					return
				}
			}
		}
	}()

	return func() error {
		hbCancel()
		// Join before holderCancel: a mid-decision tick must record its loss
		// cause first, and first-cancel-wins keeps it over the nil below.
		// TestReleaseRacingTheTickKeepsTheLossCause pins this order.
		<-done
		// The holder context always ends here, on the ordinary release path
		// too, so a clean release does not leave a child context hanging off
		// the caller's own for the rest of the process's life.
		holderCancel(nil)
		if lost.Load() {
			return errS3LockLost
		}
		// A fresh context: the caller's may already be canceled, and releasing
		// on it would leave the lock object blocking others until its TTL.
		relCtx, cancel := context.WithTimeout(context.Background(), b.lock.releaseTimeout)
		defer cancel()
		return b.releaseLock(relCtx, key, token)
	}
}

// heartbeatTick refreshes the deadline and returns false only on a token
// mismatch, setting lost (read by release) and canceling the holder context
// (read by the run); transient errors return true for the next tick to retry.
func (b *Backend) heartbeatTick(
	hbCtx context.Context, key, token string, lost *atomic.Bool, holderCancel context.CancelCauseFunc,
) bool {
	opCtx, cancel := context.WithTimeout(hbCtx, b.lock.heartbeatOpTimeout)
	defer cancel()

	ours, err := b.verifyOwner(opCtx, key, token)
	if err != nil {
		return true
	}
	if !ours {
		lost.Store(true)
		holderCancel(errS3LockLost)
		return false
	}
	// A failure here is transient (e.g. a dropped connection): the next
	// tick will retry with a deadline computed at that later time.
	_ = b.putLock(opCtx, key, token, time.Now().UTC().Add(b.lock.ttl), putCondition{})
	return true
}

// releaseLock deletes the lock object only if a HEAD still sees token on it; a
// missing object is already released. The delete is unconditional, the one
// unarbitrated write left, bounded by the heartbeat's token check.
func (b *Backend) releaseLock(ctx context.Context, key, token string) error {
	headers, err := b.client.headObject(ctx, key)
	if errors.Is(err, errS3NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if headers.Get("X-Amz-Meta-Token") != token {
		return nil
	}
	return b.client.deleteObject(ctx, key)
}

// abandonLockObject is the best-effort, token-guarded release run when an
// acquisition errs after its PUT, so an object with no heartbeat does not block
// others for a lockTTL. It never calls holderCancel: acquireLock sets the cause.
func (b *Backend) abandonLockObject(key, token string) {
	// A fresh context: a dead acquisition context is one way this is reached,
	// and it would fail the cleanup for the very reason it was needed.
	ctx, cancel := context.WithTimeout(context.Background(), b.lock.releaseTimeout)
	defer cancel()
	_ = b.releaseLock(ctx, key, token)
}

// lockExpired reports whether the lock may be reclaimed: the recorded deadline
// decides, else Last-Modified age against ttl, else yes, so a corrupt lock
// object can never block acquisition forever.
func lockExpired(headers http.Header, ttl time.Duration) bool {
	now := time.Now().UTC()
	if dl := strings.TrimSpace(headers.Get("X-Amz-Meta-Deadline")); dl != "" {
		if t, err := time.Parse(time.RFC3339, dl); err == nil {
			return now.After(t)
		}
		// malformed deadline -> do not hard-fail; fall back to age-based staleness
	}
	if lm := strings.TrimSpace(headers.Get("Last-Modified")); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			return now.Sub(t) > ttl
		}
	}
	return true // uninterpretable timing -> reclaimable, never a permanent block
}
