-- Several apps of one environment may share a host on disjoint paths; the rules
-- are enforced by internal/hostshare under an advisory lock, not by the schema.
DROP INDEX IF EXISTS domains_host_key;
CREATE INDEX IF NOT EXISTS domains_host_idx ON domains(host);
