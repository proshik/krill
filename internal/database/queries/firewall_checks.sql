-- name: RecordFirewallCheck :exec
INSERT INTO firewall_checks (node, checked_at) VALUES ($1, now())
ON CONFLICT (node) DO UPDATE SET checked_at = EXCLUDED.checked_at;

-- name: ListFirewallChecks :many
SELECT node, checked_at FROM firewall_checks;
