package collections

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// errTestBoom stands in for a lookup failure's cause in
// TestReportOutdatedTiers. Declared as a static package-level sentinel,
// rather than an inline fmt.Errorf/errors.New call, purely to satisfy err113 -
// production code never compares against it.
var errTestBoom = errors.New("boom")

// TestOutdatedNilConfig pins the nil-config guard split out of the offline
// check: before that split, cfg == nil fell into "cfg == nil || cfg.Offline"
// and returned the misleading helpers.ErrOfflineMode - a nil config is not
// offline mode, it is a defensive-only condition that deserves its own
// sentinel. This can never happen from a production call site - every
// caller in cmd/go-galaxy/commands builds a non-nil *config.Config before
// reaching here - so this test is documentary rather than pinned against a
// reachable production state; it exists on the same convention
// server_candidates_test.go's serverCandidatesCases already follows for its
// own "cfg == nil" row.
func TestOutdatedNilConfig(t *testing.T) {
	t.Parallel()
	err := Outdated(context.Background(), nil, infra.New(noopPrinter{}, nil))
	if !errors.Is(err, helpers.ErrConfigIsNil) {
		t.Errorf("Outdated(nil config) = %v, want errors.Is helpers.ErrConfigIsNil", err)
	}
	if errors.Is(err, helpers.ErrOfflineMode) {
		t.Errorf("Outdated(nil config) = %v, must not also match helpers.ErrOfflineMode", err)
	}
}

// TestClassifyOutdated checks the locked-vs-latest comparison, including the
// two failure modes where either side does not parse as semver: these must
// be reported through Err rather than silently treated as up-to-date.
//
// Mutation: dropping classifyOutdated's err != nil branch (returning
// outdatedEntry{Name: name, Locked: locked, Latest: latest, Newer: newer}
// unconditionally, discarding isNewerVersion's error) makes the
// "locked does not parse as semver" subtest fail with
// `classifyOutdated("garbage", "2.0.0").Err is nil, want a parse-failure error`
// and the "latest does not parse as semver" subtest fail with
// `classifyOutdated("1.0.0", "garbage").Err is nil, want a parse-failure error`
// - run and confirmed.
func TestClassifyOutdated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		locked    string
		latest    string
		wantNewer bool
		wantErr   bool
	}{
		{name: "latest is newer", locked: "1.0.0", latest: "2.0.0", wantNewer: true, wantErr: false},
		{name: "locked equals latest", locked: "2.0.0", latest: "2.0.0", wantNewer: false, wantErr: false},
		{name: "locked is newer than latest", locked: "2.0.0", latest: "1.0.0", wantNewer: false, wantErr: false},
		{name: "locked does not parse as semver", locked: "garbage", latest: "2.0.0", wantNewer: false, wantErr: true},
		{name: "latest does not parse as semver", locked: "1.0.0", latest: "garbage", wantNewer: false, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entry := classifyOutdated("ns.name", tt.locked, tt.latest)
			if entry.Newer != tt.wantNewer {
				t.Errorf("classifyOutdated(%q, %q).Newer = %v, want %v", tt.locked, tt.latest, entry.Newer, tt.wantNewer)
			}
			if tt.wantErr && entry.Err == nil {
				t.Errorf("classifyOutdated(%q, %q).Err is nil, want a parse-failure error", tt.locked, tt.latest)
			}
			if !tt.wantErr && entry.Err != nil {
				t.Errorf("classifyOutdated(%q, %q).Err = %v, want nil on success", tt.locked, tt.latest, entry.Err)
			}
		})
	}
}

