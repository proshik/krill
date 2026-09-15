package server

import (
	"html/template"
	"net/http"
	"sync/atomic"

	"github.com/proshik/krill/internal/web/i18n"
)

// StartupPhase is what the process is busy with before its router is ready,
// shown on the starting page.
type StartupPhase int

const (
	// PhaseMigrating: database migrations are being applied. The zero value, so
	// a fresh gate reports it before anyone sets a phase.
	PhaseMigrating StartupPhase = iota
	// PhaseStarting: services are being wired.
	PhaseStarting
	// PhaseGateway: the Traefik gateway is being reconciled, which can take
	// minutes when its image has to be pulled.
	PhaseGateway
)

// messageKey is the i18n key describing the phase.
func (p StartupPhase) messageKey() string {
	switch p {
	case PhaseStarting:
		return "startup.starting"
	case PhaseGateway:
		return "startup.gateway"
	default:
		return "startup.migrating"
	}
}

// StartupGate is the listener's handler from the moment the port opens. Until
// the real router is swapped in, it answers every request — any method, any
// path — with a small self-refreshing "Krill is starting" page and a 503, so a
// browser (or the Updates page polling across a restart) sees progress instead
// of "connection refused". After Swap it delegates every request to the router.
//
// The page refreshes itself with a script that polls its own URL and reloads
// once anything answers, rather than with a meta refresh: a meta refresh that
// hits a refused connection — a new binary crash-looping through its restart —
// leaves the browser on its own error page, which never retries. The meta
// refresh stays only as the fallback for a browser without JS. A 502 or 504 is
// not an answer from Krill: through the panel domain Traefik returns those
// while the process is down, and reloading onto Traefik's error page would end
// the polling, so the script keeps polling. A 503 is this gate's own page.
//
// The gate logs nothing per request: the gateway polls the provider endpoint
// every few seconds during startup, and the access log belongs to the router.
type StartupGate struct {
	real  atomic.Pointer[http.Handler]
	phase atomic.Int32
}

// NewStartupGate returns a gate serving the starting page in PhaseMigrating.
func NewStartupGate() *StartupGate { return &StartupGate{} }

// SetPhase changes the phase the starting page reports.
func (g *StartupGate) SetPhase(p StartupPhase) { g.phase.Store(int32(p)) }

// Swap hands every subsequent request to h, which must not be nil. It is safe
// to call while requests are being served; the server's Handler field is never
// reassigned after Serve started.
func (g *StartupGate) Swap(h http.Handler) { g.real.Store(&h) }

// ServeHTTP delegates to the swapped-in handler, or serves the starting page.
func (g *StartupGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := g.real.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}

	loc := requestLocale(r)
	ctx := i18n.WithLocale(r.Context(), loc)
	page := startupPageData{
		Lang:    loc,
		Title:   i18n.T(ctx, "startup.title"),
		Message: i18n.T(ctx, StartupPhase(g.phase.Load()).messageKey()),
		Hint:    i18n.T(ctx, "startup.hint"),
		// Reloading the answer to a form POST would make the browser ask to
		// resubmit it on every poll; that page follows its URL with a GET.
		Reload: r.Method == http.MethodGet || r.Method == http.MethodHead,
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", "3")
	// htmx 2 acts on HX-Refresh before it looks at the status, so a polling
	// fragment or a boosted click becomes a full load of this page instead of
	// being swapped into a piece of the old one.
	if r.Header.Get("HX-Request") == "true" {
		h.Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	// The template is static and the data plain strings, so the only error left
	// is a write to a client that went away — nothing to act on, and the gate
	// does not log per request.
	_ = startupPage.Execute(w, page)
}

type startupPageData struct {
	Lang, Title, Message, Hint string
	// Reload: the page may reload itself, because the request it answered
	// was a GET or HEAD.
	Reload bool
}

// startupPage is self-contained on purpose: while the gate is up, nothing else
// is served — not even /static.
var startupPage = template.Must(template.New("startup").Parse(`<!doctype html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<noscript><meta http-equiv="refresh" content="3"></noscript>
<title>{{.Title}}</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;background:#f8fafc;color:#0f172a}
main{max-width:32rem;padding:2rem;text-align:center}
h1{margin:0 0 .75rem;font-size:1.5rem;font-weight:600}
p{margin:.25rem 0;line-height:1.5}
.hint{color:#64748b;font-size:.875rem}
@media (prefers-color-scheme:dark){body{background:#0f172a;color:#e2e8f0}.hint{color:#94a3b8}}
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
<p class="hint">{{.Hint}}</p>
</main>
<script>
(function () {
  var leaving = false;
  setInterval(function () {
    if (leaving) return;
    fetch(location.href, {cache: "no-store"}).then(function (res) {
      if (leaving) return;
      if (res.status === 502 || res.status === 504) return;
      leaving = true;
      {{if .Reload}}location.reload();{{else}}location.replace(location.href.split("#")[0]);{{end}}
    }, function () {});
  }, 3000);
})();
</script>
</body>
</html>
`))
