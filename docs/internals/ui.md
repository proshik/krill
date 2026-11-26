# Web UI

Templates, the page frame, CSS, i18n, client-side helpers in app.js, and their traps.

## Rules

### All UI strings go through `i18n.T(ctx, "key")`

**All UI strings go through `i18n.T(ctx, "key")`** with keys in the English catalog (`internal/web/i18n/en.go`). No raw user-facing literals in templates or handlers. The router's locale middleware puts `requestLocale(r)` into the context with `i18n.WithLocale`: the `krill_lang` cookie (set by `POST /lang` from the Settings language selector) when it holds a supported locale, otherwise the browser's `Accept-Language` (`i18n.MatchAcceptLanguage`), otherwise `i18n.DefaultLocale`. `StartupGate` uses the same function, so the "Krill is starting" page speaks the same language. The catalogs are `en` + `ru` (`internal/web/i18n/en.go`/`ru.go`) with the same keys; keep them in sync.

### UI text is lowercase, in the catalogs themselves

*(2026-11-22)*

Every `en` and `ru` UI value starts lowercase, headings included ("git credentials", "узлы"), as the design mockups write it — not via CSS, so screen readers, copy and both locales get the same text. Proper names and identifiers keep their case (Krill, Telegram, GitHub, S3, HSTS, URLs, env var names), and a new sentence after a period inside a value starts with a capital. Where Go joins two messages into sentences, or for a browser tab title that starts with a page name (not a resource name), wrap it in `i18n.Sentence`. The rule is for the interface only: everything that goes into a real Telegram message (the `tg.*` keys, in both catalogs, and the text `internal/notify` builds) keeps its sentence case, and the digest preview on the notifications page renders that same text unchanged.

### Every form is guarded against double submission

*(2026-09-22)*

