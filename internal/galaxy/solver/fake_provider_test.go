package solver

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"sync"
)

// fakeProvider is the in-memory Provider double, keyed by package and by
// "pkg@version", with call counters for laziness assertions. Universe
// shuffles on every call so a core that trusts provider order fails tests.
type fakeProvider struct {
	versions      map[string][]string
	deps          map[string]map[string]string
	highestOf     map[string]string
	noHighest     map[string]bool
	universeCalls map[string]int
	highestCalls  map[string]int
	depsCalls     map[string]int
	mu            sync.Mutex
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		versions:      make(map[string][]string),
		deps:          make(map[string]map[string]string),
		highestOf:     make(map[string]string),
		noHighest:     make(map[string]bool),
		universeCalls: make(map[string]int),
		highestCalls:  make(map[string]int),
		depsCalls:     make(map[string]int),
	}
}

func (f *fakeProvider) Highest(_ context.Context, pkg string) (Version, bool, error) {
	f.mu.Lock()
	f.highestCalls[pkg]++
	f.mu.Unlock()

	if f.noHighest[pkg] {
		return Version{}, false, nil
	}
	if raw, ok := f.highestOf[pkg]; ok {
		v, err := NewVersion(raw)
		if err != nil {
			return Version{}, false, fmt.Errorf("fakeProvider: bad highest override %q for %s: %w", raw, pkg, err)
		}
		return v, true, nil
	}

	raw := f.versions[pkg]
	if len(raw) == 0 {
		return Version{}, false, nil
	}
	parsed := make([]Version, 0, len(raw))
	for _, r := range raw {
		v, err := NewVersion(r)
		if err != nil {
			continue
		}
		parsed = append(parsed, v)
	}
	if len(parsed) == 0 {
		return Version{}, false, nil
	}
	ordered := buildUniverse(parsed)
	return ordered[0], true, nil
}

func (f *fakeProvider) Universe(_ context.Context, pkg string) ([]Version, error) {
	f.mu.Lock()
	f.universeCalls[pkg]++
	f.mu.Unlock()

	raw := append([]string{}, f.versions[pkg]...)
	//nolint:gosec // G404: shuffles a fixture's order, not security sensitive.
	rand.Shuffle(len(raw), func(i, j int) { raw[i], raw[j] = raw[j], raw[i] })

	out := make([]Version, 0, len(raw))
	for _, r := range raw {
		v, err := NewVersion(r)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeProvider) Dependencies(_ context.Context, pkg string, v Version) (map[string]Constraint, error) {
	key := pkg + "@" + v.Original()
	f.mu.Lock()
	f.depsCalls[key]++
	f.mu.Unlock()

	d, ok := f.deps[key]
	if !ok {
		return map[string]Constraint{}, nil
	}
	out := make(map[string]Constraint, len(d))
	maps.Copy(out, d)
	return out, nil
}

// withVersions registers pkg's published versions.
func (f *fakeProvider) withVersions(pkg string, versions ...string) *fakeProvider {
	f.versions[pkg] = append([]string{}, versions...)
	return f
}

// withDeps registers pkg@version's dependency map.
func (f *fakeProvider) withDeps(pkg, version string, deps map[string]string) *fakeProvider {
	f.deps[pkg+"@"+version] = deps
	return f
}

// withHighest overrides pkg's registry-reported highest version,
// independent of its true universe maximum, to steer the probe-or-fetch fork.
func (f *fakeProvider) withHighest(pkg, version string) *fakeProvider {
	f.highestOf[pkg] = version
	return f
}

// withNoHighest makes the Highest probe report ok=false for pkg, forcing
// the core to fall back to a full Universe fetch.
func (f *fakeProvider) withNoHighest(pkg string) *fakeProvider {
	f.noHighest[pkg] = true
	return f
}

// totalUniverseCalls sums every package's Universe call count.
func (f *fakeProvider) totalUniverseCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.universeCalls {
		total += n
	}
	return total
}

// totalHighestCalls sums every package's Highest call count.
func (f *fakeProvider) totalHighestCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.highestCalls {
		total += n
	}
	return total
}

// totalDepsCalls sums every "pkg@version" key's Dependencies call count.
func (f *fakeProvider) totalDepsCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.depsCalls {
		total += n
	}
	return total
}
