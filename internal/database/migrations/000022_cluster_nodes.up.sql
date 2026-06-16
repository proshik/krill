CREATE TABLE cluster_nodes (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    ssh_host      TEXT NOT NULL,
    ssh_port      INTEGER NOT NULL DEFAULT 22,
    ssh_user      TEXT NOT NULL,
    ssh_key       TEXT NOT NULL,                 -- secret.Enc(PEM private key)
    host_key      TEXT NOT NULL DEFAULT '',      -- accepted SSH host key (accept-new)
    swarm_node_id TEXT NOT NULL DEFAULT '',      -- resolved Swarm node ID after join
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
