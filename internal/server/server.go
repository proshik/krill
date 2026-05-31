package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/web"
)

// Server держит зависимости HTTP-слоя.
type Server struct {
	cfg      config.Config
	auth     *auth.Service
	q        *db.Queries
	deployer *deploy.Deployer
	engine   docker.Engine
}

func New(cfg config.Config, authSvc *auth.Service, q *db.Queries, d *deploy.Deployer, e docker.Engine) *Server {
	return &Server{cfg: cfg, auth: authSvc, q: q, deployer: d, engine: e}
}

// Router собирает chi-роутер.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(web.Static())))

	r.Get("/login", s.loginPage)
	r.Post("/login", s.loginSubmit)
	r.Get("/logout", s.logout)

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth(s.auth))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/apps", http.StatusSeeOther)
		})
		r.Get("/apps", s.listApps)
		r.Get("/apps/new", s.newApp)
		r.Post("/apps", s.createApp)
		r.Get("/apps/{id}", s.appDetail)
		r.Get("/apps/{id}/status", s.appStatus)
		r.Post("/apps/{id}/deploy", s.deployApp)
		r.Get("/ws/apps/{id}/logs", s.appLogs)
	})

	return r
}
