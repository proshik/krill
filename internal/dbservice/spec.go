package dbservice

import "github.com/proshik/krill/internal/dbservice/drivers"

// volumeName/dbConstraint — thin wrappers kept for existing dbservice callers
// (RemoveInstanceContainers, tests); the pure logic itself now lives in
// drivers (shared with BuildSpec).
func volumeName(appName string) string { return drivers.VolumeName(appName) }

// dbConstraint pins a managed DB to a chosen node, or to the control-plane
// (manager) when none is selected. The named volume lives on that node.
func dbConstraint(nodeHostname string) string { return drivers.DBConstraint(nodeHostname) }
