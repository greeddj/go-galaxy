package treearchive

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// linkMaxHops bounds the symlinks one target may pass through before it
	// is refused as a loop; it must equal manifest's chainMaxLinkHops, or a
	// written artifact could carry a chain that check refuses.
	linkMaxHops = 8

	modeFile    = 0o644
	modeExec    = 0o755
	modeDir     = 0o755
	modeSymlink = 0o777
)

// Options shapes one plan: Root is the repository path walked from, Reserved
// the count of lead documents charged to the entry budget up front, Digests
// asks for per-blob sha256 in Rows, Subject names the tree in link warnings.
type Options struct {
	Root     string
	Subject  string
	Rules    Rules
	Reserved int64
	Digests  bool
}

// Row is one planned entry as a listing sees it: its tree-relative name,
// whether it is a directory, and - for a file or a link to one, when
// Options.Digests was set - the lowercase hex sha256 of the blob it carries.
type Row struct {
	Name   string
	Digest string
	Dir    bool
}

// plannedEntry is one tar entry decided by the plan and written by Write.
// src is the repository path streamed for a regular file; it is empty for a
// directory and a symlink.
type plannedEntry struct {
	name     string
	src      string
	linkname string
	size     int64
	mode     int64
	typeflag byte
}

// Plan is the decided shape of one artifact: entries in write order, their
// listing rows and the walk's warnings. It keeps its Source, which Write reads
// again.
type Plan struct {
	src      Source
	dirs     map[string][]Entry
	digests  map[string]string
	root     string
	subject  string
	rows     []Row
	entries  []plannedEntry
	warnings []string
	rules    Rules
	total    int64
	count    int64
	hash     bool
}

// Rows returns the planned entries as listing rows, in write order, without
// a row for the root itself.
func (p *Plan) Rows() []Row { return p.rows }

// Warnings returns what the walk skipped, for the caller to print.
func (p *Plan) Warnings() []string { return p.warnings }

// PlanTree walks src under opts.Root and decides every entry of the
// artifact. Nothing is written.
func PlanTree(ctx context.Context, src Source, opts Options) (*Plan, error) {
	subject := opts.Subject
	if subject == "" {
		subject = "the tree"
	}
	p := &Plan{
		src:     src,
		dirs:    make(map[string][]Entry),
		digests: make(map[string]string),
		root:    opts.Root,
		subject: subject,
		rules:   opts.Rules,
		count:   opts.Reserved,
		hash:    opts.Digests,
	}
	if err := p.walk(ctx, RootName); err != nil {
		return nil, err
	}
	return p, nil
}

// repoPath maps a tree-relative path onto the repository.
func (p *Plan) repoPath(rel string) string {
	if rel == RootName {
		return p.root
	}
	return JoinPath(p.root, rel)
}

// display renders a tree-relative path as its repository path for a message
// or a warning.
func (p *Plan) display(rel string) string {
	return DisplayPath(p.repoPath(rel))
}

func (p *Plan) readDir(repo string) ([]Entry, error) {
	if entries, ok := p.dirs[repo]; ok {
		return entries, nil
	}
	entries, err := p.src.ReadDir(repo)
	if err != nil {
		return nil, err
	}
	p.dirs[repo] = entries
	return entries, nil
}

