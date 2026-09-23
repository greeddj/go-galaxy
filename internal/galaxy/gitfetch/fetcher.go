package gitfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/greeddj/go-galaxy/internal/galaxy/collectionbuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/rolebuild"
)

const (
	refsHeadsPrefix = "refs/heads/"
	refsTagsPrefix  = "refs/tags/"
	headName        = "HEAD"
	// shallowDepth is the only depth this tool ever asks for: the one commit
	// it builds from. Anything deeper would be history nobody reads.
	shallowDepth = 1
)

// Fetcher implements gitsource.Client. One value serves a whole run; it holds
// no per-acquisition state.
type Fetcher struct {
	httpClient *http.Client
	tempDir    func() string
	maxPack    int64
}

// Option adjusts a Fetcher at construction.
type Option func(*Fetcher)

// WithPackCap overrides the on-disk byte cap of one fetch. Production leaves
// it at helpers.GitPackMaxSize; a test lowers it to prove the cap bites.
func WithPackCap(n int64) Option {
	return func(f *Fetcher) {
		if n > 0 {
			f.maxPack = n
		}
	}
}

// New builds a Fetcher over httpClient (the run's git HTTP client, see
// fetch.NewGit) with object stores created under tempDir. It runs the
// one-time go-git hardening.
func New(httpClient *http.Client, tempDir func() string, opts ...Option) *Fetcher {
	harden()
	f := &Fetcher{httpClient: httpClient, tempDir: tempDir, maxPack: helpers.GitPackMaxSize}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// advertisement is what one advertised-references round trip told us.
type advertisement struct {
	refs       map[string]plumbing.Hash
	peeled     map[string]plumbing.Hash
	head       *plumbing.Hash
	caps       *capability.List
	headTarget string
	shallow    bool
	shaInWant  bool
}

// Advertise resolves ref against the remote's advertised references with no
// pack transfer. A commit ref is answered without a round trip: it is its own
// answer.
func (f *Fetcher) Advertise(ctx context.Context, u gitsource.URL, ref gitsource.Ref, auth gitsource.Credential) (string, string, error) {
	if ref.IsCommit() {
		return ref.Name, ref.Name, nil
	}
	sess, adv, err := f.openAndAdvertise(ctx, u, auth)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = sess.Close() }()
	commit, refName, _, err := resolve(adv, ref, u.String())
	if err != nil {
		return "", "", err
	}
	return commit.String(), refName, nil
}

// fetchSpec is what both request kinds hand the fetch: the ref asked for,
// the commit pinned ("" when none), and the remote and credential the search
// fallback reopens a session with.
type fetchSpec struct {
	url    gitsource.URL
	ref    gitsource.Ref
	commit string
	auth   gitsource.Credential
}

// fetched is one commit brought to disk: its tree, commit, resolved ref name
// and warnings, and the object store the tree reads from, which the caller
// closes once its build is done.
type fetched struct {
	src      *treeSource
	store    *objectStore
	refName  string
	warnings []string
	commit   plumbing.Hash
}

// Acquire fetches the commit req names, reads its tree and builds the
// collections under req.Subdir. See the package comment for the shape of the
// fetch; the wants are always hashes learned from the advertisement.
func (f *Fetcher) Acquire(ctx context.Context, req gitsource.Request) (gitsource.Result, error) {
	if req.TempFile == nil {
		return gitsource.Result{}, fmt.Errorf("%w: no temp file supplier", helpers.ErrConfigIsNil)
	}
	display := req.URL.String()
	fd, err := f.fetch(ctx, fetchSpec{url: req.URL, ref: req.Ref, commit: req.Commit, auth: req.Auth})
	if err != nil {
		return gitsource.Result{}, err
	}
	defer fd.store.Close()

	collections, buildWarnings, err := buildAll(ctx, fd.src, req, display)
	if err != nil {
		return gitsource.Result{}, err
	}
	return gitsource.Result{
		Commit:       fd.commit.String(),
		RefName:      fd.refName,
		Collections:  collections,
		Warnings:     append(fd.warnings, buildWarnings...),
		BytesFetched: fd.store.bytes(),
	}, nil
}

