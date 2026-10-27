package collections_test

// This file pins, through the public Lock, that --collection narrows lock
// --refresh to named Galaxy collections and their locked deps, --check and
// --dry-run too, refusing unlocked names and unloadable files.

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
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

const (
	selectionLibV110   = "1.1.0"
	selectionLibV210   = "2.1.0"
	selectionAppNeeds  = ">=1.0.0"
	selectionAnnounced = "Setting aside the pins of acme.app, acme.lib"
	selectionPrefix    = "Setting aside the pins of "
)

// The Printer methods a tierPrinter tells apart.
const (
	tierPrint   = "Printf"
	tierPersist = "PersistentPrintf"
	tierOk      = "Okf"
	tierUpdate  = "Updatef"
	tierError   = "Errorf"
	tierWarn    = "Warnf"
	tierDebug   = "Debugf"
)

// tierLine is one printed line: the Printer method that printed it, its text,
// and how many requests had been made since the printer was built.
type tierLine struct {
	tier     string
	text     string
	requests int
}

// tierPrinter records every line in print order with its tier and the request
// count at that moment, so a test can place a line in the run.
type tierPrinter struct {
	requests func() int
	lines    []tierLine
	base     int
	mu       sync.Mutex
}

// newTierPrinter counts requests from what requests reports now.
func newTierPrinter(requests func() int) *tierPrinter {
	return &tierPrinter{requests: requests, base: requests()}
}

func (p *tierPrinter) Printf(format string, args ...any) { p.recordf(tierPrint, format, args...) }
func (p *tierPrinter) PersistentPrintf(format string, args ...any) {
	p.recordf(tierPersist, format, args...)
}
func (p *tierPrinter) Okf(format string, args ...any) { p.recordf(tierOk, format, args...) }
func (p *tierPrinter) OkVersionf(version, format string, args ...any) {
	p.record(tierOk, renderVersionLine(version, "", format, args...))
}
func (p *tierPrinter) Updatef(format string, args ...any) { p.recordf(tierUpdate, format, args...) }
func (p *tierPrinter) Errorf(format string, args ...any)  { p.recordf(tierError, format, args...) }
func (p *tierPrinter) ErrorVersionf(version, cause, format string, args ...any) {
	p.record(tierError, renderVersionLine(version, cause, format, args...))
}
func (p *tierPrinter) Warnf(format string, args ...any)  { p.recordf(tierWarn, format, args...) }
func (p *tierPrinter) Debugf(format string, args ...any) { p.recordf(tierDebug, format, args...) }
func (p *tierPrinter) DebugSincef(_ time.Time, format string, args ...any) {
	p.recordf(tierDebug, format, args...)
}

func (p *tierPrinter) recordf(tier, format string, args ...any) {
	p.record(tier, fmt.Sprintf(format, args...))
}

func (p *tierPrinter) record(tier, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, tierLine{tier: tier, text: text, requests: p.requests() - p.base})
}

// matching returns, in print order, the lines on tier ("" for any) whose text
// contains substr.
func (p *tierPrinter) matching(tier, substr string) []tierLine {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []tierLine
	for _, line := range p.lines {
		if (tier == "" || line.tier == tier) && strings.Contains(line.text, substr) {
			out = append(out, line)
		}
	}
	return out
}

// assertAnnounced fails unless the run's one announcement is want, printed on
// the report tier before any request; "" wants no announcement at all.
func assertAnnounced(t *testing.T, p *tierPrinter, want string) {
	t.Helper()
	got := p.matching("", selectionPrefix)
	if want == "" {
		if len(got) != 0 {
			t.Fatalf("announced %+v, want nothing", got)
		}
		return
	}
	if len(got) != 1 || got[0].tier != tierPersist || got[0].text != want {
		t.Fatalf("announced %+v, want one %s line %q", got, tierPersist, want)
	}
	if got[0].requests != 0 {
		t.Fatalf("announced after %d requests, want before the first", got[0].requests)
	}
}

// printTiers puts a tierPrinter counting f's Galaxy requests in front of f.
func printTiers(f *e2eFixture) *tierPrinter {
	printer := newTierPrinter(f.server.Total)
	f.runtime.Output = printer
	return printer
}

