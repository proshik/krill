// Package observability runs Krill's own Grafana Alloy agent on every node.
// The agent ships host metrics to a Prometheus remote_write receiver and
// container logs to Loki; Krill keeps only the addresses and credentials.
//
// The agent is a global Swarm service whose configuration and passwords
// travel as Swarm configs and secrets named after their content, so a change
// is a new object plus a rolling update, and nothing secret sits in the
// service spec. Reconcile converges the service to the stored settings;
// Reconciler runs it in the background.
package observability
