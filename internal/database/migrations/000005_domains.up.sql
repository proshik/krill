CREATE TABLE domains (
    id             BIGSERIAL PRIMARY KEY,
    application_id BIGINT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    host           TEXT NOT NULL,
    tls            BOOLEAN NOT NULL DEFAULT false,
    is_primary     BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX domains_host_key ON domains(host);
CREATE INDEX domains_application_id_idx ON domains(application_id);

-- Backfill: one primary domain per existing application from applications.domain.
INSERT INTO domains (application_id, host, tls, is_primary)
SELECT id, domain, false, true FROM applications;
