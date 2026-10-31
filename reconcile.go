package cplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Outcome is the result of a reconcile run. It carries the observation and
// decision from the last pass so the caller can report why a deploy failed.
type Outcome struct {
	Desired     Deployment
	Observation Observation
	Decision    Decision
	Settled     bool
	RolledBack  bool
}

// Reconciler drives the observe-decide-apply loop. Every branch it takes comes
// from Decide, and every branch it waits on is bounded, so the loop exits on a
// cancel, a settled container, or a failure — never by spinning.
type Reconciler struct {
	Desired   Deployment
	Runtime   Runtime
	Inspector ContainerInspector
	// Sleep defaults to a context-aware real sleep.
	Sleep Sleeper
	// Now defaults to time.Now.
	Now func() time.Time
	// Log defaults to slog.Default().
	Log *slog.Logger
	// MaxPasses bounds the loop. Zero means run until the reconcile settles.
	MaxPasses int
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) sleep() Sleeper {
	if r.Sleep != nil {
		return r.Sleep
	}
	return sleepContext
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Reconciler) ready(ctx context.Context) (bool, error) {
	return r.Runtime.Ready(ctx)
}

// Run reconciles until the container settles, a decision reports a failure, the
// context ends, or MaxPasses is reached.
func (r *Reconciler) Run(ctx context.Context) (Outcome, error) {
	outcome := Outcome{Desired: r.Desired}
	if err := r.capturePrevious(ctx); err != nil {
		return outcome, err
	}
	outcome.Desired = r.Desired

	for pass := 0; r.MaxPasses <= 0 || pass < r.MaxPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		now := r.now()
		observed, err := Observe(ctx, r.Desired, r.Inspector, r.ready, now)
		if err != nil {
			if errors.Is(err, ErrUsage) {
				return outcome, err
			}
			return outcome, fmt.Errorf("%w: %w", ErrCompose, err)
		}
		decision := Decide(r.Desired, observed, now)
		outcome.Observation = observed
		outcome.Decision = decision
		r.log().Info("reconcile",
			"phase", observed.Phase,
			"action", decision.Action,
			"reason", decision.Reason,
			"image", decision.Image)

		switch decision.Action {
		case ActionNone:
			if observed.Phase == PhaseHealthy {
				outcome.Settled = true
			}
			return outcome, nil

		case ActionWait:
			// Probe with the configured interval and attempt budget. A probe
			// that never succeeds is not this loop's failure to detect: Decide
			// owns the health budget and turns a late boot into a rollback.
			//
			// The wait is capped at the budget that remains. A retry budget
			// larger than the health budget must not stretch a deploy past the
			// point Decide has already called it late.
			remaining := r.Desired.Health.Budget - Elapsed(observed.BootStartedAt, now)
			if remaining > 0 {
				waitCtx, cancel := context.WithTimeout(ctx, remaining)
				err := WaitReady(waitCtx, r.Desired.Health, r.Runtime, r.sleep())
				cancel()
				if err != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return outcome, ctxErr
					}
				}
			}

		case ActionStart:
			if err := r.Runtime.Start(ctx, decision.Image); err != nil {
				return outcome, fmt.Errorf("%w: start %s: %w", ErrCompose, decision.Image, err)
			}

		case ActionStopAndStart:
			if err := r.Runtime.Stop(ctx); err != nil {
				return outcome, fmt.Errorf("%w: stop: %w", ErrCompose, err)
			}
			if err := r.Runtime.Start(ctx, decision.Image); err != nil {
				cause := fmt.Errorf("%w: start %s: %w", ErrCompose, decision.Image, err)
				return outcome, r.restore(ctx, cause)
			}

		case ActionRollback:
			if err := r.Runtime.Stop(ctx); err != nil {
				return outcome, fmt.Errorf("%w: stop before rollback: %w", ErrCompose, err)
			}
			if err := r.Runtime.Start(ctx, decision.Image); err != nil {
				return outcome, fmt.Errorf("%w: rollback to %s: %w", ErrCompose, decision.Image, err)
			}
			outcome.RolledBack = true
			return outcome, fmt.Errorf("%w: %s", ErrRolledBack, decision.Reason)

		case ActionReportFailure:
			return outcome, fmt.Errorf("%w: %s", ErrDeploy, decision.Reason)
		}

		// Every continuation sleeps, so a runtime that answers every probe
		// instantly still cannot turn this into a busy loop.
		if err := r.sleep()(ctx, r.Desired.Health.Interval); err != nil {
			return outcome, err
		}
	}

	return outcome, nil
}

// capturePrevious reads the image the running container was created from
// before this run changes anything. It is the only rollback target, so no file
// can name a pruned image or a tag that has since moved.
func (r *Reconciler) capturePrevious(ctx context.Context) error {
	if r.Desired.PreviousImage != "" {
		return nil
	}
	state, err := r.Inspector.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("%w: inspect running container: %w", ErrCompose, err)
	}
	if !state.Exists {
		return nil
	}
	r.Desired.PreviousImage = state.ImageRef
	if r.Desired.PreviousImage == "" {
		r.Desired.PreviousImage = state.Digest
	}
	return nil
}

// restore starts the captured previous image after a failed replacement. It
// returns the original failure, annotated with whether the restore worked, so
// the CLI still reports the compose class rather than a health class.
func (r *Reconciler) restore(ctx context.Context, cause error) error {
	previous := r.Desired.PreviousImage
	if previous == "" {
		return cause
	}
	if err := r.Runtime.Start(ctx, previous); err != nil {
		return fmt.Errorf("%w; restoring %s also failed: %w", cause, previous, err)
	}
	return fmt.Errorf("%w; restored %s", cause, previous)
}
