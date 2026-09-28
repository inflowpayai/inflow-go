package platform

import (
	"context"
	"errors"
	"time"
)

// Poll leaves terminal-state interpretation and server cancellation to the
// protocol flow. The caller supplies a deadline for the entire operation.
// A read returns its value, whether it is terminal, and the delay before another read.
func Poll[T any](ctx context.Context, read func(context.Context) (T, bool, time.Duration, error)) (T, error) {
	var zero T
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, done, delay, err := read(ctx)
		if err != nil {
			return zero, err
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if done {
			return value, nil
		}
		if delay <= 0 {
			return zero, errors.New("poll interval must be positive")
		}
		if err := Wait(ctx, delay); err != nil {
			return zero, err
		}
	}
}
