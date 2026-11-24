package api

import "time"

// SetAppLogsTimeout shortens AppLogs' read bound for a test and returns the
// function restoring it.
func SetAppLogsTimeout(d time.Duration) func() {
	old := appLogsTimeout
	appLogsTimeout = d
	return func() { appLogsTimeout = old }
}
