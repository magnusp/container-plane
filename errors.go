package cplane

import "errors"

// Failure classes. The CLI maps these to exit codes so a workflow can tell a
// missing image, a broken compose file, and a failed health gate apart without
// parsing log text.
var (
	// ErrUsage marks bad flags or a missing required value.
	ErrUsage = errors.New("usage")

	// ErrPull marks an image pull that did not complete. Nothing was stopped
	// when this happens, so there is no previous image to restore.
	ErrPull = errors.New("image pull failed")

	// ErrCompose marks a failed Docker or Docker Compose operation: network
	// creation, stop, inspect, or start.
	ErrCompose = errors.New("compose command failed")

	// ErrDeploy marks a deployment that could not settle and had no previous
	// image to restore.
	ErrDeploy = errors.New("deploy failed")

	// ErrRolledBack marks a deployment that failed its health gate and restored
	// the image that was running before it.
	ErrRolledBack = errors.New("deploy rolled back")

	// ErrNoProbe reports a service with no Docker healthcheck and no --probe.
	ErrNoProbe = errors.New("no readiness probe configured")
)
