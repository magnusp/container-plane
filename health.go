package cplane

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotReady reports a readiness probe that never succeeded within its retry
// budget.
var ErrNotReady = errors.New("readiness probe never succeeded")

// WaitReady polls the runtime's readiness probe once per Health.Interval, with
// Health.Timeout bounding each attempt and Health.Retries attempts in total.
// It returns as soon as one attempt succeeds, so a fast boot is not made to wait
// out the interval.
func WaitReady(ctx context.Context, health Health, runtime Runtime, sleep Sleeper) error {
	attempts := health.Retries
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, health.Timeout)
		ready, err := runtime.Ready(attemptCtx)
		cancel()
		if err == nil && ready {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
		}
		if attempt == attempts {
			break
		}
		if err := sleep(ctx, health.Interval); err != nil {
			return err
		}
	}
	if lastErr != nil {
		return fmt.Errorf("%w: %w", ErrNotReady, lastErr)
	}
	return ErrNotReady
}

// Elapsed reports how long a boot has been running, clamped at zero so a clock
// that moved backwards cannot produce a negative age.
func Elapsed(since, now time.Time) time.Duration {
	elapsed := now.Sub(since)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}
