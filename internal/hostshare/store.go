package hostshare

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/traefik"
)

// Store checks and writes domain rows in one transaction under a per-host
// advisory lock, so two concurrent claims cannot both pass Check.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// AppRef is an application whose labels must be re-applied after a change.
type AppRef struct {
	ID   int64
	Port int32
}

type AddParams struct {
	AppID   int64
	Host    string
	TLS     bool
	Exposed bool
	Paths   []string
}

func (s *Store) inHostTx(ctx context.Context, host string, fn func(q *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "krill.domain-host:"+host); err != nil {
		return fmt.Errorf("lock host: %w", err)
	}
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func claims(rows []db.ListDomainClaimsByHostRow) []Claim {
	out := make([]Claim, 0, len(rows))
	for _, r := range rows {
		out = append(out, Claim{
			DomainID: r.ID, AppID: r.ApplicationID, AppName: r.AppName, EnvID: r.EnvironmentID,
			Paths: traefik.NormalizePaths(traefik.SplitPaths(r.Paths)),
		})
	}
	return out
}

// Add creates a non-primary domain row. On a host already in use the new row
// takes the host's TLS, whatever p.TLS says.
func (s *Store) Add(ctx context.Context, p AddParams) (db.Domain, error) {
	paths := traefik.NormalizePaths(p.Paths)
	var out db.Domain
	err := s.inHostTx(ctx, p.Host, func(q *db.Queries) error {
		if err := panelAvailable(ctx, q, p.Host); err != nil {
			return err
		}
		app, err := q.GetApplication(ctx, p.AppID)
		if err != nil {
			return fmt.Errorf("load application: %w", err)
		}
		rows, err := q.ListDomainClaimsByHost(ctx, p.Host)
		if err != nil {
			return fmt.Errorf("list host claims: %w", err)
		}
		if err := Check(claims(rows), Claim{AppID: app.ID, AppName: app.Name, EnvID: app.EnvironmentID, Paths: paths}); err != nil {
			return err
		}
		tls := p.TLS
		if len(rows) > 0 {
			tls = rows[0].Tls
		}
		out, err = q.CreateDomain(ctx, db.CreateDomainParams{
			ApplicationID: app.ID, Host: p.Host, Tls: tls, IsPrimary: false,
			Exposed: p.Exposed, Paths: strings.Join(paths, "\n"),
		})
		return err
	})
	return out, err
}

// UpdatePaths changes a row's exposure and paths.
func (s *Store) UpdatePaths(ctx context.Context, domainID int64, exposed bool, paths []string) error {
	d, err := db.New(s.pool).GetDomain(ctx, domainID)
	if err != nil {
		return fmt.Errorf("load domain: %w", err)
	}
	norm := traefik.NormalizePaths(paths)
	return s.inHostTx(ctx, d.Host, func(q *db.Queries) error {
		rows, err := q.ListDomainClaimsByHost(ctx, d.Host)
		if err != nil {
			return fmt.Errorf("list host claims: %w", err)
		}
		all := claims(rows)
		var self *Claim
		for i := range all {
			if all[i].DomainID == domainID {
				self = &all[i]
			}
		}
		if self == nil {
			return pgx.ErrNoRows
		}
		cand := *self
		cand.Paths = norm
		if err := Check(all, cand); err != nil {
			return err
		}
		return q.UpdateDomainExposure(ctx, db.UpdateDomainExposureParams{
			ID: domainID, Exposed: exposed, Paths: strings.Join(norm, "\n"),
		})
	})
}

// SetTLS switches TLS for every row of the domain's host and returns the apps whose labels
// must be re-applied.
func (s *Store) SetTLS(ctx context.Context, domainID int64, tls bool) ([]AppRef, error) {
	d, err := db.New(s.pool).GetDomain(ctx, domainID)
	if err != nil {
		return nil, fmt.Errorf("load domain: %w", err)
	}
	var refs []AppRef
	err = s.inHostTx(ctx, d.Host, func(q *db.Queries) error {
		rows, err := q.ListDomainClaimsByHost(ctx, d.Host)
		if err != nil {
			return fmt.Errorf("list host claims: %w", err)
		}
		found := false
		for _, row := range rows {
			if row.ID == domainID {
				found = true
			}
		}
		if !found {
			return pgx.ErrNoRows
		}
		if err := q.SetDomainTLSByHost(ctx, db.SetDomainTLSByHostParams{Host: d.Host, Tls: tls}); err != nil {
			return err
		}
		seen := map[int64]bool{}
		for _, r := range rows {
			if !seen[r.ApplicationID] {
				seen[r.ApplicationID] = true
				refs = append(refs, AppRef{ID: r.ApplicationID, Port: r.AppPort})
			}
		}
		return nil
	})
	return refs, err
}

// panelAvailable is checked under the host lock shared with SetPanelDomain.
func panelAvailable(ctx context.Context, q *db.Queries, host string) error {
	row, err := q.GetPanelGateway(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read panel settings: %w", err)
	}
	if row.State != "off" && strings.EqualFold(row.Host, host) {
		return &ConflictError{Key: KeyHostTaken}
	}
	return nil
}

// CreateApplication reserves an exclusive primary host and creates its app and
// domain atomically. It must use the same lock as Add after host uniqueness is removed.
func (s *Store) CreateApplication(ctx context.Context, p db.CreateApplicationParams) (db.Application, error) {
	var out db.Application
	err := s.inHostTx(ctx, p.Domain, func(q *db.Queries) error {
		if err := panelAvailable(ctx, q, p.Domain); err != nil {
			return err
		}
		n, err := q.CountDomainsByHost(ctx, p.Domain)
		if err != nil {
			return err
		}
		if n > 0 {
			return &ConflictError{Key: KeyHostTaken}
		}
		out, err = q.CreateApplication(ctx, p)
		if err != nil {
			return err
		}
		_, err = q.CreateDomain(ctx, db.CreateDomainParams{
			ApplicationID: out.ID, Host: p.Domain, IsPrimary: true,
		})
		return err
	})
	return out, err
}

// SetPanelDomain reserves the panel host exclusively against every app claim.
func (s *Store) SetPanelDomain(ctx context.Context, host string) error {
	return s.inHostTx(ctx, host, func(q *db.Queries) error {
		n, err := q.CountDomainsByHost(ctx, host)
		if err != nil {
			return err
		}
		if n > 0 {
			return &ConflictError{Key: KeyHostTaken}
		}
		return q.SetPanelDomainPending(ctx, host)
	})
}