// lookup finds the entry at the tree-relative path rel.
func (p *Plan) lookup(rel string) (Entry, bool, error) {
	dir, name := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = RootName
	}
	entries, err := p.readDir(p.repoPath(dir))
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// walk collects the rows and entries under the tree-relative directory
// relDir in the tree's own order, parents before children.
func (p *Plan) walk(ctx context.Context, relDir string) error {
	entries, err := p.readDir(p.repoPath(relDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := relJoin(relDir, e.Name)
		if strings.Count(p.repoPath(rel), "/")+1 > helpers.GitTreeMaxDepth {
			return fmt.Errorf("%w: %s", helpers.ErrGitTreeTooDeep, p.display(rel))
		}
		if err := p.visit(ctx, rel, e); err != nil {
			return err
		}
	}
	return nil
}

// visit handles one entry of the walk: the rules first, with a directory's
// rules applied to a submodule since that is what a checkout would show,
// then the kind's own recording.
func (p *Plan) visit(ctx context.Context, rel string, e Entry) error {
	if p.rules.Skip(rel, e.Kind == EntryDir || e.Kind == EntrySubmodule) {
		return nil
	}
	switch e.Kind {
	case EntryDir:
		if err := p.addEntry(rel, Row{Name: rel, Dir: true}, plannedEntry{name: rel, mode: modeDir, typeflag: tar.TypeDir}); err != nil {
			return err
		}
		return p.walk(ctx, rel)
	case EntryFile, EntryExecutable:
		return p.addFile(rel, e)
	case EntrySubmodule:
		p.warnings = append(p.warnings, "skipping submodule "+p.display(rel)+": submodules are never fetched")
		return nil
	case EntrySymlink:
		return p.addSymlink(rel, e)
	default:
		return fmt.Errorf("%w: %s has an unknown entry kind %d", helpers.ErrGitTreeEntryInvalid, p.display(rel), e.Kind)
	}
}

// addEntry charges the name and count budgets and records one row with its
// tar entry.
func (p *Plan) addEntry(rel string, row Row, entry plannedEntry) error {
	if len(rel) > helpers.ArchiveMaxEntryNameLen {
		return fmt.Errorf("%w: an entry names itself in %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, len(rel), helpers.ArchiveMaxEntryNameLen)
	}
	p.count++
	if p.count > helpers.ArchiveMaxEntryCount {
		return fmt.Errorf("%w: more than %d entries", helpers.ErrArchiveTooManyEntries, helpers.ArchiveMaxEntryCount)
	}
	p.rows = append(p.rows, row)
	p.entries = append(p.entries, entry)
	return nil
}

// addFile charges the size budgets against the declared size, hashes the
// blob when digests were asked for and records a file row.
func (p *Plan) addFile(rel string, e Entry) error {
	if err := p.chargeSize(rel, e.Size); err != nil {
		return err
	}
	digest, err := p.hashBlob(rel, e.Size)
	if err != nil {
		return err
	}
	mode := int64(modeFile)
	if e.Kind == EntryExecutable {
		mode = modeExec
	}
	return p.addEntry(rel, Row{Name: rel, Digest: digest},
		plannedEntry{name: rel, src: p.repoPath(rel), size: e.Size, mode: mode, typeflag: tar.TypeReg})
}

// chargeSize charges one file's declared size against the per-entry and
// the per-archive caps, before a byte of it is read.
func (p *Plan) chargeSize(rel string, size int64) error {
	if size > helpers.ArchiveMaxEntrySize {
		return fmt.Errorf("%w: %s is %d bytes", helpers.ErrArchiveEntryIsTooLarge, p.display(rel), size)
	}
	if p.total+size > helpers.ArchiveMaxTotalSize {
		return fmt.Errorf("%w: %d bytes", helpers.ErrArchiveExceedsMaxSize, helpers.ArchiveMaxTotalSize)
	}
	p.total += size
	return nil
}

// hashBlob digests the blob at rel, which must stream exactly its declared
// size. Without Options.Digests it returns "" and reads nothing; Write applies
// the same size check.
func (p *Plan) hashBlob(rel string, declared int64) (string, error) {
	if !p.hash {
		return "", nil
	}
	repo := p.repoPath(rel)
	if digest, ok := p.digests[repo]; ok {
		return digest, nil
	}
	r, err := p.src.Open(repo)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", p.display(rel), err)
	}
	if n != declared {
		return "", fmt.Errorf("%w: %s streamed %d bytes where the tree declares %d",
			helpers.ErrGitCommitMismatch, p.display(rel), n, declared)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	p.digests[repo] = digest
	return digest, nil
}

// addSymlink records a link as a tar symlink pointing straight at the final
// entry its chain resolves to. A target the artifact will not carry is skipped
// with a warning; a dangling or looping link is refused.
func (p *Plan) addSymlink(rel string, e Entry) error {
	res, err := p.resolveLink(rel, e)
	if err != nil {
		return err
	}
	if res.reason == "" {
		if res.kind == EntryDir && p.rules.Skip(rel, true) {
			return nil
		}
		res.reason = p.skipReason(rel, res)
	}
	if res.reason != "" {
		p.warnings = append(p.warnings, "skipping symlink "+p.display(rel)+": "+res.reason)
		return nil
	}
	linkname := relativeLink(path.Dir(rel), res.final)
	if len(linkname) > helpers.ArchiveMaxEntryNameLen {
		return fmt.Errorf("%w: the link target of %s is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, p.display(rel), len(linkname), helpers.ArchiveMaxEntryNameLen)
	}
	entry := plannedEntry{name: rel, linkname: linkname, mode: modeSymlink, typeflag: tar.TypeSymlink}
	if res.kind == EntryDir {
		return p.addEntry(rel, Row{Name: rel, Dir: true}, entry)
	}
	digest, err := p.hashBlob(res.final, res.size)
	if err != nil {
		return err
	}
	return p.addEntry(rel, Row{Name: rel, Digest: digest}, entry)
}

// skipReason names why a resolved link is still left out: a target the
// extractor would refuse (its own directory, spelled "."), or one the rules
// keep out of the artifact, which the chain check could not follow.
func (p *Plan) skipReason(rel string, res linkResult) string {
	switch {
	case res.final == path.Dir(rel):
		return "target resolves to its own directory, which the extractor refuses"
	case p.rules.Excludes(res.final, res.kind == EntryDir):
		return "target is excluded from the build"
	default:
		return ""
	}
}

// linkResult is what resolving one link yields: the final tree-relative
// path with its kind and declared size, or a reason the link is skipped.
type linkResult struct {
	final  string
	reason string
	size   int64
	kind   EntryKind
}

// linkState is the resolution in progress: the link whose target is being
// followed, that target as read, and the hops spent so far.
type linkState struct {
	cur    string
	target string
	hops   int
}

// resolveLink follows the link at rel to the real entry it names, through
// directory symlinks on the way and symlinks at the end, up to linkMaxHops.
func (p *Plan) resolveLink(rel string, e Entry) (linkResult, error) {
	target, err := p.readLinkTarget(rel, e)
	if err != nil {
		return linkResult{}, err
	}
	outside := "target outside " + p.subject
	st := linkState{cur: rel, target: target, hops: 1}
	for {
		if path.IsAbs(st.target) {
			return linkResult{reason: outside}, nil
		}
		resolved := path.Join(path.Dir(st.cur), st.target)
		if resolved == RootName || resolved == ".." || strings.HasPrefix(resolved, "../") {
			return linkResult{reason: outside}, nil
		}
		res, again, err := p.followComponents(rel, resolved, &st)
		if err != nil || !again {
			return res, err
		}
	}
}

// followComponents walks resolved component by component. Meeting a symlink
// on the way rewrites st to continue from it and reports again; otherwise
// the walk ends at the final entry or at a refusal.
func (p *Plan) followComponents(rel, resolved string, st *linkState) (linkResult, bool, error) {
	components := strings.Split(resolved, "/")
	prefix := ""
	for i, c := range components {
		prefix = JoinPath(prefix, c)
		entry, ok, err := p.lookup(prefix)
		if err != nil {
			return linkResult{}, false, err
		}
		if !ok {
			return linkResult{}, false, fmt.Errorf("%w: %s points at %s, which does not exist",
				helpers.ErrGitSymlinkUnresolvable, p.display(rel), p.display(prefix))
		}
		if entry.Kind == EntrySymlink {
			err := p.hop(rel, prefix, entry, strings.Join(components[i+1:], "/"), st)
			return linkResult{}, err == nil, err
		}
		res, done, err := p.stepEntry(rel, prefix, entry, i == len(components)-1)
		if done {
			return res, false, err
		}
	}
	return linkResult{}, false, fmt.Errorf("%w: %s resolves to nothing", helpers.ErrGitSymlinkUnresolvable, p.display(rel))
}

// hop moves the resolution onto the symlink met at prefix, carrying the
// components not yet walked along behind its target.
func (p *Plan) hop(rel, prefix string, entry Entry, rest string, st *linkState) error {
	st.hops++
	if st.hops > linkMaxHops {
		return fmt.Errorf("%w: %s resolves through more than %d links",
			helpers.ErrGitSymlinkUnresolvable, p.display(rel), linkMaxHops)
	}
	next, err := p.readLinkTarget(prefix, entry)
	if err != nil {
		return err
	}
	st.cur = prefix
	st.target = JoinPath(next, rest)
	return nil
}

// stepEntry judges one non-link component: a directory is walked through
// or, when last, is the answer; a file is the answer only when last; a
// submodule ends the resolution with a skip.
func (p *Plan) stepEntry(rel, prefix string, entry Entry, last bool) (linkResult, bool, error) {
	switch entry.Kind {
	case EntryDir:
		if last {
			return linkResult{final: prefix, kind: EntryDir}, true, nil
		}
		return linkResult{}, false, nil
	case EntryFile, EntryExecutable:
		if last {
			return linkResult{final: prefix, kind: entry.Kind, size: entry.Size}, true, nil
		}
		return linkResult{}, true, fmt.Errorf("%w: %s passes through %s, which is not a directory",
			helpers.ErrGitSymlinkUnresolvable, p.display(rel), p.display(prefix))
	case EntrySubmodule:
		return linkResult{reason: "target is a submodule, which is never fetched"}, true, nil
	case EntrySymlink:
		// followComponents takes every symlink onto hop before it gets here.
		fallthrough
	default:
		return linkResult{}, true, fmt.Errorf("%w: %s has an unknown entry kind %d",
			helpers.ErrGitTreeEntryInvalid, p.display(prefix), entry.Kind)
	}
}

// readLinkTarget reads a link's blob, which is its target, refusing an
// empty one, one with a NUL and one past the name cap.
func (p *Plan) readLinkTarget(rel string, e Entry) (string, error) {
	if e.Size > helpers.ArchiveMaxEntryNameLen {
		return "", fmt.Errorf("%w: the link target of %s is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, p.display(rel), e.Size, helpers.ArchiveMaxEntryNameLen)
	}
	r, err := p.src.Open(p.repoPath(rel))
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, helpers.ArchiveMaxEntryNameLen+1))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", p.display(rel), err)
	}
	return checkLinkTarget(p.display(rel), data)
}

// checkLinkTarget applies the shape rules to a link's raw target.
func checkLinkTarget(display string, data []byte) (string, error) {
	switch {
	case len(data) > helpers.ArchiveMaxEntryNameLen:
		return "", fmt.Errorf("%w: the link target of %s is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, display, len(data), helpers.ArchiveMaxEntryNameLen)
	case len(data) == 0:
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetIsEmpty, display)
	case bytes.IndexByte(data, 0) >= 0:
		return "", fmt.Errorf("%w: the target of %s contains a NUL", helpers.ErrSymlinkTarget, display)
	}
	return string(data), nil
}

// relativeLink renders target, a clean tree-relative path, relative to
// fromDir (RootName for the root), as the Linkname must read for both the
// extractor and the chain check to land on target.
func relativeLink(fromDir, target string) string {
	var from []string
	if fromDir != RootName {
		from = strings.Split(fromDir, "/")
	}
	to := strings.Split(target, "/")
	i := 0
	for i < len(from) && i < len(to) && from[i] == to[i] {
		i++
	}
	parts := make([]string, 0, len(from)-i+len(to)-i)
	for range from[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, to[i:]...)
	return strings.Join(parts, "/")
}
