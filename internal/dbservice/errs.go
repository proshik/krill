package dbservice

import "strings"

// isNotFound reports whether err is a docker "no such service/target" error,
// which is a benign outcome when removing an already-absent service/proxy.
// The codebase detects docker errors by string match (see client.go), not
// errdefs, so this follows the same convention.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such") || strings.Contains(s, "not found")
}
