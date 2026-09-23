package collections_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/rolebuild"
	"github.com/greeddj/go-galaxy/internal/testing/faketree"
)

// fakeGitRole is the role a fake repository carries at one commit: its
// meta/main.yml dependencies as ansible writes them, an optional role_name, and
// extra files beside the minimal meta, tasks and defaults tree.
type fakeGitRole struct {
	files    map[string]string
	roleName string
	deps     []string
}

// metaMain renders the role's meta/main.yml.
func (r fakeGitRole) metaMain() string {
	var b strings.Builder
	b.WriteString("galaxy_info:\n  author: test\n")
	if r.roleName != "" {
		b.WriteString("  role_name: " + r.roleName + "\n")
	}
	b.WriteString("dependencies:")
	if len(r.deps) == 0 {
		b.WriteString(" []\n")
		return b.String()
	}
	b.WriteString("\n")
	for _, d := range r.deps {
		b.WriteString("  - " + d + "\n")
	}
	return b.String()
}

// tree is the role as a treearchive.Source the real builder reads.
func (r fakeGitRole) tree() *faketree.Tree {
	t := faketree.New().
		File("meta/main.yml", r.metaMain()).
		File("tasks/main.yml", "- debug:\n    msg: hi\n").
		File("defaults/main.yml", "greeting: hi\n")
	for p, data := range r.files {
		t.File(p, data)
	}
	return t
}

// addRole registers a role at commit for the repository; a commit may carry
// a role beside collections, as a repository may.
func (r *fakeGitRepo) addRole(commit string, role fakeGitRole) {
	if r.roles == nil {
		r.roles = make(map[string]fakeGitRole)
	}
	r.roles[commit] = role
}

// AcquireRole resolves the ref like Acquire and builds the commit's role
// through the real rolebuild.Build, so the cached artifact is byte for byte
// what production would build from the same tree.
func (c *fakeGitClient) AcquireRole(ctx context.Context, req gitsource.RoleRequest) (gitsource.RoleResult, error) {
	c.mu.Lock()
	c.roleAcquires++
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return gitsource.RoleResult{}, err
	}
	if req.TempFile == nil {
		return gitsource.RoleResult{}, errFakeGitNoTempFile
	}
	repo, commit, refName, err := c.resolve(req.URL, req.Ref, req.Auth)
	if err != nil {
		return gitsource.RoleResult{}, err
	}
	if req.Commit != "" {
		commit = req.Commit
	}
	role, ok := repo.roles[commit]
	if !ok {
		if !repo.hasCommit(commit) {
			return gitsource.RoleResult{}, fmt.Errorf("%w: %s", helpers.ErrGitCommitNotFound, commit)
		}
		return gitsource.RoleResult{}, fmt.Errorf("%w: commit %s carries no meta/main.yml", helpers.ErrRoleMetaNotFound, commit)
	}
	built, err := rolebuild.Build(ctx, role.tree(), rolebuild.TempFileFunc(req.TempFile))
	if err != nil {
		return gitsource.RoleResult{}, err
	}
	return gitsource.RoleResult{
		Cleanup:        built.Cleanup,
		Dependencies:   built.Meta.Dependencies,
		Commit:         commit,
		RefName:        refName,
		GalaxyRoleName: built.Meta.RoleName,
		ArtifactPath:   built.ArtifactPath,
		ArtifactSHA:    built.SHA256,
		Warnings:       built.Warnings,
		BytesFetched:   2048,
	}, nil
}

// roleAcquireCount reports how many role acquisitions the client served.
func (c *fakeGitClient) roleAcquireCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roleAcquires
}
