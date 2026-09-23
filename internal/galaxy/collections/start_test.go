package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// capturingPrinter is an output.Printer stub recording each output tier in its
// own slice, so a test can assert a line landed on the expected tier rather
// than being swallowed or emitted on the wrong one.
type capturingPrinter struct {
	noopPrinter

	prints   []string
	persists []string
	warns    []string
	debugs   []string
	oks      []string
	updates  []string
	errs     []string
	mu       sync.Mutex
}

func (p *capturingPrinter) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prints = append(p.prints, fmt.Sprintf(format, args...))
}

// Okf records a success-tier line, the same tier canSkipInstall's
// "Installed:"/"Cached:" lines use and classifyDryRun's "Would install:"
// lines share.
func (p *capturingPrinter) Okf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.oks = append(p.oks, fmt.Sprintf(format, args...))
}

// OkVersionf records a success-tier line into the same slice Okf does, so an
// assertion that nothing reached the success tier covers both methods.
func (p *capturingPrinter) OkVersionf(version, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.oks = append(p.oks, renderVersionLine(version, "", format, args...))
}

// Updatef records an update-tier line (intact, but something newer exists) in
// its own slice, so an "up to date" assertion never matches its opposite.
func (p *capturingPrinter) Updatef(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates = append(p.updates, fmt.Sprintf(format, args...))
}

// ErrorVersionf records an error-tier line into the same slice Errorf does,
// for the reason OkVersionf shares its own with Okf.
func (p *capturingPrinter) ErrorVersionf(version, cause, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, renderVersionLine(version, cause, format, args...))
}

// PersistentPrintf records a result-tier line: output that must survive even
// in quiet mode (see the Printer interface doc comment for the tier split).
func (p *capturingPrinter) PersistentPrintf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persists = append(p.persists, fmt.Sprintf(format, args...))
}

func (p *capturingPrinter) Warnf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warns = append(p.warns, fmt.Sprintf(format, args...))
}

// Errorf records an error-tier line, the tier classifyDryRun's "Would fail:"
// lines share with a real install's own "Failed:" lines.
func (p *capturingPrinter) Errorf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, fmt.Sprintf(format, args...))
}

func (p *capturingPrinter) Debugf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.debugs = append(p.debugs, fmt.Sprintf(format, args...))
}

