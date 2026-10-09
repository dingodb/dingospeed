package task

import (
	"context"
	"errors"
	"golang.org/x/sync/errgroup"
)

// ExecuteManifest shares bounded file scheduling between preheat and repository
// sync. Preparation is sequential (blob/reference creation may deduplicate),
// transfers are bounded, and every queued transfer is joined on every exit path.
// A nil transfer means that preparation found complete local content.
func ExecuteManifest(ctx context.Context, limit, count int, prepare func(context.Context, int) (func(context.Context) error, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	group, workCtx := errgroup.WithContext(ctx)
	if limit < 1 {
		limit = 1
	}
	group.SetLimit(limit)
	var prepareErr error
	for i := 0; i < count; i++ {
		if workCtx.Err() != nil {
			break
		}
		transfer, err := prepare(workCtx, i)
		if err != nil {
			// Metadata clients may flatten cancellation into an opaque error.
			// Preserve cancellation identity so a failed transfer remains the
			// cause when it cancelled concurrent preparation.
			if workCtx.Err() != nil {
				err = workCtx.Err()
			}
			prepareErr = err
			cancel()
			break
		}
		if transfer != nil {
			group.Go(func() error {
				if err := workCtx.Err(); err != nil {
					return err
				}
				return transfer(workCtx)
			})
		}
	}
	err := group.Wait()
	if prepareErr != nil {
		if errors.Is(prepareErr, context.Canceled) && err != nil {
			return err
		}
		return prepareErr
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}
