package deploy

// Deployment triggers (the deployments.trigger CHECK): who asked for a deploy.
const (
	TriggerManual  = "manual"  // a person, from the web UI
	TriggerWebhook = "webhook" // a GitHub push or a CI deploy hook
	TriggerAPI     = "api"     // an agent-API token, over REST or MCP
	TriggerSystem  = "system"  // the control plane itself (the organization network migration)
)
