package cache

import (
	"context"
	"errors"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// LockLostError returns helpers.ErrCacheLockLost, even for a nil err, when
// holder (from Backend.Lock) lost the lock and parent is not canceled. err is
// kept with %v, never %w, so none of its sentinels outranks exit 8 in exitcode.
func LockLostError(parent, holder context.Context, err error) error {
	if holder == nil {
		return err
	}
	if parent.Err() != nil {
		return err
	}
	if !errors.Is(context.Cause(holder), helpers.ErrCacheLockLost) {
		return err
	}
	if err == nil {
		return helpers.ErrCacheLockLost
	}
	//nolint:errorlint // deliberately %v, not %w: see the doc comment above and the sentinel's own.
	return fmt.Errorf("%w: %v", helpers.ErrCacheLockLost, err)
}