// AcquireRole fetches the commit req names and builds the role its tree
// root is. The fetch is the one Acquire does; only the build differs, and a
// tree that is not a role is refused by it under helpers.ErrRoleMetaNotFound.
func (f *Fetcher) AcquireRole(ctx context.Context, req gitsource.RoleRequest) (gitsource.RoleResult, error) {
	if req.TempFile == nil {
		return gitsource.RoleResult{}, fmt.Errorf("%w: no temp file supplier", helpers.ErrConfigIsNil)
	}
	display := req.URL.String()
	fd, err := f.fetch(ctx, fetchSpec{url: req.URL, ref: req.Ref, commit: req.Commit, auth: req.Auth})
	if err != nil {
		return gitsource.RoleResult{}, err
	}
	defer fd.store.Close()

	built, err := rolebuild.Build(ctx, fd.src, rolebuild.TempFileFunc(req.TempFile))
	if err != nil {
		return gitsource.RoleResult{}, fmt.Errorf("%s: %w", display, err)
	}
	return gitsource.RoleResult{
		Cleanup:        built.Cleanup,
		Dependencies:   built.Meta.Dependencies,
		Commit:         fd.commit.String(),
		RefName:        fd.refName,
		GalaxyRoleName: built.Meta.RoleName,
		ArtifactPath:   built.ArtifactPath,
		ArtifactSHA:    built.SHA256,
		Warnings:       append(fd.warnings, built.Warnings...),
		BytesFetched:   fd.store.bytes(),
	}, nil
}

// fetch runs one advertisement, fetches the chosen commit into a new object
// store and reads its root tree from the store, not the wire; on success the
// store is the caller's to close.
func (f *Fetcher) fetch(ctx context.Context, spec fetchSpec) (*fetched, error) {
	display := spec.url.String()
	sess, adv, err := f.openAndAdvertise(ctx, spec.url, spec.auth)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	target, refName, warnings, err := chooseTarget(adv, spec, display)
	if err != nil {
		return nil, err
	}
	store, err := newObjectStore(storageDirFor(f.tempDir), f.maxPack)
	if err != nil {
		return nil, err
	}
	commit, err := f.fetchCommit(ctx, sess, adv, store, target, spec, display)
	if err != nil {
		store.Close()
		return nil, err
	}
	src, err := newTreeSource(store.storer, commit)
	if err != nil {
		store.Close()
		return nil, err
	}
	return &fetched{src: src, store: store, refName: refName, warnings: warnings, commit: target}, nil
}

// openAndAdvertise opens an upload-pack session to u and reads its
// advertisement. The session stays open for the fetch that follows; the
// caller closes it.
func (f *Fetcher) openAndAdvertise(ctx context.Context, u gitsource.URL, cred gitsource.Credential,
) (transport.UploadPackSession, *advertisement, error) {
	display := u.String()
	ep, err := transport.NewEndpoint(u.String())
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", helpers.ErrInvalidGitURL, display, err)
	}
	if ep.Protocol != u.Scheme {
		return nil, nil, fmt.Errorf("%w: %s parsed as protocol %q", helpers.ErrInvalidGitURL, display, ep.Protocol)
	}
	tr, err := f.transportFor(ep)
	if err != nil {
		return nil, nil, err
	}
	auth, err := authFor(u, cred)
	if err != nil {
		return nil, nil, err
	}
	sess, err := tr.NewUploadPackSession(ep, auth)
	if err != nil {
		return nil, nil, classifyTransportError(err, display)
	}
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		_ = sess.Close()
		return nil, nil, classifyTransportError(err, display)
	}
	return sess, newAdvertisement(ar), nil
}

func newAdvertisement(ar *packp.AdvRefs) *advertisement {
	adv := &advertisement{
		refs:      ar.References,
		peeled:    ar.Peeled,
		head:      ar.Head,
		caps:      ar.Capabilities,
		shallow:   ar.Capabilities.Supports(capability.Shallow),
		shaInWant: ar.Capabilities.Supports(capability.AllowReachableSHA1InWant) || ar.Capabilities.Supports(capability.AllowTipSHA1InWant),
	}
	for _, v := range ar.Capabilities.Get(capability.SymRef) {
		if name, target, ok := strings.Cut(v, ":"); ok && name == headName {
			adv.headTarget = target
		}
	}
	return adv
}

