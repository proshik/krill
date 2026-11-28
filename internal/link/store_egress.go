package link

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
)

// Destinations is the relay's destination list.
func (s *Store) Destinations(ctx context.Context) ([]Destination, error) {
	rows, err := s.q.ListLinkDestinations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Destination, 0, len(rows))
	for _, r := range rows {
		out = append(out, Destination{Host: r.Host, Port: int(r.Port)})
	}
	return out, nil
}

// MaxDestinations bounds the relay's list: the whole list travels in one
// control line (maxMessage) to every client, in its hello and in lists. Fifty
// names of the longest DNS length still fit.
const MaxDestinations = 50

// ErrTooManyDestinations refuses a destination past MaxDestinations.
var ErrTooManyDestinations = errors.New("the relay's destination list is full")

// addDestination adds d to the relay's list; false when it was there. The
// settings row lock serializes additions, so the bound holds.
func (s *Store) addDestination(ctx context.Context, d Destination) (bool, error) {
	added := false
	err := s.inTx(ctx, func(q *db.Queries) error {
		if _, err := q.LockLinkSettings(ctx); err != nil {
			return err
		}
		n, err := q.CountLinkDestinations(ctx)
		if err != nil {
			return err
		}
		if n >= MaxDestinations {
			ok, err := q.LinkDestinationAllowed(ctx, db.LinkDestinationAllowedParams{Host: d.Host, Port: int32(d.Port)})
			if err != nil || ok {
				return err
			}
			return ErrTooManyDestinations
		}
		rows, err := q.AddLinkDestination(ctx, db.AddLinkDestinationParams{Host: d.Host, Port: int32(d.Port)})
		added = rows == 1
		return err
	})
	return added, err
}

// removeDestination removes d from the relay's list; false when it was not
// there.
func (s *Store) removeDestination(ctx context.Context, d Destination) (bool, error) {
	n, err := s.q.DeleteLinkDestination(ctx, db.DeleteLinkDestinationParams{Host: d.Host, Port: int32(d.Port)})
	return n == 1, err
}

// destinationAllowed reports whether d is on the relay's list.
func (s *Store) destinationAllowed(ctx context.Context, d Destination) (bool, error) {
	return s.q.LinkDestinationAllowed(ctx, db.LinkDestinationAllowedParams{Host: d.Host, Port: int32(d.Port)})
}

// Grants lists the organizations granted the link.
func (s *Store) Grants(ctx context.Context) (map[int64]bool, error) {
	ids, err := s.q.ListLinkOrgGrants(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// Granted reports whether an organization is granted the link.
func (s *Store) Granted(ctx context.Context, orgID int64) (bool, error) {
	return s.q.LinkOrgGranted(ctx, orgID)
}

// setGrant grants or revokes an organization.
func (s *Store) setGrant(ctx context.Context, orgID int64, on bool) error {
	if on {
		return s.q.GrantLinkOrg(ctx, orgID)
	}
	_, err := s.q.RevokeLinkOrg(ctx, orgID)
	return err
}

// AppDestinations is an app's saved selection.
func (s *Store) AppDestinations(ctx context.Context, appID int64) ([]Destination, error) {
	rows, err := s.q.ListAppLinkDestinations(ctx, appID)
	if err != nil {
		return nil, err
	}
	out := make([]Destination, 0, len(rows))
	for _, r := range rows {
		out = append(out, Destination{Host: r.Host, Port: int(r.Port)})
	}
	return out, nil
}

// setAppDestinations replaces an app's saved selection; check, when set,
// sees the selection as it is under the app's row lock and can refuse.
func (s *Store) setAppDestinations(ctx context.Context, appID int64, ds []Destination, check func(saved []Destination) error) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		// Two replacements of one selection must not merge: the row lock
		// orders them (an empty selection has no rows of its own to lock).
		if _, err := q.LockLinkEgressApp(ctx, appID); err != nil {
			return err
		}
		if check != nil {
			rows, err := q.ListAppLinkDestinations(ctx, appID)
			if err != nil {
				return err
			}
			saved := make([]Destination, 0, len(rows))
			for _, r := range rows {
				saved = append(saved, Destination{Host: r.Host, Port: int(r.Port)})
			}
			if err := check(saved); err != nil {
				return err
			}
		}
		if err := q.DeleteAppLinkDestinations(ctx, appID); err != nil {
			return err
		}
		for _, d := range ds {
			if err := q.AddAppLinkDestination(ctx, db.AddAppLinkDestinationParams{ApplicationID: appID, Host: d.Host, Port: int32(d.Port)}); err != nil {
				return err
			}
		}
		return nil
	})
}

