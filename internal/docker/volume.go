package docker

import "strconv"

// VolumeName builds the Docker named-volume for an application volume.
// Deterministic and namespaced by app id so volumes never collide across
// tenants. Callers are responsible for validating name (a short slug) before
// passing it here so the result is always a safe Docker volume name.
func VolumeName(appID int64, name string) string {
	return "krill-vol-" + strconv.FormatInt(appID, 10) + "-" + name
}
