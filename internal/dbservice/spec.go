package dbservice

func volumeName(appName string) string { return appName + "-data" }

// dbConstraint pins a managed DB to a chosen node, or to the control-plane
// (manager) when none is selected. The named volume lives on that node.
func dbConstraint(nodeHostname string) string {
	if nodeHostname != "" {
		return "node.hostname==" + nodeHostname
	}
	return "node.role==manager"
}