// hasPrintContaining reports whether any recorded Printf line contains substr.
func (p *capturingPrinter) hasPrintContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.prints {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasPersistentPrintContaining reports whether any recorded PersistentPrintf
// line contains substr.
func (p *capturingPrinter) hasPersistentPrintContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.persists {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasUpdateContaining reports whether any recorded Updatef line contains
// substr.
func (p *capturingPrinter) hasUpdateContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.updates {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasWarnContaining reports whether any recorded Warnf line contains substr.
func (p *capturingPrinter) hasWarnContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.warns {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasDebugContaining reports whether any recorded Debugf line contains substr.
func (p *capturingPrinter) hasDebugContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.debugs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasOkContaining reports whether any recorded Okf line contains substr.
func (p *capturingPrinter) hasOkContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.oks {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasErrContaining reports whether any recorded Errorf line contains substr.
func (p *capturingPrinter) hasErrContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.errs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// okLines returns a snapshot copy of every recorded Okf line, in recording
// order - used by tests asserting classifyDryRun's per-collection report
// order rather than just membership.
func (p *capturingPrinter) okLines() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.oks))
	copy(out, p.oks)
	return out
}

// TestSweepDeadRunTempsIsBestEffort pins that a failing SweepTemp is logged as
// a warning and never aborts the install, and that a nil extracted store does
// not panic.
func TestSweepDeadRunTempsIsBestEffort(t *testing.T) {
	t.Parallel()

	// A regular file as cacheDir makes SweepTemp's directory read fail with a
	// real, non-not-exist error.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	backend := local.New(notADir)
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	sweepDeadRunTemps(context.Background(), runtime, backend, nil)

	if !printer.hasWarnContaining("Failed to sweep leftover download temps") {
		t.Fatalf("expected a download-temp sweep warning to be recorded, got %v", printer.warns)
	}
}

// callLogBackend is a cacheManager.Backend recording only its Close call. The
// embedded interface is nil on purpose: any other method unwindBackend grew a
// call to would panic rather than pass unnoticed.
type callLogBackend struct {
	cacheManager.Backend

	calls *[]string
}

// Close records the call and succeeds.
func (b callLogBackend) Close(context.Context) error {
	*b.calls = append(*b.calls, "close")
	return nil
}

// unwindCase is one row of TestUnwindBackendReleasesBeforeClose: whether a
// lock was granted, and the call sequence the unwind must produce.
type unwindCase struct {
	name      string
	wantCalls []string
	granted   bool
}

// unwindCases covers a granted lock (release, then close) and a failed Lock,
// which must still close the backend and call no release closure.
func unwindCases() []unwindCase {
	return []unwindCase{
		{name: "lock granted", granted: true, wantCalls: []string{"release", "close"}},
		{name: "lock never granted", granted: false, wantCalls: []string{"close"}},
	}
}

// TestUnwindBackendReleasesBeforeClose pins that unwindBackend releases the
// lock before closing the backend; no backend depends on the order today, so
// changing it must be deliberate.
func TestUnwindBackendReleasesBeforeClose(t *testing.T) {
	t.Parallel()

	for _, tc := range unwindCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls []string
			var release func() error
			if tc.granted {
				release = func() error {
					calls = append(calls, "release")
					return nil
				}
			}

			unwindBackend(context.Background(), callLogBackend{calls: &calls}, release)

			if !slices.Equal(calls, tc.wantCalls) {
				t.Fatalf("unwindBackend made calls %v, want %v", calls, tc.wantCalls)
			}
		})
	}
}

// newLockContentionFixture returns a config whose cache lock is already held,
// and its release. flock(2) conflicts between file descriptions, not
// processes, so holding it in-process makes initInstall's acquisition fail.
func newLockContentionFixture(t *testing.T) (*config.Config, func() error) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	release, err := store.AcquireLock(cacheDir)
	if err != nil {
		t.Fatalf("hold the cache lock: %v", err)
	}
	return &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}, release
}

// TestInitInstallLockFailureUnwindsWithNothingToRelease pins that a failed
// Lock returns ErrAnotherInstanceIsRunning with no state and no holder context,
// and that the same directory succeeds once the lock is free.
func TestInitInstallLockFailureUnwindsWithNothingToRelease(t *testing.T) {
	t.Parallel()
	cfg, release := newLockContentionFixture(t)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	lockCtx, state, err := initInstall(context.Background(), cfg, runtime)
	if err == nil {
		t.Fatalf("expected initInstall to fail while another acquirer holds the cache lock")
	}
	if !errors.Is(err, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("initInstall error = %v, want errors.Is helpers.ErrAnotherInstanceIsRunning", err)
	}
	if state != nil {
		t.Fatalf("initInstall returned a state alongside its failure: %+v", state)
	}
	if lockCtx != nil {
		t.Fatalf("initInstall returned holder context %v from an arm that never took the lock, want nil", lockCtx)
	}

	if err := release(); err != nil {
		t.Fatalf("release the held cache lock: %v", err)
	}
	assertInitInstallSucceedsOnFreeLock(t, cfg, runtime)
}

// assertInitInstallSucceedsOnFreeLock checks initInstall succeeds with a holder
// context. It reads t.Context() rather than taking a ctx, which keeps revive's
// context-as-argument rule satisfied.
func assertInitInstallSucceedsOnFreeLock(t *testing.T, cfg *config.Config, runtime *infra.Infra) {
	t.Helper()
	lockCtx, state, err := initInstall(t.Context(), cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall once the cache lock is free: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})
	if lockCtx == nil {
		t.Fatalf("initInstall returned no holder context on success")
	}
}
