package dbservice

import (
	"context"
	"fmt"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

type DBStore struct{ q *db.Queries }

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) GetInstance(ctx context.Context, id int64) (Instance, error) {
	r, err := s.q.GetDBInstance(ctx, id)
	if err != nil {
		return Instance{}, err
	}
	pw, err := secret.Dec(r.SuperuserPassword)
	if err != nil {
		return Instance{}, fmt.Errorf("db instance %d superuser password: %w", r.ID, err)
	}
	return Instance{
		ID: r.ID, OrganizationID: r.OrganizationID, Engine: r.Engine, Name: r.Name,
		AppName: r.AppName, Image: r.Image, Superuser: r.Superuser,
		SuperuserPassword: pw,
		ExternalPort:      r.ExternalPort, Status: r.Status, NodeHostname: r.NodeHostname,
		ConsoleExternalPort: r.ConsoleExternalPort,
	}, nil
}

func (s *DBStore) SetInstanceStatus(ctx context.Context, id int64, status string) error {
	return s.q.UpdateDBInstanceStatus(ctx, db.UpdateDBInstanceStatusParams{ID: id, Status: status})
}

func (s *DBStore) DeleteInstanceRow(ctx context.Context, id int64) error {
	return s.q.DeleteDBInstance(ctx, id)
}

func (s *DBStore) SetInstanceNode(ctx context.Context, id int64, hostname string) error {
	return s.q.SetDBInstanceNode(ctx, db.SetDBInstanceNodeParams{ID: id, NodeHostname: hostname})
}
