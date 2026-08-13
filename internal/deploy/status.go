package deploy

import "github.com/proshik/krill/internal/docker"

// Application statuses.
const (
	StatusIdle      = "idle"
	StatusDeploying = "deploying"
	StatusRunning   = "running"
	StatusError     = "error"
)

// DeriveStatus computes the "live" status from the Swarm state, falling back to
// the stored status only when Swarm has nothing to say — that is, when no
// service exists yet (never deployed, or removed).
//
// A service that exists but is scaled to zero is stopped, and that is reported
// as such regardless of the stored column. Consulting the column here made the
// reported status depend on which surface performed the stop: the UI writes
// "idle" before scaling down, the agent API does not, so the same stopped app
// read back as "idle" through one and "running" — next to 0/0 replicas —
// through the other.
func DeriveStatus(s docker.ServiceState, dbStatus string) string {
	if !s.Found {
		return dbStatus
	}
	if s.Desired == 0 {
		return StatusIdle
	}
	if s.Running >= s.Desired {
		return StatusRunning
	}
	return StatusDeploying
}
