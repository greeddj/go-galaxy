package collections

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// artifactDeadlineError relabels err as helpers.ErrArtifactDownloadDeadline
// only when dlCtx's own budget ended the work while parent is live; the cause
// is rendered with %v so a context error cannot steal the exit classification.
func artifactDeadlineError(parent, dlCtx context.Context, budget time.Duration, err error) error {
	if err == nil || errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		return err
	}
	// A digest mismatch keeps ExitIntegrity and its evict-and-refetch; it never
	// masks a deadline, since a digest is compared only after a complete copy.
	if errors.Is(err, helpers.ErrSHA256Mismatch) {
		return err
	}
	// An unusable backend keeps ExitUsage: relabeling it ExitNetwork would ask
	// CI to retry a broken configuration, and a stalling remote could pick it.
	if errors.Is(err, helpers.ErrCacheBackendUnusable) {
		return err
	}
	if parent.Err() != nil || !errors.Is(dlCtx.Err(), context.DeadlineExceeded) {
		return err
	}
	//nolint:errorlint // deliberately %v, not %w: see the doc comment above and helpers.ErrArtifactDownloadDeadline's own.
	return fmt.Errorf("%w after %s: %v", helpers.ErrArtifactDownloadDeadline, budget, err)
}
