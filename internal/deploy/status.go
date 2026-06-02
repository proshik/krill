package deploy

import "github.com/proshik/krill/internal/docker"

// Application statuses.
const (
	StatusIdle      = "idle"
	StatusDeploying = "deploying"
	StatusRunning   = "running"
	StatusError     = "error"
)

// DeriveStatus computes the "live" status from the Swarm state, falling back to the DB status.
func DeriveStatus(s docker.ServiceState, dbStatus string) string {
	if !s.Found || s.Desired == 0 {
		return dbStatus
	}
	if s.Running >= s.Desired {
		return StatusRunning
	}
	return StatusDeploying
}
