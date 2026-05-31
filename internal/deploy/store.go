package deploy

import (
	"context"

	db "github.com/proshik/krill/internal/database/gen"
)

// DBStore реализует AppStore поверх sqlc-запросов.
type DBStore struct {
	q *db.Queries
}

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) GetApplication(ctx context.Context, id int64) (App, error) {
	a, err := s.q.GetApplication(ctx, id)
	if err != nil {
		return App{}, err
	}
	return App{
		ID:     a.ID,
		Name:   a.Name,
		Image:  a.Image,
		Tag:    a.Tag,
		Domain: a.Domain,
		Port:   a.Port,
		Env:    a.Env,
	}, nil
}

func (s *DBStore) SetStatus(ctx context.Context, id int64, status string) error {
	return s.q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: id, Status: status})
}
