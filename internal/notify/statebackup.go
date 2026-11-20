package notify

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/proshik/krill/internal/web/i18n"
)

// stateOverdueGrace is how long after a scheduled time a backup may still be
// running or retrying before the digest calls it overdue.
const stateOverdueGrace = time.Hour

// stateFailuresMax caps the failed runs listed in the digest.
const stateFailuresMax = 3

// StateDetail is what a state backup failure alert says besides the reason.
type StateDetail struct{ Trigger, From, To string }

// StateRun is one journalled run of the state backup.
type StateRun struct {
	Trigger, Status, Error, From, To string
	Size                             int64
	At                               time.Time
}

// StateStatus is what the digest reads about Krill's own state backup.
type StateStatus struct {
	Configured, Paused bool
	Schedule           string    // "" = on demand
	Since              time.Time // settings updated_at
	LastSuccess        time.Time // zero = never
	Runs               []StateRun
}

// StateReader is the digest's view of the state backup (the owner of the
// journal adapts it).
type StateReader interface {
	StateBackupStatus(ctx context.Context, since time.Time) (StateStatus, error)
	PruneStateRuns(ctx context.Context, before time.Time) error
}

// SetStateBackups wires the state backup reader; without it the digest has
// no "Krill state" block.
func (d *Digest) SetStateBackups(r StateReader) { d.state = r }

// StateBackupFailed alerts the infrastructure channels that a state backup
// failed (every trigger, pre-update included). Async, like the other alerts.
func (s *Service) StateBackupFailed(_ context.Context, trigger, from, to, reason string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		s.stateBackupFailed(ctx, trigger, from, to, reason)
	}()
}

func (s *Service) stateBackupFailed(ctx context.Context, trigger, from, to, reason string) {
	chans, err := s.store.InfraChannels(ctx)
	if err != nil {
		// The store returns the usable channels together with the error for
		// rows it had to skip; only give up when nothing is usable.
		s.log.Warn("notify: load infra channels failed", "err", err)
		if len(chans) == 0 {
			return
		}
	}
	ev := Event{
		Kind: StateBackupFailed, Time: s.now(), Instance: s.instance(ctx), Detail: reason,
		State: &StateDetail{Trigger: trigger, From: from, To: to},
	}
	for _, c := range chans {
		if !c.Enabled || !c.wants(ev.Kind) {
			continue
		}
		_ = s.sendTo(ctx, c, ev)
	}
}

// stateTriggerLine names what started a run, with the versions of a
// pre-update one.
func stateTriggerLine(ctx context.Context, trigger, from, to string) string {
	if trigger == "" {
		return ""
	}
	label := trigger
	if key := "tg.state_trigger." + trigger; i18n.T(ctx, key) != key {
		label = i18n.T(ctx, key)
	}
	if trigger == "pre_update" && (from != "" || to != "") {
		label += " · " + from + " → " + to
	}
	return label
}

// cronLoc is the zone the cron scheduler fires in (cron.New defaults to
// time.Local); overridable in tests.
var cronLoc = time.Local

// stateOverdue reports whether a scheduled backup is overdue: the schedule's
// next due time after the later of the last success and the moment the
// settings were saved, plus the grace period, has passed. An on-demand
// schedule (or one that does not parse) is never overdue.
func stateOverdue(schedule string, lastSuccess, since, now time.Time) bool {
	if strings.TrimSpace(schedule) == "" {
		return false
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return false
	}
	base := lastSuccess
	if since.After(base) {
		base = since
	}
	if base.IsZero() {
		return false
	}
	// Next evaluates the schedule in base's zone; the scheduler fires in
	// cronLoc, while stored times may come back in UTC.
	return sched.Next(base.In(cronLoc)).Add(stateOverdueGrace).Before(now.In(cronLoc))
}

// stateBlock is the digest's collected state backup section.
type stateBlock struct {
	Err     bool // the status could not be read
	Status  StateStatus
	Overdue bool
}

func (d *Digest) stateBlock(ctx context.Context, since, now time.Time) *stateBlock {
	if d.state == nil {
		return nil
	}
	st, err := d.state.StateBackupStatus(ctx, since)
	if err != nil {
		slog.Warn("notify digest: state backup status unavailable", "err", err)
		return &stateBlock{Err: true}
	}
	return &stateBlock{
		Status:  st,
		Overdue: st.Configured && !st.Paused && stateOverdue(st.Schedule, st.LastSuccess, st.Since, now),
	}
}

type stateLine struct {
	lvl  *level // nil: a neutral line
	text string
}

// stateLines renders the "Krill state" block's lines (without the heading).
func stateLines(ctx context.Context, d digestData) []stateLine {
	st, b := d.State.Status, d.State
	mk := func(l level) *level { return &l }
	if b.Err {
		return []stateLine{{nil, i18n.T(ctx, "tg.dg.no_data")}}
	}
	if !st.Configured {
		return []stateLine{{mk(lvlWarn), i18n.T(ctx, "tg.dg.state_off")}}
	}
	if st.Paused {
		return []stateLine{{mk(lvlWarn), i18n.T(ctx, "tg.dg.state_paused")}}
	}
	var failed []StateRun
	okN := 0
	var lastOK StateRun
	for _, r := range st.Runs {
		if r.Status == "error" {
			failed = append(failed, r)
			continue
		}
		if r.Status != "ok" {
			continue
		}
		okN++
		if r.At.After(lastOK.At) {
			lastOK = r
		}
	}
	var out []stateLine
	for i, r := range failed {
		if i == stateFailuresMax {
			out = append(out, stateLine{mk(lvlCrit), i18n.Tf(ctx, "tg.dg.more", len(failed)-i)})
			break
		}
		parts := []string{whenLabel(r.At, d.Now, d.Loc, d.Lang), stateTriggerLine(ctx, r.Trigger, r.From, r.To)}
		if e := oneLine(r.Error, 120); e != "" {
			parts = append(parts, e)
		}
		out = append(out, stateLine{mk(lvlCrit), i18n.Tf(ctx, "tg.dg.state_failed", strings.Join(parts, " · "))})
	}
	if okN > 0 {
		out = append(out, stateLine{mk(lvlOK), i18n.Tf(ctx, "tg.dg.state_ok", okN, humanBytes(lastOK.Size), whenLabel(lastOK.At, d.Now, d.Loc, d.Lang))})
		return out
	}
	last := i18n.T(ctx, "tg.dg.state_never")
	if !st.LastSuccess.IsZero() {
		last = whenLabel(st.LastSuccess, d.Now, d.Loc, d.Lang)
	}
	switch {
	case b.Overdue:
		out = append(out, stateLine{mk(lvlWarn), i18n.Tf(ctx, "tg.dg.state_overdue", last)})
	case len(failed) > 0:
		// the failures already say it
	case strings.TrimSpace(st.Schedule) == "":
		out = append(out, stateLine{nil, i18n.Tf(ctx, "tg.dg.state_on_demand", last)})
	default:
		out = append(out, stateLine{nil, i18n.Tf(ctx, "tg.dg.state_last", last)})
	}
	return out
}
