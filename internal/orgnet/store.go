package orgnet

import (
	"context"

	db "github.com/proshik/krill/internal/database/gen"
)

// DBStore implements Store on top of sqlc queries.
type DBStore struct {
	q *db.Queries
}

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) ListOrganizations(ctx context.Context) ([]Org, error) {
	rows, err := s.q.ListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	orgs := make([]Org, 0, len(rows))
	for _, o := range rows {
		orgs = append(orgs, Org{ID: o.ID, NetworkName: o.NetworkName})
	}
	return orgs, nil
}

func (s *DBStore) SetOrganizationNetwork(ctx context.Context, orgID int64, network string) error {
	return s.q.SetOrganizationNetwork(ctx, db.SetOrganizationNetworkParams{ID: orgID, NetworkName: network})
}

func (s *DBStore) ListAppIDsByOrg(ctx context.Context, orgID int64) ([]int64, error) {
	return s.q.ListApplicationIDsByOrg(ctx, orgID)
}

func (s *DBStore) ListInstanceIDsByOrg(ctx context.Context, orgID int64) ([]int64, error) {
	return s.q.ListDBInstanceIDsByOrg(ctx, orgID)
}
