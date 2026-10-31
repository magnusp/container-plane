package cplane

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FakeCall is one recorded operation against a FakeRuntime, in the order it
// happened. It is what lets a test assert the sequence of Docker operations a
// deploy performs.
type FakeCall struct {
	Op    string
	Image string
}

// FakeRuntime is an in-memory Runtime and ContainerInspector. It models the
// present-and-running and absent cases; PhaseExited needs a real container
// lifecycle and is covered by the decision table instead.
type FakeRuntime struct {
	// Image is the reference the fake boots as; its digest is derived from it.
	Image string
	// IsRunning and IsReady let a test place the fake in any Phase before starting.
	IsRunning bool
	IsReady   bool
	// BootProbes is how many readiness probes answer false before the container
	// reports ready. Zero means the container is ready as soon as it starts.
	BootProbes int
	// NoHealthcheck makes Inspect report no health verdict, so Observe uses the
	// runtime's probe instead.
	NoHealthcheck bool
	// StartedAt is the boot instant the fake reports. Zero means "now" at the
	// moment the fake's clock is read.
	StartedAt time.Time
	// Now supplies the clock; nil means time.Now.
	Now func() time.Time
	// Fail* inject an error at the named operation.
	FailPull      error
	FailNetwork   error
	FailInspect   error
	FailStop      error
	FailStart     error
	FailStartOnce error
	ProbeErr      error

	mu        sync.Mutex
	calls     []FakeCall
	probes    int
	digest    string
	startedAt time.Time
}

var _ Runtime = (*FakeRuntime)(nil)
var _ ContainerInspector = (*FakeRuntime)(nil)

// NewFakeRuntime returns a fake holding no container yet, resolved to remember
// image as the desired one.
func NewFakeRuntime(image string) *FakeRuntime {
	fake := &FakeRuntime{Image: image}
	fake.digest = fake.digestFor(image)
	return fake
}

func (f *FakeRuntime) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *FakeRuntime) digestFor(image string) string {
	return "sha256:" + imageDigestSeed(image)
}

func imageDigestSeed(image string) string {
	var hash uint64 = 14695981039346656037
	for _, b := range []byte(image) {
		hash ^= uint64(b)
		hash *= 1099511628211
	}
	return fmt.Sprintf("%016x", hash)
}

// Seed places a running container on image without recording a start call, so
// a test can set the scene it is about to assert on.
func (f *FakeRuntime) Seed(image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Image = image
	f.digest = f.digestFor(image)
	f.IsRunning = true
	f.IsReady = true
	f.startedAt = f.now()
}

// Reset forgets the recorded operations, so a test can seed a starting state
// without its own setup showing up in the calls it asserts on.
func (f *FakeRuntime) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// Calls returns the recorded operations in order.
func (f *FakeRuntime) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.calls...)
}

// Ops returns the recorded operation names in order, for compact assertions.
func (f *FakeRuntime) Ops() []string {
	ops := make([]string, 0, len(f.calls))
	for _, call := range f.Calls() {
		ops = append(ops, call.Op)
	}
	return ops
}

func (f *FakeRuntime) record(op, image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeCall{Op: op, Image: image})
}

func (f *FakeRuntime) Inspect(_ context.Context) (ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailInspect != nil {
		return ContainerState{}, f.FailInspect
	}
	started := f.startedAt
	if f.StartedAt != (time.Time{}) {
		started = f.StartedAt
	} else if started.IsZero() && f.IsRunning {
		started = f.now()
	}
	state := ContainerState{Exists: f.IsRunning, Running: f.IsRunning, StartedAt: started}
	if f.IsRunning {
		state.Digest = f.digest
		state.ImageRef = f.Image
		if !f.NoHealthcheck {
			state.Health = "starting"
			if f.IsReady {
				state.Health = "healthy"
			}
		}
	}
	return state, nil
}

func (f *FakeRuntime) ImageDigest(_ context.Context, image string) (string, error) {
	return f.digestFor(image), nil
}

func (f *FakeRuntime) Pull(_ context.Context, image string) error {
	f.record("pull", image)
	return f.FailPull
}

func (f *FakeRuntime) EnsureNetwork(_ context.Context) error {
	f.record("network", "")
	return f.FailNetwork
}

func (f *FakeRuntime) Stop(_ context.Context) error {
	f.record("stop", f.Image)
	if f.FailStop != nil {
		return f.FailStop
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.IsRunning = false
	f.IsReady = false
	return nil
}

func (f *FakeRuntime) Start(_ context.Context, image string) error {
	f.record("start", image)
	if f.FailStart != nil {
		return f.FailStart
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailStartOnce != nil {
		err := f.FailStartOnce
		f.FailStartOnce = nil
		return err
	}
	f.Image = image
	f.digest = f.digestFor(image)
	f.IsRunning = true
	f.startedAt = f.now()
	f.probes = f.BootProbes
	f.IsReady = f.probes == 0
	return nil
}

func (f *FakeRuntime) Ready(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.IsRunning {
		return false, nil
	}
	if f.ProbeErr != nil {
		return false, f.ProbeErr
	}
	if f.probes > 0 {
		f.probes--
		return false, nil
	}
	// A real container's healthcheck goes healthy and stays healthy until it
	// fails, so the fake has to remember the transition.
	f.IsReady = true
	return true, nil
}