// advertised reports whether h is a ref's own hash, which every upload-pack
// serves as a want; a peeled annotated-tag commit is not, since git answers a
// want for it with "not our ref" unless it is also a ref's tip.
func (a *advertisement) advertised(h plumbing.Hash) bool {
	for _, v := range a.refs {
		if v == h {
			return true
		}
	}
	return false
}

// wantFor returns target when it is a ref's own hash, else the tag object
// that peels to it (its pack carries the commit and tree too), else target
// unchanged for the sha-in-want path.
func (a *advertisement) wantFor(target plumbing.Hash) plumbing.Hash {
	if a.advertised(target) {
		return target
	}
	for name, peeled := range a.peeled {
		if peeled == target {
			if tag, ok := a.refs[name]; ok {
				return tag
			}
		}
	}
	return target
}

// tips returns every distinct advertised ref hash, sorted so a full fetch's
// want list is the same from run to run.
func (a *advertisement) tips() []plumbing.Hash {
	seen := make(map[plumbing.Hash]struct{}, len(a.refs))
	for _, v := range a.refs {
		seen[v] = struct{}{}
	}
	out := make([]plumbing.Hash, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// resolve maps ref onto its advertised commit and full ref name; an unqualified
// name takes the branch over a same-named tag with a warning, as git checkout
// does, and a tag resolves to its peeled commit when one is advertised.
func resolve(adv *advertisement, ref gitsource.Ref, display string) (plumbing.Hash, string, []string, error) {
	switch ref.Kind {
	case gitsource.RefHEAD:
		if adv.head == nil {
			return plumbing.ZeroHash, "", nil, fmt.Errorf("%w: %s advertises no HEAD", helpers.ErrGitRefNotFound, display)
		}
		name := headName
		if adv.headTarget != "" {
			name = adv.headTarget
		}
		return *adv.head, name, nil, nil
	case gitsource.RefCommit:
		return plumbing.NewHash(ref.Name), ref.Name, nil, nil
	case gitsource.RefQualified:
		h, ok := adv.lookup(ref.Name)
		if !ok {
			return plumbing.ZeroHash, "", nil, fmt.Errorf("%w: %s does not advertise %s", helpers.ErrGitRefNotFound, display, ref.Name)
		}
		return h, ref.Name, nil, nil
	case gitsource.RefName:
		return resolveName(adv, ref.Name, display)
	default:
		return resolveName(adv, ref.Name, display)
	}
}

// resolveName resolves an unqualified name: branch first, tag second, with a
// warning when both exist and an ErrGitRefNotFound when neither does.
func resolveName(adv *advertisement, name, display string) (plumbing.Hash, string, []string, error) {
	branch, hasBranch := adv.lookup(refsHeadsPrefix + name)
	tag, hasTag := adv.lookup(refsTagsPrefix + name)
	switch {
	case hasBranch && hasTag:
		warning := fmt.Sprintf("%s names both a branch and a tag on %s; using the branch (spell refs/tags/%s for the tag)",
			name, display, name)
		return branch, refsHeadsPrefix + name, []string{warning}, nil
	case hasBranch:
		return branch, refsHeadsPrefix + name, nil, nil
	case hasTag:
		return tag, refsTagsPrefix + name, nil, nil
	default:
		return plumbing.ZeroHash, "", nil, fmt.Errorf("%w: %s advertises neither a branch nor a tag named %q",
			helpers.ErrGitRefNotFound, display, name)
	}
}

// lookup returns the commit a full ref name points at, preferring the peeled
// hash of an annotated tag.
func (a *advertisement) lookup(name string) (plumbing.Hash, bool) {
	if h, ok := a.peeled[name]; ok {
		return h, true
	}
	h, ok := a.refs[name]
	return h, ok
}

// chooseTarget decides which commit the acquisition is for: the requested
// commit when one is pinned, else whatever the ref resolves to now.
func chooseTarget(adv *advertisement, spec fetchSpec, display string) (plumbing.Hash, string, []string, error) {
	if spec.commit != "" {
		if !gitsource.IsCommitHash(spec.commit) {
			return plumbing.ZeroHash, "", nil, fmt.Errorf("%w: %q", helpers.ErrInvalidGitLocator, spec.commit)
		}
		name := spec.ref.Name
		if name == "" {
			name = spec.commit
		}
		return plumbing.NewHash(spec.commit), name, nil, nil
	}
	return resolve(adv, spec.ref, display)
}

// fetchCommit wants target directly, shallow when allowed, if it is an
// advertised tip or the remote serves it by hash, else searches; one still
// missing is ErrGitCommitNotFound if never advertised, else ErrGitCommitMismatch.
func (f *Fetcher) fetchCommit(ctx context.Context, sess transport.UploadPackSession, adv *advertisement,
	store *objectStore, target plumbing.Hash, spec fetchSpec, display string,
) (*object.Commit, error) {
	want := adv.wantFor(target)
	advertised := adv.advertised(want)
	if advertised || adv.shaInWant {
		if err := fetchPack(ctx, sess, adv, store, []plumbing.Hash{want}, shallowDepthIf(adv.shallow), display); err != nil {
			return nil, err
		}
	} else if err := f.fetchBySearch(ctx, sess, adv, store, target, spec, display); err != nil {
		return nil, err
	}
	commit, err := object.GetCommit(store.storer, target)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) && !advertised {
			return nil, fmt.Errorf("%w: %s does not hold commit %s", helpers.ErrGitCommitNotFound, display, target)
		}
		return nil, fmt.Errorf("%w: %s advertised %s but did not ship it: %w", helpers.ErrGitCommitMismatch, display, target, err)
	}
	return commit, nil
}

