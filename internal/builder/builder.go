package builder

import (
	"context"
	"io"
	"path/filepath"
)

// BuildRequest — параметры сборки образа из git+Dockerfile.
type BuildRequest struct {
	AppID          int64
	DeployID       int64
	GitURL         string
	GitBranch      string
	DockerfilePath string // относительно корня репо
	ImageTag       string // напр. krill-7:42
}

// Builder клонирует репозиторий и собирает образ, стримя вывод в out.
type Builder interface {
	Build(ctx context.Context, req BuildRequest, out io.Writer) error
}

// cloneArgs — argv для git clone (мелкий клон одной ветки).
func cloneArgs(gitURL, branch, dir string) []string {
	return []string{"clone", "--branch", branch, "--depth", "1", gitURL, dir}
}

// buildArgs — argv для docker build.
func buildArgs(tag, dockerfile, context string) []string {
	return []string{"build", "-t", tag, "-f", dockerfile, context}
}

// contextDir — каталог сборки: директория, где лежит Dockerfile.
func contextDir(repoDir, dockerfilePath string) string {
	return filepath.Join(repoDir, filepath.Dir(dockerfilePath))
}
