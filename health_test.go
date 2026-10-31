package cplane

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testHealth() Health {
	return Health{
		Interval: time.Millisecond,
		Timeout:  time.Second,
		Retries:  5,
		Budget:   time.Minute,
	}
}

func TestWaitReadySucceedsAfterRetries(t *testing.T) {
	fake := NewFakeRuntime("app:v1")
	fake.BootProbes = 2
	if err := fake.Start(t.Context(), "app:v1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := WaitReady(t.Context(), testHealth(), fake, sleepContext); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestWaitReadyGivesUp(t *testing.T) {
	fake := NewFakeRuntime("app:v1")
	fake.BootProbes = 1 << 30
	if err := fake.Start(t.Context(), "app:v1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	health := testHealth()
	health.Retries = 2
	if err := WaitReady(t.Context(), health, fake, sleepContext); !errors.Is(err, ErrNotReady) {
		t.Fatalf("WaitReady error = %v, want ErrNotReady", err)
	}
}

func TestWaitReadyReportsProbeErrors(t *testing.T) {
	fake := NewFakeRuntime("app:v1")
	fake.ProbeErr = errors.New("docker exec failed")
	if err := fake.Start(t.Context(), "app:v1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	health := testHealth()
	health.Retries = 2
	err := WaitReady(t.Context(), health, fake, sleepContext)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("WaitReady error = %v, want ErrNotReady", err)
	}
	if !errors.Is(err, fake.ProbeErr) {
		t.Errorf("WaitReady error = %v, want it to wrap the probe error", err)
	}
}

func TestWaitReadyUsesAtLeastOneAttempt(t *testing.T) {
	fake := NewFakeRuntime("app:v1")
	if err := fake.Start(t.Context(), "app:v1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	health := testHealth()
	health.Retries = 0
	if err := WaitReady(t.Context(), health, fake, sleepContext); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestWaitReadyHonoursACancelledContext(t *testing.T) {
	fake := NewFakeRuntime("app:v1")
	fake.BootProbes = 1 << 30
	if err := fake.Start(t.Context(), "app:v1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sleep := func(context.Context, time.Duration) error { return context.Canceled }
	if err := WaitReady(ctx, testHealth(), fake, sleep); !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady error = %v, want the context error", err)
	}
}
