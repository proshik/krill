---
id: KRL-046
title: Every time the UI shows names its zone, so the two sides of a link can be compared
kind: fix
status: backlog
priority: 3
component: krill
depends_on: []
touches:
  - internal/web/templates/**
  - internal/server/firewall_handlers.go
  - internal/server/bucket_handlers.go
  - internal/web/static/app.js
  - docs/internals/ui.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-28"
---

## Goal

The UI prints times in the Krill process's zone (`time.Local`) without saying which
zone that is, so two hosts in different zones show the same moment differently and
nobody can tell. Live check on v0.17.0, two installations joined by a Krill link:
on the client (in Moscow), Settings → link showed the session as "с 2026-11-28 11:13";
on the relay (in Amsterdam), the same session showed as "с 2026-11-28 09:13". Only the
Updates page labels its zone ("последняя проверка: 2026-11-27 21:37 UTC").

When this is finished, every absolute time the web UI renders carries its UTC offset,
built by one helper, so a reader can compare times across hosts and with the browser's
clock. Storage, schedules and the zone the process runs in stay as they are.

The proposed solution is to keep showing the host's local time, which is also the zone
backup cron schedules fire in, and always add the offset. One helper in
`internal/web/templates` takes a `time.Time` and a layout (date, date + minutes,
date + seconds, clock) and returns `t.Local()` formatted, followed by the offset, for
example `2026-11-28 11:13 UTC+03:00`, or `UTC` alone when the offset is zero. Where a
time stands as its own node, the template wraps it in
`<time datetime="<RFC3339 with offset>" title="<the same instant in UTC>">`. Where it
is interpolated into an `i18n.Tf` sentence, the helper's string is passed as the
argument. The offset is an identifier, not a catalog string. Any new words (a title or
hint) go through `i18n.T` with keys in both `en` and `ru`, lowercase in the catalogs.

## Acceptance

- [ ] WHEN the same moment is rendered by two Krill processes whose `time.Local` differ
      (for example `Europe/Moscow` and `Europe/Amsterdam`) THEN both strings carry their
      offset (`… 11:13 UTC+03:00` and `… 09:13 UTC+01:00`) and convert to the same UTC
      instant. A unit test proves this with explicit locations, not with the test host's
      zone.
- [ ] WHEN Settings → link renders a session start, a last-seen time, a refused-session
      time, a code expiry or a certificate date on the client or the relay side THEN the
      text carries the offset (`link.templ`: `linkClock`, `linkDate`, the `code_open`
      clock).
- [ ] WHEN any other page renders an absolute time THEN it goes through the same helper
      and carries the offset: deploy history, container tasks, backup objects and the
      restore confirm, the backup card's last run, bucket backups' last restore, the
      Krill state backup's next run, last success and snapshots, the firewall's
      last-read time and its revert time in the flash, API token expiry, and the Updates
      page's last check (which today hardcodes `UTC`; it moves to the helper too, so
      there is one format).
- [ ] WHEN a time stands as its own element (deploy history `when`, task `when`, backup
      object, state-backup snapshot) THEN it is a `<time>` element whose `datetime` is
      RFC 3339 with the offset.
- [ ] WHEN the log viewer shows line times, which `app.js` formats in the browser's zone
      today, THEN the zone is stated there too: either the line text carries the
      browser's offset, or the `<time>` element (which already carries `datetime`) gets
      a `title` with the full timestamp and offset. The spec picks one, and the choice is
      written down in `docs/internals/ui.md`.
- [ ] WHEN the change is done THEN no `.Local().Format(` and no `.UTC().Format(` meant
      for display remain in `internal/web/templates` (`.templ` and `.go`) or in
      `internal/server` outside the helper. A `grep` in Checks shows this.
- [ ] WHEN `docs/internals/ui.md` is read THEN a rule says the UI shows times through
      the helper, in the host's zone with the offset, and why (backup cron schedules
      fire in `time.Local`).

## Checks

- Unit tests for the helper: zero offset, positive and negative offsets, a DST zone on
  both sides of a transition. For the zero `time.Time`, the helper keeps what the call
  sites rely on today; some check `IsZero` first, for example `link.templ:240`.
- A template-level test: the link page rendered with a fixed `Since` contains the
  offset.
- `grep -rnE '\.(Local|UTC)\(\)\.Format\(' internal/web/templates internal/server --include='*.go' --include='*.templ' | grep -v _test.go | grep -v _templ.go`
  returns only the helper and non-display uses.
- `make generate` (templ), `make test`.
- Not run: a live check on two hosts in different zones. An operator repeats the
  link-page comparison from the Goal after release.

## Limits / non-goals

- Only display. No change to how times are stored (`timestamptz`), to the agent API
  (`internal/api` already returns RFC 3339), to the `krill` CLI
  (`internal/krillcli/cli/read.go:384`), or to Telegram texts (the digest has its own
  per-channel zone, `internal/notify/digest.go:88-105`).
- No new setting: no `KRILL_TZ` and no per-user or per-organization zone. The zone stays
  whatever the process gets (`TZ`, else `/etc/localtime`).
- Relative times (`ago()` in `canvas.go:142`, `metrics.status.ago`) stay as they are.
- Backup schedules keep firing in `time.Local` (`internal/backup/scheduler.go:50`,
  `internal/notify/statebackup.go:101`). Labeling the cron form with the zone is welcome
  if it is a one-line hint, but it is not required.

## Context

Where the zone comes from (checked in code):
- No `KRILL_*` zone variable exists (`internal/config`, `docs/configuration.md`). Display
  code calls `t.Local()`, so the zone is Go's `time.Local`: `TZ` if set, else
  `/etc/localtime`. `install.sh` runs Krill as a systemd host binary and sets no `TZ`,
  so it is the host's zone. The image is distroless and has no `/etc/localtime`, so it
  would be UTC unless `TZ` is set; `cmd/krill/main.go:15` embeds `time/tzdata`, so a
  `TZ=Europe/Moscow` would load. Not verified on the two live hosts: what their
  `/etc/localtime` says. The observed two-hour gap matches UTC+3 against UTC+1.

Call sites (v0.17.0, line numbers at `fd7bee2`):
- Link: `internal/web/templates/link.templ:84` `linkDate`, `:86` `linkClock`, used at
  `:239` (`link.peer_session`), `:241` (`link.peer_last_seen`), `:246` and `:344`
  (`link.cert_until`), `:250` (`link.peer_duplicate`), `:253` (`link.peer_incompatible`),
  `:326` (`link.client_session`), `:335` (client failure time); `:276` `ExpiresAt` clock
  (`link.code_open`).
- Labeled today: `internal/web/templates/updates.templ:107`
  (`v.CheckedAt.UTC().Format("2006-01-02 15:04 UTC")`).
- Unlabeled local: `app_detail.templ:512` (deploy history),
  `app_settings.templ:397` (container tasks), `backup_block.templ:202` and `:211`
  (backup object, `backup.restore_q`), `canvas.go:254` (`backup.last_run_at`),
  `state_backup.templ:64` `sbkTime` (`sbk.next_run`, `sbk.last_ok`) and `:196`
  (snapshots), `firewall.templ:132` (`firewall.last_ok`), `api_tokens.templ:50`
  (`api_tokens.expires_at`, a date only), `internal/server/bucket_handlers.go:294`
  (`bbackup.last_restore_line`), `internal/server/firewall_handlers.go:509`
  (revert clock in the armed-switch flash).
- Browser zone: `internal/web/static/app.js:128-136` `fmtTime`, used at `:157`. The
  `<time>` element already gets `dateTime = line.t`.

Alternatives considered:
- UTC everywhere, as the Updates page does now. It is unambiguous, but backup cron
  schedules fire in local time, so a schedule set for 03:00 and a last run shown as
  "00:00 UTC" would disagree on a host at UTC+3.
- Render in the browser's zone from `<time datetime>` with a small `app.js` pass. Both
  hosts would then show the viewer's zone, but it adds a JS rewrite of server text
  (boosted swaps, OOB swaps, sentences built by `i18n.Tf`) and breaks without JS.
- A `KRILL_TZ` setting or a per-user zone is a larger change, and the offset label
  solves the comparison without it.
