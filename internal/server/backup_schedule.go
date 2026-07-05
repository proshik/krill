package server

import (
	"net/http"
	"strings"
)

// safeReturnPath returns ret only if it is a safe same-origin path to redirect
// to (a rooted path, not a scheme/host-relative URL), else "".
func safeReturnPath(_ *http.Request, ret string) string {
	if ret == "" || !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") {
		return ""
	}
	if strings.Contains(ret, "://") {
		return ""
	}
	return ret
}

// SafeReturnPathForTest exposes safeReturnPath to the server_test package.
func SafeReturnPathForTest(r *http.Request, ret string) string { return safeReturnPath(r, ret) }
