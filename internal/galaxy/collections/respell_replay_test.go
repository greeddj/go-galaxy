package collections

// This file pins the resolve replay across respelled constraints: the replay
// key holds each constraint canonical, and a snapshot an older release
// recorded as spelled replays through its spec made canonical.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

const respellNewerVersion = "1.1.0"

// respellFixture is a fake Galaxy serving acme.app, acme.lib and acme.tool at
// 1.0.0, the config a run against it takes, and the Store every run shares.
type respellFixture struct {
	srv     *fakegalaxy.Server
	cfg     *config.Config
	runtime *infra.Infra
	st      *store.Store
}

func newRespellFixture(t *testing.T) respellFixture {
	t.Helper()
	srv := fakegalaxy.New(t)
	for _, name := range []string{"app", "lib", "tool"} {
		srv.AddVersion("acme", name, testVersion100, nil)
	}
	return respellFixture{
		srv:     srv,
		cfg:     &config.Config{Server: srv.URL(), Workers: 2},
		runtime: infra.New(noopPrinter{}, srv.Client()),
		st:      store.New(),
	}
}

// root is a Galaxy root on the fixture's server, as prepareRoots leaves one.
func (f respellFixture) root(name, constraint string) collection {
	return collection{Namespace: "acme", Name: name, Constraint: constraint, Source: f.srv.URL(), Type: typeGalaxy}
}

// resolve runs a top-level resolve over roots against the shared Store.
func (f respellFixture) resolve(roots ...collection) (map[string]collection, error) {
	resolved, _, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(f.cfg, f.runtime, f.st), roots, resolveTopLevel)
	return resolved, err
}

// mustRecord resolves roots so the Store records their resolution at 1.0.0.
func (f respellFixture) mustRecord(t *testing.T, roots ...collection) {
	t.Helper()
	resolved, err := f.resolve(roots...)
	if err != nil {
		t.Fatalf("recording resolve: %v", err)
	}
	if got := resolved["acme.app"].Version; got != testVersion100 {
		t.Fatalf("recording resolve took acme.app %q, want %s", got, testVersion100)
	}
}

// recordAsSpelled rewrites the recorded spec's constraints as an older release
// kept them, through NormalizeConstraint alone, with the matching hash.
func (f respellFixture) recordAsSpelled(spelled map[string]string, noDeps bool) {
	spec := f.st.RequirementsSnapshot()
	for fqdn, constraint := range spelled {
		entry := spec[fqdn]
		entry.Constraint = helpers.NormalizeConstraint(constraint)
		spec[fqdn] = entry
	}
	f.st.SetRequirements(spec)
	f.st.SetMetaRequirements(requirementsSignatureFromSpec(spec, noDeps, serversSignature(f.cfg)), f.cfg.Server)
}

// publishNewer drops every cached answer and publishes acme.app 1.1.0, so only
// a replay still answers 1.0.0.
func (f respellFixture) publishNewer() {
	f.st.ClearCaches()
	f.srv.AddVersion("acme", "app", respellNewerVersion, nil)
}

// assertAppReplayed asserts a rerun kept acme.app at the recorded 1.0.0.
func assertAppReplayed(t *testing.T, resolved map[string]collection, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("rerun did not replay the last resolution: %v", err)
	}
	if got := resolved["acme.app"].Version; got != testVersion100 {
		t.Fatalf("rerun took acme.app %q, want the recorded %s", got, testVersion100)
	}
}

// TestRespelledConstraintReplaysTheLastResolution pins that a respelled
// constraint replays: with acme.app failing and 1.1.0 published, only the
// replay answers, and it answers 1.0.0.
func TestRespelledConstraintReplaysTheLastResolution(t *testing.T) {
	t.Parallel()
	cases := []struct{ record, replay string }{
		{">=1.0.0", ">= 1.0.0"},
		{">=1.0.0,<2.0.0", ">=1.0.0, <2.0.0"},
		{">=1.0.0,<2.0.0", ">=1.0.0 <2.0.0"},
		{"1.0.0 - 1.9.0", ">=1.0.0,<=1.9.0"},
		{"==1.0.0", "1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.record+" then "+tc.replay, func(t *testing.T) {
			t.Parallel()
			f := newRespellFixture(t)
			f.mustRecord(t, f.root("app", tc.record))
			f.publishNewer()
			armHardFailure(f.srv, "app")
			resolved, err := f.resolve(f.root("app", tc.replay))
			assertAppReplayed(t, resolved, err)
		})
	}
}

