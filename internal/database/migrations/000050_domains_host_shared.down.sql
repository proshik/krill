-- Fails if a host is still shared: un-share it before migrating down.
DROP INDEX IF EXISTS domains_host_idx;
CREATE UNIQUE INDEX domains_host_key ON domains(host);
