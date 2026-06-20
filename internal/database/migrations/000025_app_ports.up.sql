-- Raw L4 host ports published straight into an app container (host publish mode),
-- bypassing Traefik. host_port is unique per protocol across the cluster
-- (conservative, mirrors managed-DB external_port). The app↔DB cross-conflict is
-- enforced in the handler (can't be a single-table index).
CREATE TABLE app_ports (
    id             BIGSERIAL PRIMARY KEY,
    application_id BIGINT  NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    host_port      INTEGER NOT NULL,
    container_port INTEGER NOT NULL,
    protocol       TEXT    NOT NULL DEFAULT 'tcp' CHECK (protocol IN ('tcp','udp')),
    UNIQUE (host_port, protocol)
);
CREATE INDEX idx_app_ports_application_id ON app_ports (application_id);
