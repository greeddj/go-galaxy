package gitfetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"

	"github.com/greeddj/go-galaxy/internal/galaxy/collectionbuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegit"
)

func hashOf(c byte) plumbing.Hash { return plumbing.NewHash(strings.Repeat(string(c), 40)) }

// TestResolveRefusals pins the advertisement lookups that find nothing: no
// HEAD, an unadvertised qualified ref and an unknown name, each as
// ErrGitRefNotFound.
func TestResolveRefusals(t *testing.T) {
	t.Parallel()
	adv := &advertisement{
		refs:   map[string]plumbing.Hash{"refs/heads/main": hashOf('a')},
		peeled: map[string]plumbing.Hash{},
	}
	for _, ref := range []gitsource.Ref{
		{Kind: gitsource.RefHEAD},
		{Kind: gitsource.RefQualified, Name: "refs/heads/absent"},
		{Kind: gitsource.RefName, Name: "absent"},
	} {
		if _, _, _, err := resolve(adv, ref, "remote"); !errors.Is(err, helpers.ErrGitRefNotFound) {
			t.Errorf("resolve(%+v) = %v, want ErrGitRefNotFound", ref, err)
		}
	}
}

// TestResolveAnswers pins what resolves: HEAD to its symref target or bare
// name, a commit to itself, a tag to its peeled commit, an unknown kind as a name.
func TestResolveAnswers(t *testing.T) {
	t.Parallel()
	head := hashOf('a')
	adv := &advertisement{
		refs: map[string]plumbing.Hash{
			"refs/heads/main": head,
			"refs/tags/v1":    hashOf('b'),
		},
		peeled: map[string]plumbing.Hash{"refs/tags/v1": hashOf('c')},
		head:   &head,
	}
	commit := hashOf('d').String()
	for _, tc := range []struct {
		name     string
		wantName string
		ref      gitsource.Ref
		want     plumbing.Hash
	}{
		{name: "bare HEAD", ref: gitsource.Ref{Kind: gitsource.RefHEAD}, want: head, wantName: "HEAD"},
		{name: "commit", ref: gitsource.Ref{Kind: gitsource.RefCommit, Name: commit}, want: hashOf('d'), wantName: commit},
		{name: "peeled tag", ref: gitsource.Ref{Kind: gitsource.RefQualified, Name: "refs/tags/v1"}, want: hashOf('c'), wantName: "refs/tags/v1"},
		{name: "unknown kind", ref: gitsource.Ref{Kind: gitsource.RefKind(99), Name: "main"}, want: head, wantName: "refs/heads/main"},
	} {
		got, name, _, err := resolve(adv, tc.ref, "remote")
		if err != nil || got != tc.want || name != tc.wantName {
			t.Errorf("%s: resolve = (%s, %q, %v), want (%s, %q)", tc.name, got, name, err, tc.want, tc.wantName)
		}
	}
	adv.headTarget = "refs/heads/main"
	if _, name, _, err := resolve(adv, gitsource.Ref{Kind: gitsource.RefHEAD}, "remote"); err != nil || name != "refs/heads/main" {
		t.Errorf("HEAD with a symref = (%q, %v), want refs/heads/main", name, err)
	}
}

// TestChooseTargetPinnedCommit pins that a pinned commit wins without the
// advertisement, takes the ref's name when there is one, and must be a full hash.
func TestChooseTargetPinnedCommit(t *testing.T) {
	t.Parallel()
	adv := &advertisement{refs: map[string]plumbing.Hash{}, peeled: map[string]plumbing.Hash{}}
	pinned := hashOf('e').String()

	got, name, _, err := chooseTarget(adv, fetchSpec{commit: pinned}, "remote")
	if err != nil || got.String() != pinned || name != pinned {
		t.Errorf("unnamed pin = (%s, %q, %v), want the commit as its own name", got, name, err)
	}
	_, name, _, err = chooseTarget(adv, fetchSpec{commit: pinned, ref: gitsource.Ref{Kind: gitsource.RefName, Name: "gone"}}, "remote")
	if err != nil || name != "gone" {
		t.Errorf("named pin = (%q, %v), want the ref's name even when it is no longer advertised", name, err)
	}
	for _, bad := range []string{"abc", strings.Repeat("g", 40), strings.ToUpper(pinned)[:39]} {
		if _, _, _, err := chooseTarget(adv, fetchSpec{commit: bad}, "remote"); !errors.Is(err, helpers.ErrInvalidGitLocator) {
			t.Errorf("chooseTarget(%q) = %v, want ErrInvalidGitLocator", bad, err)
		}
	}
}

// TestWantForAnAnnotatedTag pins that a commit reached only through an
// annotated tag is wanted as the tag object, an advertised tip as itself,
// and anything else unchanged for the by-hash path.
func TestWantForAnAnnotatedTag(t *testing.T) {
	t.Parallel()
	adv := &advertisement{
		refs:   map[string]plumbing.Hash{"refs/heads/main": hashOf('a'), "refs/tags/v1": hashOf('b')},
		peeled: map[string]plumbing.Hash{"refs/tags/v1": hashOf('c'), "refs/tags/orphan": hashOf('d')},
	}
	for target, want := range map[plumbing.Hash]plumbing.Hash{
		hashOf('a'): hashOf('a'),
		hashOf('c'): hashOf('b'),
		hashOf('d'): hashOf('d'),
		hashOf('f'): hashOf('f'),
	} {
		if got := adv.wantFor(target); got != want {
			t.Errorf("wantFor(%s) = %s, want %s", target, got, want)
		}
	}
	if tips := adv.tips(); len(tips) != 2 || tips[0] != hashOf('a') || tips[1] != hashOf('b') {
		t.Errorf("tips = %v, want the two ref hashes sorted", tips)
	}
}

