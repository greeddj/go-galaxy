package collections

import (
	"context"
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// Two forty-hex commits for the git rows below: the one a lockfile pinned and
// the one the remote's branch has moved on to.
const (
	gitOutdatedOldCommit = "0123456789abcdef0123456789abcdef01234567"
	gitOutdatedNewCommit = "fedcba9876543210fedcba9876543210fedcba98"
	gitOutdatedRepoURL   = "https://git.example/acme/app.git"
)

// errStubGitAcquire is the stub client's answer to an Acquire it must never
// receive: outdated only ever advertises, so a fetch here is a defect.
var errStubGitAcquire = errors.New("stub git client: Acquire must not be called by outdated")

// stubGitClient is the smallest gitsource.Client lookupGitOutdated can run
// with: every advertisement answers tip and is counted, so a row can pin that
// a commit-pinned entry costs no round trip.
type stubGitClient struct {
	tip        string
	advertises int
}

func (c *stubGitClient) Advertise(_ context.Context, _ gitsource.URL, ref gitsource.Ref, _ gitsource.Credential) (string, string, error) {
	c.advertises++
	return c.tip, ref.Name, nil
}

func (c *stubGitClient) Acquire(context.Context, gitsource.Request) (gitsource.Result, error) {
	return gitsource.Result{}, errStubGitAcquire
}

func (c *stubGitClient) AcquireRole(context.Context, gitsource.RoleRequest) (gitsource.RoleResult, error) {
	return gitsource.RoleResult{}, errStubGitAcquire
}

type lookupGitOutdatedCase struct {
	client         *stubGitClient
	wantErr        error
	name           string
	ref            string
	wantLatest     string
	wantAdvertises int
	wantNewer      bool
}

// lookupGitOutdatedCases covers lookupGitOutdated's shapes: a moved branch is
// commit drift in full hashes, a commit ref is current with no advertisement,
// and a run with no git client is a configuration defect, not a remote failure.
func lookupGitOutdatedCases() []lookupGitOutdatedCase {
	return []lookupGitOutdatedCase{
		{
			name:           "branch moved",
			ref:            "main",
			client:         &stubGitClient{tip: gitOutdatedNewCommit},
			wantLatest:     gitOutdatedNewCommit,
			wantNewer:      true,
			wantAdvertises: 1,
		},
		{
			name:           "branch unmoved",
			ref:            "main",
			client:         &stubGitClient{tip: gitOutdatedOldCommit},
			wantLatest:     gitOutdatedOldCommit,
			wantAdvertises: 1,
		},
		{
			name:       "commit ref never advertises",
			ref:        gitOutdatedOldCommit,
			client:     &stubGitClient{tip: gitOutdatedNewCommit},
			wantLatest: gitOutdatedOldCommit,
		},
		{
			name:    "no git client wired",
			ref:     "main",
			wantErr: helpers.ErrConfigIsNil,
		},
	}
}

func TestLookupGitOutdated(t *testing.T) {
	t.Parallel()
	for _, tc := range lookupGitOutdatedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkLookupGitOutdated(t, tc)
		})
	}
}

func checkLookupGitOutdated(t *testing.T, tc lookupGitOutdatedCase) {
	t.Helper()
	runtime := infra.New(noopPrinter{}, nil)
	if tc.client != nil {
		runtime.Git = tc.client
	}
	deps := newCollectionDeps(&config.Config{}, runtime, nil)
	entry := lookupGitOutdated(context.Background(), deps, lockfile.Entry{
		Name:   "acme.app",
		Type:   lockfile.TypeGit,
		Source: gitOutdatedRepoURL,
		Ref:    tc.ref,
		Commit: gitOutdatedOldCommit,
	})

	if tc.wantErr != nil {
		if !errors.Is(entry.Err, tc.wantErr) {
			t.Fatalf("Err = %v, want errors.Is %v", entry.Err, tc.wantErr)
		}
		return
	}
	if entry.Err != nil {
		t.Fatalf("Err = %v, want nil", entry.Err)
	}
	if entry.Name != "acme.app" || entry.Locked != gitOutdatedOldCommit {
		t.Fatalf("entry = %+v, want Name acme.app and Locked %s", entry, gitOutdatedOldCommit)
	}
	if entry.Latest != tc.wantLatest {
		t.Fatalf("Latest = %q, want %q (the full commit, never an abbreviation)", entry.Latest, tc.wantLatest)
	}
	if entry.Newer != tc.wantNewer {
		t.Fatalf("Newer = %v, want %v", entry.Newer, tc.wantNewer)
	}
	if got := tc.client.advertises; got != tc.wantAdvertises {
		t.Fatalf("advertises = %d, want %d", got, tc.wantAdvertises)
	}
}