// TestSnapshotRecordedAsSpelledReplays pins that a snapshot keeping each
// constraint as spelled, as an older release recorded it, still replays.
func TestSnapshotRecordedAsSpelledReplays(t *testing.T) {
	t.Parallel()
	for _, spelled := range []string{">= 1.0.0", "=>1.0.0", ">=1.0.0 <2.0.0", "==1.0.0", "1.0.0 - 1.9.0"} {
		t.Run(spelled, func(t *testing.T) {
			t.Parallel()
			f := newRespellFixture(t)
			f.mustRecord(t, f.root("app", spelled))
			f.recordAsSpelled(map[string]string{"acme.app": spelled}, false)
			f.publishNewer()
			armHardFailure(f.srv, "app")
			resolved, err := f.resolve(f.root("app", spelled))
			assertAppReplayed(t, resolved, err)
		})
	}
}

// TestSnapshotRecordedAsSpelledInAnotherModeResolvesAgain pins that the
// fallback keeps the --no-deps check: a snapshot recorded under --no-deps is
// no match for a run with dependencies, which resolves 1.1.0 afresh.
func TestSnapshotRecordedAsSpelledInAnotherModeResolvesAgain(t *testing.T) {
	t.Parallel()
	for _, spelled := range []string{">= 1.0.0", ">=1.0.0 <2.0.0"} {
		t.Run(spelled, func(t *testing.T) {
			t.Parallel()
			f := newRespellFixture(t)
			f.mustRecord(t, f.root("app", spelled))
			f.recordAsSpelled(map[string]string{"acme.app": spelled}, true)
			f.publishNewer()
			resolved, err := f.resolve(f.root("app", spelled))
			if err != nil {
				t.Fatalf("rerun: %v", err)
			}
			if got := resolved["acme.app"].Version; got != respellNewerVersion {
				t.Fatalf("rerun took acme.app %q, want a fresh %s", got, respellNewerVersion)
			}
		})
	}
}

// TestIncrementalResolveKeepsRespelledRoot pins that a respelled root counts
// as unchanged when another root is added: acme.app keeps 1.0.0 from the
// snapshot while it fails, and only acme.tool is solved.
func TestIncrementalResolveKeepsRespelledRoot(t *testing.T) {
	t.Parallel()
	for _, asSpelled := range []bool{false, true} {
		name := "canonical record"
		if asSpelled {
			name = "as spelled record"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRespellFixture(t)
			f.mustRecord(t, f.root("app", ">= 1.0.0"), f.root("lib", "^1.0.0"))
			if asSpelled {
				f.recordAsSpelled(map[string]string{"acme.app": ">= 1.0.0"}, false)
			}
			f.publishNewer()
			armHardFailure(f.srv, "app")
			resolved, err := f.resolve(f.root("app", ">=1.0.0"), f.root("lib", "^1.0.0"), f.root("tool", "^1.0.0"))
			assertAppReplayed(t, resolved, err)
			if got := resolved["acme.tool"].Version; got != testVersion100 {
				t.Fatalf("rerun took acme.tool %q, want %s", got, testVersion100)
			}
		})
	}
}

