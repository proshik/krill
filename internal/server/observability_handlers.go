package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/proshik/krill/internal/observability"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// observabilityCtl is the part of *observability.Reconciler the page needs.
type observabilityCtl interface {
	Trigger()
	Status(ctx context.Context) observability.Status
}

var _ observabilityCtl = (*observability.Reconciler)(nil)

// SetObservability wires Settings → Observability. With a nil ctl or check the
// feature stays unwired.
func (s *Server) SetObservability(ctl observabilityCtl, check func(context.Context, observability.Settings) observability.Report) {
	if ctl == nil || check == nil {
		return
	}
	s.obs.ctl = ctl
	s.obs.check = check
}

func (s *Server) obsWired() bool { return s.obs.ctl != nil }

func observabilityBack(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/observability"
}

func (s *Server) observabilityPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	v := templates.ObservabilityView{Wired: s.obsWired(), MemoryLimit: strconv.Itoa(observability.NodeMemoryLimit>>20) + " MiB"}
	if v.Wired {
		row, err := s.q.GetObservabilitySettings(r.Context())
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			logFrom(r).Error("observabilityPage: read settings failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		v.Enabled = row.Enabled
		v.MetricsURL, v.MetricsUser, v.MetricsHasPassword = row.MetricsUrl, row.MetricsUser, row.MetricsPassword != ""
		v.LogsURL, v.LogsUser, v.LogsHasPassword = row.LogsUrl, row.LogsUser, row.LogsPassword != ""
		st := s.obs.ctl.Status(r.Context())
		v.Busy, v.LastRun, v.LastErr, v.StateErr = st.Busy, st.LastRun, st.LastErr, st.StateErr
		v.Found, v.Running, v.Desired, v.Failed = st.Service.Found, st.Service.Running, st.Service.Desired, st.Service.Failed
		v.CoverageExpected, v.CoverageDeployed, v.CoverageMissing = st.Coverage.Expected, st.Coverage.Deployed, st.Coverage.Missing
	}
	render(w, r, http.StatusOK, templates.Observability(o, role, v))
}

// flashObsErr maps a settings error to its flash; anything unexpected is
// logged and reported as internal.
func (s *Server) flashObsErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, observability.ErrInvalidURL):
		s.flashErrT(w, r, "flash.err.obs_invalid_url")
	case errors.Is(err, observability.ErrInvalidUser):
		s.flashErrT(w, r, "flash.err.obs_invalid_user")
	case errors.Is(err, observability.ErrInvalidPassword):
		s.flashErrT(w, r, "flash.err.obs_invalid_password")
	case errors.Is(err, observability.ErrNothingConfigured):
		s.flashErrT(w, r, "flash.err.obs_nothing")
	case errors.Is(err, observability.ErrNotSaved):
		s.flashErrT(w, r, "flash.err.obs_not_saved")
	case errors.Is(err, observability.ErrPasswordRequired):
		s.flashErrT(w, r, "flash.err.obs_password_required")
	case errors.Is(err, observability.ErrClearNotConfirmed):
		s.flashErrT(w, r, "flash.err.obs_clear_confirm")
	case errors.Is(err, observability.ErrClearWithAddress):
		s.flashErrT(w, r, "flash.err.obs_clear_with_address")
	case errors.Is(err, secret.ErrUndecryptable):
		s.flashErrT(w, r, "flash.err.obs_undecryptable")
	default:
		logFrom(r).Error(op+": failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
	}
}