// fetchBySearch fetches the hinted ref's full history and, if that missed,
// every tip on a fresh session: over ssh go-git closes the channel with the
// first pack's reader, so a second UploadPack would hit a closed stream.
func (f *Fetcher) fetchBySearch(ctx context.Context, sess transport.UploadPackSession, adv *advertisement,
	store *objectStore, target plumbing.Hash, spec fetchSpec, display string,
) error {
	hinted := false
	if spec.ref.Kind != gitsource.RefCommit {
		if tip, _, _, err := resolve(adv, spec.ref, display); err == nil {
			hinted = true
			if err := fetchPack(ctx, sess, adv, store, []plumbing.Hash{adv.wantFor(tip)}, 0, display); err != nil {
				return err
			}
		}
	}
	if hinted && hasCommit(store, target) {
		return nil
	}
	if hinted {
		fresh, freshAdv, err := f.openAndAdvertise(ctx, spec.url, spec.auth)
		if err != nil {
			return err
		}
		defer func() { _ = fresh.Close() }()
		sess, adv = fresh, freshAdv
	}
	return fetchPack(ctx, sess, adv, store, adv.tips(), 0, display)
}

func hasCommit(store *objectStore, h plumbing.Hash) bool {
	_, err := object.GetCommit(store.storer, h)
	return err == nil
}

func shallowDepthIf(shallow bool) int {
	if shallow {
		return shallowDepth
	}
	return 0
}

// fetchPack runs one upload-pack exchange on the open session: wants by
// hash, a deepen only when the remote advertised shallow, no progress, and
// the pack streamed straight into the on-disk store.
func fetchPack(ctx context.Context, sess transport.UploadPackSession, adv *advertisement, store *objectStore,
	wants []plumbing.Hash, depth int, display string,
) error {
	if len(wants) == 0 {
		return fmt.Errorf("%w: %s advertises nothing to fetch", helpers.ErrGitRefNotFound, display)
	}
	req, err := newUploadPackRequest(adv, wants, depth, display)
	if err != nil {
		return err
	}
	resp, err := sess.UploadPack(ctx, req)
	if err != nil {
		return classifyTransportError(err, display)
	}
	defer func() { _ = resp.Close() }()
	if depth > 0 && len(resp.Shallows) > 0 {
		if err := store.storer.SetShallow(resp.Shallows); err != nil {
			return fmt.Errorf("%w: recording shallow boundary: %w", helpers.ErrGitTransportFailed, err)
		}
	}
	if err := packfile.UpdateObjectStorage(store.storer, demux(req, resp)); err != nil {
		if errors.Is(err, helpers.ErrResponseTooLarge) || isContextError(err) {
			return err
		}
		return classifyTransportError(err, display)
	}
	return nil
}

