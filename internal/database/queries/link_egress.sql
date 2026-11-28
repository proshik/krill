-- Krill link egress (KRL-002). Hosts are stored normalized (panel.NormalizeHost:
-- lower case, no trailing dot), so they are compared as stored.

-- name: ListLinkDestinations :many
-- relay: the destination list.
SELECT host, port FROM link_destinations ORDER BY host, port;

-- name: AddLinkDestination :execrows
INSERT INTO link_destinations (host, port) VALUES ($1, $2)
ON CONFLICT (host, port) DO NOTHING;

-- name: DeleteLinkDestination :execrows
DELETE FROM link_destinations WHERE host = $1 AND port = $2;

-- name: LinkDestinationAllowed :one
-- relay: whether one egress stream's destination is on the list.
SELECT EXISTS (SELECT 1 FROM link_destinations WHERE host = $1 AND port = $2)::boolean;

-- name: ListLinkOrgGrants :many
-- client: the organizations the operator opened the link to.
SELECT org_id FROM link_org_grants ORDER BY org_id;

-- name: GrantLinkOrg :exec
INSERT INTO link_org_grants (org_id) VALUES ($1) ON CONFLICT (org_id) DO NOTHING;

-- name: RevokeLinkOrg :execrows
DELETE FROM link_org_grants WHERE org_id = $1;

-- name: LinkOrgGranted :one
SELECT EXISTS (SELECT 1 FROM link_org_grants WHERE org_id = $1)::boolean;

-- name: ListAppLinkDestinations :many
-- client: an app's saved selection.
SELECT host, port FROM app_link_destinations WHERE application_id = $1 ORDER BY host, port;

-- name: DeleteAppLinkDestinations :exec
DELETE FROM app_link_destinations WHERE application_id = $1;

-- name: AddAppLinkDestination :exec
INSERT INTO app_link_destinations (application_id, host, port) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: ListAppLinkEgress :many
-- client: the selection a deploy of the app applies — the saved one while the
-- installation is a client and the app's organization is granted, nothing
-- otherwise.
SELECT d.host, d.port
FROM app_link_destinations d
JOIN applications a ON a.id = d.application_id
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
JOIN link_org_grants g ON g.org_id = p.organization_id
JOIN link_settings s ON s.id AND s.role = 'client'
WHERE d.application_id = $1
ORDER BY d.host, d.port;

-- name: LinkEgressDecision :one
-- client: the data-plane facts one egress connection is decided on — whether
-- the app's organization is granted and whether (host, port) is in the app's
-- saved selection — read for every connection, so a revoke applies at once.
SELECT
  EXISTS (
    SELECT 1 FROM applications a
    JOIN environments e ON e.id = a.environment_id
    JOIN projects p ON p.id = e.project_id
    JOIN link_org_grants g ON g.org_id = p.organization_id
    WHERE a.id = sqlc.arg(app_id)
  )::boolean AS granted,
  EXISTS (
    SELECT 1 FROM app_link_destinations ad
    WHERE ad.application_id = sqlc.arg(app_id) AND ad.host = sqlc.arg(host)::text AND ad.port = sqlc.arg(port)::int
  )::boolean AS selected;

-- name: ListLinkEgressApps :many
-- client: every saved selection with its app's place and grant, for the
-- sidecar's aliases and routes, the manifest and the Link page.
SELECT d.application_id, d.host, d.port, a.name AS app_name, e.name AS env_name,
       p.name AS project_name, p.organization_id AS org_id,
       (g.org_id IS NOT NULL)::boolean AS granted
FROM app_link_destinations d
JOIN applications a ON a.id = d.application_id
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
LEFT JOIN link_org_grants g ON g.org_id = p.organization_id
ORDER BY d.application_id, d.host, d.port;

-- name: GetLinkEgressAppPath :one
-- client: an app's project/environment/name and organization.
SELECT a.name AS app_name, e.name AS env_name, p.name AS project_name, p.organization_id AS org_id
FROM applications a
JOIN environments e ON e.id = a.environment_id
JOIN projects p ON p.id = e.project_id
WHERE a.id = $1;

-- name: ListLatestLinkApplied :many
-- client: for each listed app, the newest successful deployment that recorded
-- its link selection (a link section in applied_config). A deployment by a
-- release without egress records none and is skipped: after a rollback onto
-- it and back, the selection the egress release last applied still counts.
SELECT DISTINCT ON (d.application_id) d.application_id, d.id, d.applied_config
FROM deployments d
WHERE d.application_id = ANY(sqlc.arg(ids)::bigint[]) AND d.status = 'done'
  AND d.applied_config -> 'sections' -> 'link' IS NOT NULL
ORDER BY d.application_id, d.started_at DESC, d.id DESC;

-- name: LockLinkEgressApp :one
-- client: serializes replacing one app's selection.
SELECT id FROM applications WHERE id = $1 FOR UPDATE;

-- name: CountLinkDestinations :one
SELECT count(*) FROM link_destinations;

-- name: ListLinkEgressServed :many
SELECT application_id, host, port, deploy_floor FROM link_egress_served ORDER BY application_id, host, port;

-- name: AddLinkEgressServed :exec
-- The floor is the app's newest deployment now: only a later one can retire
-- the name.
INSERT INTO link_egress_served (application_id, host, port, deploy_floor)
VALUES ($1, $2, $3, (SELECT COALESCE(max(id), 0) FROM deployments WHERE application_id = $1))
ON CONFLICT (application_id, host, port) DO NOTHING;

-- name: DeleteLinkEgressServed :exec
DELETE FROM link_egress_served WHERE application_id = $1 AND host = $2 AND port = $3;

-- name: DeleteLinkEgressServedApp :exec
DELETE FROM link_egress_served WHERE application_id = $1;
