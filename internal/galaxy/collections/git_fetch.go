package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// gitFetchToCache rebuilds a pinned git collection's artifact from its commit
// on a cache miss, restricted to its identity: that check is a git artifact's
// only attribution, so another identity is ErrGitArtifactIdentityMismatch.
func gitFetchToCache(ctx context.Context, deps installDeps, col collection, useCache bool) (downloadResult, error) {
	runtime := deps.runtime
	if runtime == nil || runtime.Git == nil {
		return downloadResult{}, fmt.Errorf("%w: no git client is wired into this run", helpers.ErrConfigIsNil)
	}
	req, display, err := gitRefetchRequest(deps, col)
	if err != nil {
		return downloadResult{}, err
	}

	start := time.Now()
	gitCtx, cancel := context.WithTimeout(ctx, runtime.GitDeadline())
	defer cancel()
	result, err := runtime.Git.Acquire(gitCtx, req)
	if err != nil {
		return downloadResult{}, artifactDeadlineError(ctx, gitCtx, runtime.GitDeadline(), err)
	}
	runtime.Output.DebugSincef(start, "Fetch %s@%s for %s (%d bytes)", display, req.Commit, col.key(), result.BytesFetched)
	runtime.Metrics.AddBytesDownloaded(result.BytesFetched)
	for _, warning := range result.Warnings {
		runtime.Output.Warnf("%s: %s", display, warning)
	}
	built, err := singleBuiltIdentity(result.Collections, col, display)
	if err != nil {
		return downloadResult{}, err
	}
	if !useCache || deps.artifacts == nil {
		return downloadResult{Path: built.ArtifactPath, SHA: built.ArtifactSHA, Cleanup: built.Cleanup}, nil
	}
	stored, err := commitDownload(ctx, deps.artifacts, artifactKey(col), built.ArtifactPath, built.ArtifactSHA, built.Cleanup)
	if err != nil {
		return downloadResult{}, err
	}
	runtime.Metrics.AddCacheMiss()
	return stored, nil
}

// gitRefetchRequest builds the acquisition for col's pinned locator,
// restricted to col's own identity, and the repository's display name for
// messages (the locator URL as written, not the parsed form).
func gitRefetchRequest(deps installDeps, col collection) (gitsource.Request, string, error) {
	loc, err := col.gitLocator()
	if err != nil {
		return gitsource.Request{}, "", err
	}
	if !loc.Pinned() {
		return gitsource.Request{}, "", fmt.Errorf("%w: %s is not pinned to a commit", helpers.ErrInvalidGitLocator, col.key())
	}
	u, err := gitsource.ParseURL(loc.URL)
	if err != nil {
		return gitsource.Request{}, "", err
	}
	ref, err := gitsource.ParseRef(col.Ref)
	if err != nil {
		return gitsource.Request{}, "", err
	}
	cred, _ := gitsource.MatchCredential(u, deps.runtime.GitCredentials)
	return gitsource.Request{
		Only:     &gitsource.Identity{Namespace: col.Namespace, Name: col.Name, Version: col.Version},
		URL:      u,
		Ref:      ref,
		Subdir:   loc.Subdir,
		Commit:   loc.Commit,
		Auth:     cred,
		TempFile: gitTempFile(deps.collectionDeps),
	}, helpers.URLForMessage(loc.URL), nil
}

// singleBuiltIdentity returns the one collection the restricted acquisition
// built, refusing (and releasing every built artifact) when the count or the
// identity differs from col.
func singleBuiltIdentity(built []gitsource.Collection, col collection, display string) (gitsource.Collection, error) {
	if len(built) != 1 {
		releaseBuilt(built)
		return gitsource.Collection{}, fmt.Errorf("%w: %s built %d collections for %s, want exactly one",
			helpers.ErrGitArtifactIdentityMismatch, display, len(built), col.key())
	}
	one := built[0]
	if one.Identity() != (gitsource.Identity{Namespace: col.Namespace, Name: col.Name, Version: col.Version}) {
		releaseBuilt(built)
		return gitsource.Collection{}, fmt.Errorf("%w: %s built %s.%s@%s for %s",
			helpers.ErrGitArtifactIdentityMismatch, display, one.Namespace, one.Name, one.Version, col.key())
	}
	return one, nil
}
