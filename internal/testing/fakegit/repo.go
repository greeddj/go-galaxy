package fakegit

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Names and values the AddCollection and AddRole fixtures write, spelled as
// literals rather than imported from collectionbuild or rolebuild, so the
// double cannot inherit a drift of the code under test.
const (
	galaxyFileName       = "galaxy.yml"
	readmeFileName       = "README.md"
	runtimeFileName      = "meta/runtime.yml"
	moduleFileName       = "plugins/modules/hello.py"
	roleMetaFileName     = "meta/main.yml"
	roleTasksFileName    = "tasks/main.yml"
	roleDefaultsFileName = "defaults/main.yml"
	fixtureAuthor        = "fakegit"
	fixtureEmail         = "fakegit@example.invalid"
	// fixtureFileMode is the mode AddCollection stamps on every file it
	// writes, so a fixture's tree hash does not depend on a caller's umask.
	fixtureFileMode os.FileMode = 0o644
	// fixtureDirMode is the mode WriteFile creates intermediate directories
	// with; memfs records no mode for directories in a tree, so the value
	// only has to be one MkdirAll accepts.
	fixtureDirMode os.FileMode = 0o755
)

// fixedTime stamps every object this package writes, so a fixture's commit
// hash is the same on every run and every machine.
//
//nolint:gochecknoglobals // fixed, not runtime-mutable, state.
var fixedTime = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

// FixedTime returns the instant every commit, tag and signature this package
// writes carries, so a golden test that computes a hash by hand shares the
// fake's clock. Nothing in this package calls time.Now for content.
func FixedTime() time.Time {
	return fixedTime
}

// Repo is an in-memory git repository a Server serves, built through a memfs
// worktree; RawTreeCommit writes hostile trees no worktree can. Builders fail
// via tb, and mu serializes them against the Server reading the storage.
type Repo struct {
	tb   testing.TB
	repo *git.Repository
	st   *memory.Storage
	fs   billy.Filesystem
	mu   sync.Mutex
}

// NewRepo creates an empty repository with HEAD on refs/heads/master. Taking
// testing.TB, like New, keeps the builder unreachable from production code.
func NewRepo(tb testing.TB) *Repo {
	tb.Helper()
	st := memory.NewStorage()
	fs := memfs.New()
	repo, err := git.Init(st, fs)
	if err != nil {
		tb.Fatalf("fakegit: init repository: %v", err)
	}
	return &Repo{tb: tb, repo: repo, st: st, fs: fs}
}

// WriteFile writes content at p in the worktree with mode, creating parent
// directories, and stages it. mode 0o755 stages an executable entry, any
// other mode a regular one; go-git records nothing finer.
func (r *Repo) WriteFile(p string, mode os.FileMode, content []byte) {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if dir := path.Dir(p); dir != "." {
		if err := r.fs.MkdirAll(dir, fixtureDirMode); err != nil {
			r.tb.Fatalf("fakegit: mkdir %s: %v", dir, err)
		}
	}
	if err := util.WriteFile(r.fs, p, content, mode); err != nil {
		r.tb.Fatalf("fakegit: write %s: %v", p, err)
	}
	r.stage(p)
}

// Symlink creates a symbolic link at p pointing at target and stages it.
// The target is recorded verbatim, so an absolute or escaping target is
// written exactly as given: the code under test decides what to do with it.
func (r *Repo) Symlink(target, p string) {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if dir := path.Dir(p); dir != "." {
		if err := r.fs.MkdirAll(dir, fixtureDirMode); err != nil {
			r.tb.Fatalf("fakegit: mkdir %s: %v", dir, err)
		}
	}
	if err := r.fs.Symlink(target, p); err != nil {
		r.tb.Fatalf("fakegit: symlink %s: %v", p, err)
	}
	r.stage(p)
}

