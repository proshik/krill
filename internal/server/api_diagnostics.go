package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/bucketbackup"
	"github.com/proshik/krill/internal/traefik"
	"github.com/proshik/krill/internal/volume"
)

// Diagnostics of the agent API: app config, domain status and backups, plus
// the adapters api.Service needs from the server (backup services, gateway).

// uiBackupTimeout is the run bound the web UI's "Back up now" gives a
// database or volume backup; a bucket backup gets bucketBackupTimeout.
const uiBackupTimeout = 30 * time.Minute

// apiBackups starts backups through the same services, with the same
// timeouts and the same double-run guard, as the web UI's "Back up now".
type apiBackups struct{ s *Server }

func (b apiBackups) Start(kind string, id int64, now time.Time) error {
	var err error
	switch kind {
	case api.BackupDatabase:
		if b.s.backupSvc == nil {
			return api.ErrBackupUnavailable
		}
		err = b.s.backupSvc.StartBackup(id, now, uiBackupTimeout)
	case api.BackupVolume:
		if b.s.volumeSvc == nil {
			return api.ErrBackupUnavailable
		}
		err = b.s.volumeSvc.StartVolumeBackup(id, now, uiBackupTimeout)
	case api.BackupBucket:
		if b.s.bucketBackups == nil {
			return api.ErrBackupUnavailable
		}
		err = b.s.bucketBackups.StartBackup(id, now, bucketBackupTimeout)
	default:
		return api.ErrBackupUnavailable
	}
	if errors.Is(err, backup.ErrBackupRunning) || errors.Is(err, volume.ErrVolumeBackupRunning) || errors.Is(err, bucketbackup.ErrRunning) {
		return api.ErrBackupBusy
	}
	return err
}

func (b apiBackups) Running(kind string, id int64) bool {
	switch kind {
	case api.BackupDatabase:
		return b.s.backupSvc != nil && b.s.backupSvc.Running(id)
	case api.BackupVolume:
		return b.s.volumeSvc != nil && b.s.volumeSvc.Running(id)
	case api.BackupBucket:
		return b.s.bucketBackups != nil && b.s.bucketBackups.Running(id)
	}
	return false
}

// apiGateway is what Krill knows of its gateway: Traefik publishes 80/443 in
// host mode, so it answers at the addresses of the nodes its task runs on —
// not at every manager — and, as configured evidence, at the manager's
// advertised address. A TLS check dials the node the task runs on when that
// is known, else the advertised address, else loopback (Krill runs on a
// manager).
type apiGateway struct{ s *Server }

func (g apiGateway) Locate(ctx context.Context) ([]string, string, error) {
	var addrs []string
	var errs []error
	if a := g.s.cfg.AdvertiseAddr; a != "" {
		ips, err := resolveHostIPs(ctx, a)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve KRILL_ADVERTISE_ADDR: %w", err))
		}
		for _, ip := range ips {
			addrs = append(addrs, ip.String())
		}
	}
	dial := g.s.cfg.AdvertiseAddr
	if dial == "" {
		dial = "127.0.0.1"
	}
	if g.s.engine == nil {
		return addrs, dial, errors.Join(errs...)
	}
	tasks, err := g.s.engine.ServiceTasks(ctx, traefik.ServiceName)
	if err != nil {
		errs = append(errs, fmt.Errorf("gateway tasks: %w", err))
	}
	onNode := map[string]bool{}
	for _, t := range tasks {
		if t.State == "running" {
			onNode[t.NodeID] = true
		}
	}
	if len(onNode) == 0 {
		return addrs, dial, errors.Join(errs...)
	}
	nodes, err := g.s.engine.Nodes(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list swarm nodes: %w", err))
	}
	var taskAddrs []string
	for _, n := range nodes {
		if !onNode[n.ID] {
			continue
		}
		if ip := net.ParseIP(n.Addr); ip != nil && !ip.IsUnspecified() {
			taskAddrs = append(taskAddrs, ip.String())
		}
	}
	if len(taskAddrs) > 0 {
		dial = taskAddrs[0]
	}
	return append(addrs, taskAddrs...), dial, errors.Join(errs...)
}

// apiAppConfig reports an app's saved settings, never a secret.
func (s *Server) apiAppConfig(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	res, err := s.apiSvc.AppConfig(r.Context(), ident, ref)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, res)
}

// apiDomainStatus checks the DNS and certificate of an app's domains.
func (s *Server) apiDomainStatus(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	res, err := s.apiSvc.DomainStatus(r.Context(), ident, ref)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, res)
}

// apiListBackups lists the organization's backups; ?app= or ?environment=
// narrow it.
func (s *Server) apiListBackups(w http.ResponseWriter, r *http.Request) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	q := r.URL.Query()
	res, err := s.apiSvc.ListBackups(r.Context(), ident, api.BackupFilter{App: q.Get("app"), Environment: q.Get("environment")})
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, res)
}

// apiBackupNow starts an existing backup in the background.
func (s *Server) apiBackupNow(w http.ResponseWriter, r *http.Request) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	kind := chi.URLParam(r, "kind")
	raw := chi.URLParam(r, "backupID")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, r, api.Invalid(fmt.Sprintf("backup id must be a number, got %q", raw)))
		return
	}
	res, err := s.apiSvc.BackupNow(r.Context(), ident, kind, id)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	logFrom(r).Info("api backup now requested", "kind", kind, "backup_id", id, "token_id", ident.TokenID, "user_id", ident.UserID)
	writeAPIJSON(w, res)
}
