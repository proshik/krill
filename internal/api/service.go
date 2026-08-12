package api

// Service will hold the agent-facing API operations (app/status/env/deploy/etc.),
// added starting in Task 5. It is declared here, empty, only so Server.SetAPI
// can be wired ahead of the operations: the REST and MCP adapters need a stable
// field/parameter type to hang onto now, even though nothing calls through it
// until Task 8.
type Service struct{}
