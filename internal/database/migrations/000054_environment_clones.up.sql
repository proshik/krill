-- Ownership records intentionally have no source/target FKs: pending cleanup
-- must survive project deletion and completed history must not block deletes.
CREATE TABLE environment_clones (
    request_key TEXT PRIMARY KEY CHECK (request_key ~ '^[a-f0-9]{32}$'),
    user_id BIGINT NOT NULL,
    organization_id BIGINT NOT NULL,
    project_id BIGINT NOT NULL,
    source_id BIGINT NOT NULL,
    destination_name TEXT NOT NULL,
    destination_slug TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'preparing'
        CHECK (state IN ('preparing', 'cleanup', 'succeeded', 'aborted')),
    target_id BIGINT,
    -- Typed safe identifiers only; never application snapshots or credentials.
    resource_intent TEXT NOT NULL DEFAULT '',
    deadline TIMESTAMPTZ NOT NULL,
    fences_pruned BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((state = 'succeeded') = (target_id IS NOT NULL))
);

-- A completed operation releases this reservation; history is not a perpetual
-- name claim. A fresh request may reuse a slug after the target was deleted.
CREATE TABLE environment_clone_reservations (
    project_id BIGINT NOT NULL,
    slug TEXT NOT NULL,
    request_key TEXT NOT NULL UNIQUE REFERENCES environment_clones(request_key),
    PRIMARY KEY (project_id, slug)
);