// lockTwoRoots locks newE2EFixture's acme.app and acme.lib beside a second
// root, acme.other, all at 1.0.0.
func lockTwoRoots(t *testing.T) *e2eFixture {
	t.Helper()
	f := newE2EFixture(t)
	f.server.AddVersion("acme", "other", testVersion100, nil)
	writeRequirementsMulti(t, f.cfg.RequirementsFile, "acme.app", "acme.other")
	assertLockedVersions(t, lockAndLoad(t, f), map[string]string{
		"acme.app": testVersion100, "acme.lib": testVersion100, "acme.other": testVersion100,
	})
	return f
}

// publishAppV200 publishes acme.app 2.0.0, which asks acme.lib for what 1.0.0
// asks.
func publishAppV200(f *e2eFixture) {
	f.server.AddVersion("acme", "app", e2eVersion200, map[string]string{"acme.lib": selectionAppNeeds})
}

// lockTwoRootsThenPublish is lockTwoRoots once acme.app and acme.other have
// each published 2.0.0, returning the bytes locked.
func lockTwoRootsThenPublish(t *testing.T) (*e2eFixture, []byte) {
	t.Helper()
	f := lockTwoRoots(t)
	before := sourceLockBytes(t, f.cfg.RequirementsFile)
	publishAppV200(f)
	f.server.AddVersion("acme", "other", e2eVersion200, nil)
	return f, before
}

// TestLockRefreshMovesOnlyTheNamedCollection pins that --refresh --collection
// acme.app takes acme.app's newer release and keeps acme.other's, which plain
// --refresh moves too, on the cache that locked them and on an empty one.
func TestLockRefreshMovesOnlyTheNamedCollection(t *testing.T) {
	t.Parallel()
	for _, state := range cacheStates() {
		for _, tc := range []struct {
			want      map[string]string
			name      string
			announced string
			selected  []string
		}{
			{
				name: "acme.app named", selected: []string{"acme.app"}, announced: selectionAnnounced,
				want: map[string]string{"acme.app": e2eVersion200, "acme.lib": testVersion100, "acme.other": testVersion100},
			},
			{
				name: "plain refresh",
				want: map[string]string{"acme.app": e2eVersion200, "acme.lib": testVersion100, "acme.other": e2eVersion200},
			},
		} {
			t.Run(state.name+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				f, _ := lockTwoRootsThenPublish(t)
				if state.fresh {
					f.cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
				}
				printer := printTiers(f)
				f.cfg.Refresh, f.cfg.RefreshCollections = true, tc.selected

				assertLockedVersions(t, lockAndLoad(t, f), tc.want)
				assertAnnounced(t, printer, tc.announced)
			})
		}
	}
}

// TestLockRefreshReleasesTheDependenciesOfANamedCollection pins the closure
// over galaxy.lock's deps: naming acme.app moves acme.lib with it, while
// naming acme.lib alone moves it and keeps acme.app.
func TestLockRefreshReleasesTheDependenciesOfANamedCollection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		want     map[string]string
		name     string
		selected string
	}{
		{name: "the root named", selected: "acme.app", want: map[string]string{"acme.app": e2eVersion200, "acme.lib": selectionLibV110}},
		{name: "its dependency named", selected: "acme.lib", want: map[string]string{"acme.app": testVersion100, "acme.lib": selectionLibV110}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := lockFixtureThenAdd(t, false, func(f *e2eFixture) {
				f.server.AddVersion("acme", "lib", selectionLibV110, nil)
				publishAppV200(f)
			}, "acme.app")
			f.cfg.Refresh, f.cfg.RefreshCollections = true, []string{tc.selected}

			assertLockedVersions(t, lockAndLoad(t, f), tc.want)
		})
	}
}

// TestLockCheckRefreshJudgesOnlyTheSelection pins lock --check --refresh
// --collection acme.app: a newer acme.other is no drift, a newer acme.app is,
// and the file is left as it was either way.
func TestLockCheckRefreshJudgesOnlyTheSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		appNewer  bool
		wantDrift bool
	}{
		{name: "only acme.other newer"},
		{name: "acme.app newer too", appNewer: true, wantDrift: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := lockTwoRoots(t)
			before := sourceLockBytes(t, f.cfg.RequirementsFile)
			f.server.AddVersion("acme", "other", e2eVersion200, nil)
			if tc.appNewer {
				publishAppV200(f)
			}
			f.cfg.Check, f.cfg.Refresh, f.cfg.RefreshCollections = true, true, []string{"acme.app"}

			err := collections.Lock(context.Background(), f.cfg, f.runtime)
			if tc.wantDrift && !errors.Is(err, helpers.ErrLockfileDrift) {
				t.Errorf("Lock --check = %v, want ErrLockfileDrift", err)
			}
			if !tc.wantDrift && err != nil {
				t.Errorf("Lock --check = %v, want nil", err)
			}
			assertSourceLockBytes(t, f.cfg.RequirementsFile, before)
		})
	}
}