// Commit records the staged index on the branch HEAD names and returns its
// hash. Both signatures are fixed so go-git never reads the user's gitconfig;
// empty commits are allowed so a message alone yields a distinct commit.
func (r *Repo) Commit(msg string) plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	wt, err := r.repo.Worktree()
	if err != nil {
		r.tb.Fatalf("fakegit: worktree: %v", err)
	}
	sig := fixedSignature()
	h, err := wt.Commit(msg, &git.CommitOptions{Author: &sig, Committer: &sig, AllowEmptyCommits: true})
	if err != nil {
		r.tb.Fatalf("fakegit: commit: %v", err)
	}
	return h
}

// Branch points refs/heads/name at the commit at, creating or moving it.
func (r *Repo) Branch(name string, at plumbing.Hash) {
	r.tb.Helper()
	r.setRef(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), at))
}

// SetHEAD makes HEAD a symbolic reference to refs/heads/branch. The branch
// need not exist yet: a later Commit then creates it, as git does on an
// unborn branch. A detached HEAD is set with DetachHEAD.
func (r *Repo) SetHEAD(branch string) {
	r.tb.Helper()
	r.setRef(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch)))
}

// DetachHEAD points HEAD directly at the commit at, so the advertisement
// carries a HEAD hash but no symref capability.
func (r *Repo) DetachHEAD(at plumbing.Hash) {
	r.tb.Helper()
	r.setRef(plumbing.NewHashReference(plumbing.HEAD, at))
}

// Tag creates the lightweight tag refs/tags/name at the commit at.
func (r *Repo) Tag(name string, at plumbing.Hash) {
	r.tb.Helper()
	r.setRef(plumbing.NewHashReference(plumbing.NewTagReferenceName(name), at))
}

// AnnotatedTag creates a tag object name targeting at, points refs/tags/name
// at it and returns the tag object's hash, which the advertisement peels to at.
func (r *Repo) AnnotatedTag(name string, at plumbing.Hash) plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	sig := fixedSignature()
	ref, err := r.repo.CreateTag(name, at, &git.CreateTagOptions{Tagger: &sig, Message: name})
	if err != nil {
		r.tb.Fatalf("fakegit: annotated tag %s: %v", name, err)
	}
	return ref.Hash()
}

// Head returns the commit HEAD resolves to, or plumbing.ZeroHash when HEAD
// names a branch that has no commit yet.
func (r *Repo) Head() plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, err := storer.ResolveReference(r.st, plumbing.HEAD)
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

// Blob stores content as a blob object and returns its hash, for the entries
// a RawTreeCommit tree names.
func (r *Repo) Blob(content []byte) plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	obj := r.st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		r.tb.Fatalf("fakegit: blob writer: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		r.tb.Fatalf("fakegit: write blob: %v", err)
	}
	if err := w.Close(); err != nil {
		r.tb.Fatalf("fakegit: close blob: %v", err)
	}
	h, err := r.st.SetEncodedObject(obj)
	if err != nil {
		r.tb.Fatalf("fakegit: store blob: %v", err)
	}
	return h
}

// RawTreeCommit encodes entries as one git-sorted tree and a parentless commit
// on it, returning the commit hash. "..", ".git", "a/b", duplicates and
// submodules all encode: they are the trees the code under test must refuse.
func (r *Repo) RawTreeCommit(entries []object.TreeEntry) plumbing.Hash {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	sorted := make([]object.TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Sort(object.TreeEntrySorter(sorted))

	tree := &object.Tree{Entries: sorted}
	treeObj := r.st.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		r.tb.Fatalf("fakegit: encode raw tree: %v", err)
	}
	treeHash, err := r.st.SetEncodedObject(treeObj)
	if err != nil {
		r.tb.Fatalf("fakegit: store raw tree: %v", err)
	}

	sig := fixedSignature()
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   "raw tree\n",
		TreeHash:  treeHash,
	}
	commitObj := r.st.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		r.tb.Fatalf("fakegit: encode raw commit: %v", err)
	}
	commitHash, err := r.st.SetEncodedObject(commitObj)
	if err != nil {
		r.tb.Fatalf("fakegit: store raw commit: %v", err)
	}
	return commitHash
}

