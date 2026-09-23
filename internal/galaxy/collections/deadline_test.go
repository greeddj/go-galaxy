package collections

// Pins artifactDeadlineError's classification in isolation and the cache-hit
// fetchArtifact arm's deadline end to end through installCollection, with a
// blocking Fetch stub standing in for a slow S3 object read.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// artifactDeadlineBudget is the budget every artifactDeadlineError case passes;
// only its presence in the rendered message matters.
const artifactDeadlineBudget = time.Second

// errTestArtifactDeadlineCause wraps context.DeadlineExceeded as a stalled
// client.Do does, so a %w rendering of the cause would make errors.Is match it
// and steal the sentinel's exit-code classification.
var errTestArtifactDeadlineCause = fmt.Errorf("client.Do: %w", context.DeadlineExceeded)

// artifactDeadlineErrorCase is one artifactDeadlineErrorCases table row.
type artifactDeadlineErrorCase struct {
	buildParent func() (context.Context, context.CancelFunc)
	buildDl     func(parent context.Context) (context.Context, context.CancelFunc)
	err         error
	name        string
	wantSame    bool
}

// artifactDeadlineErrorCases is TestArtifactDeadlineErrorClassification's
// table, hoisted to keep the test function within the length budget.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var artifactDeadlineErrorCases = []artifactDeadlineErrorCase{
	{
		name: "this run's own deadline fired while the parent is still live",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err: errTestArtifactDeadlineCause,
	},
	{
		// A watchdog cancel overtaken by the budget: the only row proving the
		// normalized error does not match context.Canceled.
		name: "this run's own deadline fired while the parent is still live, cause wraps context.Canceled",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err: fmt.Errorf("body read: %w", context.Canceled),
	},
	{
		// ErrSHA256Mismatch keeps its identity when the budget expired in the
		// same instant: it is ExitIntegrity and prepareWithRecovery's eviction
		// trigger.
		name: "own deadline fired while parent is live, cause is an artifact integrity failure: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      fmt.Errorf("%w: aaaa != bbbb", helpers.ErrSHA256Mismatch),
		wantSame: true,
	},
	{
		// ErrCacheBackendUnusable keeps its identity too: it is ExitUsage (no
		// retry helps), and relabeling it ExitNetwork would let a remote that
		// stalls to the budget choose the class.
		name: "own deadline fired while parent is live, cause is an unusable backend: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      fmt.Errorf("%w: endpoint answered with a redirect", helpers.ErrCacheBackendUnusable),
		wantSame: true,
	},
	{
		name: "parent explicitly canceled: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			return parent, cancel
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, artifactDeadlineBudget)
		},
		err:      context.Canceled,
		wantSame: true,
	},
	{
		// dlCtx inherits DeadlineExceeded from an expired parent, so only the
		// parent.Err() guard tells the caller's expiry from this acquisition's.
		name: "parent's own deadline (not this acquisition's budget) expired first: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			<-parent.Done()
			return parent, cancel
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, artifactDeadlineBudget)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      errTestArtifactDeadlineCause,
		wantSame: true,
	},
	{
		name: "dlCtx still live: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, time.Hour)
		},
		err:      errTestArtifactDeadlineCause,
		wantSame: true,
	},
	{
		name: "nil error stays nil",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      nil,
		wantSame: true,
	},
}

// TestArtifactDeadlineErrorClassification pins artifactDeadlineError's
// normalization table in isolation, independent of any HTTP or cache
// plumbing.
func TestArtifactDeadlineErrorClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range artifactDeadlineErrorCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent, parentCancel := tc.buildParent()
			defer parentCancel()
			dlCtx, dlCancel := tc.buildDl(parent)
			defer dlCancel()

			got := artifactDeadlineError(parent, dlCtx, artifactDeadlineBudget, tc.err)
			if tc.wantSame {
				assertArtifactDeadlineErrorUnchanged(t, got, tc.err)
				return
			}
			assertArtifactDeadlineErrorNormalized(t, got, tc.err)
		})
	}
}

// TestArtifactDeadlineErrorIsIdempotent pins that re-normalizing an already
// normalized error leaves its message unchanged, since call sites normalize
// inside a retry attempt and again around the whole helpers.Retry loop.
func TestArtifactDeadlineErrorIsIdempotent(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	dlCtx, dlCancel := context.WithTimeout(parent, time.Nanosecond)
	defer dlCancel()
	<-dlCtx.Done()

	once := artifactDeadlineError(parent, dlCtx, artifactDeadlineBudget, errTestArtifactDeadlineCause)
	twice := artifactDeadlineError(parent, dlCtx, artifactDeadlineBudget, once)

	if twice.Error() != once.Error() {
		t.Fatalf("re-normalizing changed the message: once=%q twice=%q", once.Error(), twice.Error())
	}
	if n := strings.Count(twice.Error(), helpers.ErrArtifactDownloadDeadline.Error()); n != 1 {
		t.Fatalf("sentinel text appears %d times in %q, want exactly 1 (idempotency must not double-wrap)", n, twice.Error())
	}
}