// egressDecision reads whether the app's organization is granted and whether
// d is in its saved selection.
func (s *Store) egressDecision(ctx context.Context, appID int64, d Destination) (granted, selected bool, err error) {
	r, err := s.q.LinkEgressDecision(ctx, db.LinkEgressDecisionParams{AppID: appID, Host: d.Host, Port: int32(d.Port)})
	return r.Granted, r.Selected, err
}

// EgressApp is one app with a saved selection.
type EgressApp struct {
	ID      int64
	OrgID   int64
	Path    string // project/environment/app
	Granted bool
	Saved   []Destination
}

// egressApps lists every app with a saved selection, by id.
func (s *Store) egressApps(ctx context.Context) (map[int64]*EgressApp, error) {
	rows, err := s.q.ListLinkEgressApps(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]*EgressApp{}
	for _, r := range rows {
		a := out[r.ApplicationID]
		if a == nil {
			a = &EgressApp{ID: r.ApplicationID, OrgID: r.OrgID, Granted: r.Granted,
				Path: r.ProjectName + "/" + r.EnvName + "/" + r.AppName}
			out[r.ApplicationID] = a
		}
		a.Saved = append(a.Saved, Destination{Host: r.Host, Port: int(r.Port)})
	}
	return out, nil
}

// appliedEgress is the part of deployments.applied_config the link reads:
// the selection the deployment was built with (deploy.AppliedConfig.Egress,
// same JSON key).
type appliedEgress struct {
	Egress []string `json:"egress"`
}

// appliedSelections returns, for each listed app, its newest successful
// deployment that recorded a link selection and that selection — a
// deployment by a release without egress (a rollback) records none and does
// not count as "nothing". An app without such a deployment has no entry.
func (s *Store) appliedSelections(ctx context.Context, appIDs []int64) (map[int64]appliedRec, error) {
	out := map[int64]appliedRec{}
	if len(appIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.ListLatestLinkApplied(ctx, appIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		var a appliedEgress
		if json.Unmarshal(r.AppliedConfig, &a) != nil {
			continue
		}
		rec := appliedRec{id: r.ID}
		for _, e := range a.Egress {
			if d, err := ParseDestinationString(e); err == nil {
				rec.dests = append(rec.dests, d)
			}
		}
		out[r.ApplicationID] = rec
	}
	return out, nil
}

// served lists the names the sidecar answers for, by app.
func (s *Store) served(ctx context.Context) (map[int64][]servedRec, error) {
	rows, err := s.q.ListLinkEgressServed(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64][]servedRec{}
	for _, r := range rows {
		out[r.ApplicationID] = append(out[r.ApplicationID], servedRec{dest: Destination{Host: r.Host, Port: int(r.Port)}, floor: r.DeployFloor})
	}
	return out, nil
}

// recordServed records the names the sidecar is about to answer for (a name
// already recorded keeps its floor).
func (s *Store) recordServed(ctx context.Context, appID int64, ds []Destination) error {
	for _, d := range ds {
		if err := s.q.AddLinkEgressServed(ctx, db.AddLinkEgressServedParams{ApplicationID: appID, Host: d.Host, Port: int32(d.Port)}); err != nil {
			return err
		}
	}
	return nil
}

// retireServed forgets names the sidecar no longer answers for.
func (s *Store) retireServed(ctx context.Context, appID int64, ds []Destination) error {
	for _, d := range ds {
		if err := s.q.DeleteLinkEgressServed(ctx, db.DeleteLinkEgressServedParams{ApplicationID: appID, Host: d.Host, Port: int32(d.Port)}); err != nil {
			return err
		}
	}
	return nil
}

// appPath is an app's project/environment/app and organization; false when
// the app is gone.
func (s *Store) appPath(ctx context.Context, appID int64) (string, int64, bool, error) {
	r, err := s.q.GetLinkEgressAppPath(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return r.ProjectName + "/" + r.EnvName + "/" + r.AppName, r.OrgID, true, nil
}
