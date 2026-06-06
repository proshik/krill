-- Optional container command override (maps to Swarm ContainerSpec.Args, i.e.
-- docker-compose `command:`). Overrides the image's default CMD while keeping
-- its ENTRYPOINT. Example: "start-dev" for the Keycloak image. NULL = use the
-- image default.
ALTER TABLE applications ADD COLUMN command TEXT;