`krillGuardForms` in `app.js` gives each boosted form `hx-sync="this:drop"`, because htmx's default queues a second submit and sends it after the first. It also sets an `hx-disabled-elt` list covering the form's submit buttons, including ones tied to it with `form=`. The list uses plain selectors, since htmx splits it on top-level commas and `find` reaches only the first match. A natively submitted form (`hx-boost="false"`, and the confirm dialog's `form.submit()`) is marked busy once by `krillFormBusy`, and a page restored from the back/forward cache is reset. A new form needs nothing extra. The guard doesn't cover two tabs or a retrying client, so a handler whose duplicate is harmful still needs its own server-side guard. Toggles post the state they want (`desiredState`, e.g. `enabled=1`/`0`) instead of flipping the current one, because a duplicate flip undoes the first.

### Unsaved changes in a sibling form ask first

*(2026-11-21)*

A domain card holds several small forms (exposure + paths with their own Save, Toggle HTTPS, Make primary, Delete, basic-auth, IP list). Ticking "Open to the internet" and then pressing "Toggle HTTPS" reloaded the page and silently dropped the tick (seen on a live install; reproduced in a dev window). A container marked `data-unsaved-scope` (with `data-unsaved-msg`/`data-unsaved-label`) now gets a capture-phase submit check in `app.js` (`krillFormDirty`): submitting one form while another form in the scope differs from its default values opens the styled confirm, and "continue" re-dispatches with `requestSubmit` (skipping the check once), so htmx and a form's own confirm still run. Only the domain card is a scope so far; mark another multi-form card the same way if it has the same trap.

## Pitfalls

### Tailwind supplies only its preflight

*(2026-11-22)*

`internal/web/styles/input.css` imports `tailwindcss/preflight.css` and is otherwise plain CSS: tokens (light on `:root`, dark on `:root[data-mode="dark"]`, the head script in `shell.templ` sets `data-mode` and `data-choice` (system/light/dark) before first paint; the bar's theme button cycles system → light/dark → system, and on system the stored choice is removed so the page follows `prefers-color-scheme` live again) and base in `@layer base`, the design system in `@layer components`. No utility classes are generated, so a class name in a template never turns into a Tailwind utility. The preflight's `* { margin: 0 }` also removes the `margin: auto` that centres a modal `<dialog>`: `dialog.confirm` sets it back (v0.13.1 showed every confirmation in the top-left corner); a new dialog needs the same. Fonts (Outfit, Onest, JetBrains Mono) are self-hosted under `internal/web/static/fonts/` with their OFL texts. Regenerate with `make css` or `make generate`; `internal/web/static/app.css` is a committed artifact. `app.css` and `app.js` are linked with `?v=web.AssetVersion` (a hash of both), so a browser refetches them after an upgrade.

### The page frame lives in `shell.templ`

`Page(Shell)` draws the bar (organization or environment variant, tabs by role and instance gate), the confirm dialog, the toasts (the flash message) and the palette; `SettingsPage` adds the settings sub-navigation; `Sheet` is the panel over a page (`?new=…` or a resource's sheet). Pages use the design system's classes (`sec`, `field`, `pill`, `btn`, `state`, …), not inline styles.

### Confirm dialogs + log terminals: `app.js` loads ONCE in the layout `<head>` (deferred), NOT per-tab

Destructive forms carry `hx-boost="false"` + `data-confirm` (server-rendered i18n message) and call `krillConfirm(this)` (the `#confirm` dialog; `data-more` adds what is lost, `data-ok` the verb, `data-kind="primary"` a non-destructive button) — native `confirm()` is broken under hx-boost. Log terminals are `<div class="term" data-term-log=...>` (xterm.js/addon-fit.js loaded per tab, lazy); `krillEnhance` (on `DOMContentLoaded`+`htmx:afterSettle`) auto-mounts them and inits the env editor — so do NOT add inline mount calls or per-tab `app.js`. `.term` has an explicit height in CSS, or FitAddon computes 0 rows on a boosted swap.

### An out-of-band swap needs an explicit `hx-swap-oob="true"`

*(2026-11-25)*

htmx 2 reads `hx-swap-oob` through `getAttribute(...) || getAttribute("data-…")`, so a bare attribute (what templ renders for `hx-swap-oob?={ oob }`) is the empty string, read as "no oob": the element is swapped in with the rest of the response instead. In the deployments poll that left duplicate ids inside the polled list. Use `{ swapOOB(oob)... }` (`templates/app_sheet.go`), which renders `hx-swap-oob="true"`. The canvas status poll's `canvasWhere` and node-chip counts had the bare form too, so on an open canvas "where it runs" and the node counts never followed the 10-second poll (only the badges, written by `ui.StateID` with `"true"`, did); they use `swapOOB` since 2026-11-26. Swap the smallest element that changes: the pending-change dots are per-tab slots (`tab-dot-<tab>`), because replacing the whole tab bar every 3 s would undo the user's sideways scroll of it on a phone.

### The sheet head and "runs on" share the deployments poll

On the deployments overview the sheet head's state (`AppStateBadge`) and the "runs on" list (`appRuns`, id `app-runs`) are both refreshed out of band by the deployments tab's 3-second poll (`DeploymentsPoll`), from one Swarm read (`Server.readAppLive` with tasks); the badge does not poll itself there (`AppCtx.headPollURL` is `""`). `ServiceState` and `ServiceTasks` list the tasks in two requests, so a task that starts or stops between them could still put "online" next to a lone "starting" row: `countTasks` recounts the running and failed replicas (and a global service's desired count) from the list the page shows; a replicated service read at zero whose tasks Swarm still wants running (a Start or Stop between the reads) counts those tasks as desired, so "stopped" never sits next to a running row. A service that is not there is not asked for tasks (the daemon answers "not found"); when a read fails, the poll leaves the head and the list as they are rather than show "no running replicas" next to "online". The first render of the tab uses the same read, and its controls and pending markers follow it (`c.App.Status`) rather than the canvas's earlier bulk read. On every other tab and on a deployment's log the badge still polls `…/status`, which uses `readAppLive` without tasks. Until 2026-11-26 the list was rendered once with the page while the head polled itself: a page opened during a restart (the new task "starting") kept that row for hours while the head moved on to "online" (seen on a production install). The first render takes the head's node badge from the same `displayAppStatus` the polls use. The list shows only tasks Swarm wants running (`ServiceTasks` filters `desired-state=running`), so it no longer prints a task's desired state ("→ running" repeated itself on every starting task); during a StartFirst update it shows the serving task and the starting one, ordered by `sortedTasks` (running first, then by node and task ID, so a poll does not reshuffle the rows). A crash loop between attempts lists no task (the next one waits out the restart delay with desired state `ready`): "no running replicas" next to the head's "deploying". The canvas card keeps its own 10-second poll (badge and placement in one response, read by separate Swarm calls), so it can trail the head by up to that period.

### templ component-on-its-own-line gotcha

A `@Component(...)` call inline after text renders as literal `@Component` text. Put component calls on their own node:
```
<div>
  @StatusBadge(status)
</div>
```

### Literal `@` between two templ `{ }` expressions breaks the parser

`@` starts a component-call token anywhere in templ text, not just right after other text — `{ a } @ { b }` fails to parse even though neither `a` nor `b` is a component. Build the whole string as one Go expression instead: `{ a + " @ " + b }` (see `database_detail.templ`'s `dbName @ instanceName` line).
