-- Self-check fixture for `make lint-migrations`: shapes the previous release tolerates, which
-- squawk must accept with the rules in .squawk.toml. This file is never applied to a database.

-- A new table may carry NOT NULL columns, a UNIQUE and a foreign key of its own.
CREATE TABLE gadgets (
    id         BIGSERIAL PRIMARY KEY,
    org_id     BIGINT NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    slug       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

-- The repo's usual new-table shape: the unique index and foreign key are added after the
-- CREATE TABLE in the same migration, which the previous binary cannot notice either.
CREATE TABLE widgets (id BIGSERIAL PRIMARY KEY, owner_id BIGINT NOT NULL, name TEXT NOT NULL);
CREATE UNIQUE INDEX widgets_name_uniq ON widgets (name);
ALTER TABLE widgets ADD CONSTRAINT widgets_owner_fk FOREIGN KEY (owner_id) REFERENCES users (id);

-- A nullable column on an existing table.
ALTER TABLE users ADD COLUMN nickname TEXT;

-- A NOT NULL column on an existing table, with a default.
ALTER TABLE users ADD COLUMN theme TEXT NOT NULL DEFAULT 'system';

-- A non-unique index.
CREATE INDEX users_nickname_idx ON users (nickname);

-- An intentional break with the escape hatch: the ignore sits on the line directly before the
-- statement, and the reason follows it on the same line.
-- squawk-ignore ban-drop-column -- safe from v0.3.0: no release since v0.2.0 reads legacy_note
ALTER TABLE users DROP COLUMN legacy_note;
