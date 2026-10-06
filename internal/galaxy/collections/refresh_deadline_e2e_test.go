package collections_test

// This file pins the advertisement --refresh makes for a recorded git pin: it
// runs under the git deadline, as the fetch after it does, and ends the run as
// that deadline rather than when the run's own context ends.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// stallingAdvertiser is the fake git client with an advertisement that
// answers only once its context ends, as a remote that stops responding.
type stallingAdvertiser struct {
	*fakeGitClient
}

func (stallingAdvertiser) Advertise(ctx context.Context, _ gitsource.URL, _ gitsource.Ref, _ gitsource.Credential) (string, string, error) {
	<-ctx.Done()
	return "", "", ctx.Err()
}

// TestRefreshAdvertisementEndsAtTheGitDeadline pins that install --refresh
// over a recorded git collection pin and a recorded git role pin ends at the
// git deadline when the remote stalls before it advertises.
func TestRefreshAdvertisementEndsAtTheGitDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		requirements string
	}{
		{name: "git collection", requirements: "collections:\n  - git+" + gitAppURL + ",main\n"},
		{name: "git role", requirements: "roles:\n  - src: git+" + roleBaseURL + "\n    name: base\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRoleFixture(t)
			f.writeRequirements(t, tc.requirements)
			f.mustInstall(t)
			f.runtime.Git = stallingAdvertiser{fakeGitClient: f.git}
			f.runtime.GitFetchDeadline = 50 * time.Millisecond
			f.cfg.Refresh = true

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := collections.Start(ctx, f.cfg, f.runtime)
			if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
				t.Fatalf("install --refresh while the advertisement stalls: %v, want %v", err, helpers.ErrArtifactDownloadDeadline)
			}
			if ctx.Err() != nil {
				t.Fatalf("the run ended with its own context (%v), not at the git deadline", ctx.Err())
			}
		})
	}
}
