package cplane

import (
	"context"
	"time"
)

// Runtime is the write seam between the decision logic and Docker. Decide
// never touches it; only the executor does, so a decision can be tested with
// nothing running.
type Runtime interface {
	// Pull fetches the desired image before anything is stopped.
	Pull(ctx context.Context, image string) error
	// EnsureNetwork joins or creates the shared network, when one is configured.
	EnsureNetwork(ctx context.Context) error
	// Stop stops the service, honouring the configured drain.
	Stop(ctx context.Context) error
	// Start starts the service with the given image reference.
	Start(ctx context.Context, image string) error
	// Ready runs the readiness probe inside the running container.
	Ready(ctx context.Context) (bool, error)
}

// Sleeper pauses between reconcile passes. Tests substitute one that advances a
// fake clock, which is what keeps the loop from spending real time waiting.
type Sleeper func(ctx context.Context, d time.Duration) error

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
