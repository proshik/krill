package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/statebackup"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// stateBackupCtl is the part of *statebackup.Service the page needs.
type stateBackupCtl interface {
	View(ctx context.Context) (statebackup.Settings, error)
	Save(ctx context.Context, in statebackup.Input) error
	SetEnabled(ctx context.Context, on bool) error
	Start(trigger statebackup.Trigger) error
	InFlight() int
	Runs(ctx context.Context, n int) ([]statebackup.Run, error)
	LastSuccess(ctx context.Context) (statebackup.Run, bool, error)
}

var _ stateBackupCtl = (*statebackup.Service)(nil)

const (
	stateBackupRunsShown = 10
	defaultStateRetain   = 14
)

// SetStateBackup wires Settings → Krill backups. secretKeySet reports whether
// KRILL_SECRET_KEY is configured (without it the page warns that stored
// secrets are plain text). A nil ctl leaves the feature unwired.
func (s *Server) SetStateBackup(ctl stateBackupCtl, secretKeySet bool) {
	if ctl == nil {
		return
	}
	s.sbk.ctl = ctl
	s.sbk.secretKeySet = secretKeySet
}

func (s *Server) sbkWired() bool { return s.sbk.ctl != nil }

func stateBackupBack(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/krill-backups"
}

// stateBackupMode reports whether Krill's state is backed up before an
// update: "on", "off" (not set up or unavailable) or "paused".
func (s *Server) stateBackupMode(ctx context.Context) string {
	if !s.sbkWired() {
		return "off"
	}
	v, err := s.sbk.ctl.View(ctx)
	if err != nil {
		return "off"
	}
	switch {
	case v.Configured():
		return "on"
	case v.Ready():
		return "paused"
	}
	return "off"
}

