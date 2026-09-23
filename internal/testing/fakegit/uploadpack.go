package fakegit

import (
	"bytes"
	"io"
	"slices"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Wire constants of the upload-pack exchange: the service name, the agent
// string, the one depth this fake honors and the pack encoder's delta window,
// which matches go-git's own server.
const (
	uploadPackService = "git-upload-pack"
	agentValue        = "fakegit"
	honoredDepth      = packp.DepthCommits(1)
	packWindow        = 10
	donePayload       = "done"
)

// uploadPackReply is one upload-pack answer, rendered in full before any write
// so a blocking fault never holds the repository lock. The pack is kept apart
// from the header so StallAfterBytes counts pack bytes alone.
type uploadPackReply struct {
	badRequest string
	header     []byte
	pack       []byte
}

// advertise writes the reference advertisement for repo with caps to w, with
// the smart-HTTP service prefix when withServicePrefix is set. No side-band is
// advertised, so the pack follows the NAK raw and a stall is a byte count.
func advertise(w io.Writer, repo *Repo, caps Capabilities, withServicePrefix bool) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()

	ar := packp.NewAdvRefs()
	if withServicePrefix {
		ar.Prefix = [][]byte{[]byte("# service=" + uploadPackService), pktline.Flush}
	}
	// capability.List.Set only fails for a capability that takes no argument
	// being given one, or the reverse; these calls are spelled correctly by
	// construction, so their errors are asserted away.
	_ = ar.Capabilities.Set(capability.Agent, agentValue)
	_ = ar.Capabilities.Set(capability.OFSDelta)
	_ = ar.Capabilities.Set(capability.NoProgress)
	if caps.Shallow {
		_ = ar.Capabilities.Set(capability.Shallow)
	}
	if caps.AllowReachableSHA1 {
		_ = ar.Capabilities.Set(capability.AllowReachableSHA1InWant)
	}

	if err := addReferences(repo, ar); err != nil {
		return err
	}
	if err := addHead(repo, ar); err != nil {
		return err
	}
	return ar.Encode(w)
}

// addReferences lists every hash reference under refs/ in ar, with a
// peeled entry for each one that names a tag object. The caller holds
// repo.mu.
func addReferences(repo *Repo, ar *packp.AdvRefs) error {
	iter, err := repo.st.IterReferences()
	if err != nil {
		return err
	}
	return iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference || ref.Name() == plumbing.HEAD {
			return nil
		}
		ar.References[ref.Name().String()] = ref.Hash()
		if tag, tagErr := object.GetTag(repo.st, ref.Hash()); tagErr == nil {
			ar.Peeled[ref.Name().String()] = tag.Target
		}
		return nil
	})
}

// addHead sets ar.Head to the commit HEAD resolves to and, when HEAD is
// symbolic, records the symref capability for it. A HEAD that names an
// unborn branch advertises nothing, as git does. The caller holds repo.mu.
func addHead(repo *Repo, ar *packp.AdvRefs) error {
	ref, err := repo.st.Reference(plumbing.HEAD)
	if err != nil {
		// No HEAD at all is an empty advertisement, not a failure.
		return nil
	}
	if ref.Type() == plumbing.SymbolicReference {
		if err := ar.AddReference(ref); err != nil {
			return err
		}
		ref, err = storer.ResolveReference(repo.st, ref.Target())
		if err != nil {
			// An unborn branch: HEAD exists but resolves to nothing yet.
			return nil
		}
	}
	h := ref.Hash()
	ar.Head = &h
	return nil
}

// readUploadRequest reads one upload-pack request off r through "done" or
// EOF, or reports closed on a leading flush, as go-git ends an ssh session.
// It drains to "done" itself because UploadRequest.Decode stops at the flush.
func readUploadRequest(r io.Reader) (*packp.UploadRequest, bool, error) {
	buf, closed, err := drainRequest(r)
	if err != nil || closed {
		return nil, closed, err
	}
	req := packp.NewUploadRequest()
	if decErr := req.Decode(buf); decErr != nil {
		return nil, false, decErr
	}
	return req, false, nil
}

