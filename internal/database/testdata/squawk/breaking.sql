-- Self-check fixture for `make lint-migrations`: one statement per rule kept in .squawk.toml.
-- Each statement is preceded by an `-- expect: <rule>` line; the target fails unless squawk
-- reports every expected rule, so removing a kept rule from the config is caught.
-- This file is never applied to a database.

-- expect: ban-drop-column
ALTER TABLE users DROP COLUMN is_admin;

-- expect: ban-drop-table
DROP TABLE sessions;

-- expect: renaming-column
ALTER TABLE users RENAME COLUMN email TO email_address;

-- expect: renaming-table
ALTER TABLE users RENAME TO accounts;

-- expect: changing-column-type
ALTER TABLE users ALTER COLUMN email TYPE VARCHAR(320);

-- expect: adding-required-field
ALTER TABLE users ADD COLUMN nickname TEXT NOT NULL;

-- expect: adding-not-nullable-field
ALTER TABLE projects ALTER COLUMN description SET NOT NULL;

-- expect: disallowed-unique-constraint
ALTER TABLE users ADD CONSTRAINT users_email_uniq UNIQUE (email);

-- expect: adding-foreign-key-constraint
ALTER TABLE deployments ADD CONSTRAINT deployments_user_fk FOREIGN KEY (user_id) REFERENCES users (id);

-- expect: ban-drop-not-null
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