// assertArtifactDeadlineErrorUnchanged fails the test unless got is want,
// returned verbatim by artifactDeadlineError (nil stays nil).
func assertArtifactDeadlineErrorUnchanged(t *testing.T, got, want error) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("artifactDeadlineError = %v, want nil", got)
		}
		return
	}
	if !errors.Is(got, want) {
		t.Fatalf("artifactDeadlineError = %v, want unchanged %v", got, want)
	}
}

// assertArtifactDeadlineErrorNormalized fails the test unless got matches the
// deadline sentinel, matches neither context error (the %v-not-%w contract),
// and its message contains cause's text.
func assertArtifactDeadlineErrorNormalized(t *testing.T, got, cause error) {
	t.Helper()
	if !errors.Is(got, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("artifactDeadlineError = %v, want errors.Is ErrArtifactDownloadDeadline", got)
	}
	if errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("artifactDeadlineError = %v, must not match context.DeadlineExceeded", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("artifactDeadlineError = %v, must not match context.Canceled", got)
	}
	if !strings.Contains(got.Error(), cause.Error()) {
		t.Fatalf("artifactDeadlineError = %q, want it to contain the cause %q", got.Error(), cause.Error())
	}
}

// blockingFetchArtifacts wraps a real local.Artifacts store; with blocking set,
// Fetch waits for ctx to end like a dripped S3 read that never completes.
// fetchCalls and deleteCalls count every call.
type blockingFetchArtifacts struct {
	*local.Artifacts

	blocking    bool
	fetchCalls  atomic.Int32
	deleteCalls atomic.Int32
}

// Fetch blocks on ctx until it is done and returns ctx.Err() when blocking is
// set; otherwise it delegates to the real local store.
func (a *blockingFetchArtifacts) Fetch(ctx context.Context, key string) (cacheManager.ArtifactFile, error) {
	a.fetchCalls.Add(1)
	if a.blocking {
		<-ctx.Done()
		return cacheManager.ArtifactFile{}, ctx.Err()
	}
	return a.Artifacts.Fetch(ctx, key)
}

// Delete counts every call before delegating to the real local store.
func (a *blockingFetchArtifacts) Delete(ctx context.Context, key string) error {
	a.deleteCalls.Add(1)
	return a.Artifacts.Delete(ctx, key)
}

// newDeadlineTestFixture builds a collection and installDeps over stub. Only
// Has() must report the key for prepareInstall to take the cache-hit path, so
// the seeded bytes' content is irrelevant while Fetch is stubbed.
func newDeadlineTestFixture(
	t *testing.T,
	cacheDir string,
	stub *blockingFetchArtifacts,
	deadline time.Duration,
) (collection, installDeps) {
	t.Helper()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}

	cfg := &config.Config{CacheDir: cacheDir, Workers: 1}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	runtime.ArtifactDownloadDeadline = deadline
	st := store.New()

	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      stub,
		root:           newTestCollectionsRoot(t, filepath.Join(t.TempDir(), "install")),
	}
	return col, deps
}

// TestCachedArtifactFetchHonorsTheDownloadDeadline pins that a cache-hit Fetch
// is bounded by the acquisition deadline and that the deadline never triggers
// prepareWithRecovery's eviction (Delete stays at zero).
func TestCachedArtifactFetchHonorsTheDownloadDeadline(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	stub := &blockingFetchArtifacts{Artifacts: local.NewArtifacts(cacheDir), blocking: true}
	col, deps := newDeadlineTestFixture(t, cacheDir, stub, 20*time.Millisecond)
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("stand-in for a cached artifact whose read never completes"))

	err := installCollection(context.Background(), col, deps, nil, nil, downloadResult{})
	if err == nil {
		t.Fatal("expected an error once the download deadline fired, got nil")
	}
	// Call counts come before the error shape: a widened recovery arm evicts
	// and then fails at metadata resolution, which would mask the Delete count.
	if got := stub.fetchCalls.Load(); got != 1 {
		t.Fatalf("Fetch calls = %d, want 1", got)
	}
	if got := stub.deleteCalls.Load(); got != 0 {
		t.Fatalf("Delete calls = %d, want 0 (the deadline is outside the evict-and-refetch recovery class)", got)
	}
	if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected errors.Is ErrArtifactDownloadDeadline, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
}

// TestCachedArtifactFetchDeadlinePositiveControl is the same fixture unblocked:
// the stub serves a normal cache hit and the 20ms deadline alone fails nothing.
func TestCachedArtifactFetchDeadlinePositiveControl(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	stub := &blockingFetchArtifacts{Artifacts: local.NewArtifacts(cacheDir), blocking: false}
	col, deps := newDeadlineTestFixture(t, cacheDir, stub, 20*time.Millisecond)
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("stand-in for a real cached artifact, unpacked by installCollection below"))

	// The seeded bytes are no tar.gz, so this calls fetchArtifact, the exact
	// call the deadline wraps, rather than the extracting installCollection.
	_, err := fetchArtifact(context.Background(), deps, col, nil, true, true)
	if err != nil {
		t.Fatalf("expected the unblocked stub to delegate to the real local store, got %v", err)
	}
	if got := stub.fetchCalls.Load(); got != 1 {
		t.Fatalf("Fetch calls = %d, want 1", got)
	}
}
