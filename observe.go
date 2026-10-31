package cplane

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Phase is what the running container is doing right now. It exists so the
// reconcile cases are an exhaustive switch: a new phase has to be classified
// here and decided in decide.go rather than falling through a chain of
// conditionals that can disagree with each other.
type Phase int

const (
	PhaseAbsent      Phase = iota // no container
	PhaseWrongDigest              // running, but not the image we want
	PhaseBooting                  // running, right digest, not ready yet
	PhaseHealthy                  // running, right digest, ready
	PhaseUnhealthy                // running, right digest, was ready and stopped being
	PhaseExited                   // container exited
)

func (p Phase) String() string {
	switch p {
	case PhaseAbsent:
		return "absent"
	case PhaseWrongDigest:
		return "wrong-digest"
	case PhaseBooting:
		return "booting"
	case PhaseHealthy:
		return "healthy"
	case PhaseUnhealthy:
		return "unhealthy"
	case PhaseExited:
		return "exited"
	default:
		return fmt.Sprintf("phase(%d)", int(p))
	}
}

type Observation struct {
	Phase         Phase
	Digest        string
	ImageRef      string
	BootStartedAt time.Time
	LastChangedAt time.Time
}

// ContainerState is what the Docker seam can see about the single app
// container. Health is Docker's own healthcheck verdict, which is empty when
// the compose file declares no healthcheck.
type ContainerState struct {
	Exists    bool
	Running   bool
	Digest    string
	ImageRef  string
	StartedAt time.Time
	Health    string
}

// ContainerInspector is the read side of the Docker seam. It is separate from
// Runtime because the Runtime operations alone cannot say when the current
// boot began, and Decide needs that instant to decide whether a boot is late.
type ContainerInspector interface {
	Inspect(ctx context.Context) (ContainerState, error)
	ImageDigest(ctx context.Context, image string) (string, error)
}

// Observe classifies what is running against what is wanted. It performs no
// mutation: everything it learns comes from the inspector and the runtime's
// readiness probe.
func Observe(ctx context.Context, desired Deployment, inspector ContainerInspector, ready func(context.Context) (bool, error), now time.Time) (Observation, error) {
	state, err := inspector.Inspect(ctx)
	if err != nil {
		return Observation{}, err
	}
	if !state.Exists {
		return Observation{Phase: PhaseAbsent, LastChangedAt: now}, nil
	}
	// The only lifecycle change a single-container deployment has is the last
	// start, which is the instant the boot budget is measured from.
	lastChanged := state.StartedAt
	if lastChanged.IsZero() {
		lastChanged = now
	}
	base := Observation{
		Digest:        state.Digest,
		ImageRef:      state.ImageRef,
		BootStartedAt: state.StartedAt,
		LastChangedAt: lastChanged,
	}
	if !state.Running {
		base.Phase = PhaseExited
		return base, nil
	}

	wanted, err := inspector.ImageDigest(ctx, desired.Image)
	if err != nil {
		return Observation{}, fmt.Errorf("resolve digest for %s: %w", desired.Image, err)
	}
	if wanted != state.Digest {
		base.Phase = PhaseWrongDigest
		return base, nil
	}

	// With a healthcheck declared, Docker's verdict is the readiness of record.
	// Without one, the runtime's probe is, so readiness has a single owner
	// either way rather than two sources that can disagree.
	readyNow := state.Health == "healthy"
	if state.Health == "" {
		probeCtx, cancel := context.WithTimeout(ctx, desired.Health.Timeout)
		readyNow, err = ready(probeCtx)
		cancel()
		if err != nil {
			if errors.Is(err, ErrNoProbe) {
				return Observation{}, fmt.Errorf("%w: %w", ErrUsage, err)
			}
			return Observation{}, fmt.Errorf("readiness probe: %w", err)
		}
	}

	switch {
	case state.Health == "unhealthy":
		base.Phase = PhaseUnhealthy
	case readyNow:
		base.Phase = PhaseHealthy
	case Elapsed(state.StartedAt, now) > desired.Health.Budget:
		// Docker's verdict is authoritative while it is producing one. Once the
		// budget is spent and Docker is still unable to conclude, the tool's own
		// probe gets the deciding vote.
		//
		// This is the second source, on the one path where the first is silent.
		// A healthcheck that cannot run at all, because its binary is missing
		// from the image, leaves the container in `starting` forever. Without
		// this, a healthy application would be rolled back every budget over a
		// broken probe, which is a worse failure than the one being guarded
		// against.
		probeCtx, cancel := context.WithTimeout(ctx, desired.Health.Timeout)
		serving, probeErr := ready(probeCtx)
		cancel()
		switch {
		case probeErr != nil:
			base.Phase = PhaseUnhealthy
		case serving:
			base.Phase = PhaseHealthy
		default:
			base.Phase = PhaseUnhealthy
		}
	default:
		base.Phase = PhaseBooting
	}
	return base, nil
}