// TestFetchPackWithNothingToWant pins that an empty want list is refused as a
// remote advertising nothing, before any request is made.
func TestFetchPackWithNothingToWant(t *testing.T) {
	t.Parallel()
	err := fetchPack(context.Background(), nil, &advertisement{}, nil, nil, 0, "remote")
	if !errors.Is(err, helpers.ErrGitRefNotFound) {
		t.Fatalf("fetchPack = %v, want ErrGitRefNotFound", err)
	}
}

// TestDemuxStripsTheNegotiatedSideband pins that pack bytes reach the store
// with the sideband framing removed whichever sideband was negotiated, and
// untouched when none was.
func TestDemuxStripsTheNegotiatedSideband(t *testing.T) {
	t.Parallel()
	const payload = "PACK-bytes"
	for _, tc := range []struct {
		cap  capability.Capability
		name string
		kind sideband.Type
	}{
		{name: "side-band-64k", cap: capability.Sideband64k, kind: sideband.Sideband64k},
		{name: "side-band", cap: capability.Sideband, kind: sideband.Sideband},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := packp.NewUploadPackRequest()
			if err := req.Capabilities.Set(tc.cap); err != nil {
				t.Fatalf("set capability: %v", err)
			}
			var framed bytes.Buffer
			enc := pktline.NewEncoder(&framed)
			if err := enc.Encode(append([]byte{byte(sideband.PackData)}, payload...)); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if err := enc.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			got, err := io.ReadAll(demux(req, &framed))
			if err != nil || string(got) != payload {
				t.Fatalf("demuxed = %q, %v, want %q", got, err, payload)
			}
		})
	}
	raw := strings.NewReader(payload)
	if got := demux(packp.NewUploadPackRequest(), raw); got != raw {
		t.Fatalf("demux without a sideband wrapped the reader")
	}
}

// TestSelectOnlyRefusesAnAbsentCollection pins that a pin naming a collection
// the commit does not carry at all is ErrGitCollectionNotFound, distinct from
// the identity mismatch of one carried at another version.
func TestSelectOnlyRefusesAnAbsentCollection(t *testing.T) {
	t.Parallel()
	candidates := []collectionbuild.Candidate{{Meta: collectionbuild.GalaxyYML{Namespace: "acme", Name: "app", Version: "1.0.0"}}}
	_, err := selectOnly(candidates, gitsource.Identity{Namespace: "acme", Name: "other", Version: "1.0.0"}, "remote")
	if !errors.Is(err, helpers.ErrGitCollectionNotFound) {
		t.Fatalf("selectOnly(absent) = %v, want ErrGitCollectionNotFound", err)
	}
	_, err = selectOnly(candidates, gitsource.Identity{Namespace: "acme", Name: "app", Version: "2.0.0"}, "remote")
	if !errors.Is(err, helpers.ErrGitArtifactIdentityMismatch) {
		t.Fatalf("selectOnly(other version) = %v, want ErrGitArtifactIdentityMismatch", err)
	}
	got, err := selectOnly(candidates, gitsource.Identity{Namespace: "acme", Name: "app", Version: "1.0.0"}, "remote")
	if err != nil || len(got) != 1 {
		t.Fatalf("selectOnly(match) = %+v, %v", got, err)
	}
}

// TestAcquireRefusesAMissingTempFileSupplier pins that both build entry points
// refuse a request without a temp file supplier before touching the network.
func TestAcquireRefusesAMissingTempFileSupplier(t *testing.T) {
	t.Parallel()
	srv := fakegit.New(t)
	f := newFetcher(t)
	u := mustURL(t, srv.RepoURL("app"))
	if _, err := f.Acquire(t.Context(), gitsource.Request{URL: u}); !errors.Is(err, helpers.ErrConfigIsNil) {
		t.Errorf("Acquire = %v, want ErrConfigIsNil", err)
	}
	if _, err := f.AcquireRole(t.Context(), gitsource.RoleRequest{URL: u}); !errors.Is(err, helpers.ErrConfigIsNil) {
		t.Errorf("AcquireRole = %v, want ErrConfigIsNil", err)
	}
	if srv.Total() != 0 {
		t.Errorf("a refused request reached the remote %d times", srv.Total())
	}
}

// TestAdvertiseRefusals pins that Advertise reports a remote it cannot reach
// and a name the remote does not advertise.
func TestAdvertiseRefusals(t *testing.T) {
	t.Parallel()
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("app", app.repo)
	f := newFetcher(t)

	_, _, err := f.Advertise(t.Context(), mustURL(t, srv.RepoURL("missing")), mustRef(t, "main"), gitsource.Credential{})
	if !errors.Is(err, helpers.ErrGitTransportFailed) {
		t.Errorf("Advertise(missing repository) = %v, want ErrGitTransportFailed", err)
	}
	_, _, err = f.Advertise(t.Context(), mustURL(t, srv.RepoURL("app")), mustRef(t, "nosuch"), gitsource.Credential{})
	if !errors.Is(err, helpers.ErrGitRefNotFound) {
		t.Errorf("Advertise(nosuch) = %v, want ErrGitRefNotFound", err)
	}
}