// drainRequest copies pkt-lines from r into a buffer through the "done"
// line or EOF. It reports closed when the first line is a flush, or there
// is no line at all: the client's way of ending a session without a request.
func drainRequest(r io.Reader) (*bytes.Buffer, bool, error) {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	sc := pktline.NewScanner(r)
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if first && len(line) == 0 {
			return nil, true, nil
		}
		first = false
		var encErr error
		if len(line) == 0 {
			encErr = enc.Flush()
		} else {
			encErr = enc.Encode(line)
		}
		if encErr != nil {
			return nil, false, encErr
		}
		if string(bytes.TrimSuffix(line, []byte("\n"))) == donePayload {
			break
		}
	}
	if scanErr := sc.Err(); scanErr != nil {
		return nil, false, scanErr
	}
	return &buf, first, nil
}

// buildReply renders the answer to req under the repository lock, reading
// only fault.ServeCommit. As in git, a want must be an advertised tip (not a
// peeled hash) or, under AllowReachableSHA1, reachable from one.
func buildReply(repo *Repo, caps Capabilities, req *packp.UploadRequest, fault Fault) (uploadPackReply, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()

	tips, err := advertisedTips(repo)
	if err != nil {
		return uploadPackReply{}, err
	}
	refused, err := refusedWant(repo, caps, req.Wants, tips)
	if err != nil {
		return uploadPackReply{}, err
	}
	if refused != plumbing.ZeroHash {
		return refusalReply(refused)
	}

	wants := req.Wants
	if fault.ServeCommit != plumbing.ZeroHash {
		wants = []plumbing.Hash{fault.ServeCommit}
	}
	objs, shallows, badRequest, err := selectObjects(repo, caps, req.Depth, wants)
	if err != nil || badRequest != "" {
		return uploadPackReply{badRequest: badRequest}, err
	}
	return renderReply(repo, !req.Depth.IsZero(), shallows, objs)
}

// refusalReply renders the ERR line that answers a want this server will
// not serve, in place of any NAK or pack.
func refusalReply(refused plumbing.Hash) (uploadPackReply, error) {
	var hdr bytes.Buffer
	if err := pktline.NewEncoder(&hdr).Encodef("ERR upload-pack: not our ref %s\n", refused); err != nil {
		return uploadPackReply{}, err
	}
	return uploadPackReply{header: hdr.Bytes()}, nil
}

// selectObjects decides what the pack holds for wants at depth: the full
// closure, the shallowObjects shape for the honored depth, or a badRequest
// reason for any other. The caller holds repo.mu.
func selectObjects(repo *Repo, caps Capabilities, depth packp.Depth, wants []plumbing.Hash) (
	[]plumbing.Hash, []plumbing.Hash, string, error,
) {
	switch {
	case depth.IsZero():
		objs, err := closure(repo.st, wants)
		return objs, nil, "", err
	case !caps.Shallow:
		return nil, nil, "deepen requested but shallow was not advertised", nil
	case depth != honoredDepth || len(wants) != 1:
		return nil, nil, "only deepen 1 with a single want is served", nil
	default:
		objs, shallows, err := shallowObjects(repo, wants[0])
		return objs, shallows, "", err
	}
}

// renderReply encodes the header (the shallow update when the request was
// shallow, then the NAK) and the packfile over objs. The caller holds
// repo.mu.
func renderReply(repo *Repo, shallow bool, shallows, objs []plumbing.Hash) (uploadPackReply, error) {
	var hdr bytes.Buffer
	if shallow {
		su := packp.ShallowUpdate{Shallows: shallows}
		if err := su.Encode(&hdr); err != nil {
			return uploadPackReply{}, err
		}
	}
	var nak packp.ServerResponse
	if err := nak.Encode(&hdr, false); err != nil {
		return uploadPackReply{}, err
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, repo.st, false).Encode(objs, packWindow); err != nil {
		return uploadPackReply{}, err
	}
	return uploadPackReply{header: hdr.Bytes(), pack: pack.Bytes()}, nil
}

