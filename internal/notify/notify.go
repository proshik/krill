// Package notify sends operational alerts (deploy/backup failures, app health
// changes) to configured channels. v1 channel: Telegram. Best-effort: a failed
// send is logged, never blocks or fails the source operation. The bot token is
// encrypted at rest and never logged.
package notify

import "time"

// EventKind is the kind of operational event being reported.
type EventKind int

const (
	DeployFailed EventKind = iota
	BackupFailed
	AppDown
	AppRecovered
)

// Event is a resolved, human-targetable description of something that happened.
type Event struct {
	Kind    EventKind
	OrgID   int64
	Project string
	Env     string
	Target  string // app or db name
	Detail  string // short reason/state; may be empty
	Time    time.Time
}
