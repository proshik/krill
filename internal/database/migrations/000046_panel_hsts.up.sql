-- Strict-Transport-Security for the panel domain, in seconds. 0 is off: Krill
-- then sends max-age=0, which tells a browser to forget a policy an earlier
-- setting left behind. A policy only makes sense on a domain proven to work
-- over HTTPS — a browser that learned it for a broken domain refuses plain HTTP
-- there until it expires — so it is tied to the active state and reset whenever
-- the domain changes or is removed.
ALTER TABLE panel_gateway
    ADD COLUMN hsts_max_age INTEGER NOT NULL DEFAULT 0,
    ADD CONSTRAINT panel_gateway_hsts_chk CHECK (hsts_max_age >= 0 AND (state = 'active' OR hsts_max_age = 0));