// newUploadPackRequest builds the request for wants: a deepen only when
// depth is positive (the caller passes one only when the remote advertised
// shallow) and no-progress whenever the remote supports it.
func newUploadPackRequest(adv *advertisement, wants []plumbing.Hash, depth int, display string) (*packp.UploadPackRequest, error) {
	req := packp.NewUploadPackRequestFromCapabilities(adv.caps)
	req.Wants = wants
	if depth > 0 {
		req.Depth = packp.DepthCommits(depth)
		if err := req.Capabilities.Set(capability.Shallow); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", helpers.ErrGitTransportFailed, display, err)
		}
	}
	if adv.caps.Supports(capability.NoProgress) {
		if err := req.Capabilities.Set(capability.NoProgress); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", helpers.ErrGitTransportFailed, display, err)
		}
	}
	return req, nil
}

// demux unwraps the sideband the request negotiated, if any, so the pack
// bytes alone reach the object store.
func demux(req *packp.UploadPackRequest, resp io.Reader) io.Reader {
	switch {
	case req.Capabilities.Supports(capability.Sideband64k):
		return sideband.NewDemuxer(sideband.Sideband64k, resp)
	case req.Capabilities.Supports(capability.Sideband):
		return sideband.NewDemuxer(sideband.Sideband, resp)
	}
	return resp
}

// buildAll discovers the collections under req.Subdir and builds each one;
// with req.Only set, only the matching candidate is built and its absence is
// an error rather than an empty result.
func buildAll(ctx context.Context, src *treeSource, req gitsource.Request, display string) ([]gitsource.Collection, []string, error) {
	candidates, warnings, err := collectionbuild.Discover(src, req.Subdir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", display, err)
	}
	if req.Only != nil {
		candidates, err = selectOnly(candidates, *req.Only, display)
		if err != nil {
			return nil, nil, err
		}
	}
	collections := make([]gitsource.Collection, 0, len(candidates))
	for _, cand := range candidates {
		built, err := collectionbuild.Build(ctx, src, cand, collectionbuild.TempFileFunc(req.TempFile))
		if err != nil {
			for _, c := range collections {
				c.Cleanup()
			}
			return nil, nil, fmt.Errorf("%s: %w", display, err)
		}
		warnings = append(warnings, built.Warnings...)
		collections = append(collections, gitsource.Collection{
			Cleanup:      built.Cleanup,
			Dependencies: built.Dependencies,
			Namespace:    built.Namespace,
			Name:         built.Name,
			Version:      built.Version,
			Subdir:       built.Subdir,
			ArtifactPath: built.ArtifactPath,
			ArtifactSHA:  built.SHA256,
		})
	}
	return collections, warnings, nil
}

// selectOnly narrows candidates to the one carrying identity. A candidate
// under the same subdir with a different identity is an integrity failure:
// the pin names a collection the commit no longer builds as such.
func selectOnly(candidates []collectionbuild.Candidate, only gitsource.Identity, display string) ([]collectionbuild.Candidate, error) {
	for _, cand := range candidates {
		if cand.Meta.Namespace == only.Namespace && cand.Meta.Name == only.Name && cand.Meta.Version == only.Version {
			return []collectionbuild.Candidate{cand}, nil
		}
	}
	for _, cand := range candidates {
		if cand.Meta.Namespace == only.Namespace && cand.Meta.Name == only.Name {
			return nil, fmt.Errorf("%w: %s builds %s.%s as version %s, pinned as %s",
				helpers.ErrGitArtifactIdentityMismatch, display, cand.Meta.Namespace, cand.Meta.Name, cand.Meta.Version, only.Version)
		}
	}
	return nil, fmt.Errorf("%w: %s does not carry %s.%s", helpers.ErrGitCollectionNotFound, display, only.Namespace, only.Name)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
