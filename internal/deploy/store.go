package deploy

import (
	"context"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/traefik"
)

// DBStore implements Store on top of sqlc queries.
type DBStore struct {
	q *db.Queries
}

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) GetApplication(ctx context.Context, id int64) (App, error) {
	a, err := s.q.GetApplication(ctx, id)
	if err != nil {
		return App{}, err
	}
	doms, derr := s.q.ListDomainsByApplication(ctx, a.ID)
	if derr != nil {
		return App{}, derr
	}
	out := App{
		ID:             a.ID,
		Name:           a.Name,
		Image:          a.Image,
		Tag:            a.Tag,
		Domain:         a.Domain,
		Port:           a.Port,
		Env:            a.Env,
		SourceType:     a.SourceType,
		GitURL:         a.GitUrl,
		GitBranch:      a.GitBranch,
		DockerfilePath: a.DockerfilePath,
	}
	for _, d := range doms {
		out.Domains = append(out.Domains, traefik.Domain{Host: d.Host, TLS: d.Tls})
	}
	return out, nil
}

func (s *DBStore) SetStatus(ctx context.Context, id int64, status string) error {
	return s.q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: id, Status: status})
}

func (s *DBStore) CreateDeployment(ctx context.Context, appID int64, trigger string) (int64, error) {
	d, err := s.q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: appID, Trigger: trigger})
	if err != nil {
		return 0, err
	}
	return d.ID, nil
}

func (s *DBStore) FinishDeployment(ctx context.Context, deployID int64, status, imageTag, errMsg, log string) error {
	return s.q.FinishDeployment(ctx, db.FinishDeploymentParams{
		ID: deployID, Status: status, ImageTag: imageTag, ErrorMessage: errMsg, Log: log,
	})
}

func (s *DBStore) GetDeploymentApp(ctx context.Context, deployID int64) (App, error) {
	dep, err := s.q.GetDeployment(ctx, deployID)
	if err != nil {
		return App{}, err
	}
	return s.GetApplication(ctx, dep.ApplicationID)
}

func (s *DBStore) ClearOldDeploymentLogs(ctx context.Context) error {
	return s.q.ClearOldDeploymentLogs(ctx)
}