// TestLockDryRunRefreshReportsOnlyTheSelection pins lock --dry-run --refresh
// --collection acme.app once acme.app and acme.other both published 2.0.0:
// acme.app alone would change, and the file is left as it was.
func TestLockDryRunRefreshReportsOnlyTheSelection(t *testing.T) {
	t.Parallel()
	f, before := lockTwoRootsThenPublish(t)
	printer := printTiers(f)
	f.cfg.DryRun, f.cfg.Refresh, f.cfg.RefreshCollections = true, true, []string{"acme.app"}

	if err := collections.Lock(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Lock --dry-run: %v", err)
	}
	lines := printer.matching(tierOk, "Would ")
	changes := make([]string, 0, len(lines))
	for _, line := range lines {
		changes = append(changes, line.text)
	}
	if len(changes) != 1 || !strings.HasPrefix(changes[0], "Would update: acme.app (version 1.0.0 -> 2.0.0") {
		t.Errorf("reported %q, want acme.app's update alone", changes)
	}
	if got := printer.matching(tierPersist, "1 would be updated, 0 would be removed, 2 unchanged"); len(got) != 1 {
		t.Errorf("summary lines %+v, want one counting acme.app updated and two unchanged", got)
	}
	assertSourceLockBytes(t, f.cfg.RequirementsFile, before)
}

// TestLockRefreshNoCacheKeepsTheUnnamedPins pins that --refresh --no-cache
// --collection acme.app keeps acme.other's pin; a kept url pin is downloaded
// again rather than replayed, as TestLockRefreshNoCacheFetchesKeptSourcesAgain pins.
func TestLockRefreshNoCacheKeepsTheUnnamedPins(t *testing.T) {
	t.Parallel()
	f, _ := lockTwoRootsThenPublish(t)
	f.cfg.NoCache, f.cfg.Refresh, f.cfg.RefreshCollections = true, true, []string{"acme.app"}

	assertLockedVersions(t, lockAndLoad(t, f), map[string]string{
		"acme.app": e2eVersion200, "acme.lib": testVersion100, "acme.other": testVersion100,
	})
}

// pathRecorder records the path of every request its client makes.
type pathRecorder struct {
	rt    http.RoundTripper
	paths []string
	mu    sync.Mutex
}

func (r *pathRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	return r.rt.RoundTrip(req)
}

// under returns, in request order, the recorded paths containing segment.
func (r *pathRecorder) under(segment string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, path := range r.paths {
		if strings.Contains(path, segment) {
			out = append(out, path)
		}
	}
	return out
}

// TestLockRefreshAsksOnlyTheCollectionDocumentOfAKeptPin pins the cost of a
// pin a narrowed lock --refresh keeps over the cache that locked it: one
// collection document request, and no version document or artifact.
func TestLockRefreshAsksOnlyTheCollectionDocumentOfAKeptPin(t *testing.T) {
	t.Parallel()
	f, _ := lockTwoRootsThenPublish(t)
	recorder := &pathRecorder{rt: f.server.Client().Transport}
	f.runtime = infra.New(noopPrinter{}, &http.Client{Transport: recorder})
	f.cfg.Refresh, f.cfg.RefreshCollections = true, []string{"acme.app"}

	assertLockedVersions(t, lockAndLoad(t, f), map[string]string{
		"acme.app": e2eVersion200, "acme.lib": testVersion100, "acme.other": testVersion100,
	})
	asked := slices.Concat(recorder.under("/acme/other/"), recorder.under("acme-other-"))
	if want := []string{"/api/v3/collections/acme/other/"}; !slices.Equal(asked, want) {
		t.Errorf("requests for the kept acme.other = %q, want %q", asked, want)
	}
}

