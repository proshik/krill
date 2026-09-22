package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/observability"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/secret"
)

// appsProvider serves the apps collector's scrape module (see
// observability.RenderAppsModule). Zero until wired.
type appsProvider struct {
	token    string
	mu       sync.Mutex
	lastPoll time.Time
}

func (s *Server) SetAppsProvider(token string) { s.apps.token = token }

func (s *Server) appsProviderLastPoll() time.Time {
	s.apps.mu.Lock()
	defer s.apps.mu.Unlock()
	return s.apps.lastPoll
}

// alloyAppsModule answers the apps collector's poll. Like gatewayConfig it
// 404s anything but a poll carrying its token, and anything that came through
// the gateway (the module holds every app's scrape token). A failure answers
// 500, never an empty module: the collector keeps the module it has on a
// failed poll, while an empty one would stop every app's collection.
func (s *Server) alloyAppsModule(w http.ResponseWriter, r *http.Request) {
	got, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !bearer || s.apps.token == "" || !panel.Matches(got, s.apps.token) || s.viaGateway(r) {
		http.NotFound(w, r)
		return
	}
	rows, err := s.q.ListMetricsScrapeTargets(r.Context())
	if err != nil {
		logFrom(r).Error("alloyAppsModule: list targets failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	targets := make([]observability.AppTarget, 0, len(rows))
	for _, row := range rows {
		if row.MetricsToken == nil {
			continue
		}
		tok, derr := secret.Dec(*row.MetricsToken)
		if derr != nil {
			// One app with an undecryptable token (a rotated KRILL_SECRET_KEY)
			// must not stop every other app's collection.
			logFrom(r).Error("alloyAppsModule: app metrics token unreadable; app skipped", "err", derr, "app_id", row.AppID)
			continue
		}
		targets = append(targets, observability.AppTarget{
			OrgID: row.OrgID, AppID: row.AppID, EndpointID: row.EndpointID,
			Org: row.OrgName, Project: row.ProjectName, Env: row.EnvName, App: row.AppName,
			Token: tok, Port: row.Port, Path: row.Path, Job: row.Job,
		})
	}
	nodes, err := s.appsTaskNodes(r.Context())
	if err != nil {
		logFrom(r).Error("alloyAppsModule: list task addresses failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body, err := observability.RenderAppsModule(targets, nodes)
	if err != nil {
		logFrom(r).Error("alloyAppsModule: render failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(body); err != nil {
		logFrom(r).Warn("alloyAppsModule: write failed", "err", err)
		return
	}
	s.apps.mu.Lock()
	s.apps.lastPoll = time.Now()
	s.apps.mu.Unlock()
}

// appsTaskNodes maps each app's running task IPs to the Krill name of their
// node: the node's label from node_labels, else its Swarm hostname — the
// same krill_node the node agent puts on host metrics and logs.
func (s *Server) appsTaskNodes(ctx context.Context) (map[int64][]observability.TaskNode, error) {
	ta, ok := s.engine.(docker.TaskAddresser)
	if !ok {
		return nil, errors.New("the docker engine cannot list task addresses")
	}
	addrs, err := ta.TaskAddresses(ctx)
	if err != nil {
		return nil, err
	}
	// Read the labels directly, not through nodeLabelMap: that helper degrades
	// to hostnames on a database error, which here would flip krill_node from
	// the label to the hostname for one poll and split every series in two.
	// Failing the poll instead keeps the collector on its previous module —
	// the same "required, not best-effort" rule the node agent follows.
	rows, err := s.q.ListNodeLabels(ctx)
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string, len(rows))
	for _, l := range rows {
		labels[l.SwarmNodeID] = l.Label
	}
	out := map[int64][]observability.TaskNode{}
	for _, a := range addrs {
		id, ok := appIDFromServiceName(a.ServiceName)
		if !ok {
			continue
		}
		network, err := s.q.GetOrganizationNetworkByApp(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if network == "" {
			network = s.cfg.Network
		}
		if a.Network != network {
			continue
		}
		name := labels[a.NodeID]
		if name == "" {
			name = a.NodeHostname
		}
		if name == "" {
			continue // an unresolved node: the target waits for the next poll rather than lose krill_node
		}
		out[id] = append(out[id], observability.TaskNode{IP: a.IP, Node: name})
	}
	return out, nil
}

// appIDFromServiceName parses "krill-<id>" (docker.ServiceName); anything
// else — a DB instance, the gateway, the collector itself — is not an app.
func appIDFromServiceName(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, "krill-")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}
