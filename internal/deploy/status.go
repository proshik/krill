package deploy

import "github.com/proshik/krill/internal/docker"

// Application statuses.
const (
	StatusIdle      = "idle"
	StatusDeploying = "deploying"
	StatusRunning   = "running"
	StatusError     = "error"
	// StatusNotDeployed is never stored: DisplayStatus reports it for an app
	// Swarm has no service for while the stored status is still the initial
	// idle — it has never been deployed (or its service was removed).
	StatusNotDeployed = "not_deployed"
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

// DisplayStatus is DeriveStatus for the web UI, called only once Swarm has
// answered: it tells an app that was never deployed (no service, stored
// status still the initial idle) from one that was stopped, which both read
// "idle" otherwise. The agent API keeps DeriveStatus and its "idle".
func DisplayStatus(s docker.ServiceState, dbStatus string) string {
	if !s.Found && dbStatus == StatusIdle {
		return StatusNotDeployed
	}
	return DeriveStatus(s, dbStatus)
}
