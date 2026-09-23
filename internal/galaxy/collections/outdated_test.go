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
// TestReportOutdatedTiers; a package-level sentinel only to satisfy err113.
var errTestBoom = errors.New("boom")

// TestOutdatedNilConfig pins that a nil config is refused as
// helpers.ErrConfigIsNil and not reported as helpers.ErrOfflineMode; no
// production caller passes nil, so the guard is defensive only.
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

// TestUnhonoredFlags pins that unhonoredFlags judges a flag by its configured
// value, not by whether it was set, and keeps a fixed order, so the single
// warning line warnUnhonoredFlags prints is the same on every run.
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
// (tier, formatted message) pair; the slice sits behind a pointer so every
// value-receiver method appends to one shared recording.
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

// TestReportOutdatedTiers pins reportOutdated's tiers under --verbose: up to
// date on OkVersionf, outdated on Updatef, failed on Errorf, and the summary
// on PersistentPrintf, never the transient Printf tier --quiet would swallow.
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
	// Documentary: the checks above already imply it, but "never Printf" is
	// the invariant, so it is stated outright.
	for _, c := range calls {
		if c.tier == "Printf" {
			t.Errorf("reportOutdated must never use the transient Printf tier, got %+v", c)
		}
	}
}

// TestReportOutdatedHidesUpToDateUnlessVerbose pins that a default run
// prints only what needs attention: the outdated line, the failed lookup and
// the summary, with the up-to-date entry still counted in that summary.
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

// TestOutdatedReportsInNameOrder pins that Outdated reports in ascending name
// order from a lockfile listed in neither order; Workers is 1 so the incoming
// order is deterministic and only the sort can put it right.
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

// TestOutdatedMissingLockfileClassifiesAsLockfileError pins that no lockfile
// and no collections tree (named, not left empty) reach outdated as
// helpers.ErrLockfileMissing and ExitLock, as for the --frozen commands.
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

	// Positive control: with a lockfile present the run passes this gate and
	// stops at the offline check instead, a different sentinel.
	mustWriteFile(t, filepath.Join(dir, lockfile.DefaultName),
		[]byte("schema_version: 1\ncollections: []\n"))
	cfg.Offline = true
	if err := Outdated(context.Background(), cfg, infra.New(noopPrinter{}, nil)); errors.Is(err, helpers.ErrLockfileMissing) {
		t.Errorf("Outdated with a lockfile present still reported it missing: %v", err)
	}
}
