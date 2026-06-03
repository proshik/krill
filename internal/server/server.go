package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/web"
	"github.com/proshik/krill/internal/web/i18n"
)

// Server holds the HTTP layer dependencies.
type Server struct {
	cfg      config.Config
	auth     *auth.Service
	org      *org.Service
	q        *db.Queries
	deployer *deploy.Deployer
	engine   docker.Engine
	logHub   *deploy.DeployLogHub
	dbsvc    *dbservice.Service
}

func New(cfg config.Config, authSvc *auth.Service, orgSvc *org.Service, q *db.Queries, d *deploy.Deployer, e docker.Engine, hub *deploy.DeployLogHub, dbSvc *dbservice.Service) *Server {
	return &Server{cfg: cfg, auth: authSvc, org: orgSvc, q: q, deployer: d, engine: e, logHub: hub, dbsvc: dbSvc}
}

// Router assembles the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)

	// locale middleware: puts the default locale into the request context.
	// Currently a no-op (always en); an extension point for cookie/Accept-Language.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := i18n.WithLocale(req.Context(), i18n.DefaultLocale)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(web.Static())))

	r.Get("/login", s.loginPage)
	r.Post("/login", s.loginSubmit)
	r.Get("/logout", s.logout)

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth(s.auth))

		r.Get("/", s.home)
		r.Get("/orgs", s.listOrgs)
		r.Post("/orgs", s.createOrg)

		r.Route("/orgs/{orgID}", func(r chi.Router) {
			r.Use(auth.RequireOrgMember(s.org))

			r.Get("/", s.orgDashboard)
			r.Get("/members", s.listMembers)
			r.Get("/destinations", s.listDestinations)

			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole(auth.RoleAdmin))
				r.Post("/members", s.createMember)
				r.Post("/members/{mID}/role", s.updateMemberRole)
				r.Post("/members/{mID}/remove", s.removeMember)
					r.Post("/destinations", s.createDestination)
					r.Post("/destinations/{destID}/delete", s.deleteDestination)
				r.Post("/projects", s.createProject)
				r.Post("/projects/{projID}/delete", s.deleteProject)
				r.Post("/projects/{projID}/environments", s.createEnvironment)
				r.Post("/projects/{projID}/environments/{envID}/delete", s.deleteEnvironment)
				r.Post("/projects/{projID}/environments/{envID}/apps", s.createApp)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/delete", s.deleteApp)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains", s.addDomain)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/tls", s.toggleDomainTLS)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/delete", s.deleteDomain)
				r.Post("/projects/{projID}/environments/{envID}/databases", s.createDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/deploy", s.deployDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/start", s.startDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/stop", s.stopDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/version", s.versionDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/delete", s.deleteDatabase)
			})

			r.Get("/projects/{projID}", s.projectPage)
			r.Get("/projects/{projID}/environments/{envID}", s.projectPage)

			r.Route("/projects/{projID}/environments/{envID}/apps/{appID}", func(r chi.Router) {
				r.Get("/", s.appDetail)
				r.Get("/status", s.appStatus)
				r.Post("/deploy", s.deployApp)
				r.Post("/env", s.saveEnv)
				r.Get("/logs", s.appLogs)
				r.Get("/deployments-list", s.listDeployments)
				r.Get("/deployments/{deployID}", s.deploymentLogPage)
				r.Get("/deployments/{deployID}/logs", s.deploymentLogWS)
			})

			r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}", s.databaseDetail)
			r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/status", s.databaseStatus)
			r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/logs", s.databaseLogs)
			r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/deploy-logs", s.databaseDeployLogs)
		})
	})

	return r
}
