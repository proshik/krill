package deploy

import "github.com/proshik/krill/internal/docker"

// Application statuses.
const (
	StatusIdle      = "idle"
	StatusDeploying = "deploying"
	StatusRunning   = "running"
	StatusError     = "error"
	// StatusNotDeployed is never stored: DisplayStatus reports it for an app
	// that Swarm has no service for and that has no deployment at all.
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
// answered: it tells an app that was never deployed from one that was
// stopped, which both read "idle" otherwise. deployed says whether the app
// has any deployment: a missing service alone proves nothing, since stopping
// a global app removes its service. The agent API keeps DeriveStatus and its
// "idle".
func DisplayStatus(s docker.ServiceState, dbStatus string, deployed bool) string {
	if !s.Found && !deployed && dbStatus == StatusIdle {
		return StatusNotDeployed
	}
	return DeriveStatus(s, dbStatus)
}
