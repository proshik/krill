package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
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

	backupSvc     *backup.Service
	reloadBackups func()
}

func New(cfg config.Config, authSvc *auth.Service, orgSvc *org.Service, q *db.Queries, d *deploy.Deployer, e docker.Engine, hub *deploy.DeployLogHub, dbSvc *dbservice.Service) *Server {
	return &Server{cfg: cfg, auth: authSvc, org: orgSvc, q: q, deployer: d, engine: e, logHub: hub, dbsvc: dbSvc}
}

// SetBackups wires the backup service and a reload hook (re-reads the cron
// schedule) into the server without changing New's signature.
func (s *Server) SetBackups(svc *backup.Service, reload func()) {
	s.backupSvc = svc
	s.reloadBackups = reload
}

// Router assembles the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(csrfGuard)

	// locale middleware: picks the locale from the krill_lang cookie (set via
	// the Settings language selector), falling back to the default.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			loc := i18n.DefaultLocale
			if c, err := req.Cookie("krill_lang"); err == nil && i18n.Supported(c.Value) {
				loc = c.Value
			}
			ctx := i18n.WithLocale(req.Context(), loc)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(web.Static())))

	r.Get("/login", s.loginPage)
	r.Post("/login", s.loginSubmit)
	r.Get("/logout", s.logout)

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth(s.auth))
		r.Use(s.flashMiddleware)

		r.Get("/", s.home)
		r.Get("/orgs", s.listOrgs)
		r.Get("/orgs/switcher", s.orgSwitcher)
		r.Post("/orgs", s.createOrg)
		r.Post("/lang", s.setLanguage)

		r.Route("/orgs/{orgID}", func(r chi.Router) {
			r.Use(auth.RequireOrgMember(s.org))

			r.Get("/", s.orgDashboard)
			r.Get("/members", s.listMembers)
			r.Get("/destinations", s.listDestinations)
			r.Get("/registries", s.listRegistries)

			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole(auth.RoleAdmin))
				r.Post("/members", s.createMember)
				r.Post("/members/{mID}/role", s.updateMemberRole)
				r.Post("/members/{mID}/remove", s.removeMember)
				r.Post("/destinations", s.createDestination)
				r.Post("/destinations/{destID}/delete", s.deleteDestination)
				r.Post("/registries", s.createRegistry)
				r.Post("/registries/{regID}/delete", s.deleteRegistry)
				r.Post("/projects", s.createProject)
				r.Post("/projects/{projID}/delete", s.deleteProject)
				r.Post("/projects/{projID}/environments", s.createEnvironment)
				r.Post("/projects/{projID}/environments/{envID}/delete", s.deleteEnvironment)
				r.Post("/projects/{projID}/environments/{envID}/apps", s.createApp)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/delete", s.deleteApp)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/registry", s.setAppRegistry)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains", s.addDomain)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/tls", s.toggleDomainTLS)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/exposure", s.setDomainExposure)
				r.Post("/projects/{projID}/environments/{envID}/apps/{appID}/domains/{domainID}/delete", s.deleteDomain)
				r.Post("/projects/{projID}/environments/{envID}/databases", s.createDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/deploy", s.deployDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/start", s.startDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/stop", s.stopDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/version", s.versionDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/delete", s.deleteDatabase)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/backups", s.addBackup)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/backups/{backupID}/delete", s.deleteBackup)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/backups/{backupID}/toggle", s.toggleBackup)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/backups/{backupID}/run", s.runBackupNow)
				r.Post("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/backups/{backupID}/restore", s.restoreBackup)
				r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/backups/{backupID}/objects", s.backupObjects)
				r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/backups/{backupID}/download", s.downloadBackup)
			})

			r.Get("/projects/{projID}", s.projectPage)
			r.Get("/projects/{projID}/environments/{envID}", s.projectPage)
			r.Get("/projects/{projID}/environments/{envID}/statuses", s.envStatuses)

			r.Route("/projects/{projID}/environments/{envID}/apps/{appID}", func(r chi.Router) {
				r.Get("/", s.appDetail)
				r.Get("/status", s.appStatus)
				r.Get("/tags", s.appTags)
				r.Get("/logs", s.appLogs)
				r.Get("/deployments-list", s.listDeployments)
				r.Get("/deployments/{deployID}", s.deploymentLogPage)
				r.Get("/deployments/{deployID}/logs", s.deploymentLogWS)
				r.Group(func(r chi.Router) {
					r.Use(auth.RequireRole(auth.RoleAdmin))
					r.Post("/deploy", s.deployApp)
					r.Post("/rebuild", s.rebuildApp)
					r.Post("/reload", s.reloadApp)
					r.Post("/stop", s.stopApp)
					r.Post("/env", s.saveEnv)
					r.Post("/advanced", s.saveAdvanced)
				})
			})

			r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}", s.databaseDetail)
			r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/status", s.databaseStatus)
			r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/logs", s.databaseLogs)
			r.Get("/projects/{projID}/environments/{envID}/databases/{engine}/{dbID}/deploy-logs", s.databaseDeployLogs)
		})
	})

	return r
}
