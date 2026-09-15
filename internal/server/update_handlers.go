package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/buildinfo"
	"github.com/proshik/krill/internal/selfupdate"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/nav"
	"github.com/proshik/krill/internal/web/templates"
)

// updateChecker is the part of *selfupdate.Checker the Updates page needs.
type updateChecker interface {
	Current() string
	State() selfupdate.CheckState
	Available() (selfupdate.Release, bool)
	CheckNow(ctx context.Context) (selfupdate.Release, error)
}

// updateInstaller is the part of *selfupdate.Updater the Updates page needs.
type updateInstaller interface {
	Supported() (bool, string)
	Busy(ctx context.Context) string
	Start(tag string) error
	Rollback() error
	Status() selfupdate.Status
	LastResult() (selfupdate.Result, bool)
	Previous(ctx context.Context) (string, bool)
}

var (
	_ updateChecker   = (*selfupdate.Checker)(nil)
	_ updateInstaller = (*selfupdate.Updater)(nil)
)

// updateSupportedTTL is how long the page trusts a Supported answer: every
// call probes three directories with a write, which a page render (and every
// refresh of it) should not repeat.
const updateSupportedTTL = 30 * time.Second

// selfUpdate is what the server needs to update Krill from the UI. The zero
// value is "not wired": the page says so and every action is refused.
type selfUpdate struct {
	checker   updateChecker
	installer updateInstaller

	mu              sync.Mutex
	supported       bool
	supportedReason string
	supportedAt     time.Time
}

// SetSelfUpdate wires self-update from the UI. With either argument nil the
// feature stays unwired.
func (s *Server) SetSelfUpdate(c updateChecker, i updateInstaller) {
	if c == nil || i == nil {
		return
	}
	s.updates.checker = c
	s.updates.installer = i
}

func (s *Server) updatesWired() bool {
	return s.updates.checker != nil && s.updates.installer != nil
}

// updateSupported is installer.Supported, cached for updateSupportedTTL.
func (s *Server) updateSupported() (bool, string) {
	s.updates.mu.Lock()
	defer s.updates.mu.Unlock()
	if s.updates.supportedAt.IsZero() || time.Since(s.updates.supportedAt) >= updateSupportedTTL {
		s.updates.supported, s.updates.supportedReason = s.updates.installer.Supported()
		s.updates.supportedAt = time.Now()
	}
	return s.updates.supported, s.updates.supportedReason
}

// withUpdateBadge tells the sidebar an instance operator can install a newer
// release. It reads the checker's cached state only: no I/O per request.
func (s *Server) withUpdateBadge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.IsInstanceAdmin(r.Context()) && s.updatesWired() {
			if rel, ok := s.updates.checker.Available(); ok {
				r = r.WithContext(nav.WithUpdateAvailable(r.Context(), rel.Tag))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// updatesBack is the Updates page of the organization in the request path.
func updatesBack(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/updates"
}

// updatesPage renders the running and latest versions and the update,
// rollback and status blocks. Instance-admin only.
func (s *Server) updatesPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	render(w, r, http.StatusOK, templates.Updates(o, role, s.updatesView(r.Context())))
}

func (s *Server) updatesView(ctx context.Context) templates.UpdatesView {
	if !s.updatesWired() {
		return templates.UpdatesView{}
	}
	c, i := s.updates.checker, s.updates.installer
	st := c.State()
	rel, available := c.Available()
	current := c.Current()
	v := templates.UpdatesView{
		Wired:     true,
		Current:   current,
		IsRelease: buildinfo.IsRelease(current),
		Latest:    rel.Tag,
		LatestURL: rel.URL,
		CheckedAt: st.CheckedAt,
		Available: available,
	}
	if st.Err != nil {
		v.CheckErr = st.Err.Error()
	}
	supported, reason := s.updateSupported()
	v.Supported = supported
	if !supported {
		v.UnsupportedKey = "update.unsupported." + reason
	} else if busy := i.Busy(ctx); busy != "" {
		// Busy reads the database; an instance that cannot update anyway
		// (a dev machine) never asks.
		v.BusyKey = "update.busy." + busy
	}
	job := i.Status()
	v.Phase = string(job.Phase)
	if v.Phase == "" {
		v.Phase = string(selfupdate.PhaseIdle)
	}
	v.PhaseActive = job.Phase.Active()
	v.JobTag = job.Tag
	if job.Err != nil {
		v.JobErr = job.Err.Error()
	}
	if prev, ok := i.Previous(ctx); ok {
		v.Previous = prev
	}
	if last, ok := i.LastResult(); ok {
		v.HasLast = true
		v.LastFrom, v.LastTo, v.LastOK, v.LastReason = last.From, last.To, last.OK, last.Reason
	}
	return v
}

// checkUpdates looks up the latest release right away instead of waiting for
// the daily check. The checker throttles it to one lookup a minute.
func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.updatesWired() {
		s.flashErrT(w, r, "flash.err.update_unwired")
		return
	}
	// The lookup is bounded by the checker itself; a browser that navigates
	// away must not record a spurious "context canceled" as the check result.
	if _, err := s.updates.checker.CheckNow(context.WithoutCancel(r.Context())); err != nil {
		switch {
		case errors.Is(err, selfupdate.ErrThrottled):
			s.flashErrT(w, r, "flash.err.update_throttled")
		case errors.Is(err, selfupdate.ErrNoRelease):
			s.flashErrT(w, r, "flash.err.update_no_release")
		default:
			logFrom(r).Warn("checkUpdates: release lookup failed", "err", err)
			s.flashErrErr(w, r, "flash.err.update_check", err)
		}
		return
	}
	logFrom(r).Info("self-update check requested")
	s.flashOK(w, r, "flash.ok.update_checked")
	http.Redirect(w, r, updatesBack(o.ID), http.StatusSeeOther)
}

