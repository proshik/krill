package hostshare

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	return s.inLockedTx(ctx, []string{"krill.domain-host:" + host}, fn)
}

// inLockedTx takes the transaction-scoped advisory locks in the given order,
// then runs fn. Every caller taking several locks must use the same order.
func (s *Store) inLockedTx(ctx context.Context, keys []string, fn func(q *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	for _, key := range keys {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key); err != nil {
			return fmt.Errorf("lock %s: %w", key, err)
		}
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
			Paths:    traefik.NormalizePaths(traefik.SplitPaths(r.Paths)),
			Redirect: r.RedirectToPrimary,
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
		if err := reservedErr(ctx, q, p.Host); err != nil {
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

// SetRedirect turns a domain's redirect to its application's primary domain on
// or off. Turning it on is refused on the primary and on a host other
// applications share (a redirect answers every path of the host).
func (s *Store) SetRedirect(ctx context.Context, domainID int64, on bool) error {
	d, err := db.New(s.pool).GetDomain(ctx, domainID)
	if err != nil {
		return fmt.Errorf("load domain: %w", err)
	}
	return s.inHostTx(ctx, d.Host, func(q *db.Queries) error {
		rows, err := q.ListDomainClaimsByHost(ctx, d.Host)
		if err != nil {
			return fmt.Errorf("list host claims: %w", err)
		}
		all := claims(rows)
		var self *Claim
		for i, r := range rows {
			if r.ID == domainID {
				if on && r.IsPrimary {
					return &ConflictError{Key: KeyRedirectOnPrimary}
				}
				self = &all[i]
			}
		}
		if self == nil {
			return pgx.ErrNoRows
		}
		if on {
			cand := *self
			cand.Redirect = true
			if err := Check(all, cand); err != nil {
				return err
			}
		}
		n, err := q.SetDomainRedirect(ctx, db.SetDomainRedirectParams{ID: domainID, Redirect: on})
		if err != nil {
			return err
		}
		if n == 0 {
			// Became primary between the read and the write.
			return &ConflictError{Key: KeyRedirectOnPrimary}
		}
		return nil
	})
}

// MakePrimary makes a domain its application's primary: is_primary moves to it
// and applications.domain follows. Already primary is a no-op. Refused for a
// redirecting domain and for a host another application also claims, since a
// primary host is exclusive (ReservePrimaryHost).
//
// Lock order: an application advisory lock, the host lock, the application
// row, then domains rows. Two primary changes of one app each lock their own
// target row and then rewrite every row of the app, so without the
// application lock two of them on different hosts would deadlock. An
// application delete locks the application row and then (cascade) its
// domains, so that row comes before any domains row here.
func (s *Store) MakePrimary(ctx context.Context, domainID int64) error {
	d, err := db.New(s.pool).GetDomain(ctx, domainID)
	if err != nil {
		return fmt.Errorf("load domain: %w", err)
	}
	keys := []string{fmt.Sprintf("krill.domain-primary:%d", d.ApplicationID), "krill.domain-host:" + d.Host}
	return s.inLockedTx(ctx, keys, func(q *db.Queries) error {
		// Application row before domains rows, in the order an application
		// delete (and its cascade) takes them. A deleted app is ErrNoRows.
		if _, err := q.LockApplicationRow(ctx, d.ApplicationID); err != nil {
			return err
		}
		if err := reservedErr(ctx, q, d.Host); err != nil {
			return err
		}
		rows, err := q.ListDomainClaimsByHost(ctx, d.Host)
		if err != nil {
			return fmt.Errorf("list host claims: %w", err)
		}
		var self *db.ListDomainClaimsByHostRow
		for i, r := range rows {
			if r.ID == domainID {
				self = &rows[i]
			}
		}
		if self == nil {
			return pgx.ErrNoRows
		}
		if self.IsPrimary {
			return nil
		}
		if self.RedirectToPrimary {
			return &ConflictError{Key: KeyPrimaryRedirects}
		}
		for _, r := range rows {
			if r.ApplicationID != self.ApplicationID {
				return &ConflictError{Key: KeyPrimaryShared}
			}
		}
		if err := q.SetPrimaryDomainRow(ctx, db.SetPrimaryDomainRowParams{ApplicationID: self.ApplicationID, ID: domainID}); err != nil {
			return fmt.Errorf("move primary: %w", err)
		}
		if err := q.SetApplicationDomain(ctx, db.SetApplicationDomainParams{ID: self.ApplicationID, Domain: d.Host}); err != nil {
			return fmt.Errorf("set application domain: %w", err)
		}
		return nil
	})
}

// CreateApplication reserves an exclusive primary host and creates its app and
// domain atomically. It must use the same lock as Add after host uniqueness is removed.
func (s *Store) CreateApplication(ctx context.Context, p db.CreateApplicationParams) (db.Application, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.Application{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	out, err := CreateApplicationTx(ctx, tx, p)
	if err != nil {
		return db.Application{}, err
	}
	return out, tx.Commit(ctx)
}

// PrimarySettings are the primary domain's settings and the registry a new
// application gets in the same transaction as its creation.
type PrimarySettings struct {
	Exposed    bool
	Paths      []string
	TLS        bool
	RegistryID *int64
}

// CreateApplicationConfigured is CreateApplication with the primary domain's
// exposure, paths and TLS and the app's registry written in the same
// transaction, under the host reservation: a failure leaves neither the name
// nor the host taken, and no other app can claim the host in between. The
// host is exclusive to the new app, so no sharing rule applies.
func (s *Store) CreateApplicationConfigured(ctx context.Context, p db.CreateApplicationParams, set PrimarySettings) (db.Application, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.Application{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	out, err := CreateApplicationTx(ctx, tx, p)
	if err != nil {
		return db.Application{}, err
	}
	q := db.New(tx)
	paths := traefik.NormalizePaths(set.Paths)
	if set.Exposed || len(paths) > 0 {
		doms, err := q.ListDomainsByApplication(ctx, out.ID)
		if err != nil || len(doms) != 1 {
			return db.Application{}, fmt.Errorf("read back primary domain: %v", err)
		}
		if err := q.UpdateDomainExposure(ctx, db.UpdateDomainExposureParams{ID: doms[0].ID, Exposed: set.Exposed, Paths: strings.Join(paths, "\n")}); err != nil {
			return db.Application{}, err
		}
	}
	if set.TLS {
		if err := q.SetDomainTLSByHost(ctx, db.SetDomainTLSByHostParams{Host: p.Domain, Tls: true}); err != nil {
			return db.Application{}, err
		}
	}
	if set.RegistryID != nil {
		if err := q.SetApplicationRegistry(ctx, db.SetApplicationRegistryParams{ID: out.ID, RegistryID: set.RegistryID}); err != nil {
			return db.Application{}, err
		}
	}
	return out, tx.Commit(ctx)
}

// ReservePrimaryHost must run in the same transaction as the app/domain writes.
// Environment cloning uses it with a preallocated app ID and captured fields.
// The namespace and the reserved hosts (reservedErr) are shared with every
// domain mutation.
func ReservePrimaryHost(ctx context.Context, tx pgx.Tx, host string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "krill.domain-host:"+host); err != nil {
		return fmt.Errorf("lock host: %w", err)
	}
	q := db.New(tx)
	if err := reservedErr(ctx, q, host); err != nil {
		return err
	}
	n, err := q.CountDomainsByHost(ctx, host)
	if err != nil {
		return err
	}
	if n > 0 {
		return &ConflictError{Key: KeyHostTaken}
	}
	return nil
}

// CreateApplicationTx lets a larger atomic operation own the commit. An error
// requires the caller to roll back the transaction, including its app insert.
func CreateApplicationTx(ctx context.Context, tx pgx.Tx, p db.CreateApplicationParams) (db.Application, error) {
	if err := ReservePrimaryHost(ctx, tx, p.Domain); err != nil {
		return db.Application{}, err
	}
	q := db.New(tx)
	out, err := q.CreateApplication(ctx, p)
	if err != nil {
		return db.Application{}, err
	}
	_, err = q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: out.ID, Host: p.Domain, IsPrimary: true,
	})
	return out, err
}

// SetPanelDomain reserves the panel host exclusively against every app claim
// and the link's hosts.
func (s *Store) SetPanelDomain(ctx context.Context, host string) error {
	return s.inHostTx(ctx, host, func(q *db.Queries) error {
		if r, err := linkReservation(ctx, q, host); err != nil {
			return err
		} else if r != NotReserved {
			return &ConflictError{Key: r.Key()}
		}
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
