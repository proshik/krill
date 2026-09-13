-- name: InitPanelGateway :exec
INSERT INTO panel_gateway (id, secret) VALUES (1, $1)
ON CONFLICT (id) DO NOTHING;

-- name: GetPanelGateway :one
SELECT * FROM panel_gateway WHERE id = 1;

-- name: SetPanelGatewaySecret :exec
UPDATE panel_gateway SET secret = $1, updated_at = now() WHERE id = 1;

-- name: SetPanelDomainPending :exec
UPDATE panel_gateway SET host = $1, state = 'pending', updated_at = now() WHERE id = 1;

-- name: ActivatePanelDomain :execrows
UPDATE panel_gateway SET state = 'active', updated_at = now()
WHERE id = 1 AND state = 'pending' AND host = $1;

-- name: DisablePanelDomain :exec
UPDATE panel_gateway
SET host = '', state = 'off', direct_port_closed = false, direct_port_close_pending = false, updated_at = now()
WHERE id = 1;

-- name: SetPanelAllowedIPs :exec
UPDATE panel_gateway SET allowed_ips = $1, updated_at = now() WHERE id = 1;

-- name: SetPanelDirectPort :exec
UPDATE panel_gateway
SET direct_port_closed = $1, direct_port_close_pending = $2, updated_at = now()
WHERE id = 1;

