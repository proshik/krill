package builder

import (
	"context"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"strings"
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

// ValidateBuildRequest проверяет пользовательский ввод перед запуском git/docker.
func ValidateBuildRequest(req BuildRequest) error {
	u, err := url.Parse(req.GitURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("git_url must be an http(s) URL")
	}
	if strings.HasPrefix(req.GitBranch, "-") {
		return errors.New("git_branch must not start with '-'")
	}
	df := req.DockerfilePath
	if df != "" {
		if filepath.IsAbs(df) || strings.HasPrefix(df, "-") {
			return errors.New("dockerfile_path must be a relative path not starting with '-'")
		}
		for _, seg := range strings.Split(filepath.ToSlash(filepath.Clean(df)), "/") {
			if seg == ".." {
				return errors.New("dockerfile_path must not escape the build context")
			}
		}
	}
	return nil
}

// cloneArgs — argv для git clone (мелкий клон одной ветки).
func cloneArgs(gitURL, branch, dir string) []string {
	return []string{"clone", "--branch", branch, "--depth", "1", "--", gitURL, dir}
}

// buildArgs — argv для docker build.
func buildArgs(tag, dockerfile, context string) []string {
	return []string{"build", "-t", tag, "-f", dockerfile, context}
}

// contextDir — каталог сборки: директория, где лежит Dockerfile.
func contextDir(repoDir, dockerfilePath string) string {
	return filepath.Join(repoDir, filepath.Dir(dockerfilePath))
}