// AddCollection stages a minimal collection source tree under dir ("" for the
// root): galaxy.yml with deps and buildIgnore, README.md, one module and
// meta/runtime.yml. It does not commit, so several can share one Commit.
func (r *Repo) AddCollection(dir, namespace, name, version string, deps map[string]string, buildIgnore []string) {
	r.tb.Helper()
	join := func(p string) string {
		if dir == "" {
			return p
		}
		return path.Join(dir, p)
	}
	r.WriteFile(join(galaxyFileName), fixtureFileMode, galaxyYML(namespace, name, version, deps, buildIgnore))
	r.WriteFile(join(readmeFileName), fixtureFileMode, []byte("# "+namespace+"."+name+"\n"))
	r.WriteFile(join(moduleFileName), fixtureFileMode, []byte("#!/usr/bin/python\nDOCUMENTATION = ''\n"))
	r.WriteFile(join(runtimeFileName), fixtureFileMode, []byte("---\nrequires_ansible: '>=2.15.0'\n"))
}

// AddRole stages a minimal role tree at the repository root, the only place a
// role lives: meta/main.yml with deps and an optional role_name, tasks/main.yml
// and defaults/main.yml. It does not commit.
func (r *Repo) AddRole(deps []string, roleName string) {
	r.tb.Helper()
	r.WriteFile(roleMetaFileName, fixtureFileMode, roleMetaYML(deps, roleName))
	r.WriteFile(roleTasksFileName, fixtureFileMode, []byte("- debug: msg=hi\n"))
	r.WriteFile(roleDefaultsFileName, fixtureFileMode, []byte("---\n"))
}

// stage adds p to the index. The caller holds r.mu.
func (r *Repo) stage(p string) {
	r.tb.Helper()
	wt, err := r.repo.Worktree()
	if err != nil {
		r.tb.Fatalf("fakegit: worktree: %v", err)
	}
	if _, err := wt.Add(p); err != nil {
		r.tb.Fatalf("fakegit: stage %s: %v", p, err)
	}
}

// setRef stores ref, replacing any reference of the same name.
func (r *Repo) setRef(ref *plumbing.Reference) {
	r.tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.st.SetReference(ref); err != nil {
		r.tb.Fatalf("fakegit: set %s: %v", ref.Name(), err)
	}
}

// fixedSignature is the author, committer and tagger of everything this
// package writes.
func fixedSignature() object.Signature {
	return object.Signature{Name: fixtureAuthor, Email: fixtureEmail, When: fixedTime}
}

// galaxyYML renders the galaxy.yml AddCollection writes. Dependencies are
// sorted so the tree hash is deterministic, and values are quoted since a
// constraint such as ">=1.0.0" is not a bare YAML scalar.
func galaxyYML(namespace, name, version string, deps map[string]string, buildIgnore []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "namespace: %s\nname: %s\nversion: %q\nreadme: %s\nauthors:\n  - %s\n",
		namespace, name, version, readmeFileName, fixtureAuthor)
	if len(deps) == 0 {
		b.WriteString("dependencies: {}\n")
	} else {
		keys := make([]string, 0, len(deps))
		for k := range deps {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("dependencies:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %q: %q\n", k, deps[k])
		}
	}
	if len(buildIgnore) == 0 {
		b.WriteString("build_ignore: []\n")
	} else {
		b.WriteString("build_ignore:\n")
		for _, pattern := range buildIgnore {
			fmt.Fprintf(&b, "  - %q\n", pattern)
		}
	}
	return []byte(b.String())
}

// roleMetaYML renders the meta/main.yml AddRole writes. Dependencies are
// quoted so a spec carrying a colon, such as a repository URL, stays one
// YAML string.
func roleMetaYML(deps []string, roleName string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "galaxy_info:\n  author: %s\n", fixtureAuthor)
	if roleName != "" {
		fmt.Fprintf(&b, "  role_name: %s\n", roleName)
	}
	if len(deps) == 0 {
		b.WriteString("dependencies: []\n")
		return []byte(b.String())
	}
	b.WriteString("dependencies:\n")
	for _, dep := range deps {
		fmt.Fprintf(&b, "  - %q\n", dep)
	}
	return []byte(b.String())
}
