package deploy

import "github.com/proshik/krill/internal/docker"

// Статусы приложения.
const (
	StatusIdle      = "idle"
	StatusDeploying = "deploying"
	StatusRunning   = "running"
	StatusError     = "error"
)

// DeriveStatus вычисляет «живой» статус по состоянию Swarm, с откатом на статус из БД.
func DeriveStatus(s docker.ServiceState, dbStatus string) string {
	if !s.Found || s.Desired == 0 {
		return dbStatus
	}
	if s.Running >= s.Desired {
		return StatusRunning
	}
	return StatusDeploying
}