// TestLockRefreshRefusesANameTheLockfileDoesNotPin pins that --collection
// naming no galaxy.lock entry exits 2 before any request and leaves the file,
// with --offline or --check as without, announcing nothing.
func TestLockRefreshRefusesANameTheLockfileDoesNotPin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		offline, check bool
	}{
		{name: "refresh"},
		{name: "refresh offline", offline: true},
		{name: "check refresh", check: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newE2EFixture(t)
			lockAndLoad(t, f)
			before := sourceLockBytes(t, f.cfg.RequirementsFile)
			publishAppV200(f)
			f.server.ResetCounts()
			printer := printTiers(f)
			f.cfg.Refresh, f.cfg.Offline, f.cfg.Check = true, tc.offline, tc.check
			f.cfg.RefreshCollections = []string{"acme.app", "acme.typo"}

			err := collections.Lock(context.Background(), f.cfg, f.runtime)
			if !errors.Is(err, helpers.ErrRefreshSelectionNotLocked) || exitcode.FromError(err) != exitcode.ExitUsage {
				t.Fatalf("Lock = %v (exit %d), want ErrRefreshSelectionNotLocked, exit %d",
					err, exitcode.FromError(err), exitcode.ExitUsage)
			}
			if msg := err.Error(); !strings.Contains(msg, "collection acme.typo") || strings.Contains(msg, "collection acme.app") {
				t.Errorf("error %q, want it to name acme.typo alone", msg)
			}
			if got := f.server.Total(); got != 0 {
				t.Errorf("server saw %d requests, want none", got)
			}
			assertAnnounced(t, printer, "")
			assertSourceLockBytes(t, f.cfg.RequirementsFile, before)
		})
	}
}

// TestLockRefreshSelectionRefusesALockfileThatDoesNotLoad pins a selection over
// a galaxy.lock that does not load: exit 2 before any request, with no warning,
// no announcement and the file left, under --check as without.
func TestLockRefreshSelectionRefusesALockfileThatDoesNotLoad(t *testing.T) {
	t.Parallel()
	for _, damage := range []struct {
		apply func(string) string
		name  string
	}{
		{name: "an older schema", apply: func(s string) string {
			return strings.Replace(s, "schema_version: 5", "schema_version: 4", 1)
		}},
		{name: "merge conflict markers", apply: func(s string) string {
			return "<<<<<<< HEAD\n" + s + "=======\n>>>>>>> theirs\n"
		}},
	} {
		for _, check := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, check %t", damage.name, check), func(t *testing.T) {
				t.Parallel()
				f := newE2EFixture(t)
				lockAndLoad(t, f)
				path := lockfile.ResolveDefaultPath(f.cfg.RequirementsFile, "")
				damaged := []byte(damage.apply(string(sourceLockBytes(t, f.cfg.RequirementsFile))))
				if err := os.WriteFile(path, damaged, helpers.FileMod); err != nil {
					t.Fatalf("write lockfile: %v", err)
				}
				f.server.ResetCounts()
				printer := printTiers(f)
				f.cfg.Check, f.cfg.Refresh, f.cfg.RefreshCollections = check, true, []string{"acme.app"}

				err := collections.Lock(context.Background(), f.cfg, f.runtime)
				if !errors.Is(err, helpers.ErrRefreshSelectionLockfileUnreadable) || exitcode.FromError(err) != exitcode.ExitUsage {
					t.Fatalf("Lock = %v (exit %d), want ErrRefreshSelectionLockfileUnreadable, exit %d",
						err, exitcode.FromError(err), exitcode.ExitUsage)
				}
				if got := printer.matching(tierWarn, ""); len(got) != 0 {
					t.Errorf("warned %+v, want nothing", got)
				}
				if got := f.server.Total(); got != 0 {
					t.Errorf("server saw %d requests, want none", got)
				}
				assertAnnounced(t, printer, "")
				assertSourceLockBytes(t, f.cfg.RequirementsFile, damaged)
			})
		}
	}
}

// TestLockRefreshSelectionWithoutALockfileResolvesEverything pins that with no
// galaxy.lock there is no pin to keep or name to refuse: the run resolves
// every collection anew, as plain --refresh does.
func TestLockRefreshSelectionWithoutALockfileResolvesEverything(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	publishAppV200(f)
	f.cfg.Refresh, f.cfg.RefreshCollections = true, []string{"acme.typo"}

	assertLockedVersions(t, lockAndLoad(t, f), map[string]string{"acme.app": e2eVersion200, "acme.lib": testVersion100})
}
