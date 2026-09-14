ALTER TABLE panel_gateway DROP CONSTRAINT IF EXISTS panel_gateway_hsts_chk;
ALTER TABLE panel_gateway DROP COLUMN IF EXISTS hsts_max_age;
