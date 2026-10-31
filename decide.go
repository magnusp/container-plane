package cplane

import (
	"fmt"
	"time"
)

// Action is what the caller should do about a Decision. The decision itself
// performs nothing, so the same table drives a deploy and the tests, without
// Docker anywhere in the picture.
type Action int

const (
	ActionNone Action = iota
	ActionWait
	ActionStopAndStart
	ActionStart
	ActionRollback
	ActionReportFailure
)

func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionWait:
		return "wait"
	case ActionStopAndStart:
		return "stop-and-start"
	case ActionStart:
		return "start"
	case ActionRollback:
		return "rollback"
	case ActionReportFailure:
		return "report-failure"
	default:
		return fmt.Sprintf("action(%d)", int(a))
	}
}

type Decision struct {
	Action Action
	Image  string // target image, when the action needs one
	Reason string // one short line, surfaced by the CLI and the logs
}

// Decide maps desired state plus what is actually running to a single action.
// Every branch reads only its arguments: no Docker, no clock, no environment.
func Decide(desired Deployment, observed Observation, now time.Time) Decision {
	switch observed.Phase {
	case PhaseAbsent:
		return Decision{
			Action: ActionStart,
			Image:  desired.Image,
			Reason: "no container is running",
		}
	case PhaseWrongDigest:
		return Decision{
			Action: ActionStopAndStart,
			Image:  desired.Image,
			Reason: fmt.Sprintf("running digest %s, want %s", shortDigest(observed.Digest), desired.Image),
		}
	case PhaseBooting:
		if Elapsed(observed.BootStartedAt, now) > desired.Health.Budget {
			return rollbackDecision(desired, fmt.Sprintf("no readiness within %s of boot", desired.Health.Budget))
		}
		return Decision{
			Action: ActionWait,
			Reason: fmt.Sprintf("booting for %s of %s", Elapsed(observed.BootStartedAt, now).Truncate(time.Millisecond), desired.Health.Budget),
		}
	case PhaseHealthy:
		return Decision{
			Action: ActionNone,
			Reason: "running the desired digest and ready",
		}
	case PhaseUnhealthy:
		return rollbackDecision(desired, "readiness was lost")
	case PhaseExited:
		// Compose's restart policy owns this case. Deciding it here as well
		// would give one failure two owners.
		return Decision{
			Action: ActionNone,
			Reason: "container exited; the compose restart policy owns this case",
		}
	default:
		return Decision{
			Action: ActionReportFailure,
			Reason: fmt.Sprintf("unrecognised phase %s", observed.Phase),
		}
	}
}

func rollbackDecision(desired Deployment, cause string) Decision {
	if desired.PreviousImage == "" {
		return Decision{
			Action: ActionReportFailure,
			Reason: cause + ", and no previous image was captured",
		}
	}
	return Decision{
		Action: ActionRollback,
		Image:  desired.PreviousImage,
		Reason: cause + ", rolling back",
	}
}

func shortDigest(digest string) string {
	if len(digest) <= 19 {
		return digest
	}
	return digest[:19] + "..."
}
