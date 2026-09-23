package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// deadlineError wraps err into sentinel, once, only when this operation's own
// budget ended it: parent live, dlCtx expired and err carrying a context error.
// The cause renders with %v so a context error cannot steal the exit code.
func deadlineError(parent, dlCtx context.Context, budget time.Duration, sentinel, err error) error {
	if err == nil || errors.Is(err, sentinel) {
		return err
	}
	if parent.Err() != nil {
		return err
	}
	if !errors.Is(dlCtx.Err(), context.DeadlineExceeded) {
		return err
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return err
	}
	//nolint:errorlint // deliberately %v, not %w: see the doc comment above and the sentinel's own.
	return fmt.Errorf("%w after %s: %v", sentinel, budget, err)
}

// MetadataDeadlineError is deadlineError bound to
// helpers.ErrMetadataFetchDeadline, for loadVersionsListCached, whose paging
// loop spends one budget across every page it fetches.
func MetadataDeadlineError(parent, dlCtx context.Context, budget time.Duration, err error) error {
	return deadlineError(parent, dlCtx, budget, helpers.ErrMetadataFetchDeadline, err)
}
