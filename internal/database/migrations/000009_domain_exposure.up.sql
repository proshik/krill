ALTER TABLE domains
    ADD COLUMN exposed BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN paths   TEXT    NOT NULL DEFAULT '';

-- preserve current behavior: every existing domain stays public
UPDATE domains SET exposed = true;
