DROP TABLE IF EXISTS deployments;
ALTER TABLE applications
  DROP COLUMN IF EXISTS dockerfile_path,
  DROP COLUMN IF EXISTS git_branch,
  DROP COLUMN IF EXISTS git_url,
  DROP COLUMN IF EXISTS source_type;
