package cplane

import (
	"fmt"
	"time"
)

// Health is the readiness gate for one deployment. Budget bounds how long a
// boot may run before it is called failed; Interval, Timeout, and Retries
// bound the individual probe attempts.
type Health struct {
	Interval time.Duration
	Timeout  time.Duration
	Retries  int
	Budget   time.Duration
}

// Deployment is everything the decision logic needs about the desired
// container. PreviousImage is captured from the running container before the
// first mutation and never read from a file.
type Deployment struct {
	Image         string
	PreviousImage string
	Health        Health
}

// Validate rejects a deployment that could not produce a decision, rather than
// letting a zero duration silently mean "poll instantly" or "never give up".
func (d Deployment) Validate() error {
	if d.Image == "" {
		return fmt.Errorf("%w: image is required", ErrUsage)
	}
	if d.Health.Interval <= 0 {
		return fmt.Errorf("%w: health interval must be positive", ErrUsage)
	}
	if d.Health.Timeout <= 0 {
		return fmt.Errorf("%w: health timeout must be positive", ErrUsage)
	}
	if d.Health.Retries <= 0 {
		return fmt.Errorf("%w: health retries must be positive", ErrUsage)
	}
	if d.Health.Budget <= 0 {
		return fmt.Errorf("%w: health budget must be positive", ErrUsage)
	}
	return nil
}