// advertisedTips collects the hashes git accepts as a want without
// allow-*-sha1-in-want: reference hashes and HEAD, never a peeled tag target.
// The caller holds repo.mu.
func advertisedTips(repo *Repo) ([]plumbing.Hash, error) {
	ar := packp.NewAdvRefs()
	if err := addReferences(repo, ar); err != nil {
		return nil, err
	}
	if err := addHead(repo, ar); err != nil {
		return nil, err
	}
	tips := make([]plumbing.Hash, 0, len(ar.References)+1)
	for _, h := range ar.References {
		tips = append(tips, h)
	}
	if ar.Head != nil {
		tips = append(tips, *ar.Head)
	}
	return tips, nil
}

// refusedWant returns the first want that is neither a tip nor, under
// AllowReachableSHA1, reachable from one, else plumbing.ZeroHash. The caller
// holds repo.mu.
func refusedWant(repo *Repo, caps Capabilities, wants, tips []plumbing.Hash) (plumbing.Hash, error) {
	var reachable []plumbing.Hash
	reachableKnown := false
	for _, w := range wants {
		if slices.Contains(tips, w) {
			continue
		}
		if !caps.AllowReachableSHA1 {
			return w, nil
		}
		if !reachableKnown {
			var err error
			reachable, err = closure(repo.st, tips)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			reachableKnown = true
		}
		if !slices.Contains(reachable, w) {
			return w, nil
		}
	}
	return plumbing.ZeroHash, nil
}

// shallowObjects lists what a depth-1 fetch of want ships: the tag object if
// any, the commit and its tree closure, no parents; that commit is the one
// shallow. The caller holds repo.mu.
func shallowObjects(repo *Repo, want plumbing.Hash) ([]plumbing.Hash, []plumbing.Hash, error) {
	var objs []plumbing.Hash
	commitHash := want
	if tag, tagErr := object.GetTag(repo.st, want); tagErr == nil {
		objs = append(objs, want)
		commitHash = tag.Target
	}
	commit, err := object.GetCommit(repo.st, commitHash)
	if err != nil {
		return nil, nil, err
	}
	treeObjs, err := closure(repo.st, []plumbing.Hash{commit.TreeHash})
	if err != nil {
		return nil, nil, err
	}
	objs = append(objs, commitHash)
	objs = append(objs, treeObjs...)
	return objs, []plumbing.Hash{commitHash}, nil
}

// closure lists every object reachable from roots once, skipping submodules as
// git does. It avoids revlist.Objects, whose tree walker refuses the hostile
// entry names RawTreeCommit exists to ship.
func closure(st storer.EncodedObjectStorer, roots []plumbing.Hash) ([]plumbing.Hash, error) {
	seen := make(map[plumbing.Hash]bool)
	var out []plumbing.Hash
	pending := slices.Clone(roots)
	for len(pending) > 0 {
		h := pending[0]
		pending = pending[1:]
		if seen[h] {
			continue
		}
		seen[h] = true
		obj, err := st.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
		next, err := children(st, obj)
		if err != nil {
			return nil, err
		}
		pending = append(pending, next...)
	}
	return out, nil
}

// children lists the objects obj refers to directly; see closure.
func children(st storer.EncodedObjectStorer, obj plumbing.EncodedObject) ([]plumbing.Hash, error) {
	decoded, err := object.DecodeObject(st, obj)
	if err != nil {
		return nil, err
	}
	switch o := decoded.(type) {
	case *object.Commit:
		return append([]plumbing.Hash{o.TreeHash}, o.ParentHashes...), nil
	case *object.Tag:
		return []plumbing.Hash{o.Target}, nil
	case *object.Tree:
		var next []plumbing.Hash
		for _, e := range o.Entries {
			if e.Mode != filemode.Submodule {
				next = append(next, e.Hash)
			}
		}
		return next, nil
	default:
		return nil, nil
	}
}
