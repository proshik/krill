package dbservice

import (
	"context"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

type DBStore struct{ q *db.Queries }

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) GetPostgres(ctx context.Context, id int64) (PostgresDB, error) {
	r, err := s.q.GetPostgres(ctx, id)
	if err != nil {
		return PostgresDB{}, err
	}
	return PostgresDB{
		ID: r.ID, EnvironmentID: r.EnvironmentID, Name: r.Name, AppName: r.AppName,
		DatabaseName: r.DatabaseName, DatabaseUser: r.DatabaseUser, DatabasePassword: secret.Dec(r.DatabasePassword),
		Image: r.Image, ExternalPort: r.ExternalPort, Status: r.Status, NodeHostname: r.NodeHostname,
	}, nil
}

func (s *DBStore) GetRedis(ctx context.Context, id int64) (RedisDB, error) {
	r, err := s.q.GetRedis(ctx, id)
	if err != nil {
		return RedisDB{}, err
	}
	return RedisDB{
		ID: r.ID, EnvironmentID: r.EnvironmentID, Name: r.Name, AppName: r.AppName,
		Password: secret.Dec(r.Password), Image: r.Image, ExternalPort: r.ExternalPort, Status: r.Status, NodeHostname: r.NodeHostname,
	}, nil
}

func (s *DBStore) SetPostgresStatus(ctx context.Context, id int64, status string) error {
	return s.q.UpdatePostgresStatus(ctx, db.UpdatePostgresStatusParams{ID: id, Status: status})
}

func (s *DBStore) SetRedisStatus(ctx context.Context, id int64, status string) error {
	return s.q.UpdateRedisStatus(ctx, db.UpdateRedisStatusParams{ID: id, Status: status})
}

func (s *DBStore) DeletePostgresRow(ctx context.Context, id int64) error {
	return s.q.DeletePostgres(ctx, id)
}

func (s *DBStore) DeleteRedisRow(ctx context.Context, id int64) error {
	return s.q.DeleteRedis(ctx, id)
}