// installUpdate starts installing the posted release, which must be the latest
// one the checker knows. The job runs in the background; the page follows it
// through updateStatus.
func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.updatesWired() {
		s.flashErrT(w, r, "flash.err.update_unwired")
		return
	}
	tag := strings.TrimSpace(r.FormValue("tag"))
	if err := s.updates.installer.Start(tag); err != nil {
		s.flashUpdateErr(w, r, "installUpdate", err)
		return
	}
	logFrom(r).Info("self-update requested", "to", tag)
	s.flashOK(w, r, "flash.ok.update_started")
	http.Redirect(w, r, updatesBack(o.ID), http.StatusSeeOther)
}

// rollbackUpdate swaps the running binary back to the previous one and
// restarts, for a new release that serves the UI but is broken.
func (s *Server) rollbackUpdate(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.updatesWired() {
		s.flashErrT(w, r, "flash.err.update_unwired")
		return
	}
	if err := s.updates.installer.Rollback(); err != nil {
		s.flashUpdateErr(w, r, "rollbackUpdate", err)
		return
	}
	logFrom(r).Info("self-update rollback requested", "to", s.updates.installer.Status().Tag)
	s.flashOK(w, r, "flash.ok.update_rollback_started")
	http.Redirect(w, r, updatesBack(o.ID), http.StatusSeeOther)
}

// flashUpdateErr explains why an update or rollback was refused.
func (s *Server) flashUpdateErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	var busy *selfupdate.BusyError
	switch {
	case errors.Is(err, selfupdate.ErrUnsupported):
		s.flashErrT(w, r, "flash.err.update_unsupported")
	case errors.Is(err, selfupdate.ErrJobRunning):
		s.flashErrT(w, r, "flash.err.update_running")
	case errors.Is(err, selfupdate.ErrInvalidTag), errors.Is(err, selfupdate.ErrNotLatest):
		s.flashErrT(w, r, "flash.err.update_not_latest")
	case errors.Is(err, selfupdate.ErrNotNewer):
		s.flashErrT(w, r, "flash.err.update_not_newer")
	case errors.Is(err, selfupdate.ErrPending):
		s.flashErrT(w, r, "flash.err.update_pending")
	case errors.Is(err, selfupdate.ErrNoPrevious):
		s.flashErrT(w, r, "flash.err.update_no_previous")
	case errors.As(err, &busy):
		ctx := r.Context()
		s.flashErr(w, r, i18n.Tf(ctx, "flash.err.update_busy", i18n.T(ctx, "update.busy."+busy.Reason)))
	default:
		logFrom(r).Error(op+": could not start", "err", err)
		s.flashErrT(w, r, "flash.err.update_start")
	}
}

// updateStatus is the page's polling fragment for a running job. Anything else
// gets HX-Refresh instead, because the news is outside the fragment: a request
// from a page rendered by another version means the update (or rollback) is
// done — the result and the new running version; a job that is no longer
// active on the same version failed, or came back on the old binary
// (reverted, interrupted), and the page's buttons must be enabled again. The
// starting page answers HX-Refresh the same way while the new process boots,
// and a refused connection is simply retried.
func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.updatesWired() {
		http.NotFound(w, r)
		return
	}
	current := s.updates.checker.Current()
	job := s.updates.installer.Status()
	if r.URL.Query().Get("from") != current || !job.Phase.Active() {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	render(w, r, http.StatusOK, templates.UpdateStatus(o.ID, current, string(job.Phase), true, job.Tag, ""))
}