// TestUnhonoredFlags pins unhonoredFlags' detection rule (a boolean flag's
// configured value, never whether it was explicitly set) and its fixed
// append order, since warnUnhonoredFlags' single warning line depends on
// that order being deterministic across runs.
//
// The "none set" row is this test's positive control: it proves an empty
// result is achievable from this fixture, so the "all set" row's non-empty
// result is meaningful evidence the function actually inspected cfg rather
// than always returning the same fixed slice.
//
// Mutation: swapping the --no-cache and --refresh appends inside
// unhonoredFlags makes the "all set" row's exact-order assertion fail with
// "unhonoredFlags() = [--clear-cache --refresh --no-cache --no-deps --frozen
// --s3-bucket], want [--clear-cache --no-cache --refresh --no-deps --frozen
// --s3-bucket]" - run and confirmed.
func TestUnhonoredFlags(t *testing.T) {
	t.Parallel()
	allFlags := []string{"--clear-cache", "--no-cache", "--refresh", "--no-deps", "--frozen", "--s3-bucket"}
	tests := []struct {
		name string
		want []string
		cfg  config.Config
	}{
		{name: "none set", cfg: config.Config{}, want: nil},
		{name: "clear-cache alone", cfg: config.Config{ClearCache: true}, want: []string{"--clear-cache"}},
		{name: "no-cache alone", cfg: config.Config{NoCache: true}, want: []string{"--no-cache"}},
		{name: "refresh alone", cfg: config.Config{Refresh: true}, want: []string{"--refresh"}},
		{name: "no-deps alone", cfg: config.Config{NoDeps: true}, want: []string{"--no-deps"}},
		{name: "frozen alone", cfg: config.Config{Frozen: true}, want: []string{"--frozen"}},
		{
			name: "s3-bucket alone",
			cfg:  config.Config{S3Cache: config.S3CacheConfig{Enabled: true}},
			want: []string{"--s3-bucket"},
		},
		{
			name: "all set",
			cfg: config.Config{
				ClearCache: true,
				NoCache:    true,
				Refresh:    true,
				NoDeps:     true,
				Frozen:     true,
				S3Cache:    config.S3CacheConfig{Enabled: true},
			},
			want: allFlags,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			got := unhonoredFlags(&cfg)
			if len(got) != len(tt.want) {
				t.Fatalf("unhonoredFlags() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("unhonoredFlags() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// recordingPrinter implements output.Printer and records every call as a
// (tier, formatted message) pair, so TestReportOutdatedTiers can assert each
// of reportOutdated's four lines lands on the tier the design table
// specifies, rather than merely that some text was printed somewhere. The
// call slice lives behind a pointer so every method value (all take a value
// receiver, matching noopPrinter's own shape in lock_pin_test.go) shares one
// recording.
type recordingPrinter struct {
	calls *[]recordedCall
}

type recordedCall struct {
	tier string
	msg  string
}

func newRecordingPrinter() recordingPrinter {
	calls := make([]recordedCall, 0, 8)
	return recordingPrinter{calls: &calls}
}

func (p recordingPrinter) Printf(format string, args ...any) { p.recordf("Printf", format, args...) }
func (p recordingPrinter) PersistentPrintf(format string, args ...any) {
	p.recordf("PersistentPrintf", format, args...)
}
func (p recordingPrinter) Okf(format string, args ...any) { p.recordf("Okf", format, args...) }
func (p recordingPrinter) OkVersionf(version, format string, args ...any) {
	p.recordf("OkVersionf", "%s", renderVersionLine(version, "", format, args...))
}
func (p recordingPrinter) Updatef(format string, args ...any) { p.recordf("Updatef", format, args...) }
func (p recordingPrinter) Errorf(format string, args ...any)  { p.recordf("Errorf", format, args...) }
func (p recordingPrinter) ErrorVersionf(version, cause, format string, args ...any) {
	p.recordf("ErrorVersionf", "%s", renderVersionLine(version, cause, format, args...))
}
func (p recordingPrinter) Warnf(format string, args ...any)  { p.recordf("Warnf", format, args...) }
func (p recordingPrinter) Debugf(format string, args ...any) { p.recordf("Debugf", format, args...) }
func (p recordingPrinter) DebugSincef(_ time.Time, format string, args ...any) {
	p.recordf("DebugSincef", format, args...)
}

// recordf is unexported and placed after every exported output.Printer
// method above, per the package's function-ordering convention.
func (p recordingPrinter) recordf(tier, format string, args ...any) {
	*p.calls = append(*p.calls, recordedCall{tier: tier, msg: fmt.Sprintf(format, args...)})
}

// outdatedTierFixture is one entry of each verdict reportOutdated tells
// apart, in report order: up to date, outdated, failed.
func outdatedTierFixture() []outdatedEntry {
	return []outdatedEntry{
		{Name: "ns.current", Locked: "1.0.0", Newer: false},
		{Name: "ns.stale", Locked: "1.0.0", Latest: "2.0.0", Newer: true},
		{Name: "ns.broken", Locked: "1.0.0", Err: errTestBoom},
	}
}

// TestReportOutdatedTiers pins reportOutdated's design table under
// --verbose, the one mode that prints all four lines: an up-to-date entry
// lands on OkVersionf with its version as the tag, an outdated entry on
// Updatef, a failed lookup on Errorf, and the trailing summary on
// PersistentPrintf - never on the transient Printf tier, which --quiet would
// swallow. The three per-entry tiers are three different markers, which is
// what makes the report read as three verdicts rather than as one marked
// line and one bare one; the summary is a total about no single collection,
// so it carries none.
//
// Mutation: changing the up-to-date line from OkVersionf to PersistentPrintf
// makes the "up to date lands on OkVersionf" assertion below fail with
// `reportOutdated: up-to-date line tier = "PersistentPrintf", want
// "OkVersionf"` - run and confirmed.
func TestReportOutdatedTiers(t *testing.T) {
	t.Parallel()
	printer := newRecordingPrinter()
	runtime := infra.New(printer, nil)

	reportOutdated(runtime, outdatedTierFixture(), "/tmp/lockfile.yml", true)

	calls := *printer.calls
	if len(calls) != 4 {
		t.Fatalf("reportOutdated recorded %d calls, want 4: %+v", len(calls), calls)
	}
	if calls[0].tier != "OkVersionf" {
		t.Errorf("reportOutdated: up-to-date line tier = %q, want %q", calls[0].tier, "OkVersionf")
	}
	if want := "Up to date: ns.current == 1.0.0"; calls[0].msg != want {
		t.Errorf("reportOutdated: up-to-date line = %q, want %q", calls[0].msg, want)
	}
	if calls[1].tier != "Updatef" {
		t.Errorf("reportOutdated: outdated line tier = %q, want %q", calls[1].tier, "Updatef")
	}
	if calls[2].tier != "Errorf" {
		t.Errorf("reportOutdated: lookup-failed line tier = %q, want %q", calls[2].tier, "Errorf")
	}
	if calls[3].tier != "PersistentPrintf" {
		t.Errorf("reportOutdated: summary line tier = %q, want %q", calls[3].tier, "PersistentPrintf")
	}
	// Documentary, not pinned: a fifth recorded call would already trip the
	// len(calls) != 4 check above, and a call recorded on the Printf tier in
	// one of the four known slots would already trip that slot's own
	// tier-equality check above it - so no reachable state makes this loop the
	// first assertion to fail. It states the invariant explicitly anyway, for
	// a reader who does not want to infer "never Printf" from four positive
	// checks.
	for _, c := range calls {
		if c.tier == "Printf" {
			t.Errorf("reportOutdated must never use the transient Printf tier, got %+v", c)
		}
	}
}

// TestReportOutdatedHidesUpToDateUnlessVerbose pins that a default run
// prints only what needs attention: the outdated line, the failed lookup and
// the summary, with the up-to-date entry still counted in that summary
// rather than dropped from it.
//
// Mutation: removing the verbose guard around the up-to-date line makes the
// call-count check below fail with `reportOutdated recorded 4 calls, want 3`
// - run and confirmed.
func TestReportOutdatedHidesUpToDateUnlessVerbose(t *testing.T) {
	t.Parallel()
	printer := newRecordingPrinter()
	runtime := infra.New(printer, nil)

	reportOutdated(runtime, outdatedTierFixture(), "/tmp/lockfile.yml", false)

	calls := *printer.calls
	if len(calls) != 3 {
		t.Fatalf("reportOutdated recorded %d calls, want 3: %+v", len(calls), calls)
	}
	for i, tier := range []string{"Updatef", "Errorf", "PersistentPrintf"} {
		if calls[i].tier != tier {
			t.Errorf("reportOutdated: line %d tier = %q, want %q", i, calls[i].tier, tier)
		}
	}
	if want := "/tmp/lockfile.yml: 1 up to date, 1 outdated, 1 failed"; calls[2].msg != want {
		t.Errorf("reportOutdated: summary = %q, want %q", calls[2].msg, want)
	}
}

// TestOutdatedReportsInNameOrder pins the direction of the comparison
// Outdated sorts its results with, which no other test in this package
// reaches: TestReportOutdatedTiers hands reportOutdated an already-ordered
// slice, so it exercises the report and never the sort ahead of it.
//
// The lockfile lists the three collections in an order that is neither
// ascending nor descending, and queryLatestVersions fills its result slice by
// lockfile index, so the sort is the only thing between that order and the
// report. Workers is 1 deliberately: with a parallel pool the incoming order
// would be nondeterministic, which would let a run pass by luck rather than
// by the sort.
//
// KILLING MUTATION, run for real: swapping the comparison to
// strings.Compare(b.Name, a.Name) fails this test on the loop below, with
// `report line 0 = "Up to date: acme.gamma == 1.0.0", want a line for
// acme.alpha` - never on the length check above it, which a reordering
// leaves satisfied. Reverting the argument order made it pass again.
func TestOutdatedReportsInNameOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "requirements.yml")

	srv := fakegalaxy.New(t)
	names := []string{"alpha", "beta", "gamma"}
	entries := make([]lockfile.Entry, 0, len(names))
	// Appended gamma, alpha, beta on purpose - see the doc comment above.
	for _, i := range []int{2, 0, 1} {
		srv.AddVersion("acme", names[i], "1.0.0", nil)
		entries = append(entries, lockfile.Entry{
			Name:    "acme." + names[i],
			Version: "1.0.0",
			Source:  srv.URL(),
		})
	}
	mustWriteFile(t, reqPath, []byte("collections: []\n"))
	if err := lockfile.Save(lockfile.ResolveDefaultPath(reqPath, ""), &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        srv.URL(),
		Collections:   entries,
	}); err != nil {
		t.Fatalf("save lockfile: %v", err)
	}

	printer := &capturingPrinter{}
	// Verbose, because an up-to-date entry is reported only then.
	cfg := &config.Config{Server: srv.URL(), RequirementsFile: reqPath, Workers: 1, Verbose: true}
	if err := Outdated(context.Background(), cfg, infra.New(printer, srv.Client())); err != nil {
		t.Fatalf("Outdated: err = %v, want nil", err)
	}

	// Every collection is at its latest version, so each lands on the success
	// tier and the summary line lands elsewhere - leaving oks holding exactly the
	// per-collection lines, in report order.
	if len(printer.oks) != len(names) {
		t.Fatalf("recorded %d up-to-date lines, want %d: %v", len(printer.oks), len(names), printer.oks)
	}
	for i, name := range names {
		if !strings.Contains(printer.oks[i], "acme."+name) {
			t.Fatalf("report line %d = %q, want a line for acme.%s", i, printer.oks[i], name)
		}
	}
}

// TestOutdatedMissingLockfileClassifiesAsLockfileError is outdated's share of
// the unified verdict for a missing lockfile: the same fact that reaches
// install --frozen, warm --frozen, lock --frozen, tree and explain must reach
// this command as the same sentinel and the same exit class. It used to
// arrive here as a bare fs.ErrNotExist and land in the environment-usage
// class instead.
//
// The check runs before any network contact, so the fixture needs no server:
// the requirements file exists and the lockfile beside it does not, which is
// the only condition under test.
//
// The collections tree is named and absent, deliberately. Since outdated
// gained its fallback (outdatedInput) a missing lockfile alone no longer
// ends the run - the tree has to be missing too - so a fixture that left
// DownloadPath at its zero value would still reach this verdict, but by
// os.OpenRoot("") failing rather than by the condition this test is about.
func TestOutdatedMissingLockfileClassifiesAsLockfileError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))

	cfg := &config.Config{RequirementsFile: reqPath, DownloadPath: filepath.Join(dir, "collections")}
	err := Outdated(context.Background(), cfg, infra.New(noopPrinter{}, nil))
	if !errors.Is(err, helpers.ErrLockfileMissing) {
		t.Fatalf("Outdated with no lockfile: err = %v, want errors.Is helpers.ErrLockfileMissing", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitLock {
		t.Errorf("exitcode.FromError(err) = %d, want ExitLock (%d)", got, exitcode.ExitLock)
	}

	// Positive control on the same fixture: with a lockfile in place the
	// command gets past this gate, so the failure above is the absence of the
	// file and not the fixture failing to reach the load at all. Offline
	// stops it at the next step, which is not the sentinel under test.
	mustWriteFile(t, filepath.Join(dir, lockfile.DefaultName),
		[]byte("schema_version: 1\ncollections: []\n"))
	cfg.Offline = true
	if err := Outdated(context.Background(), cfg, infra.New(noopPrinter{}, nil)); errors.Is(err, helpers.ErrLockfileMissing) {
		t.Errorf("Outdated with a lockfile present still reported it missing: %v", err)
	}
}