func (s *Server) saveObservability(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.obsWired() {
		s.flashErrT(w, r, "flash.err.obs_unwired")
		return
	}
	in := observability.Input{
		Metrics: observability.TargetInput{URL: r.FormValue("metrics_url"), User: r.FormValue("metrics_user"), Password: r.FormValue("metrics_password"), Clear: r.FormValue("metrics_clear") == "on"},
		Logs:    observability.TargetInput{URL: r.FormValue("logs_url"), User: r.FormValue("logs_user"), Password: r.FormValue("logs_password"), Clear: r.FormValue("logs_clear") == "on"},
	}
	if err := observability.Save(r.Context(), s.q, in); err != nil {
		s.flashObsErr(w, r, "saveObservability", err)
		return
	}

	// Whether to trigger a reconcile depends only on the raw enabled bit, not
	// on whether the stored passwords still decrypt: Save deliberately keeps a
	// password that no longer decrypts (KRILL_SECRET_KEY rotated), and the
	// reconcile still needs to run for whatever DID change (a new URL, say).
	row, err := s.q.GetObservabilitySettings(r.Context())
	if err != nil {
		logFrom(r).Error("saveObservability: read settings after save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	enabled := row.Enabled
	logFrom(r).Info("observability settings saved",
		"metrics", strings.TrimSpace(in.Metrics.URL) != "", "logs", strings.TrimSpace(in.Logs.URL) != "", "enabled", enabled)
	if enabled {
		s.obs.ctl.Trigger()
	}

	// Load decrypts the stored passwords; a stored one that no longer
	// decrypts must not be reported as a plain success, even though the save
	// itself (and the trigger above) already went through.
	if _, err := observability.Load(r.Context(), s.q); err != nil {
		if errors.Is(err, secret.ErrUndecryptable) {
			logFrom(r).Warn("saveObservability: a stored password cannot be decrypted with the current key")
			s.flashErrT(w, r, "flash.err.obs_undecryptable")
			return
		}
		logFrom(r).Error("saveObservability: reload after save failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}

	if enabled {
		s.flashOK(w, r, "flash.ok.obs_saved_applying")
	} else {
		s.flashOK(w, r, "flash.ok.obs_saved")
	}
	http.Redirect(w, r, observabilityBack(o.ID), http.StatusSeeOther)
}

func (s *Server) setObservabilityEnabled(w http.ResponseWriter, r *http.Request, on bool) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.obsWired() {
		s.flashErrT(w, r, "flash.err.obs_unwired")
		return
	}
	if err := observability.SetEnabled(r.Context(), s.q, on); err != nil {
		s.flashObsErr(w, r, "setObservabilityEnabled", err)
		return
	}
	s.obs.ctl.Trigger()
	logFrom(r).Info("observability toggled", "enabled", on)
	if on {
		s.flashOK(w, r, "flash.ok.obs_enabled")
	} else {
		s.flashOK(w, r, "flash.ok.obs_disabled")
	}
	http.Redirect(w, r, observabilityBack(o.ID), http.StatusSeeOther)
}

func (s *Server) enableObservability(w http.ResponseWriter, r *http.Request) {
	s.setObservabilityEnabled(w, r, true)
}

func (s *Server) disableObservability(w http.ResponseWriter, r *http.Request) {
	s.setObservabilityEnabled(w, r, false)
}

// checkObservability checks the saved addresses and flashes one line per
// target. It runs on the request: the checker is bounded by its own timeout.
func (s *Server) checkObservability(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	if !s.obsWired() {
		s.flashErrT(w, r, "flash.err.obs_unwired")
		return
	}
	st, err := observability.Load(r.Context(), s.q)
	if err != nil {
		s.flashObsErr(w, r, "checkObservability", err)
		return
	}
	if !st.Metrics.Configured() && !st.Logs.Configured() {
		s.flashObsErr(w, r, "checkObservability", observability.ErrNothingConfigured)
		return
	}
	msg, allOK := checkMessage(r.Context(), s.obs.check(r.Context(), st))
	logFrom(r).Info("observability check", "ok", allOK)
	kind := "ok"
	if !allOK {
		kind = "err"
	}
	s.setFlash(w, r, kind, msg)
	http.Redirect(w, r, observabilityBack(o.ID), http.StatusSeeOther)
}

// checkArgKeys are the observability.Result keys whose i18n text takes a %s
// argument (Detail) — the other keys never take Detail, so it would be wrong
// to feed Tf a key that has no verb just because Detail happened to be "".
var checkArgKeys = map[string]bool{
	"obs.check.auth":    true,
	"obs.check.http":    true,
	"obs.check.network": true,
}

func checkMessage(ctx context.Context, rep observability.Report) (string, bool) {
	allOK := true
	var parts []string
	add := func(labelKey string, res *observability.Result) {
		text := i18n.T(ctx, "obs.check.not_configured")
		if res != nil {
			allOK = allOK && res.OK
			if checkArgKeys[res.Key] {
				text = i18n.Tf(ctx, res.Key, res.Detail)
			} else {
				text = i18n.T(ctx, res.Key)
			}
		}
		parts = append(parts, i18n.T(ctx, labelKey)+": "+text)
	}
	add("obs.check.metrics", rep.Metrics)
	add("obs.check.logs", rep.Logs)
	return strings.Join(parts, ". "), allOK
}
