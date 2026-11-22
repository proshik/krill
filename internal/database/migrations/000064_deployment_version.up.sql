-- The human-facing version a deployment shipped (an image tag, a mutable tag
-- with its short digest, or branch@commit for a Dockerfile build), shown in
-- notifications and the daily digest. NULL for deployments made before it
-- was recorded, which keep showing the image reference.
ALTER TABLE deployments ADD COLUMN version TEXT;
