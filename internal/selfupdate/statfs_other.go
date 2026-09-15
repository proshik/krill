//go:build !linux && !darwin

package selfupdate

import "errors"

// diskFree is not implemented off Linux and macOS; self-update only ever
// runs on Linux (see Supported), so the job fails closed if it gets here.
func diskFree(string) (uint64, error) {
	return 0, errors.New("selfupdate: free space check is not supported on this platform")
}