// TestBuildRequirementsSpecKeysCanonicalConstraint pins the spec's constraint:
// a respelling keys alike, a prerelease stays apart from the range it
// resembles, an exact git or url pin is kept, and match-all is "*".
func TestBuildRequirementsSpecKeysCanonicalConstraint(t *testing.T) {
	t.Parallel()
	specOf := func(root collection) store.RequirementSpec {
		return buildRequirementsSpec([]collection{root})[root.fqdn()]
	}
	signatureOf := func(root collection) string {
		return requirementsSignatureFromSpec(buildRequirementsSpec([]collection{root}), false, "servers")
	}
	spaced := collection{Namespace: "acme", Name: "app", Constraint: ">= 1.0", Type: typeGalaxy}
	tight := collection{Namespace: "acme", Name: "app", Constraint: ">=1.0", Type: typeGalaxy}
	if got, want := specOf(spaced).Constraint, specOf(tight).Constraint; got != ">=1.0" || want != ">=1.0" {
		t.Fatalf("spec constraints = %q and %q, want both %q", got, want, ">=1.0")
	}
	if signatureOf(spaced) != signatureOf(tight) {
		t.Fatalf("signatures of %q and %q differ", spaced.Constraint, tight.Constraint)
	}
	rangeRoot := collection{Namespace: "acme", Name: "app", Constraint: "1.0 - 2.0", Type: typeGalaxy}
	prerelease := collection{Namespace: "acme", Name: "app", Constraint: "1.0-2.0", Type: typeGalaxy}
	if got := specOf(rangeRoot).Constraint; got != ">=1.0,<=2.0" {
		t.Fatalf("spec constraint of %q = %q, want %q", rangeRoot.Constraint, got, ">=1.0,<=2.0")
	}
	if signatureOf(rangeRoot) == signatureOf(prerelease) {
		t.Fatalf("signatures of %q and %q are equal", rangeRoot.Constraint, prerelease.Constraint)
	}
	for _, typ := range []string{typeGit, typeURL} {
		pinned := collection{Namespace: "acme", Name: "app", Constraint: "1.2.3", Type: typ}
		if got := specOf(pinned).Constraint; got != "1.2.3" {
			t.Fatalf("%s root spec constraint = %q, want %q", typ, got, "1.2.3")
		}
	}
	if got := specOf(collection{Namespace: "acme", Name: "app", Type: typeGalaxy}).Constraint; got != "*" {
		t.Fatalf("empty constraint keys as %q, want %q", got, "*")
	}
	releasesOnly := collection{Namespace: "acme", Name: "app", Constraint: "==*", Type: typeGalaxy}
	if got := specOf(releasesOnly).Constraint; got != "=*" {
		t.Fatalf("spec constraint of %q = %q, want %q, apart from match-all", releasesOnly.Constraint, got, "=*")
	}
}

// respelledKeys writes content under name in a directory of its own and
// returns the replay key a deps-mode run with no servers takes from it and
// the go-galaxy hash key, both from the file as the loader reads it.
func respelledKeys(t *testing.T, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), helpers.FileMod); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	roots, _, err := loadRequirements(path, "")
	if err != nil {
		t.Fatalf("loadRequirements(%s): %v", name, err)
	}
	if roots, err = prepareRoots(roots); err != nil {
		t.Fatalf("prepareRoots(%s): %v", name, err)
	}
	file, err := requirements.Load(path, "")
	if err != nil {
		t.Fatalf("requirements.Load(%s): %v", name, err)
	}
	return requirementsSignatureFromSpec(buildRequirementsSpec(roots), false, ""), file.Hash()
}

// TestRespelledConstraintKeepsBothKeys pins that a constraint respelled across
// requirements.yml and galaxy.toml keeps the replay key and the hash key
// alike, while a changed operand moves both.
func TestRespelledConstraintKeepsBothKeys(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, yaml, toml string
		same             bool
	}{
		{name: "spaced range", yaml: ">= 1.0.0, < 2.0.0", toml: "acme.app >=1.0.0,<2.0.0", same: true},
		{name: "== exact version", yaml: "==1.0.0", toml: "acme.app 1.0.0", same: true},
		{name: "1.0 and 1.0.0", yaml: "1.0", toml: "acme.app 1.0.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			yamlReplay, yamlHash := respelledKeys(t, helpers.RequirementsYAMLName,
				"collections:\n  - name: acme.app\n    version: \""+tt.yaml+"\"\n")
			tomlReplay, tomlHash := respelledKeys(t, helpers.RequirementsTOMLName,
				"[project]\ncollections = [\""+tt.toml+"\"]\n")
			if (yamlReplay == tomlReplay) != tt.same {
				t.Errorf("replay keys equal = %t, want %t", yamlReplay == tomlReplay, tt.same)
			}
			if (yamlHash == tomlHash) != tt.same {
				t.Errorf("hash keys equal = %t, want %t", yamlHash == tomlHash, tt.same)
			}
		})
	}
}
