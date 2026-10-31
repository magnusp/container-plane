package cplane

import (
	"testing"
	"time"
)

const (
	testImage    = "registry.example/app:v2"
	testPrevious = "registry.example/app:v1"
)

func testDeployment() Deployment {
	return Deployment{
		Image:         testImage,
		PreviousImage: testPrevious,
		Health: Health{
			Interval: time.Second,
			Timeout:  time.Second,
			Retries:  3,
			Budget:   30 * time.Second,
		},
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	started := now.Add(-5 * time.Second)
	late := now.Add(-time.Minute)

	tests := []struct {
		name       string
		desired    Deployment
		observed   Observation
		now        time.Time
		wantAction Action
		wantImage  string
	}{
		{
			name:       "no container starts the desired image",
			observed:   Observation{Phase: PhaseAbsent},
			now:        now,
			wantAction: ActionStart,
			wantImage:  testImage,
		},
		{
			name:       "a different digest is replaced",
			observed:   Observation{Phase: PhaseWrongDigest, Digest: "sha256:old"},
			now:        now,
			wantAction: ActionStopAndStart,
			wantImage:  testImage,
		},
		{
			name:       "a boot inside the budget waits",
			observed:   Observation{Phase: PhaseBooting, BootStartedAt: started},
			now:        now,
			wantAction: ActionWait,
		},
		{
			name:       "a boot past the budget rolls back",
			observed:   Observation{Phase: PhaseBooting, BootStartedAt: late},
			now:        now,
			wantAction: ActionRollback,
			wantImage:  testPrevious,
		},
		{
			name:       "a boot past the budget without a previous image fails",
			desired:    Deployment{Image: testImage, Health: testDeployment().Health},
			observed:   Observation{Phase: PhaseBooting, BootStartedAt: late},
			now:        now,
			wantAction: ActionReportFailure,
		},
		{
			name:       "healthy is settled",
			observed:   Observation{Phase: PhaseHealthy},
			now:        now,
			wantAction: ActionNone,
		},
		{
			name:       "lost readiness rolls back",
			observed:   Observation{Phase: PhaseUnhealthy},
			now:        now,
			wantAction: ActionRollback,
			wantImage:  testPrevious,
		},
		{
			name:       "lost readiness without a previous image fails",
			desired:    Deployment{Image: testImage, Health: testDeployment().Health},
			observed:   Observation{Phase: PhaseUnhealthy},
			now:        now,
			wantAction: ActionReportFailure,
		},
		{
			name:       "an exited container is left to compose",
			observed:   Observation{Phase: PhaseExited},
			now:        now,
			wantAction: ActionNone,
		},
		{
			name:       "an unknown phase is reported",
			observed:   Observation{Phase: Phase(99)},
			now:        now,
			wantAction: ActionReportFailure,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := test.desired
			if desired.Image == "" {
				desired = testDeployment()
			}
			decision := Decide(desired, test.observed, test.now)
			if decision.Action != test.wantAction {
				t.Fatalf("Action = %s, want %s (%s)", decision.Action, test.wantAction, decision.Reason)
			}
			if decision.Image != test.wantImage {
				t.Errorf("Image = %q, want %q", decision.Image, test.wantImage)
			}
			if decision.Reason == "" {
				t.Error("Reason is empty; every decision must explain itself")
			}
		})
	}
}

func TestDeploymentValidate(t *testing.T) {
	valid := testDeployment()
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	for name, mutate := range map[string]func(*Deployment){
		"empty image":   func(d *Deployment) { d.Image = "" },
		"zero interval": func(d *Deployment) { d.Health.Interval = 0 },
		"zero timeout":  func(d *Deployment) { d.Health.Timeout = 0 },
		"zero retries":  func(d *Deployment) { d.Health.Retries = 0 },
		"zero budget":   func(d *Deployment) { d.Health.Budget = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			deployment := valid
			mutate(&deployment)
			if err := deployment.Validate(); err == nil {
				t.Fatal("Validate accepted an invalid deployment")
			}
		})
	}
}
