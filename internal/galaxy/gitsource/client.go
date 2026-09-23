package gitsource

import (
	"context"
	"os"
)

// Identity is the namespace, name and exact version a built collection
// declares in its galaxy.yml.
type Identity struct {
	Namespace string
	Name      string
	Version   string
}

// TempFileFunc hands the builder a temp file for an artifact and its cleanup;
// the pipeline passes the artifact store's own TempFile, so committing is a
// rename and a killed run's leftover is swept like any download temp.
type TempFileFunc func(ctx context.Context) (*os.File, func(), error)

// Request is one acquisition: with Commit empty Ref is resolved, with Commit
// set exactly that commit is fetched and Ref is only a hint. Only restricts the
// build to one pinned collection, whose absence or changed identity is an error.
type Request struct {
	Only     *Identity
	TempFile TempFileFunc
	URL      URL
	Ref      Ref
	Subdir   string
	Commit   string
	Auth     Credential
}

// Collection is one built artifact: identity, subdir ("" for the root), raw
// galaxy.yml dependencies for the caller to validate, the temp tar.gz and its
// sha256. Cleanup removes the temp file and is the caller's duty on every path.
type Collection struct {
	Cleanup      func()
	Dependencies map[string]string
	Namespace    string
	Name         string
	Version      string
	Subdir       string
	ArtifactPath string
	ArtifactSHA  string
}

// Identity returns the collection's identity triple.
func (c Collection) Identity() Identity {
	return Identity{Namespace: c.Namespace, Name: c.Name, Version: c.Version}
}

// Result is an acquisition's outcome: the commit, the full ref name reached
// ("HEAD" when the remote named no target), the built collections, warnings
// for the caller to print, and the bytes the fetch wrote to disk.
type Result struct {
	Commit       string
	RefName      string
	Collections  []Collection
	Warnings     []string
	BytesFetched int64
}

// RoleRequest is one role acquisition, Commit and Ref as in Request; the
// repository root is the role and the install name is the caller's, as in
// ansible, so nothing in the tree is compared against it.
type RoleRequest struct {
	TempFile TempFileFunc
	URL      URL
	Ref      Ref
	Commit   string
	Auth     Credential
}

// RoleDependency is one meta dependency normalized to ansible's spec keys,
// each as written and empty when absent, and otherwise unjudged: the caller
// validates it through the requirements grammar.
type RoleDependency struct {
	Src     string
	Scm     string
	Version string
	Name    string
}

// RoleResult is the built role: commit, ref name, meta dependencies, temp
// tar.gz and sha256, warnings and fetched bytes. GalaxyRoleName is informational
// (the install directory is the requirement's name); Cleanup is the caller's duty.
type RoleResult struct {
	Cleanup        func()
	Dependencies   []RoleDependency
	Commit         string
	RefName        string
	GalaxyRoleName string
	ArtifactPath   string
	ArtifactSHA    string
	Warnings       []string
	BytesFetched   int64
}

// Client is the seam between the install pipeline and a git remote: Advertise
// resolves a ref with no pack transfer, Acquire and AcquireRole build artifacts;
// every error wraps a helpers git sentinel and all three honor ctx.
type Client interface {
	Advertise(ctx context.Context, u URL, ref Ref, auth Credential) (commit, refName string, err error)
	Acquire(ctx context.Context, req Request) (Result, error)
	AcquireRole(ctx context.Context, req RoleRequest) (RoleResult, error)
}
