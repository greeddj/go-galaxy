package exitcode

import (
	"errors"
	"fmt"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// gitExitCases pins the exit class of every git-source sentinel, one row per
// sentinel; a git sentinel added to helpers needs a row here.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var gitExitCases = []exitCase{
	{name: "invalid git url", err: helpers.ErrInvalidGitURL, wantCode: ExitUsage},
	{name: "git url userinfo", err: helpers.ErrGitURLUserinfo, wantCode: ExitUsage},
	{name: "invalid git ref", err: helpers.ErrInvalidGitRef, wantCode: ExitUsage},
	{name: "abbreviated commit", err: helpers.ErrGitAbbreviatedCommit, wantCode: ExitUsage},
	{name: "invalid subdir", err: helpers.ErrInvalidGitSubdir, wantCode: ExitUsage},
	{name: "invalid locator", err: helpers.ErrInvalidGitLocator, wantCode: ExitUsage},
	{name: "name mismatch", err: helpers.ErrGitNameMismatch, wantCode: ExitUsage},
	{name: "collection not found", err: helpers.ErrGitCollectionNotFound, wantCode: ExitUsage},
	{name: "duplicate collection", err: helpers.ErrGitDuplicateCollection, wantCode: ExitUsage},
	{name: "galaxy.yml invalid", err: helpers.ErrGalaxyYMLInvalid, wantCode: ExitUsage},
	{name: "version not exact", err: helpers.ErrGitCollectionVersionNotExact, wantCode: ExitUsage},
	{name: "credential invalid", err: helpers.ErrGitCredentialInvalid, wantCode: ExitUsage},
	{name: "ssh no credential", err: helpers.ErrGitSSHNoCredential, wantCode: ExitUsage},
	{name: "transport failed", err: helpers.ErrGitTransportFailed, wantCode: ExitNetwork},
	{name: "auth failed", err: helpers.ErrGitAuthFailed, wantCode: ExitNetwork},
	{name: "ref not found", err: helpers.ErrGitRefNotFound, wantCode: ExitResolution},
	{name: "commit not found", err: helpers.ErrGitCommitNotFound, wantCode: ExitResolution},
	{name: "commit mismatch", err: helpers.ErrGitCommitMismatch, wantCode: ExitIntegrity},
	{name: "artifact identity mismatch", err: helpers.ErrGitArtifactIdentityMismatch, wantCode: ExitIntegrity},
	{name: "tree entry invalid", err: helpers.ErrGitTreeEntryInvalid, wantCode: ExitInstall},
	{name: "tree duplicate entry", err: helpers.ErrGitTreeDuplicateEntry, wantCode: ExitInstall},
	{name: "tree too deep", err: helpers.ErrGitTreeTooDeep, wantCode: ExitInstall},
	{name: "symlink unresolvable", err: helpers.ErrGitSymlinkUnresolvable, wantCode: ExitInstall},
	{name: "artifact self-check", err: helpers.ErrGitArtifactSelfCheck, wantCode: ExitInstall},
}

// TestGitSentinelsAreAllClassified checks every git sentinel both bare and
// wrapped, and refuses the generic fallback for any of them: a git sentinel
// that reaches ExitError is one this package forgot to place.
func TestGitSentinelsAreAllClassified(t *testing.T) {
	t.Parallel()
	for _, tt := range gitExitCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FromError(tt.err); got != tt.wantCode {
				t.Fatalf("FromError(bare) = %d, want %d", got, tt.wantCode)
			}
			wrapped := fmt.Errorf("context: %w", tt.err)
			if got := FromError(wrapped); got != tt.wantCode {
				t.Fatalf("FromError(wrapped) = %d, want %d", got, tt.wantCode)
			}
			if tt.wantCode == ExitError {
				t.Fatalf("a git sentinel must never classify as the generic fallback")
			}
		})
	}
}

// TestGitBuildFailureJoinedBehindInstallHeadline pins that a git build
// failure joined behind helpers.ErrInstallationFailed exits ExitInstall, and
// that an integrity sentinel joined alongside it still outranks the headline.
func TestGitBuildFailureJoinedBehindInstallHeadline(t *testing.T) {
	t.Parallel()
	joined := errors.Join(
		fmt.Errorf("%w for 1 collections", helpers.ErrInstallationFailed),
		fmt.Errorf("acme.app: %w: name %q", helpers.ErrGitTreeEntryInvalid, ".."),
	)
	if got := FromError(joined); got != ExitInstall {
		t.Fatalf("FromError(install headline + tree entry) = %d, want %d", got, ExitInstall)
	}
	withIntegrity := errors.Join(joined, helpers.ErrGitCommitMismatch)
	if got := FromError(withIntegrity); got != ExitIntegrity {
		t.Fatalf("FromError(install headline + commit mismatch) = %d, want %d", got, ExitIntegrity)
	}
}

// TestGitUsageOutranksNothingAboveIt pins the table position of the git usage
// members: a usage sentinel joined with a git transport failure classifies as
// the transport failure, because isNetworkError sits above isUsageError.
func TestGitUsageOutranksNothingAboveIt(t *testing.T) {
	t.Parallel()
	joined := errors.Join(helpers.ErrInvalidGitRef, helpers.ErrGitTransportFailed)
	if got := FromError(joined); got != ExitNetwork {
		t.Fatalf("FromError(usage + transport) = %d, want %d", got, ExitNetwork)
	}
}
