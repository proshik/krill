ALTER TABLE applications ADD COLUMN env JSONB NOT NULL DEFAULT '{}';
UPDATE applications SET env = COALESCE(
    (SELECT jsonb_object_agg(split_part(line, '=', 1), substring(line from position('=' in line) + 1))
     FROM unnest(string_to_array(env_text, E'\n')) AS line
     WHERE line <> '' AND position('=' in line) > 0), '{}'::jsonb);
