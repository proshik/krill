CREATE TABLE metric_samples (
    id              BIGSERIAL PRIMARY KEY,
    ts              TIMESTAMPTZ NOT NULL DEFAULT now(),
    component       TEXT NOT NULL,
    cpu_pct         REAL NOT NULL,
    mem_bytes       BIGINT NOT NULL,
    mem_limit_bytes BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX metric_samples_ts_idx ON metric_samples(ts);
CREATE INDEX metric_samples_component_ts_idx ON metric_samples(component, ts);
