package cplane

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubInspector struct {
	state  ContainerState
	digest string
	err    error
}

func (s stubInspector) Inspect(context.Context) (ContainerState, error) {
	return s.state, s.err
}

func (s stubInspector) ImageDigest(context.Context, string) (string, error) {
	return s.digest, s.err
}

func readyFunc(ready bool) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return ready, nil }
}

func TestObserve(t *testing.T) {
	started := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	desired := testDeployment()
	const wanted = "sha256:wanted"

	tests := []struct {
		name      string
		state     ContainerState
		digest    string
		ready     bool
		elapsed   time.Duration
		wantPhase Phase
	}{
		{
			name:      "no container is absent",
			wantPhase: PhaseAbsent,
		},
		{
			name:      "stopped container has exited",
			state:     ContainerState{Exists: true, Digest: wanted, StartedAt: started},
			digest:    wanted,
			elapsed:   time.Second,
			wantPhase: PhaseExited,
		},
		{
			name:      "different digest is drift",
			state:     ContainerState{Exists: true, Running: true, Digest: "sha256:other", StartedAt: started, Health: "healthy"},
			digest:    wanted,
			elapsed:   time.Second,
			wantPhase: PhaseWrongDigest,
		},
		{
			name:      "starting healthcheck is booting",
			state:     ContainerState{Exists: true, Running: true, Digest: wanted, StartedAt: started, Health: "starting"},
			digest:    wanted,
			elapsed:   time.Second,
			wantPhase: PhaseBooting,
		},
		{
			name:      "healthy healthcheck is healthy",
			state:     ContainerState{Exists: true, Running: true, Digest: wanted, StartedAt: started, Health: "healthy"},
			digest:    wanted,
			elapsed:   time.Second,
			wantPhase: PhaseHealthy,
		},
		{
			name:      "unhealthy healthcheck is unhealthy",
			state:     ContainerState{Exists: true, Running: true, Digest: wanted, StartedAt: started, Health: "unhealthy"},
			digest:    wanted,
			elapsed:   time.Second,
			wantPhase: PhaseUnhealthy,
		},
		{
			name:      "no healthcheck past the budget is unhealthy",
			state:     ContainerState{Exists: true, Running: true, Digest: wanted, StartedAt: started},
			digest:    wanted,
			elapsed:   desired.Health.Budget + time.Second,
			wantPhase: PhaseUnhealthy,
		},
		{
			name:      "no healthcheck inside the budget is booting",
			state:     ContainerState{Exists: true, Running: true, Digest: wanted, StartedAt: started},
			digest:    wanted,
			elapsed:   desired.Health.Budget,
			wantPhase: PhaseBooting,
		},
		{
			name:      "no healthcheck and a ready probe is healthy",
			state:     ContainerState{Exists: true, Running: true, Digest: wanted, StartedAt: started},
			digest:    wanted,
			elapsed:   time.Second,
			ready:     true,
			wantPhase: PhaseHealthy,
		},
		{
			name:      "a declared healthcheck outranks the probe",
			state:     ContainerState{Exists: true, Running: true, Digest: wanted, StartedAt: started, Health: "unhealthy"},
			digest:    wanted,
			elapsed:   time.Second,
			ready:     true,
			wantPhase: PhaseUnhealthy,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := started.Add(test.elapsed)
			observed, err := Observe(t.Context(), desired, stubInspector{state: test.state, digest: test.digest}, readyFunc(test.ready), now)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if observed.Phase != test.wantPhase {
				t.Fatalf("Phase = %s, want %s", observed.Phase, test.wantPhase)
			}
			if !observed.BootStartedAt.Equal(test.state.StartedAt) && !test.state.StartedAt.IsZero() {
				t.Errorf("BootStartedAt = %s, want %s", observed.BootStartedAt, test.state.StartedAt)
			}
		})
	}
}

func TestObserveReportsInspectorFailure(t *testing.T) {
	want := errors.New("no daemon")
	_, err := Observe(t.Context(), testDeployment(), stubInspector{err: want}, readyFunc(false), time.Now())
	if !errors.Is(err, want) {
		t.Fatalf("Observe error = %v, want %v", err, want)
	}
}

func TestObserveReportsProbeFailure(t *testing.T) {
	want := errors.New("docker exec failed")
	desired := testDeployment()
	inspector := stubInspector{
		state:  ContainerState{Exists: true, Running: true, Digest: "sha256:x", StartedAt: time.Now()},
		digest: "sha256:x",
	}
	_, err := Observe(t.Context(), desired, inspector, func(context.Context) (bool, error) { return false, want }, time.Now())
	if !errors.Is(err, want) {
		t.Fatalf("Observe error = %v, want %v", err, want)
	}
}

// A healthcheck that cannot run leaves the container in `starting` forever, so
// Docker never reaches a verdict. Once the budget is spent, the tool's own
// probe decides instead of rolling back a serving application.
func TestObserveTrustsItsOwnProbeWhenDockerCannotConclude(t *testing.T) {
	started := time.Now().Add(-10 * time.Minute)
	inspector := stubInspector{
		state:  ContainerState{Exists: true, Running: true, Digest: "sha256:x", StartedAt: started, Health: "starting"},
		digest: "sha256:x",
	}

	observed, err := Observe(t.Context(), testDeployment(), inspector, readyFunc(true), time.Now())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.Phase != PhaseHealthy {
		t.Errorf("phase = %s, want %s for an application the probe can reach", observed.Phase, PhaseHealthy)
	}
}

func TestObserveStillReportsUnhealthyWhenTheProbeAgrees(t *testing.T) {
	started := time.Now().Add(-10 * time.Minute)
	inspector := stubInspector{
		state:  ContainerState{Exists: true, Running: true, Digest: "sha256:x", StartedAt: started, Health: "starting"},
		digest: "sha256:x",
	}

	observed, err := Observe(t.Context(), testDeployment(), inspector, readyFunc(false), time.Now())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.Phase != PhaseUnhealthy {
		t.Errorf("phase = %s, want %s", observed.Phase, PhaseUnhealthy)
	}
}

func TestObserveWithoutAHealthcheckNeedsAProbe(t *testing.T) {
	inspector := stubInspector{
		state:  ContainerState{Exists: true, Running: true, Digest: "sha256:x", StartedAt: time.Now()},
		digest: "sha256:x",
	}
	_, err := Observe(t.Context(), testDeployment(), inspector, func(context.Context) (bool, error) {
		return false, ErrNoProbe
	}, time.Now())
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Observe error = %v, want ErrUsage", err)
	}
}

func TestObserveDoesNotTreatAFailedFallbackProbeAsReady(t *testing.T) {
	started := time.Now().Add(-10 * time.Minute)
	inspector := stubInspector{
		state:  ContainerState{Exists: true, Running: true, Digest: "sha256:x", StartedAt: started, Health: "starting"},
		digest: "sha256:x",
	}

	observed, err := Observe(t.Context(), testDeployment(), inspector, func(context.Context) (bool, error) {
		return false, errors.New("connection refused")
	}, time.Now())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.Phase != PhaseUnhealthy {
		t.Errorf("phase = %s, want %s; a failed fallback probe is not readiness", observed.Phase, PhaseUnhealthy)
	}
}
