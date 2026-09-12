-- Intentionally a no-op. Rolling the image back to the Docker Hub reference would
-- point every MinIO instance at a repository that no longer exists, so a down
-- migration would only break redeploys again.
SELECT 1;
