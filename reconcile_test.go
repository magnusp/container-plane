package cplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

type testClock struct {
	now time.Time
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// deploymentFor is the shape a deploy builds: no previous image yet, because
// the reconciler captures it from the running container.
func deploymentFor(image string) Deployment {
	deployment := testDeployment()
	deployment.Image = image
	deployment.PreviousImage = ""
	return deployment
}

func newTestReconciler(fake *FakeRuntime, clock *testClock, desired Deployment) *Reconciler {
	fake.Now = clock.Now
	return &Reconciler{
		Desired:   desired,
		Runtime:   fake,
		Inspector: fake,
		Sleep:     clock.Sleep,
		Now:       clock.Now,
		Log:       discardLogger(),
	}
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func TestDeployReplacesTheRunningImage(t *testing.T) {
	clock := newClock()
	fake := NewFakeRuntime(testImage)
	fake.BootProbes = 2
	fake.Seed(testPrevious)

	outcome, err := newTestReconciler(fake, clock, deploymentFor(testImage)).Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v (reason %q)", err, outcome.Decision.Reason)
	}
	if !outcome.Settled {
		t.Fatal("the deploy never settled")
	}
	if outcome.Desired.PreviousImage != testPrevious {
		t.Errorf("PreviousImage = %q, want the running container's image %q", outcome.Desired.PreviousImage, testPrevious)
	}
	if got := fake.Ops(); !slices.Equal(got, []string{"stop", "start"}) {
		t.Fatalf("recorded calls = %v, want stop then start", got)
	}
	if fake.Image != testImage {
		t.Errorf("running image = %q, want %q", fake.Image, testImage)
	}
}

func TestDeployFromScratchStartsWithoutStopping(t *testing.T) {
	clock := newClock()
	fake := NewFakeRuntime(testImage)
	fake.BootProbes = 1

	outcome, err := newTestReconciler(fake, clock, deploymentFor(testImage)).Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !outcome.Settled {
		t.Fatal("the deploy never settled")
	}
	if got := fake.Ops(); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("recorded calls = %v, want [start]", got)
	}
	if outcome.Desired.PreviousImage != "" {
		t.Errorf("PreviousImage = %q, want empty: there was nothing to replace", outcome.Desired.PreviousImage)
	}
}

func TestDeployRollsBackWhenTheBudgetIsSpent(t *testing.T) {
	clock := newClock()
	fake := NewFakeRuntime(testImage)
	fake.BootProbes = 1 << 30 // the boot never becomes ready
	fake.Seed(testPrevious)

	desired := deploymentFor(testImage)
	desired.Health.Budget = 2 * time.Second

	outcome, err := newTestReconciler(fake, clock, desired).Run(t.Context())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("Run error = %v, want ErrRolledBack (reason %q)", err, outcome.Decision.Reason)
	}
	if !outcome.RolledBack {
		t.Error("Outcome.RolledBack should be set")
	}
	if got := fake.Ops(); !slices.Equal(got, []string{"stop", "start", "stop", "start"}) {
		t.Fatalf("recorded calls = %v, want the failed start and the rollback", got)
	}
	if fake.Image != testPrevious {
		t.Errorf("running image = %q, want the restored %q", fake.Image, testPrevious)
	}
	if outcome.Decision.Image != testPrevious {
		t.Errorf("rollback target = %q, want %q", outcome.Decision.Image, testPrevious)
	}
}

func TestDeployFailsWithoutAPreviousImage(t *testing.T) {
	clock := newClock()
	fake := NewFakeRuntime(testImage)
	fake.BootProbes = 1 << 30

	desired := deploymentFor(testImage)
	desired.Health.Budget = 2 * time.Second

	outcome, err := newTestReconciler(fake, clock, desired).Run(t.Context())
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("Run error = %v, want ErrDeploy (reason %q)", err, outcome.Decision.Reason)
	}
	if outcome.Decision.Action != ActionReportFailure {
		t.Fatalf("Action = %s, want %s", outcome.Decision.Action, ActionReportFailure)
	}
	if got := fake.Ops(); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("recorded calls = %v, want only the failed start", got)
	}
}

func TestDeployRestoresPreviousAfterAStartFailure(t *testing.T) {
	clock := newClock()
	fake := NewFakeRuntime(testImage)
	fake.Seed(testPrevious)
	fake.FailStartOnce = errors.New("image does not run")

	outcome, err := newTestReconciler(fake, clock, deploymentFor(testImage)).Run(t.Context())
	if !errors.Is(err, ErrCompose) {
		t.Fatalf("Run error = %v, want ErrCompose (reason %q)", err, outcome.Decision.Reason)
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Errorf("error = %v, want it to say the previous image was restored", err)
	}
	if fake.Image != testPrevious {
		t.Errorf("running image = %q, want the restored %q", fake.Image, testPrevious)
	}
	if got := fake.Ops(); !slices.Equal(got, []string{"stop", "start", "start"}) {
		t.Fatalf("recorded calls = %v, want stop, failed start, restore", got)
	}
}

func TestReconcileStopsWhenTheContextEnds(t *testing.T) {
	clock := newClock()
	fake := NewFakeRuntime(testImage)
	fake.BootProbes = 1 << 30
	fake.Seed(testPrevious)

	reconciler := newTestReconciler(fake, clock, deploymentFor(testImage))
	reconciler.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if _, err := reconciler.Run(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}

func TestReconcileBoundedToOnePass(t *testing.T) {
	clock := newClock()
	fake := NewFakeRuntime(testImage)
	fake.Seed(testPrevious)

	reconciler := newTestReconciler(fake, clock, deploymentFor(testImage))
	reconciler.MaxPasses = 1
	outcome, err := reconciler.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Settled {
		t.Error("a single pass cannot have settled a deploy that just started")
	}
	if got := fake.Ops(); !slices.Equal(got, []string{"stop", "start"}) {
		t.Fatalf("recorded calls = %v, want [stop start]", got)
	}
}

func TestReconcileWrapsAnInspectFailureAsCompose(t *testing.T) {
	fake := NewFakeRuntime(testImage)
	fake.FailInspect = errors.New("no daemon")

	_, err := newTestReconciler(fake, newClock(), deploymentFor(testImage)).Run(t.Context())
	if !errors.Is(err, ErrCompose) {
		t.Fatalf("Run error = %v, want ErrCompose", err)
	}
}

func TestReconcileSleepsAtLeastOncePerPass(t *testing.T) {
	clock := newClock()
	fake := NewFakeRuntime(testImage)
	fake.BootProbes = 1 << 30
	fake.Seed(testPrevious)

	desired := deploymentFor(testImage)
	desired.Health.Budget = 10 * time.Second

	reconciler := newTestReconciler(fake, clock, desired)
	passes := 0
	reconciler.Sleep = func(_ context.Context, d time.Duration) error {
		passes++
		clock.now = clock.now.Add(d)
		return nil
	}
	if _, err := reconciler.Run(t.Context()); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("Run error = %v, want ErrRolledBack", err)
	}
	if passes == 0 {
		t.Error("the loop never slept, so a runtime answering instantly would spin it")
	}
}