func (s *Server) stateBackupPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	v := templates.StateBackupView{Wired: s.sbkWired(), SecretKeySet: s.sbk.secretKeySet}
	if v.Wired {
		ctx := r.Context()
		// View never decrypts, so the page renders even when KRILL_SECRET_KEY
		// was rotated; the secret key itself is never put into the view.
		st, err := s.sbk.ctl.View(ctx)
		if err != nil {
			logFrom(r).Error("stateBackupPage: reading the settings failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		v.Enabled, v.Ready, v.HasSecret = st.Enabled, st.Ready(), st.HasSecretKey
		v.Endpoint, v.Bucket, v.Region, v.AccessKey = st.Endpoint, st.Bucket, st.Region, st.AccessKey
		v.Prefix, v.Recipient, v.Retention = st.Prefix, st.Recipient, st.Retention
		v.Preset = backup.SchedulePreset(st.Schedule)
		if v.Preset == "custom" {
			v.CustomCron = st.Schedule
		}
		if st.Bucket == "" && st.Schedule == "" {
			// Nothing saved yet: offer the usual defaults.
			v.Preset = "daily"
		}
		if v.Retention < 1 {
			v.Retention = defaultStateRetain
		}
		v.Running = s.sbk.ctl.InFlight() > 0
		if v.Ready && v.Enabled {
			if next, ok := statebackup.NextRun(st.Schedule, time.Now()); ok {
				v.NextRun = next
			}
		}
		last, has, err := s.sbk.ctl.LastSuccess(ctx)
		if err != nil {
			logFrom(r).Error("stateBackupPage: reading the last success failed", "err", err)
		} else if has {
			v.HasLast, v.LastAt, v.LastSize = true, last.FinishedAt, last.SizeBytes
		}
		runs, err := s.sbk.ctl.Runs(ctx, stateBackupRunsShown)
		if err != nil {
			logFrom(r).Error("stateBackupPage: reading the runs failed", "err", err)
		}
		for _, run := range runs {
			v.Runs = append(v.Runs, templates.StateBackupRun{
				At: run.StartedAt, Trigger: string(run.Trigger), OK: run.Status == "ok",
				SizeBytes: run.SizeBytes, ObjectKey: run.ObjectKey, Error: run.Error,
			})
		}
	}
	render(w, r, http.StatusOK, templates.StateBackupPage(o, role, v))
}

// flashStateBackupErr maps a service error to its flash; anything unexpected
// is logged and reported as internal, without its text.
func (s *Server) flashStateBackupErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	var storage *statebackup.StorageError
	switch {
	case errors.Is(err, statebackup.ErrInvalidRecipient):
		s.flashErrT(w, r, "flash.err.sbk_recipient")
	case errors.Is(err, statebackup.ErrSecretKeyRequired):
		s.flashErrT(w, r, "flash.err.sbk_secret_required")
	case errors.Is(err, statebackup.ErrRetention):
		s.flashErrT(w, r, "flash.err.sbk_retention")
	case errors.Is(err, statebackup.ErrInvalidSchedule):
		s.flashErrT(w, r, "flash.err.sbk_schedule")
	case errors.Is(err, statebackup.ErrBucketRequired):
		s.flashErrT(w, r, "flash.err.sbk_bucket")
	case errors.Is(err, statebackup.ErrStateBackupRunning):
		s.flashErrT(w, r, "flash.err.sbk_running")
	case errors.Is(err, statebackup.ErrNotConfigured):
		s.flashErrT(w, r, "flash.err.sbk_not_configured")
	case errors.As(err, &storage):
		// The text is capped and free of secrets by the service.
		s.flashErr(w, r, i18n.Tf(r.Context(), "flash.err.sbk_storage", storage.Msg))
	default:
		logFrom(r).Error(op+": failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
	}
}

func (s *Server) saveStateBackup(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.sbkWired() {
		http.NotFound(w, r)
		return
	}
	retention, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("retention"))) // 0 on junk: ErrRetention
	in := statebackup.Input{
		Endpoint: r.FormValue("endpoint"), Bucket: r.FormValue("bucket"), Region: r.FormValue("region"),
		AccessKey: r.FormValue("access_key"), SecretKey: r.FormValue("secret_key"), Prefix: r.FormValue("prefix"),
		Recipient: r.FormValue("recipient"), Preset: r.FormValue("schedule_preset"), CustomCron: r.FormValue("schedule_custom"),
		Retention: retention,
	}
	if err := s.sbk.ctl.Save(r.Context(), in); err != nil {
		s.flashStateBackupErr(w, r, "saveStateBackup", err)
		return
	}
	logFrom(r).Info("state backup settings saved", "preset", in.Preset, "retention", in.Retention)
	s.flashOK(w, r, "flash.ok.sbk_saved")
	http.Redirect(w, r, stateBackupBack(o.ID), http.StatusSeeOther)
}

func (s *Server) runStateBackup(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.sbkWired() {
		http.NotFound(w, r)
		return
	}
	if err := s.sbk.ctl.Start(statebackup.TriggerManual); err != nil {
		s.flashStateBackupErr(w, r, "runStateBackup", err)
		return
	}
	logFrom(r).Info("state backup started manually")
	s.flashOK(w, r, "flash.ok.sbk_started")
	http.Redirect(w, r, stateBackupBack(o.ID), http.StatusSeeOther)
}

// setStateBackupEnabled posts the state it wants (enabled=1|0), so a repeated
// submit does the same thing twice instead of flipping back.
func (s *Server) setStateBackupEnabled(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.sbkWired() {
		http.NotFound(w, r)
		return
	}
	on := r.FormValue("enabled") == "1"
	if err := s.sbk.ctl.SetEnabled(r.Context(), on); err != nil {
		s.flashStateBackupErr(w, r, "setStateBackupEnabled", err)
		return
	}
	logFrom(r).Info("state backup toggled", "enabled", on)
	if on {
		s.flashOK(w, r, "flash.ok.sbk_enabled")
	} else {
		s.flashOK(w, r, "flash.ok.sbk_paused")
	}
	http.Redirect(w, r, stateBackupBack(o.ID), http.StatusSeeOther)
}
